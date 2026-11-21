// Package api 实现浏览器调用的 Connect 接口，以及节点加入用的 EnrollService。
package api

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/release"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/oauth"
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

// maxAvatarBytes 是头像的大小上限。界面上传前会缩小到很小的尺寸，这里只防止绕过界面塞大文件。
const maxAvatarBytes = 256 << 10

// SetupTokenTTL 是面板初始化令牌的有效期。
const SetupTokenTTL = 24 * time.Hour

// SettingSetupToken 是面板初始化令牌哈希在 settings 表里的键。
const SettingSetupToken = "setup_token"

// API 汇总所有 Connect 接口的实现。
type API struct {
	// membersMu：成员和角色的修改一个一个来。检查（角色还在不在、是不是最后一个管理员、范围有没有超出）和写入之间
	// 不能插进别的修改，否则能造出指向已删角色的成员、把管理员删光（2026-11-19-87c02b5#7、#8）。
	membersMu sync.Mutex

	Q        *store.Queries
	Auth     *auth.Service
	Nodes    *nodes.Registry
	Projects *projects.Service
	CA       *tlsutil.CA
	Sealer   *sealer.Sealer
	Panel    *PanelInfo
	Log      *slog.Logger
	// OAuth 是给 MCP 客户端用的授权服务器（见 docs/mcp.md#oauth），授权页的接口要用它。
	OAuth *oauth.Server
	// Audit 是审计日志（见 docs/audit.md）。
	Audit *audit.Log
	// Recordings 是终端和命令的录制（见 docs/audit.md#终端与命令录制）。
	Recordings *audit.Recordings
	// AuditView 是界面上查看的审计库：备用面板上是从主面板同步来的那份，主面板上为 nil（就是 Audit）。
	AuditView *audit.Log
	// Shipper 把审计记录外送（见 docs/audit.md#外送），备用面板上为 nil。
	Shipper *audit.Shipper

	updates panelUpdates
	reads   readDedup
	origins originDenials
	// directRot：正在给节点换直连下载的密钥（见 revokeDirectLinks）。
	directRot directRotation
	// enrollAlerts 合并同一个接入码的加入通知；enrollNames 让同时加入的节点不会撞名（见 enroll_limits.go）。
	enrollAlerts enrollAlerts
	enrollNames  sync.Mutex
}

// Mount 把所有接口挂到 mux 上。
func (a *API) Mount(mux *http.ServeMux) {
	chain := []connect.Interceptor{langInterceptor{}, a.Auth.Interceptor(
		nagiv1connect.SetupServiceGetSetupStateProcedure,
		nagiv1connect.SetupServiceSetupProcedure,
		nagiv1connect.AuthServiceLoginProcedure,
		nagiv1connect.AuthServiceBeginPasskeyLoginProcedure,
		nagiv1connect.AuthServicePasskeyLoginProcedure,
		// 登录的第二步改用手机确认（见 docs/android.md#手机确认）。
		nagiv1connect.AuthServiceBeginLoginApprovalProcedure,
		nagiv1connect.AuthServiceCompleteLoginApprovalProcedure,
		nagiv1connect.AuthServiceLogoutProcedure,
		// 要求重新验证的会话，其他接口都被拒绝，只有这个能用（处理函数自己找会话）。
		nagiv1connect.AuthServiceReverifyProcedure,
		nagiv1connect.EnrollServiceEnrollProcedure,
		// 安卓 App 的配对和换取令牌（见 docs/android.md#appservice），凭配对码或硬件密钥的签名。
		nagiv1connect.AppServiceGetPairingProcedure,
		nagiv1connect.AppServicePairProcedure,
		nagiv1connect.AppServiceChallengeProcedure,
		nagiv1connect.AppServiceTokenProcedure,
	)}
	if a.Panel.Standby != nil {
		chain = append(chain, readOnly{a})
	}
	chain = append(chain, activityLog{a}, roleCheck{}, scopeCheck{a}, elevationCheck{})
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
	mux.Handle(nagiv1connect.NewAppServiceHandler(&appService{a}, interceptors))
	mux.Handle("/hooks/", a.standbyBlock(a.WebhookHandler()))
	mux.Handle("/ws/exec", a.standbyBlock(a.TerminalHandler()))
	mux.Handle("/ws/node-shell", a.standbyBlock(a.NodeShellHandler()))
	mux.Handle("/api/node-file", a.standbyBlock(a.NodeFileHandler()))
	// 直连下载的签名链接（见 directdl.go）。
	mux.Handle("GET /api/node-file/direct", a.standbyBlock(a.NodeDirectHandler()))
	mux.Handle("GET /api/volume-file/direct", a.VolumeDirectHandler())
	mux.Handle("POST /api/upload", a.standbyBlock(a.UploadHandler()))
	mux.Handle("GET /api/volume-file", a.VolumeFileHandler())
	// 经面板下载的签名链接（见 downloadlink.go）：只凭链接，不看 cookie。节点文件在备用面板上不能下载，由处理函数判断。
	mux.Handle("GET /api/download/{token}", a.DownloadLinkHandler())
	mux.Handle("POST /api/volume-file", a.standbyBlock(a.VolumeFileHandler()))
	// 分块上传的一块和放弃（见 chunkupload.go）。
	mux.Handle("PUT /api/volume-file", a.standbyBlock(a.VolumeFileHandler()))
	mux.Handle("DELETE /api/volume-file", a.standbyBlock(a.VolumeFileHandler()))
	mux.Handle("GET /install.sh", a.InstallHandler())
}

