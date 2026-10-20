// Package api 实现浏览器调用的 Connect 接口，以及节点加入用的 EnrollService。
package api

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/projects"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxRequestBytes 是接口请求体（解压后）的上限。compose、环境变量这些都远小于它。
const maxRequestBytes = 4 << 20

// SetupTokenTTL 是面板初始化令牌的有效期。
const SetupTokenTTL = 24 * time.Hour

// SettingSetupToken 是面板初始化令牌哈希在 settings 表里的键。
const SettingSetupToken = "setup_token"

// API 汇总所有 Connect 接口的实现。
type API struct {
	Q        *store.Queries
	Auth     *auth.Service
	Nodes    *nodes.Registry
	Projects *projects.Service
	CA       *tlsutil.CA
	Sealer   *sealer.Sealer
	Panel    *PanelInfo
	Log      *slog.Logger
}

// Mount 把所有接口挂到 mux 上。
func (a *API) Mount(mux *http.ServeMux) {
	chain := []connect.Interceptor{a.Auth.Interceptor(
		nagiv1connect.SetupServiceGetSetupStateProcedure,
		nagiv1connect.SetupServiceSetupProcedure,
		nagiv1connect.AuthServiceLoginProcedure,
		nagiv1connect.AuthServiceLogoutProcedure,
		nagiv1connect.EnrollServiceEnrollProcedure,
	)}
	if a.Panel.Standby != nil {
		chain = append(chain, readOnly{a})
	}
	interceptors := connect.WithHandlerOptions(
		connect.WithInterceptors(chain...),
		// 请求体在检查登录之前就会被读进内存并解析（包括解压后的大小），所以要限制大小。
		// 文件上传走 /api/upload，不经过这里。
		connect.WithReadMaxBytes(maxRequestBytes),
	)
	mux.Handle(nagiv1connect.NewSetupServiceHandler(&setupService{a}, interceptors))
	mux.Handle(nagiv1connect.NewAuthServiceHandler(&authService{a}, interceptors))
	mux.Handle(nagiv1connect.NewNodeServiceHandler(&nodeService{a}, interceptors))
	mux.Handle(nagiv1connect.NewProjectServiceHandler(&projectService{a}, interceptors))
	mux.Handle(nagiv1connect.NewEnrollServiceHandler(&enrollService{a}, interceptors))
	mux.Handle(nagiv1connect.NewSettingsServiceHandler(&settingsService{a}, interceptors))
	mux.Handle("/hooks/", a.standbyBlock(a.WebhookHandler()))
	mux.Handle("/ws/exec", a.standbyBlock(a.TerminalHandler()))
	mux.Handle("POST /api/upload", a.standbyBlock(a.UploadHandler()))
	mux.Handle("GET /install.sh", a.InstallHandler())
	mux.Handle("GET /download/", a.DownloadHandler())
}

// ---- 初始化 ----

type setupService struct{ *API }

func (s *setupService) GetSetupState(ctx context.Context, _ *connect.Request[nagiv1.GetSetupStateRequest]) (*connect.Response[nagiv1.GetSetupStateResponse], error) {
	ok, err := s.Auth.HasUsers(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.GetSetupStateResponse{Initialized: ok}), nil
}

func (s *setupService) Setup(ctx context.Context, req *connect.Request[nagiv1.SetupRequest]) (*connect.Response[nagiv1.SetupResponse], error) {
	if ok, err := s.Auth.HasUsers(ctx); err != nil {
		return nil, err
	} else if ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("面板已经初始化过了"))
	}
	stored, err := s.Q.GetSetting(ctx, SettingSetupToken)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("初始化令牌无效，请使用启动日志里的初始化链接"))
	}
	want, expires, _ := strings.Cut(stored, ":")
	if exp, _ := strconv.ParseInt(expires, 10, 64); time.Now().Unix() > exp {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("初始化链接已过期（24 小时内有效），重启面板获取新的链接"))
	}
	got := auth.HashToken(req.Msg.GetToken())
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("初始化令牌无效，请使用启动日志里的初始化链接"))
	}
	username := strings.TrimSpace(req.Msg.GetUsername())
	if username == "" || utf8.RuneCountInString(username) > 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("用户名不能为空，且不超过 32 个字符"))
	}
	if utf8.RuneCountInString(req.Msg.GetPassword()) < 8 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("密码至少 8 个字符"))
	}
	userID, err := s.Auth.CreateUser(ctx, username, req.Msg.GetPassword())
	if err != nil {
		return nil, err
	}
	_ = s.Q.DeleteSetting(ctx, SettingSetupToken)
	cookie, err := s.Auth.NewSession(ctx, userID)
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(&nagiv1.SetupResponse{})
	resp.Header().Set("Set-Cookie", cookie.String())
	s.Log.Info("面板已完成初始化", "user", username)
	return resp, nil
}

