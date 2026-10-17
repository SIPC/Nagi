// Package api 实现浏览器调用的 Connect 接口，以及节点加入用的 EnrollService。
package api

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SettingSetupToken 是面板初始化令牌哈希在 settings 表里的键。
const SettingSetupToken = "setup_token"

// API 汇总所有 Connect 接口的实现。
type API struct {
	Q      *store.Queries
	Auth   *auth.Service
	Nodes  *nodes.Registry
	CA     *tlsutil.CA
	Sealer *sealer.Sealer
	Panel  *PanelInfo
	Log    *slog.Logger
}

// Mount 把所有接口挂到 mux 上。
func (a *API) Mount(mux *http.ServeMux) {
	interceptors := connect.WithInterceptors(a.Auth.Interceptor(
		nagiv1connect.SetupServiceGetSetupStateProcedure,
		nagiv1connect.SetupServiceSetupProcedure,
		nagiv1connect.AuthServiceLoginProcedure,
		nagiv1connect.AuthServiceLogoutProcedure,
		nagiv1connect.EnrollServiceEnrollProcedure,
	))
	mux.Handle(nagiv1connect.NewSetupServiceHandler(&setupService{a}, interceptors))
	mux.Handle(nagiv1connect.NewAuthServiceHandler(&authService{a}, interceptors))
	mux.Handle(nagiv1connect.NewNodeServiceHandler(&nodeService{a}, interceptors))
	mux.Handle(nagiv1connect.NewProjectServiceHandler(&projectService{a}, interceptors))
	mux.Handle(nagiv1connect.NewEnrollServiceHandler(&enrollService{a}, interceptors))
	mux.Handle(nagiv1connect.NewSettingsServiceHandler(&settingsService{a}, interceptors))
	mux.Handle("/hooks/", a.WebhookHandler())
	mux.Handle("/ws/exec", a.TerminalHandler())
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
	want, err := s.Q.GetSetting(ctx, SettingSetupToken)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("初始化令牌无效，请使用启动日志里的初始化链接"))
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
	cookie, err := s.Auth.Login(ctx, auth.ClientIP(req.Peer().Addr), strings.TrimSpace(req.Msg.GetUsername()), req.Msg.GetPassword())
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
	return connect.NewResponse(&nagiv1.MeResponse{Username: auth.Username(ctx)}), nil
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
		n := &nagiv1.Node{Id: row.ID, Name: row.Name, Online: s.Nodes.Online(row.ID)}
		if row.LastSeen > 0 {
			n.LastSeen = timestamppb.New(time.Unix(row.LastSeen, 0))
		}
		if conn, err := s.Nodes.Get(row.ID); err == nil && conn.LastReport() != nil {
			n.Info = conn.LastReport().GetNode()
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

// RemoveNode 移除节点：标记为已移除（之后拒绝它连接），删掉它的配置备份和 webhook，断开当前连接。
// 节点上的项目照常运行。以后用新的加入令牌重新加入时会恢复。
func (s *nodeService) RemoveNode(ctx context.Context, req *connect.Request[nagiv1.RemoveNodeRequest]) (*connect.Response[nagiv1.RemoveNodeResponse], error) {
	id := req.Msg.GetNodeId()
	if _, err := s.Q.GetNode(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("节点不存在"))
	} else if err != nil {
		return nil, err
	}
	if err := s.Q.RevokeNode(ctx, id); err != nil {
		return nil, err
	}
	_ = s.Q.DeleteNodeData(ctx, id)
	_ = s.Q.DeleteNodeWebhooks(ctx, id)
	s.Nodes.Disconnect(id)
	s.Log.Info("节点已移除", "node", id)
	return connect.NewResponse(&nagiv1.RemoveNodeResponse{}), nil
}

// ---- 项目：转发给节点 ----

type projectService struct{ *API }