// ---- 初始化 ----

type setupService struct{ *API }

func (s *setupService) GetSetupState(ctx context.Context, _ *connect.Request[nagiv1.GetSetupStateRequest]) (*connect.Response[nagiv1.GetSetupStateResponse], error) {
	ok, err := s.Auth.HasUsers(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.GetSetupStateResponse{Initialized: ok, PasskeyDomain: s.Auth.PasskeyDomain(ctx)}), nil
}

func (s *setupService) Setup(ctx context.Context, req *connect.Request[nagiv1.SetupRequest]) (*connect.Response[nagiv1.SetupResponse], error) {
	if ok, err := s.Auth.HasUsers(ctx); err != nil {
		return nil, err
	} else if ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("面板已完成初始化"))
	}
	stored, err := s.Q.GetSetting(ctx, SettingSetupToken)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, i18n.New("初始化链接不正确，请使用面板启动日志里的初始化链接"))
	}
	want, expires, _ := strings.Cut(stored, ":")
	if exp, _ := strconv.ParseInt(expires, 10, 64); time.Now().Unix() > exp {
		return nil, connect.NewError(connect.CodePermissionDenied, i18n.New("初始化链接已过期（24 小时内有效），请重启面板获取新的链接"))
	}
	got := auth.HashToken(req.Msg.GetToken())
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, i18n.New("初始化链接不正确，请使用面板启动日志里的初始化链接"))
	}
	username := strings.TrimSpace(req.Msg.GetUsername())
	if username == "" || utf8.RuneCountInString(username) > 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("用户名不能为空，且不超过 32 个字符"))
	}
	if utf8.RuneCountInString(req.Msg.GetPassword()) < 8 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("密码至少 8 个字符"))
	}
	userID, err := s.Auth.CreateUser(ctx, username, req.Msg.GetPassword(), auth.AdminRoleID)
	if err != nil {
		return nil, err
	}
	_ = s.Q.DeleteSetting(ctx, SettingSetupToken)
	cookie, err := s.Auth.NewSession(ctx, userID, auth.RequestIP(req.Peer().Addr, req.Header()))
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
	token := auth.SessionToken(req.Header())
	u, err := s.Auth.SessionUser(ctx, token)
	if err == nil {
		s.Record(auth.WithUser(ctx, u), audit.Event{Category: audit.Login, Action: "Logout"})
	}
	cookie := s.Auth.Logout(ctx, token)
	// 退出登录后，这次登录签过的直连下载链接也不该再能用（见 revokeDirectLinks）。没有登录的请求不换：
	// 谁都能调用退出登录，不能让人借它不停地换密钥。
	if err == nil {
		s.revokeDirectLinks()
	}
	resp := connect.NewResponse(&nagiv1.LogoutResponse{})
	resp.Header().Set("Set-Cookie", cookie.String())
	return resp, nil
}

