package api

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// roleCheck 按角色拦截接口，并去掉回复里当前角色不该看到的内容。放在登录检查之后。
type roleCheck struct{}

func permissionDenied() error {
	return connect.NewError(connect.CodePermissionDenied, i18n.New("当前账号没有权限执行这个操作"))
}

// 公开接口（登录、初始化）没有当前用户，由登录检查决定放不放行。
func roleAllowed(ctx context.Context, procedure string) bool {
	u := auth.CurrentUser(ctx)
	if u.TokenID != 0 && !auth.TokenAllowed(u, procedure) {
		return false
	}
	return u.ID == 0 || auth.Allowed(u.Role, procedure)
}

func (roleCheck) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !roleAllowed(ctx, req.Spec().Procedure) {
			return nil, permissionDenied()
		}
		res, err := next(ctx, req)
		if err == nil {
			redactFor(auth.CurrentUser(ctx), res.Any())
		}
		return res, err
	}
}

func (roleCheck) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (roleCheck) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !roleAllowed(ctx, conn.Spec().Procedure) {
			return permissionDenied()
		}
		return next(ctx, conn)
	}
}

// redactFor 按角色去掉回复里的敏感内容。只读角色看不到容器配置（环境变量、compose）和密钥；
// 主密钥路径只给管理员看。
func redactFor(u auth.User, msg any) {
	if u.ID == 0 || u.Role == auth.RoleAdmin {
		return
	}
	if res, ok := msg.(*nagiv1.GetSettingsResponse); ok && res.GetSettings() != nil {
		res.Settings.MasterKeyPath = ""
	}
	if u.Role != auth.RoleViewer {
		return
	}
	switch m := msg.(type) {
	case *nagiv1.ListProjectsResponse:
		for _, s := range m.GetProjects() {
			redactProject(s.GetProject())
		}
	case *nagiv1.ProjectSummary:
		redactProject(m.GetProject())
	case *nagiv1.Project:
		redactProject(m)
	}
}

func redactProject(p *nagiv1.Project) {
	if p == nil {
		return
	}
	p.SharedEnvDraft = nil
	if p.Settings != nil {
		p.Settings.SharedEnv = nil
	}
	for _, c := range p.GetContainers() {
		c.Config, c.Draft, c.DeployKey, c.WebhookToken = nil, nil, "", ""
	}
}

// ---- 成员 ----

func (s *settingsService) ListUsers(ctx context.Context, _ *connect.Request[nagiv1.ListUsersRequest]) (*connect.Response[nagiv1.ListUsersResponse], error) {
	rows, err := s.Q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListUsersResponse{}
	for _, r := range rows {
		scope, err := s.userScope(ctx, r)
		if err != nil {
			return nil, err
		}
		out.Users = append(out.Users, &nagiv1.PanelUser{
			Id: r.ID, Username: r.Username, Role: r.Role, TotpEnabled: r.TotpSecret != "",
			CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0)), IsSelf: r.ID == auth.CurrentUser(ctx).ID, Scope: scope,
			DisplayName: r.DisplayName, Avatar: r.Avatar, AvatarType: avatarType(r.Avatar),
		})
	}
	return connect.NewResponse(out), nil
}