func (s *projectService) agent(nodeID string) (nagiv1.AgentServiceClient, error) {
	conn, err := s.Nodes.Get(nodeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return conn.Agent, nil
}

func (s *projectService) ListAllProjects(ctx context.Context, _ *connect.Request[nagiv1.ListAllProjectsRequest]) (*connect.Response[nagiv1.ListAllProjectsResponse], error) {
	rows, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListAllProjectsResponse{}
	for _, row := range rows {
		// 在线节点用最近一次上报；离线节点用面板上的配置备份（没有运行状态）。
		if conn, err := s.Nodes.Get(row.ID); err == nil && conn.LastReport() != nil {
			for _, p := range conn.LastReport().GetProjects() {
				out.Projects = append(out.Projects, &nagiv1.NodeProject{NodeId: row.ID, Project: p})
			}
			continue
		}
		backups, err := s.Q.ListProjectBackups(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		for _, b := range backups {
			plain, err := s.Sealer.Open(b.Data, nodes.BackupContext(b.NodeID, b.ProjectID))
			if err != nil {
				s.Log.Warn("无法解密项目配置备份", "node", b.NodeID, "project", b.ProjectID, "err", err)
				continue
			}
			p := &nagiv1.Project{}
			if proto.Unmarshal(plain, p) == nil {
				out.Projects = append(out.Projects, &nagiv1.NodeProject{NodeId: row.ID, Project: &nagiv1.ProjectSummary{Project: p}})
			}
		}
	}
	return connect.NewResponse(out), nil
}

func (s *projectService) CreateNodeProject(ctx context.Context, req *connect.Request[nagiv1.CreateNodeProjectRequest]) (*connect.Response[nagiv1.CreateNodeProjectResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.CreateProject(ctx, &nagiv1.CreateProjectRequest{
		Name: req.Msg.GetName(), Dir: req.Msg.GetDir(), Config: req.Msg.GetConfig(),
	})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.CreateNodeProjectResponse{Project: resp.GetProject()}), nil
}

func (s *projectService) GetNodeProject(ctx context.Context, req *connect.Request[nagiv1.GetNodeProjectRequest]) (*connect.Response[nagiv1.GetNodeProjectResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.GetProject(ctx, &nagiv1.GetProjectRequest{Id: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.GetNodeProjectResponse{Project: resp.GetProject()}), nil
}

func (s *projectService) SaveNodeProjectDraft(ctx context.Context, req *connect.Request[nagiv1.SaveNodeProjectDraftRequest]) (*connect.Response[nagiv1.SaveNodeProjectDraftResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.SaveDraft(ctx, &nagiv1.SaveDraftRequest{
		Id: req.Msg.GetProjectId(), BaseRevision: req.Msg.GetBaseRevision(), Config: req.Msg.GetConfig(),
	})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.SaveNodeProjectDraftResponse{Project: resp.GetProject()}), nil
}

func (s *projectService) DiscardNodeProjectDraft(ctx context.Context, req *connect.Request[nagiv1.DiscardNodeProjectDraftRequest]) (*connect.Response[nagiv1.DiscardNodeProjectDraftResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.DiscardDraft(ctx, &nagiv1.DiscardDraftRequest{
		Id: req.Msg.GetProjectId(), BaseRevision: req.Msg.GetBaseRevision(),
	})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.DiscardNodeProjectDraftResponse{Project: resp.GetProject()}), nil
}

func (s *projectService) SaveNodeProjectRoute(ctx context.Context, req *connect.Request[nagiv1.SaveNodeProjectRouteRequest]) (*connect.Response[nagiv1.SaveNodeProjectRouteResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.SaveRoute(ctx, &nagiv1.SaveRouteRequest{ProjectId: req.Msg.GetProjectId(), Route: req.Msg.GetRoute()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.SaveNodeProjectRouteResponse{Project: resp.GetProject()}), nil
}

func (s *projectService) DeleteNodeProjectRoute(ctx context.Context, req *connect.Request[nagiv1.DeleteNodeProjectRouteRequest]) (*connect.Response[nagiv1.DeleteNodeProjectRouteResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.DeleteRoute(ctx, &nagiv1.DeleteRouteRequest{ProjectId: req.Msg.GetProjectId(), Name: req.Msg.GetName()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(&nagiv1.DeleteNodeProjectRouteResponse{Project: resp.GetProject()}), nil
}

func (s *projectService) ListNodeProjectVersions(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.ListVersionsResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListVersions(ctx, &nagiv1.ListVersionsRequest{Id: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *projectService) ListNodeProjectSnapshots(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef]) (*connect.Response[nagiv1.ListSnapshotsResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	resp, err := agent.ListSnapshots(ctx, &nagiv1.ListSnapshotsRequest{Id: req.Msg.GetProjectId()})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

// forwardEvents 把节点返回的事件流转发给浏览器。
func forwardEvents(ctx context.Context, recv func() (*nagiv1.DeployEvent, error), out *connect.ServerStream[nagiv1.DeployEvent]) error {
	for {
		ev, err := recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fromGRPC(err)
		}
		if err := out.Send(ev); err != nil {
			return err
		}
	}
}

func (s *projectService) RollbackNodeProject(ctx context.Context, req *connect.Request[nagiv1.RollbackNodeProjectRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := agent.Rollback(ctx, &nagiv1.RollbackRequest{Id: req.Msg.GetProjectId(), Version: req.Msg.GetVersion(), RestoreData: req.Msg.GetRestoreData()})
	if err != nil {
		return fromGRPC(err)
	}
	return forwardEvents(ctx, stream.Recv, out)
}

func (s *projectService) CreateNodeProjectSnapshot(ctx context.Context, req *connect.Request[nagiv1.NodeProjectRef], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := agent.CreateSnapshot(ctx, &nagiv1.CreateSnapshotRequest{Id: req.Msg.GetProjectId()})
	if err != nil {
		return fromGRPC(err)
	}
	return forwardEvents(ctx, stream.Recv, out)
}

func (s *projectService) RestoreNodeProjectSnapshot(ctx context.Context, req *connect.Request[nagiv1.RestoreNodeProjectSnapshotRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := agent.RestoreSnapshot(ctx, &nagiv1.RestoreSnapshotRequest{Id: req.Msg.GetProjectId(), SnapshotId: req.Msg.GetSnapshotId()})
	if err != nil {
		return fromGRPC(err)
	}
	return forwardEvents(ctx, stream.Recv, out)
}

func (s *projectService) DeleteNodeProject(ctx context.Context, req *connect.Request[nagiv1.DeleteNodeProjectRequest]) (*connect.Response[nagiv1.DeleteNodeProjectResponse], error) {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return nil, err
	}
	if _, err := agent.DeleteProject(ctx, &nagiv1.DeleteProjectRequest{
		Id: req.Msg.GetProjectId(), BaseRevision: req.Msg.GetBaseRevision(),
		DeleteDir: req.Msg.GetDeleteDir(), ConfirmName: req.Msg.GetConfirmName(),
	}); err != nil {
		return nil, fromGRPC(err)
	}
	_ = s.Q.DeleteProjectBackup(ctx, store.DeleteProjectBackupParams{NodeID: req.Msg.GetNodeId(), ProjectID: req.Msg.GetProjectId()})
	_ = s.Q.DeleteProjectWebhook(ctx, store.DeleteProjectWebhookParams{NodeID: req.Msg.GetNodeId(), ProjectID: req.Msg.GetProjectId()})
	s.Log.Info("项目已删除", "node", req.Msg.GetNodeId(), "project", req.Msg.GetProjectId(), "delete_dir", req.Msg.GetDeleteDir())
	return connect.NewResponse(&nagiv1.DeleteNodeProjectResponse{}), nil
}

func (s *projectService) DeployNodeProject(ctx context.Context, req *connect.Request[nagiv1.DeployNodeProjectRequest], out *connect.ServerStream[nagiv1.DeployEvent]) error {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := agent.DeployProject(ctx, &nagiv1.DeployProjectRequest{
		Id: req.Msg.GetProjectId(), BaseRevision: req.Msg.GetBaseRevision(), Pull: req.Msg.GetPull(),
	})
	if err != nil {
		return fromGRPC(err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fromGRPC(err)
		}
		if err := out.Send(ev); err != nil {
			return err
		}
	}
}

func (s *projectService) StreamNodeProjectLogs(ctx context.Context, req *connect.Request[nagiv1.StreamNodeProjectLogsRequest], out *connect.ServerStream[nagiv1.LogLine]) error {
	agent, err := s.agent(req.Msg.GetNodeId())
	if err != nil {
		return err
	}
	stream, err := agent.StreamLogs(ctx, &nagiv1.StreamLogsRequest{
		Id: req.Msg.GetProjectId(), Service: req.Msg.GetService(), Tail: req.Msg.GetTail(), Follow: req.Msg.GetFollow(),
	})
	if err != nil {
		return fromGRPC(err)
	}
	for {
		line, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fromGRPC(err)
		}
		if err := out.Send(line); err != nil {
			return err
		}
	}
}

// ---- 节点加入 ----

type enrollService struct{ *API }

func (s *enrollService) Enroll(ctx context.Context, req *connect.Request[nagiv1.EnrollRequest]) (*connect.Response[nagiv1.EnrollResponse], error) {
	now := time.Now().Unix()
	n, err := s.Q.UseEnrollToken(ctx, store.UseEnrollTokenParams{
		UsedAt:    sql.NullInt64{Int64: now, Valid: true},
		TokenHash: auth.HashToken(req.Msg.GetToken()),
		ExpiresAt: now,
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("加入令牌无效、已使用或已过期"))
	}
	if len(req.Msg.GetWireguardPublicKey()) != 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("WireGuard 公钥无效"))
	}
	csr, err := x509.ParseCertificateRequest(req.Msg.GetCsr())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("证书签名请求无效: %w", err))
	}
	// 节点 ID 由节点的身份公钥算出：同一个节点加入不同的面板，ID 都一样。
	nodeID := nodes.NodeID(csr.RawSubjectPublicKeyInfo)

	if n, err := s.Q.GetNode(ctx, nodeID); err == nil && n.Revoked != 0 {
		// 已移除的节点用新令牌重新加入：恢复。
		if err := s.Q.UnrevokeNode(ctx, nodeID); err != nil {
			return nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		name := strings.TrimSpace(req.Msg.GetHostname())
		if name == "" {
			name = nodeID
		}
		if err := s.Q.CreateNode(ctx, store.CreateNodeParams{ID: nodeID, Name: name, CreatedAt: now}); err != nil {
			return nil, err
		}
	} else if err != nil {
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
func fromGRPC(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	return connect.NewError(connect.Code(st.Code()), errors.New(st.Message()))
}