func (s *authService) Me(ctx context.Context, _ *connect.Request[nagiv1.MeRequest]) (*connect.Response[nagiv1.MeResponse], error) {
	u := auth.CurrentUser(ctx)
	resp := &nagiv1.MeResponse{Username: u.Name, TotpEnabled: s.Auth.TOTPEnabled(ctx, u.ID), Role: u.Role, Permissions: u.EffectivePerms().Names(),
		HasPhone: s.Auth.HasPhone(ctx, u.ID)}
	if u.Elevated() {
		resp.ElevatedUntil = timestamppb.New(time.Unix(u.ElevatedUntil, 0))
		resp.ElevatedBy = u.ElevatedBy
	}
	if row, err := s.Q.GetUserByID(ctx, u.ID); err == nil {
		resp.Language = row.Language
		resp.DisplayName = row.DisplayName
		resp.Avatar = row.Avatar
		resp.AvatarType = avatarType(row.Avatar)
		resp.RoleId = row.RoleID
		if r, err := s.Q.GetRole(ctx, row.RoleID); err == nil {
			resp.RoleName, resp.RoleBuiltin = r.Name, r.Builtin
		}
		if !u.IsAdmin() {
			resp.Scope, _ = userScopeOf(ctx, s.Q, row)
		}
	}
	return connect.NewResponse(resp), nil
}

// SetLanguage 保存当前用户的界面语言。
func (s *authService) SetLanguage(ctx context.Context, req *connect.Request[nagiv1.SetLanguageRequest]) (*connect.Response[nagiv1.SetLanguageResponse], error) {
	lang := req.Msg.GetLanguage()
	if l, ok := i18n.Match(lang); lang != "" && (!ok || string(l) != lang) {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("语言只能是 zh-CN 或 en"))
	}
	if err := s.Q.SetUserLanguage(ctx, store.SetUserLanguageParams{Language: lang, ID: auth.CurrentUser(ctx).ID}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.SetLanguageResponse{}), nil
}

// 项目页排序的上限：顺序里最多这么多项，每项不超过这么长（项目 ID、分组名都远短于此）。
const (
	maxOrderItems = 5000
	maxOrderItem  = 128
)

// GetProjectOrder 返回自己的项目页排序。
func (s *authService) GetProjectOrder(ctx context.Context, _ *connect.Request[nagiv1.GetProjectOrderRequest]) (*connect.Response[nagiv1.ProjectOrder], error) {
	o, err := s.Q.GetProjectOrder(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.ProjectOrder{Sort: o.Sort, Groups: o.Groups, Projects: o.Projects}), nil
}

// checkOrder 检查要保存的排序：排序方式认识，顺序里的每项非空、不太长、没有换行。
func checkOrder(sort string, lists ...[]string) error {
	if sort != "" && sort != "name" && sort != "custom" {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("排序方式不正确"))
	}
	for _, list := range lists {
		if len(list) > maxOrderItems {
			return connect.NewError(connect.CodeInvalidArgument, i18n.New("排序的项目过多"))
		}
		for _, v := range list {
			if v == "" || len(v) > maxOrderItem || strings.ContainsAny(v, "\r\n") {
				return connect.NewError(connect.CodeInvalidArgument, i18n.New("排序的项目不正确"))
			}
		}
	}
	return nil
}

// SetProjectOrder 保存自己的项目页排序，整份覆盖。只影响自己看到的顺序，不检查项目和分组是否存在。
func (s *authService) SetProjectOrder(ctx context.Context, req *connect.Request[nagiv1.ProjectOrder]) (*connect.Response[nagiv1.SetProjectOrderResponse], error) {
	m := req.Msg
	if err := checkOrder(m.GetSort(), m.GetGroups(), m.GetProjects()); err != nil {
		return nil, err
	}
	if err := s.Q.SetProjectOrder(ctx, auth.CurrentUser(ctx).ID, store.ProjectOrder{Sort: m.GetSort(), Groups: m.GetGroups(), Projects: m.GetProjects()}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.SetProjectOrderResponse{}), nil
}

// GetNodeOrder 返回自己的节点页排序。
func (s *authService) GetNodeOrder(ctx context.Context, _ *connect.Request[nagiv1.GetNodeOrderRequest]) (*connect.Response[nagiv1.NodeOrder], error) {
	o, err := s.Q.GetNodeOrder(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.NodeOrder{Sort: o.Sort, Groups: o.Groups, Nodes: o.Nodes}), nil
}

// SetNodeOrder 保存自己的节点页排序，整份覆盖。同 SetProjectOrder，不检查节点和分组是否存在。
func (s *authService) SetNodeOrder(ctx context.Context, req *connect.Request[nagiv1.NodeOrder]) (*connect.Response[nagiv1.SetNodeOrderResponse], error) {
	m := req.Msg
	if err := checkOrder(m.GetSort(), m.GetGroups(), m.GetNodes()); err != nil {
		return nil, err
	}
	if err := s.Q.SetNodeOrder(ctx, auth.CurrentUser(ctx).ID, store.NodeOrder{Sort: m.GetSort(), Groups: m.GetGroups(), Nodes: m.GetNodes()}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.SetNodeOrderResponse{}), nil
}