// ---- 登录 ----

type authService struct{ *API }

func (s *authService) Login(ctx context.Context, req *connect.Request[nagiv1.LoginRequest]) (*connect.Response[nagiv1.LoginResponse], error) {
	cookie, err := s.Auth.Login(ctx, auth.RequestIP(req.Peer().Addr, req.Header()), strings.TrimSpace(req.Msg.GetUsername()), req.Msg.GetPassword(), req.Msg.GetTotpCode())
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(&nagiv1.LoginResponse{})
	resp.Header().Set("Set-Cookie", cookie.String())
	return resp, nil
}

func (s *authService) Logout(ctx context.Context, req *connect.Request[nagiv1.LogoutRequest]) (*connect.Response[nagiv1.LogoutResponse], error) {
	cookie := s.Auth.Logout(ctx, auth.SessionToken(req.Header()))
	resp := connect.NewResponse(&nagiv1.LogoutResponse{})
	resp.Header().Set("Set-Cookie", cookie.String())
	return resp, nil
}

func (s *authService) Me(ctx context.Context, _ *connect.Request[nagiv1.MeRequest]) (*connect.Response[nagiv1.MeResponse], error) {
	u := auth.CurrentUser(ctx)
	return connect.NewResponse(&nagiv1.MeResponse{Username: u.Name, TotpEnabled: s.Auth.TOTPEnabled(ctx, u.ID)}), nil
}

func (s *authService) ChangePassword(ctx context.Context, req *connect.Request[nagiv1.ChangePasswordRequest]) (*connect.Response[nagiv1.ChangePasswordResponse], error) {
	u := auth.CurrentUser(ctx)
	if err := s.Auth.ChangePassword(ctx, u.ID, auth.SessionToken(req.Header()), req.Msg.GetCurrent(), req.Msg.GetNewPassword()); err != nil {
		return nil, err
	}
	s.Log.Info("密码已修改", "user", u.Name)
	return connect.NewResponse(&nagiv1.ChangePasswordResponse{}), nil
}

// ---- 节点 ----

type nodeService struct{ *API }

func (s *nodeService) ListNodes(ctx context.Context, _ *connect.Request[nagiv1.ListNodesRequest]) (*connect.Response[nagiv1.ListNodesResponse], error) {
	rows, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListNodesResponse{}
	for _, row := range rows {
		n := &nagiv1.Node{Id: row.ID, Name: row.Name, Online: s.Nodes.Online(row.ID), Pending: row.Pending != 0}
		if row.LastSeen > 0 {
			n.LastSeen = timestamppb.New(time.Unix(row.LastSeen, 0))
		}
		if c, err := s.Q.CountNodeContainers(ctx, row.ID); err == nil {
			n.Containers = int32(c)
		}
		if conn, err := s.Nodes.Peek(row.ID); err == nil && conn.LastReport() != nil {
			n.Info = conn.LastReport().GetNode()
			n.Usage = conn.LastReport().GetUsage()
			if ip := conn.Remote(); ip.IsValid() && !ip.IsLoopback() && !n.Info.GetEmbedded() {
				n.Address = ip.String()
			} else {
				n.Address = s.Panel.PublicAddress(ctx)
			}
		} else if len(row.Info) > 0 {
			info := &nagiv1.NodeInfo{}
			if proto.Unmarshal(row.Info, info) == nil {
				n.Info = info
			}
		}
		out.Nodes = append(out.Nodes, n)
	}
	return connect.NewResponse(out), nil
}

// ApproveNode 批准用接入码加入的节点。
func (s *nodeService) ApproveNode(ctx context.Context, req *connect.Request[nagiv1.ApproveNodeRequest]) (*connect.Response[nagiv1.ApproveNodeResponse], error) {
	if err := s.Nodes.Approve(ctx, req.Msg.GetNodeId()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&nagiv1.ApproveNodeResponse{}), nil
}

// QueryNodeMetrics 查询节点上的监控数据，监控数据存在节点本地。
func (s *nodeService) QueryNodeMetrics(ctx context.Context, req *connect.Request[nagiv1.QueryNodeMetricsRequest]) (*connect.Response[nagiv1.QueryMetricsResponse], error) {
	conn, err := s.Nodes.Get(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	resp, err := conn.Agent.QueryMetrics(ctx, &nagiv1.QueryMetricsRequest{
		Prefix: req.Msg.GetPrefix(), From: req.Msg.GetFrom(), To: req.Msg.GetTo(),
	})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

// RemoveNode 移除节点：标记为已移除（之后拒绝它连接），断开当前连接。节点上还有容器时拒绝。
// 以后用新的加入令牌重新加入时会恢复。
func (s *nodeService) RemoveNode(ctx context.Context, req *connect.Request[nagiv1.RemoveNodeRequest]) (*connect.Response[nagiv1.RemoveNodeResponse], error) {
	id := req.Msg.GetNodeId()
	if _, err := s.Q.GetNode(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("节点不存在"))
	} else if err != nil {
		return nil, err
	}
	if n, err := s.Q.CountNodeContainers(ctx, id); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("节点上还有 %d 个容器，请先把它们迁移到其他节点或者删除", n))
	}
	if err := s.forgetNode(ctx, id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.RemoveNodeResponse{}), nil
}