// userScope 读出成员的访问范围（见 docs/panel.md#访问范围）。
func (s *settingsService) userScope(ctx context.Context, u store.User) (*nagiv1.UserScope, error) {
	sc := &nagiv1.UserScope{AllProjects: u.AllProjects != 0, AllNodes: u.AllNodes != 0}
	var err error
	if !sc.AllProjects {
		if sc.ProjectIds, err = s.Q.ListUserProjects(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if !sc.AllNodes {
		if sc.NodeIds, err = s.Q.ListUserNodes(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	return sc, nil
}

// checkScope 检查指定的项目、节点都存在。
func (s *settingsService) checkScope(ctx context.Context, sc *nagiv1.UserScope) error {
	if !sc.GetAllProjects() {
		for _, id := range sc.GetProjectIds() {
			if _, err := s.Q.GetProject(ctx, id); errors.Is(err, sql.ErrNoRows) {
				return connect.NewError(connect.CodeInvalidArgument, i18n.New("项目不存在"))
			} else if err != nil {
				return err
			}
		}
	}
	if !sc.GetAllNodes() {
		for _, id := range sc.GetNodeIds() {
			if n, err := s.Q.GetNode(ctx, id); errors.Is(err, sql.ErrNoRows) || err == nil && n.Revoked != 0 {
				return connect.NewError(connect.CodeInvalidArgument, i18n.New("节点不存在"))
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}

// saveScope 保存成员的访问范围。先用 checkScope 检查过。
func (s *settingsService) saveScope(ctx context.Context, id int64, sc *nagiv1.UserScope) error {
	flag := func(all bool) int64 {
		if all {
			return 1
		}
		return 0
	}
	if err := s.Q.SetUserScope(ctx, store.SetUserScopeParams{AllProjects: flag(sc.GetAllProjects()), AllNodes: flag(sc.GetAllNodes()), ID: id}); err != nil {
		return err
	}
	if err := s.Q.ClearUserProjects(ctx, id); err != nil {
		return err
	}
	if err := s.Q.ClearUserNodes(ctx, id); err != nil {
		return err
	}
	if !sc.GetAllProjects() {
		for _, p := range sc.GetProjectIds() {
			if err := s.Q.AddUserProject(ctx, store.AddUserProjectParams{UserID: id, ProjectID: p}); err != nil {
				return err
			}
		}
	}
	if !sc.GetAllNodes() {
		for _, n := range sc.GetNodeIds() {
			if err := s.Q.AddUserNode(ctx, store.AddUserNodeParams{UserID: id, NodeID: n}); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkNewPassword(password string) error {
	if len([]rune(password)) < 8 {
		return connect.NewError(connect.CodeInvalidArgument, i18n.New("密码至少 8 个字符"))
	}
	return nil
}

func (s *settingsService) CreateUser(ctx context.Context, req *connect.Request[nagiv1.CreateUserRequest]) (*connect.Response[nagiv1.PanelUser], error) {
	m := req.Msg
	username := strings.TrimSpace(m.GetUsername())
	if username == "" || len([]rune(username)) > 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("用户名不能为空，且不超过 32 个字符"))
	}
	if !auth.ValidRole(m.GetRole()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("角色只能是管理员、操作员或只读"))
	}
	if err := checkNewPassword(m.GetPassword()); err != nil {
		return nil, err
	}
	scope := m.GetScope()
	if scope == nil {
		scope = &nagiv1.UserScope{AllProjects: true, AllNodes: true}
	}
	if err := s.checkScope(ctx, scope); err != nil {
		return nil, err
	}
	if _, err := s.Q.GetUserByUsername(ctx, username); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, i18n.New("用户名已被使用"))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	id, err := s.Auth.CreateUser(ctx, username, m.GetPassword(), m.GetRole())
	if err != nil {
		return nil, err
	}
	if err := s.saveScope(ctx, id, scope); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.PanelUser{Id: id, Username: username, Role: m.GetRole(), CreatedAt: timestamppb.Now(), Scope: scope}), nil
}

// lastAdmin 判断这个用户是不是最后一个管理员：面板不能没有管理员。
func (s *settingsService) lastAdmin(ctx context.Context, u store.User) (bool, error) {
	if u.Role != auth.RoleAdmin {
		return false, nil
	}
	n, err := s.Q.CountAdmins(ctx)
	return n <= 1, err
}

func (s *settingsService) UpdateUser(ctx context.Context, req *connect.Request[nagiv1.UpdateUserRequest]) (*connect.Response[nagiv1.UpdateUserResponse], error) {
	m := req.Msg
	u, err := s.Q.GetUserByID(ctx, m.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("成员不存在"))
	}
	if sc := m.GetScope(); sc != nil {
		if err := s.checkScope(ctx, sc); err != nil {
			return nil, err
		}
	}
	if role := m.GetRole(); role != "" && role != u.Role {
		if !auth.ValidRole(role) {
			return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("角色只能是管理员、操作员或只读"))
		}
		last, err := s.lastAdmin(ctx, u)
		if err != nil {
			return nil, err
		}
		if last {
			return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("至少保留一个管理员"))
		}
		if err := s.Q.SetUserRole(ctx, store.SetUserRoleParams{Role: role, ID: u.ID}); err != nil {
			return nil, err
		}
	}
	if m.GetPassword() != "" {
		if err := checkNewPassword(m.GetPassword()); err != nil {
			return nil, err
		}
		hash, err := auth.HashPassword(m.GetPassword())
		if err != nil {
			return nil, err
		}
		if err := s.Q.UpdatePassword(ctx, store.UpdatePasswordParams{PasswordHash: hash, ID: u.ID}); err != nil {
			return nil, err
		}
		// 别人重置了密码，这个用户已有的登录和访问令牌都要失效。
		if err := s.Q.DeleteUserSessions(ctx, u.ID); err != nil {
			return nil, err
		}
		if err := s.Q.DeleteUserAccessTokens(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if m.GetClearTotp() {
		if err := s.Q.ClearUserTOTP(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if sc := m.GetScope(); sc != nil {
		if err := s.saveScope(ctx, u.ID, sc); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&nagiv1.UpdateUserResponse{}), nil
}

func (s *settingsService) DeleteUser(ctx context.Context, req *connect.Request[nagiv1.DeleteUserRequest]) (*connect.Response[nagiv1.DeleteUserResponse], error) {
	id := req.Msg.GetId()
	if id == auth.CurrentUser(ctx).ID {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("不能删除当前登录的账号"))
	}
	u, err := s.Q.GetUserByID(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("成员不存在"))
	}
	last, err := s.lastAdmin(ctx, u)
	if err != nil {
		return nil, err
	}
	if last {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("至少保留一个管理员"))
	}
	if err := s.Q.DeleteUser(ctx, id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.DeleteUserResponse{}), nil
}

// ---- 会话（只能管自己的） ----

// sessionIDLen 是给界面用的会话标识的长度：令牌哈希的开头一段，不足以当作凭据。
const sessionIDLen = 16

func (s *settingsService) ListSessions(ctx context.Context, req *connect.Request[nagiv1.ListSessionsRequest]) (*connect.Response[nagiv1.ListSessionsResponse], error) {
	rows, err := s.Q.ListUserSessions(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	current := auth.HashToken(auth.SessionToken(req.Header()))
	out := &nagiv1.ListSessionsResponse{}
	for _, r := range rows {
		out.Sessions = append(out.Sessions, &nagiv1.LoginSession{
			Id: r.TokenHash[:sessionIDLen], Ip: r.Ip, Current: r.TokenHash == current,
			CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0)), ExpiresAt: timestamppb.New(time.Unix(r.ExpiresAt, 0)),
		})
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) RevokeSession(ctx context.Context, req *connect.Request[nagiv1.RevokeSessionRequest]) (*connect.Response[nagiv1.RevokeSessionResponse], error) {
	n, err := s.Q.DeleteUserSession(ctx, store.DeleteUserSessionParams{UserID: auth.CurrentUser(ctx).ID, Substr: req.Msg.GetId()})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("登录设备不存在"))
	}
	return connect.NewResponse(&nagiv1.RevokeSessionResponse{}), nil
}