// avatarType 按内容判断头像的类型，不是允许的图片时返回空。头像显示在页面里，所以不接受 SVG 这类能带脚本的格式。
func avatarType(avatar []byte) string {
	switch t := http.DetectContentType(avatar); t {
	case "image/png", "image/jpeg", "image/webp":
		return t
	}
	return ""
}

// UpdateProfile 保存当前用户的显示名称和头像。
func (s *authService) UpdateProfile(ctx context.Context, req *connect.Request[nagiv1.UpdateProfileRequest]) (*connect.Response[nagiv1.UpdateProfileResponse], error) {
	name := strings.TrimSpace(req.Msg.GetDisplayName())
	if utf8.RuneCountInString(name) > 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("显示名称不能超过 32 个字符"))
	}
	avatar := req.Msg.GetAvatar()
	if len(avatar) > maxAvatarBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("头像不能超过 256 KB"))
	}
	if len(avatar) == 0 {
		avatar = []byte{} // 列不能为 NULL。
	} else if avatarType(avatar) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("头像只能是 PNG、JPEG 或 WebP 图片"))
	} else if w, h, err := avatarSize(avatar); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("头像只能是 PNG、JPEG 或 WebP 图片"))
	} else if w > maxAvatarSide || h > maxAvatarSide {
		// 文件小、尺寸大的图在 App 里解出来上百 MB（v0.35.0#17）。界面上传前会缩到 128，正常用不会遇到。
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("头像的宽和高不能超过 1024 像素"))
	}
	if err := s.Q.SetUserProfile(ctx, store.SetUserProfileParams{DisplayName: name, Avatar: avatar, ID: auth.CurrentUser(ctx).ID}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.UpdateProfileResponse{}), nil
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
		n.UnknownContainers = s.Projects.UnknownContainers(ctx, row.ID)
		n.StaleProjects = s.Projects.StaleProjects(ctx, row.ID)
		n.AlertRules = nodes.NodeAlertRules(row)
		if n.Tags, err = s.Q.NodeTags(ctx, row.ID); err != nil {
			return nil, err
		}
		if n.Group, err = s.Q.NodeGroup(ctx, row.ID); err != nil {
			return nil, err
		}
		n.Isolated = s.Nodes.Isolated(ctx, row.ID)
		n.RelayPool = s.Nodes.InRelayPool(ctx, row.ID)
		if conn, err := s.Nodes.Peek(row.ID); err == nil && conn.LastReport() != nil {
			n.Info = conn.LastReport().GetNode()
			n.Usage = conn.LastReport().GetUsage()
			n.Address = s.nodeAddress(ctx, conn)
			n.OwnEmbedded = conn.OwnEmbedded()
		} else if len(row.Info) > 0 {
			info := &nagiv1.NodeInfo{}
			if proto.Unmarshal(row.Info, info) == nil {
				n.Info = info
			}
		}
		n.Info = egressSupport(n.Info)
		out.Nodes = append(out.Nodes, n)
	}
	return connect.NewResponse(out), nil
}

// egressMinVersion 是支持容器出站限制的最低节点版本（见 docs/agent.md#出站限制）。
const egressMinVersion = "v0.29.0"

// egressSupport：更早的节点不认代理计划里的出站限制，也不报 egress_error，节点页不会提示。面板按版本补上原因，
// 节点页照常显示「无法限制容器出站」，提示升级节点。
func egressSupport(info *nagiv1.NodeInfo) *nagiv1.NodeInfo {
	if info == nil || info.GetEgressError() != "" || !release.Before(info.GetVersion(), egressMinVersion) {
		return info
	}
	out := proto.Clone(info).(*nagiv1.NodeInfo)
	out.EgressError = i18n.Errorf("节点版本 %s 不支持出站限制，这台节点上的项目不受限制。升级到 %s 或更新版本后生效。", info.GetVersion(), egressMinVersion).Error()
	return out
}

// ApproveNode 批准用接入码加入的节点。
func (s *nodeService) ApproveNode(ctx context.Context, req *connect.Request[nagiv1.ApproveNodeRequest]) (*connect.Response[nagiv1.ApproveNodeResponse], error) {
	if err := s.Nodes.Approve(ctx, req.Msg.GetNodeId()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&nagiv1.ApproveNodeResponse{}), nil
}