// forgetNode 让面板忘记节点：拒绝它再连接。
func (s *nodeService) forgetNode(ctx context.Context, id string) error {
	if err := s.Q.RevokeNode(ctx, id); err != nil {
		return err
	}
	s.Nodes.Disconnect(id)
	s.Log.Info("节点已移除", "node", id)
	return nil
}

// ---- 节点加入 ----

type enrollService struct{ *API }

func (s *enrollService) Enroll(ctx context.Context, req *connect.Request[nagiv1.EnrollRequest]) (*connect.Response[nagiv1.EnrollResponse], error) {
	// 先检查请求本身，全部合格再消耗令牌、改数据库：无效的请求不能用掉令牌，也不能借令牌恢复别的已移除节点。
	meshKey := req.Msg.GetWireguardPublicKey()
	if len(meshKey) != 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("WireGuard 公钥无效"))
	}
	csr, err := x509.ParseCertificateRequest(req.Msg.GetCsr())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("证书签名请求无效: %w", err))
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("证书签名请求的签名无效: %w", err))
	}
	// 节点 ID 由节点的身份公钥算出：同一个节点加入不同的面板，ID 都一样。
	nodeID := nodes.NodeID(csr.RawSubjectPublicKeyInfo)
	if owner, err := s.Q.MeshKeyOwner(ctx, store.MeshKeyOwnerParams{MeshKey: meshKey, ID: nodeID}); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("这个 WireGuard 公钥已经属于节点 %s", owner))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := time.Now().Unix()
	multiUse, err := s.Q.UseEnrollToken(ctx, store.UseEnrollTokenParams{
		UsedAt:    sql.NullInt64{Int64: now, Valid: true},
		TokenHash: auth.HashToken(req.Msg.GetToken()),
		ExpiresAt: now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("加入令牌无效、已使用或已过期"))
	} else if err != nil {
		return nil, err
	}
	// 用接入码（可以多次使用，经过别的面板转交）加入的节点要管理员批准；用安装命令加入的直接可用。
	pending := multiUse
	if n, err := s.Q.GetNode(ctx, nodeID); err == nil {
		if n.Revoked == 0 && n.Pending == 0 {
			pending = 0 // 已经批准过的节点重新加入，不用再批准
		}
		if err := s.Q.SetNodeEnrollment(ctx, store.SetNodeEnrollmentParams{MeshKey: meshKey, Pending: pending, ID: nodeID}); err != nil {
			return nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		name := strings.TrimSpace(req.Msg.GetHostname())
		if name == "" {
			name = nodeID
		}
		if err := s.Q.CreateNode(ctx, store.CreateNodeParams{ID: nodeID, Name: name, CreatedAt: now, MeshKey: meshKey, Pending: pending}); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	cert, err := s.CA.SignCSR(req.Msg.GetCsr(), nodeID, nodes.NodeCertValidity)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	s.Log.Info("节点已加入", "node", nodeID, "hostname", req.Msg.GetHostname())
	return connect.NewResponse(&nagiv1.EnrollResponse{
		NodeId: nodeID, Certificate: cert.Raw, CaCertificate: s.CA.Cert.Raw,
	}), nil
}

// ---- 错误转换 ----

// fromGRPC 把节点返回的 gRPC 错误转换成 Connect 错误。两者的错误码编号相同。
// fromStream 转换节点的流式调用（部署、迁移等）中途返回的错误。连接在中途断了（节点重启、网络中断）时，
// 操作可能没做完也可能已经做完，gRPC 的原始信息（"error reading from server: EOF"）用户看不懂，换成说明。
func fromStream(err error) error {
	if status.Code(err) == codes.Unavailable {
		return connect.NewError(connect.CodeUnavailable, errors.New("和节点的连接在操作过程中断开了，操作可能没做完，也可能已经做完。等节点重新连上后，看一下项目的状态和部署记录"))
	}
	return fromGRPC(err)
}

func fromGRPC(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	return connect.NewError(connect.Code(st.Code()), errors.New(st.Message()))
}