// RenameNode 修改节点在面板上的名称。名字只用于显示，但不能和别的节点重名，否则界面上分不清。
// 节点离线时也能改。名称按 cleanNodeName 整理空格；带不可见字符的拒绝。重名按骨架比较（见 nameSkeleton）：
// 形近字、全角字母和已有的名称一样的也算重名。
func (s *nodeService) RenameNode(ctx context.Context, req *connect.Request[nagiv1.RenameNodeRequest]) (*connect.Response[nagiv1.RenameNodeResponse], error) {
	id := req.Msg.GetNodeId()
	if strings.ContainsFunc(req.Msg.GetName(), invisibleRune) {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("节点名称不能包含控制字符、不可见字符或未分配的字符"))
	}
	name := cleanNodeName(req.Msg.GetName())
	if name == "" || utf8.RuneCountInString(name) > maxNodeName {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("节点名称不能为空，且不超过 64 个字符"))
	}
	// 和节点加入挑名字用同一把锁：同时加入的节点不会和改好的名称撞名。
	s.enrollNames.Lock()
	defer s.enrollNames.Unlock()
	rows, err := s.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	// 成员只和他看得到的节点比：和看不到的节点比的话，把节点改名成猜的名称、看报不报重名，就能确认有没有
	// 这台节点。范围外正好有同名的，管理员那边按 ID 区分。
	x, err := s.loadAccess(ctx, auth.CurrentUser(ctx))
	if err != nil {
		return nil, err
	}
	old := ""
	skeleton := nameSkeleton(name)
	for _, row := range rows {
		if row.ID == id {
			old = row.Name
			continue
		}
		if !x.node(row.ID) || nameSkeleton(row.Name) != skeleton {
			continue
		}
		if row.Name == name {
			return nil, connect.NewError(connect.CodeAlreadyExists, i18n.Errorf("已有名为 %s 的节点", name))
		}
		return nil, connect.NewError(connect.CodeAlreadyExists, i18n.Errorf("名称与节点 %s 过于相似", i18n.NodeRef(row.ID, row.Name)))
	}
	if err := s.Nodes.Rename(ctx, id, name); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if old != name {
		s.rememberNodeName(ctx, id, old)
	}
	return connect.NewResponse(&nagiv1.RenameNodeResponse{}), nil
}

// QueryNodeMetrics 查询节点上的监控数据，监控数据存在节点本地。
func (s *nodeService) QueryNodeMetrics(ctx context.Context, req *connect.Request[nagiv1.QueryNodeMetricsRequest]) (*connect.Response[nagiv1.QueryMetricsResponse], error) {
	if req.Msg.GetNodeId() == "" {
		return s.hiddenNodeMetrics(ctx, req.Msg)
	}
	conn, err := s.Nodes.Get(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	resp, err := conn.Agent.QueryMetrics(ctx, &nagiv1.QueryMetricsRequest{
		Prefix: req.Msg.GetPrefix(), From: req.Msg.GetFrom(), To: req.Msg.GetTo(), Step: req.Msg.GetStep(),
	})
	if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

// GetNodeStorage 返回节点的磁盘占用明细（只读，只有管理员能调用）。
func (s *nodeService) GetNodeStorage(ctx context.Context, req *connect.Request[nagiv1.NodeRef]) (*connect.Response[nagiv1.NodeStorage], error) {
	conn, err := s.Nodes.Get(req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	resp, err := conn.Agent.GetStorage(ctx, &nagiv1.GetStorageRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("节点 %s 版本过低，升级后才能查看磁盘明细", s.nodeRef(ctx, req.Msg.GetNodeId())))
	} else if err != nil {
		return nil, fromGRPC(err)
	}
	return connect.NewResponse(resp), nil
}

// nodeAddress 返回在线节点的地址：面板看到的来源 IP，从本机连来的节点（面板自带的）用面板的对外地址。
// 不看节点上报的内容：待批准节点的详情靠这个地址辨认机器。
func (a *API) nodeAddress(ctx context.Context, conn *nodes.Conn) string {
	if ip, ok := conn.Source(); ok {
		return ip.String()
	}
	return a.Panel.PublicAddress(ctx)
}

// RemoveNode 移除节点：标记为已移除（之后拒绝它连接），断开当前连接。节点上还有容器时拒绝。
// 以后用新的加入令牌重新加入时会恢复。
func (s *nodeService) RemoveNode(ctx context.Context, req *connect.Request[nagiv1.RemoveNodeRequest]) (*connect.Response[nagiv1.RemoveNodeResponse], error) {
	id := req.Msg.GetNodeId()
	if _, err := s.Q.GetNode(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("节点不存在"))
	} else if err != nil {
		return nil, err
	}
	if n, err := s.Q.CountNodeContainers(ctx, id); err != nil {
		return nil, err
	} else if n > 0 {
		if n == 1 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("节点上还有 1 个容器，请先迁移到其他节点或删除"))
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.Errorf("节点上还有 %d 个容器，请先迁移到其他节点或删除", n))
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
	if err := s.Q.DeleteNodeFromScopes(ctx, id); err != nil {
		return err
	}
	if err := s.Q.DeleteNodeTags(ctx, id); err != nil {
		return err
	}
	if err := s.Q.SetNodeGroup(ctx, id, ""); err != nil {
		return err
	}
	s.Projects.ForgetEntryNode(ctx, id)
	s.Nodes.Disconnect(id)
	s.Nodes.Forget(id) // 还没恢复的告警不再出现在「需要处理」里
	s.Log.Info("节点已移除", "node", id)
	return nil
}

// ---- 节点加入 ----

type enrollService struct{ *API }

func (s *enrollService) Enroll(ctx context.Context, req *connect.Request[nagiv1.EnrollRequest]) (*connect.Response[nagiv1.EnrollResponse], error) {
	// 先检查请求本身，全部合格再消耗令牌、改数据库：无效的请求不能用掉令牌，也不能借令牌恢复别的已移除节点。
	meshKey := req.Msg.GetWireguardPublicKey()
	if k, err := mesh.ParseKey(meshKey); err != nil || !mesh.ValidPublicKey(k) {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("节点的 WireGuard 公钥格式不正确，请在节点上重新执行加入命令"))
	}
	csr, err := x509.ParseCertificateRequest(req.Msg.GetCsr())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Detail(i18n.New("节点的证书请求无法解析，请在节点上重新执行加入命令"), err))
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.Detail(i18n.New("节点的证书请求签名不正确，请在节点上重新执行加入命令"), err))
	}
	// 节点 ID 由节点的身份公钥算出：同一个节点加入不同的面板，ID 都一样。
	nodeID := nodes.NodeID(csr.RawSubjectPublicKeyInfo)
	if owner, err := s.Q.MeshKeyOwner(ctx, store.MeshKeyOwnerParams{MeshKey: meshKey, ID: nodeID}); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, i18n.Errorf("该 WireGuard 公钥已属于节点 %s", owner))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := time.Now().Unix()
	tokenHash := auth.HashToken(req.Msg.GetToken())
	multiUse, err := s.Q.UseEnrollToken(ctx, store.UseEnrollTokenParams{
		UsedAt:    sql.NullInt64{Int64: now, Valid: true},
		TokenHash: tokenHash,
		ExpiresAt: now,
		Uses:      inviteMaxUses,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodePermissionDenied, i18n.New("加入令牌已失效（已使用、已过期或接入码已达到加入次数上限），请在面板上重新生成"))
	} else if err != nil {
		return nil, err
	}
	// 用接入码（可以多次使用，经过别的面板转交）加入的节点要管理员批准；用安装命令加入的直接可用。
	pending := multiUse
	if n, err := s.Q.GetNode(ctx, nodeID); err == nil {
		if n.Revoked == 0 && n.Pending == 0 {
			pending = 0 // 已经批准过的节点重新加入，不用再批准
		}
		// 移除过的节点重新加入时沿用原来的名称：移除期间可能有别的节点用了同样的名称，原来的名称也可能是旧版本存下的、
		// 带着现在不允许的字符，所以按加入的规则再处理一次。
		s.enrollNames.Lock()
		err := s.Q.SetNodeEnrollment(ctx, store.SetNodeEnrollmentParams{MeshKey: meshKey, Pending: pending, ID: nodeID})
		if err == nil && n.Revoked != 0 {
			var rows []store.Node
			if rows, err = s.Q.ListNodes(ctx); err == nil {
				names := nodeNames{}
				for _, row := range rows {
					if row.ID != nodeID {
						names.add(row.Name)
					}
				}
				if name := enrollName(n.Name, nodeID, names.taken); name != n.Name {
					_, err = s.Q.RenameNode(ctx, store.RenameNodeParams{Name: name, ID: nodeID})
				}
			}
		}
		s.enrollNames.Unlock()
		if err != nil {
			return nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		// 名称按 RenameNode 的规则处理，重名时加后缀（见 enrollName）。挑名字和建节点在一把锁里，同时加入的不会撞名。
		s.enrollNames.Lock()
		rows, err := s.Q.ListNodes(ctx)
		if err == nil {
			names := make(nodeNames, len(rows))
			for _, row := range rows {
				names.add(row.Name)
			}
			name := enrollName(req.Msg.GetHostname(), nodeID, names.taken)
			err = s.Q.CreateNode(ctx, store.CreateNodeParams{ID: nodeID, Name: name, CreatedAt: now, MeshKey: meshKey, Pending: pending})
		}
		s.enrollNames.Unlock()
		if err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	cert, err := s.CA.SignCSR(req.Msg.GetCsr(), nodeID, nodes.NodeCertValidity)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// 日志和审计里的主机名也去掉控制字符、截短（节点提交的内容不可信）。
	hostname := enrollName(req.Msg.GetHostname(), nodeID, func(string) bool { return false })
	s.Log.Info("节点已加入", "node", nodeID, "hostname", hostname)
	// 节点加入记进审计并通知（见 docs/audit.md#安全告警）。加入命令是管理员生成的，加入本身由节点发起，没有登录的成员。
	ip := auth.RequestIP(req.Peer().Addr, req.Header())
	name := s.nodeName(ctx, nodeID)
	s.Audit.Record(ctx, audit.Event{Category: audit.Operation, Action: "Enroll", ActorType: audit.ActorNode, ActorID: nodeID,
		ActorName: hostname, Via: audit.ViaNode, IP: ip, TargetType: "node", TargetID: nodeID, TargetName: name})
	if s.Panel != nil && s.Panel.Standby == nil {
		// 来源地址写在标题里：令牌被别人抢先用掉时，管理员一眼能看出加入的不是自己的机器。
		title := i18n.Errorf("节点 %s 已从 %s 加入", name, ip).Error()
		if pending != 0 {
			title = i18n.Errorf("节点 %s 从 %s 用接入码加入，等待批准", name, ip).Error()
		}
		alert := &nagiv1.Alert{Key: "security:enroll:" + nodeID, Title: title,
			Message: i18n.Errorf("地址：%s，节点 ID：%s", ip, nodeID).Error(), Time: timestamppb.Now()}
		// 接入码能反复用：同一个接入码的通知合并（见 enrollAlerts），免得拿到接入码的人刷告警渠道。
		if multiUse == 0 || s.enrollAlerts.add(tokenHash, name+"（"+ip+"）", func(count int, names []string) {
			s.Nodes.PanelAlert(context.Background(), inviteAlert(tokenHash, count, names))
		}) {
			go s.Nodes.PanelAlert(context.WithoutCancel(ctx), alert)
		}
	}
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
		return connect.NewError(connect.CodeUnavailable, i18n.New("与节点的连接在操作中断开，结果未知。节点重新连接后，请查看项目状态和部署记录"))
	}
	return fromGRPC(err)
}

func fromGRPC(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if !isStatus(err) {
		// 面板自己的说明包着节点的错误：文字和 st.Message() 一样，保留原错误好按请求的语言显示。
		return connect.NewError(connect.Code(st.Code()), err)
	}
	return connect.NewError(connect.Code(st.Code()), errors.New(st.Message()))
}

// isStatus 判断 err 本身就是节点返回的 gRPC 错误（不是面板的错误包着它）。
func isStatus(err error) bool {
	_, ok := err.(interface{ GRPCStatus() *status.Status })
	return ok
}

func (s *nodeService) AdoptNodeContainer(ctx context.Context, req *connect.Request[nagiv1.AdoptNodeContainerRequest]) (*connect.Response[nagiv1.AdoptNodeContainerResponse], error) {
	if err := s.Projects.AdoptContainer(ctx, req.Msg.GetNodeId(), req.Msg.GetProjectId(), req.Msg.GetContainer(), s.adoptCheck(ctx, req.Msg.GetNodeId())); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&nagiv1.AdoptNodeContainerResponse{}), nil
}

func (s *nodeService) ConfirmPendingHA(ctx context.Context, req *connect.Request[nagiv1.ConfirmPendingHARequest]) (*connect.Response[nagiv1.ConfirmPendingHAResponse], error) {
	if err := s.Projects.ConfirmPendingHA(ctx, req.Msg.GetProjectId(), req.Msg.GetContainer(), req.Msg.GetDiscard()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.ConfirmPendingHAResponse{}), nil
}

func (s *nodeService) AdoptStaleProject(ctx context.Context, req *connect.Request[nagiv1.AdoptStaleProjectRequest]) (*connect.Response[nagiv1.AdoptStaleProjectResponse], error) {
	if operatorLimited(ctx) {
		return nil, privilegedDenied(auth.CurrentUser(ctx), errAdoptStale())
	}
	if err := s.Projects.AdoptStaleProject(ctx, req.Msg.GetNodeId(), req.Msg.GetProjectId()); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&nagiv1.AdoptStaleProjectResponse{}), nil
}

func (s *nodeService) SetNodeAlertRules(ctx context.Context, req *connect.Request[nagiv1.SetNodeAlertRulesRequest]) (*connect.Response[nagiv1.SetNodeAlertRulesResponse], error) {
	if err := s.Nodes.SetNodeAlertRules(ctx, req.Msg.GetNodeId(), req.Msg.GetRules()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&nagiv1.SetNodeAlertRulesResponse{}), nil
}

// IsolateNode 隔离节点（见 docs/mesh.md#隔离）：只有管理员（角色检查）、先做高权限确认、输入节点名。
func (s *nodeService) IsolateNode(ctx context.Context, req *connect.Request[nagiv1.IsolateNodeRequest]) (*connect.Response[nagiv1.IsolateNodeResponse], error) {
	return s.setIsolated(ctx, req.Msg, true)
}

// UnisolateNode 解除隔离，同样要高权限确认。
func (s *nodeService) UnisolateNode(ctx context.Context, req *connect.Request[nagiv1.IsolateNodeRequest]) (*connect.Response[nagiv1.IsolateNodeResponse], error) {
	return s.setIsolated(ctx, req.Msg, false)
}

func (s *nodeService) setIsolated(ctx context.Context, m *nagiv1.IsolateNodeRequest, on bool) (*connect.Response[nagiv1.IsolateNodeResponse], error) {
	n, err := s.Q.GetNode(ctx, m.GetNodeId())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("节点不存在"))
	} else if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.GetConfirmName()) != n.Name {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("需输入名称确认"))
	}
	if err := s.Nodes.SetIsolated(ctx, n.ID, on); err != nil {
		return nil, err
	}
	title := i18n.Errorf("节点 %s 已被隔离", n.Name).Error()
	if !on {
		title = i18n.Errorf("节点 %s 已解除隔离", n.Name).Error()
	}
	s.notify(ctx, "isolate", title, "")
	return connect.NewResponse(&nagiv1.IsolateNodeResponse{}), nil
}

// hiddenNodeMetrics：项目页查本项目容器的占用（前缀 c:<项目 ID>/ 这类），容器在成员看不到的节点上时，回复里没有节点 ID，
// 界面不填节点。这时查项目的容器所在的、成员看不到的节点，序列合在一起返回（访问范围见 scope.go 的 checkMetricsRequest，
// 回复照常经过 filterMetrics，只剩这个项目的序列）。不说是哪台节点，离线、出错的节点跳过。
func (s *nodeService) hiddenNodeMetrics(ctx context.Context, m *nagiv1.QueryNodeMetricsRequest) (*connect.Response[nagiv1.QueryMetricsResponse], error) {
	kind, rest, _ := strings.Cut(m.GetPrefix(), ":")
	project, ok := metricProject(rest)
	if kind == "node" || kind == "m" || !ok {
		return nil, nodeNotFound()
	}
	x, err := s.loadAccess(ctx, auth.CurrentUser(ctx))
	if err != nil {
		return nil, err
	}
	rows, err := s.Q.ContainerNodes(ctx)
	if err != nil {
		return nil, err
	}
	// 容器所在的节点，加上端口映射的入口节点（请求统计记在收到请求的节点上）。
	var ids []string
	for _, r := range rows {
		if r.ProjectID == project {
			ids = append(ids, r.NodeID)
		}
	}
	if routes, err := s.Q.ListRoutes(ctx, project); err == nil {
		for _, r := range routes {
			ids = append(ids, projects.RouteProto(r).GetEntryNodes()...)
		}
	}
	out := &nagiv1.QueryMetricsResponse{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || x.node(id) {
			continue
		}
		seen[id] = true
		conn, err := s.Nodes.Get(id)
		if err != nil {
			continue
		}
		res, err := conn.Agent.QueryMetrics(ctx, &nagiv1.QueryMetricsRequest{Prefix: m.GetPrefix(), From: m.GetFrom(), To: m.GetTo(), Step: m.GetStep()})
		if err != nil {
			continue
		}
		out.Series = append(out.Series, res.GetSeries()...)
	}
	return connect.NewResponse(out), nil
}
