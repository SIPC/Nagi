package api

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 成员和角色（见 docs/panel.md#成员和角色）。要「成员」权限。不是管理员的成员管理别人时不能越过自己：
// 只能授予自己有的权限、只能把范围设在自己的范围内，不能管理管理员和权限比自己多的成员（memberGuard）。

// ---- 角色 ----

const maxRoleName = 32

// maxRoles 是角色的数量上限（含预置的）。
const maxRoles = 50

// reservedRoleNames 是预置角色在界面上显示的名称（各语言）：预置角色的名称为空时界面显示这些，自定义角色不能叫这些名字，
// 预置角色也不能改成另一个预置角色的名字，免得选角色时认错。值是这个名字属于哪个预置角色。
var reservedRoleNames = map[string]string{
	"管理员": auth.RoleAdmin, "admin": auth.RoleAdmin, "administrator": auth.RoleAdmin,
	"操作员": "operator", "operator": "operator",
	"只读": "viewer", "只读成员": "viewer", "viewer": "viewer", "read-only": "viewer", "read only": "viewer", "readonly": "viewer",
}

func roleProto(r store.Role) *nagiv1.Role {
	perms := r.Permissions
	if r.Builtin == auth.RoleAdmin {
		perms = auth.AllPerms.Names()
	} else {
		perms = auth.ParsePerms(perms).Normalize().Names()
	}
	return &nagiv1.Role{Id: r.ID, Name: r.Name, Builtin: r.Builtin, Permissions: perms, Members: uint32(r.Members),
		CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0))}
}

// rolePerms 是角色的权限，管理员是全部。
func rolePerms(r store.Role) auth.Perms {
	if r.Builtin == auth.RoleAdmin {
		return auth.AllPerms
	}
	return auth.ParsePerms(r.Permissions).Normalize()
}

// grantable 是当前成员能授予的权限。
func grantable(u auth.User) auth.Perms {
	if u.IsAdmin() {
		return auth.AllPerms.Normalize()
	}
	return u.Perms &^ auth.AdminOnlyPerms
}

func (s *settingsService) ListRoles(ctx context.Context, _ *connect.Request[nagiv1.ListRolesRequest]) (*connect.Response[nagiv1.ListRolesResponse], error) {
	rows, err := s.Q.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListRolesResponse{Grantable: grantable(auth.CurrentUser(ctx)).Names()}
	for _, r := range rows {
		out.Roles = append(out.Roles, roleProto(r))
	}
	return connect.NewResponse(out), nil
}

// checkRoleInput 检查角色的名称和权限，返回整理过的名称和权限。builtin 是这个角色的预置标记（新建和自定义角色为空）。
func (s *settingsService) checkRoleInput(ctx context.Context, id int64, builtin, name string, names []string, requireName bool) (string, auth.Perms, error) {
	name = strings.TrimSpace(name)
	if requireName && name == "" {
		return "", 0, connect.NewError(connect.CodeInvalidArgument, i18n.New("请填写角色名称"))
	}
	// 控制字符和看不见的格式字符（零宽空格之类）不要：两个名字看起来一样。
	invisible := func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
	if utf8.RuneCountInString(name) > maxRoleName || strings.ContainsFunc(name, invisible) {
		return "", 0, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("角色名称最多 %d 个字符", maxRoleName))
	}
	if owner, ok := reservedRoleNames[strings.ToLower(name)]; ok && owner != builtin {
		return "", 0, connect.NewError(connect.CodeAlreadyExists, i18n.New("角色名称已被使用"))
	}
	if name != "" {
		taken, err := s.Q.RoleNameTaken(ctx, name, id)
		if err != nil {
			return "", 0, err
		}
		if taken {
			return "", 0, connect.NewError(connect.CodeAlreadyExists, i18n.New("角色名称已被使用"))
		}
	}
	for _, n := range names {
		if !auth.ValidPermName(n) {
			return "", 0, connect.NewError(connect.CodeInvalidArgument, i18n.Errorf("权限 %s 不存在", n))
		}
	}
	perms := auth.ParsePerms(names)
	if perms&auth.AdminOnlyPerms != 0 {
		return "", 0, connect.NewError(connect.CodeInvalidArgument, i18n.New("审计日志和安全设置只给管理员"))
	}
	perms = perms.Normalize()
	if u := auth.CurrentUser(ctx); !u.IsAdmin() && perms&^u.Perms != 0 {
		return "", 0, connect.NewError(connect.CodePermissionDenied, i18n.New("只能授予自己拥有的权限"))
	}
	return name, perms, nil
}

func (s *settingsService) CreateRole(ctx context.Context, req *connect.Request[nagiv1.CreateRoleRequest]) (*connect.Response[nagiv1.Role], error) {
	s.membersMu.Lock()
	defer s.membersMu.Unlock()
	name, perms, err := s.checkRoleInput(ctx, 0, "", req.Msg.GetName(), req.Msg.GetPermissions(), true)
	if err != nil {
		return nil, err
	}
	if all, err := s.Q.ListRoles(ctx); err != nil {
		return nil, err
	} else if len(all) >= maxRoles {
		return nil, connect.NewError(connect.CodeResourceExhausted, i18n.Errorf("角色最多 %d 个", maxRoles))
	}
	id, err := s.Q.CreateRole(ctx, name, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if err := s.Q.SetRolePermissions(ctx, id, perms.Names()); err != nil {
		return nil, err
	}
	r, err := s.Q.GetRole(ctx, id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(roleProto(r)), nil
}

func roleNotFound() error { return connect.NewError(connect.CodeNotFound, i18n.New("角色不存在")) }

// editableRole 读出要修改或删除的角色：管理员角色不能改。不是管理员的成员不能改权限比自己多的角色，不能改预置角色
// （所有面板的成员都可能在用），也不能改有他管不了的成员（管理员、权限或访问范围比他大）在用的角色：
// 否则改一下角色的权限，那些成员就在更大的范围里得到这些权限。
func (s *settingsService) editableRole(ctx context.Context, id int64) (store.Role, error) {
	r, err := s.Q.GetRole(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return r, roleNotFound()
	} else if err != nil {
		return r, err
	}
	if r.Builtin == auth.RoleAdmin {
		return r, connect.NewError(connect.CodeFailedPrecondition, i18n.New("管理员角色不能修改"))
	}
	u := auth.CurrentUser(ctx)
	if u.IsAdmin() {
		return r, nil
	}
	if rolePerms(r)&^u.Perms != 0 {
		return r, connect.NewError(connect.CodePermissionDenied, i18n.New("这个角色的权限比你多，不能修改"))
	}
	if r.Builtin != "" {
		return r, connect.NewError(connect.CodePermissionDenied, i18n.New("预置角色只有管理员能修改"))
	}
	users, err := s.Q.ListUsers(ctx)
	if err != nil {
		return r, err
	}
	for _, m := range users {
		if m.RoleID == r.ID && s.manageable(ctx, m) != nil {
			return r, connect.NewError(connect.CodePermissionDenied, i18n.New("有你不能管理的成员在用这个角色，不能修改"))
		}
	}
	return r, nil
}

func (s *settingsService) UpdateRole(ctx context.Context, req *connect.Request[nagiv1.UpdateRoleRequest]) (*connect.Response[nagiv1.Role], error) {
	m := req.Msg
	s.membersMu.Lock()
	defer s.membersMu.Unlock()
	r, err := s.editableRole(ctx, m.GetId())
	if err != nil {
		return nil, err
	}
	name, perms, err := s.checkRoleInput(ctx, r.ID, r.Builtin, m.GetName(), m.GetPermissions(), false)
	if err != nil {
		return nil, err
	}
	if name != "" && name != r.Name {
		if err := s.Q.RenameRole(ctx, r.ID, name); err != nil {
			return nil, err
		}
	}
	old := rolePerms(r)
	if perms != old {
		if err := s.Q.SetRolePermissions(ctx, r.ID, perms.Names()); err != nil {
			return nil, err
		}
		// 权限变了：打开着的订阅流按新的重新计算；少了权限时换掉直连下载的密钥，已经拿到的链接立即失效。
		if old&^perms != 0 {
			s.revokeDirectLinks()
		}
		if s.Nodes != nil {
			s.Nodes.Changes.Bump()
		}
	}
	r, err = s.Q.GetRole(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(roleProto(r)), nil
}

func (s *settingsService) DeleteRole(ctx context.Context, req *connect.Request[nagiv1.DeleteRoleRequest]) (*connect.Response[nagiv1.DeleteRoleResponse], error) {
	s.membersMu.Lock()
	defer s.membersMu.Unlock()
	r, err := s.editableRole(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if r.Builtin != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("预置角色不能删除"))
	}
	if r.Members > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("角色正在使用，不能删除"))
	}
	n, err := s.Q.DeleteRole(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("角色正在使用，不能删除"))
	}
	return connect.NewResponse(&nagiv1.DeleteRoleResponse{}), nil
}

// ---- 成员 ----

func (s *settingsService) ListUsers(ctx context.Context, _ *connect.Request[nagiv1.ListUsersRequest]) (*connect.Response[nagiv1.ListUsersResponse], error) {
	rows, err := s.Q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	roles, err := s.roleMap(ctx)
	if err != nil {
		return nil, err
	}
	out := &nagiv1.ListUsersResponse{}
	me := auth.CurrentUser(ctx)
	var x *access
	var mine *nagiv1.UserScope
	if !me.IsAdmin() {
		// 不是管理员：别的成员的范围里，自己看不到的项目、节点、分组和标签不给（2026-11-19-87c02b5#11）。
		if x, err = s.loadAccess(ctx, me); err != nil {
			return nil, err
		}
		own, err := s.Q.GetUserByID(ctx, me.ID)
		if err != nil {
			return nil, err
		}
		if mine, err = s.userScope(ctx, own); err != nil {
			return nil, err
		}
	}
	for _, r := range rows {
		scope, err := s.userScope(ctx, r)
		if err != nil {
			return nil, err
		}
		if mine != nil {
			scope.ProjectIds = slices.DeleteFunc(scope.ProjectIds, func(id string) bool { return !x.project(id) })
			scope.NodeIds = slices.DeleteFunc(scope.NodeIds, func(id string) bool { return !x.nodeInScope(id) })
			if !mine.GetAllProjects() {
				scope.ProjectGroups = slices.DeleteFunc(scope.ProjectGroups, func(g string) bool { return !slices.Contains(mine.GetProjectGroups(), g) })
			}
			if !mine.GetAllNodes() {
				scope.NodeTags = slices.DeleteFunc(scope.NodeTags, func(t string) bool { return !slices.Contains(mine.GetNodeTags(), t) })
			}
		}
		role := roles[r.RoleID]
		out.Users = append(out.Users, &nagiv1.PanelUser{
			Id: r.ID, Username: r.Username, Role: auth.LegacyRole(r.Role == auth.RoleAdmin, rolePerms(role)), TotpEnabled: r.TotpSecret != "",
			CreatedAt: timestamppb.New(time.Unix(r.CreatedAt, 0)), IsSelf: r.ID == auth.CurrentUser(ctx).ID, Scope: scope,
			DisplayName: r.DisplayName, Avatar: r.Avatar, AvatarType: avatarType(r.Avatar),
			Passkeys: s.passkeys(ctx, r.ID, s.Auth.PasskeyDomain(ctx)),
			RoleId:   r.RoleID, RoleName: role.Name, RoleBuiltin: role.Builtin,
		})
	}
	return connect.NewResponse(out), nil
}

func (s *settingsService) roleMap(ctx context.Context) (map[int64]store.Role, error) {
	rows, err := s.Q.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]store.Role, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// userScope 读出成员的访问范围（见 docs/panel.md#访问范围）。
func (s *settingsService) userScope(ctx context.Context, u store.User) (*nagiv1.UserScope, error) {
	return userScopeOf(ctx, s.Q, u)
}

func userScopeOf(ctx context.Context, q *store.Queries, u store.User) (*nagiv1.UserScope, error) {
	sc := &nagiv1.UserScope{AllProjects: u.AllProjects != 0, AllNodes: u.AllNodes != 0 && u.NodesFollow == 0, NodesFollow: u.NodesFollow != 0}
	var err error
	if !sc.AllProjects {
		if sc.ProjectIds, err = q.ListUserProjects(ctx, u.ID); err != nil {
			return nil, err
		}
		if sc.ProjectGroups, err = q.ListUserProjectGroups(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if !sc.AllNodes && !sc.NodesFollow {
		if sc.NodeIds, err = q.ListUserNodes(ctx, u.ID); err != nil {
			return nil, err
		}
		if sc.NodeTags, err = q.ListUserNodeTags(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	return sc, nil
}

// checkScope 检查指定的项目、节点都存在，并整理分组和标签。不是管理员的成员只能把范围设在自己的范围内。
func (s *settingsService) checkScope(ctx context.Context, sc *nagiv1.UserScope) error {
	var err error
	if sc.GetNodesFollow() {
		sc.AllNodes, sc.NodeIds, sc.NodeTags = false, nil, nil
	}
	if sc.ProjectGroups, err = cleanLabels(sc.GetProjectGroups(), maxScopeList); err != nil {
		return err
	}
	if sc.NodeTags, err = cleanLabels(sc.GetNodeTags(), maxScopeList); err != nil {
		return err
	}
	// 不是管理员的先核对范围：不存在的和范围外的报同一个错，不能借报错探出项目、节点存不存在（2026-11-19-87c02b5#14）。
	if err := s.scopeWithin(ctx, sc); err != nil {
		return err
	}
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

// scopeWithin：不是管理员的成员给别人设的范围不能超过自己的。
func (s *settingsService) scopeWithin(ctx context.Context, sc *nagiv1.UserScope) error {
	u := auth.CurrentUser(ctx)
	if u.IsAdmin() {
		return nil
	}
	outside := connect.NewError(connect.CodePermissionDenied, i18n.New("访问范围不能超过你自己的范围"))
	own, err := s.Q.GetUserByID(ctx, u.ID)
	if err != nil {
		return err
	}
	mine, err := s.userScope(ctx, own)
	if err != nil {
		return err
	}
	x, err := s.loadAccess(ctx, u)
	if err != nil {
		return err
	}
	if !mine.GetAllProjects() {
		if sc.GetAllProjects() {
			return outside
		}
		for _, id := range sc.GetProjectIds() {
			if !x.project(id) {
				return outside
			}
		}
		for _, g := range sc.GetProjectGroups() {
			if !slices.Contains(mine.GetProjectGroups(), g) {
				return outside
			}
		}
	}
	switch {
	case mine.GetAllNodes():
	case mine.GetNodesFollow():
		// 跟随项目：项目范围已经在自己的范围内，跟随项目也就不会超出；指定的节点要是自己看得到的。
		// 节点标签不能给：带标签的节点不一定是自己看得到的，以后打上这个标签的也会进范围（2026-11-19-87c02b5#3）。
		if sc.GetAllNodes() || len(sc.GetNodeTags()) > 0 {
			return outside
		}
	default:
		if sc.GetAllNodes() || sc.GetNodesFollow() {
			return outside
		}
		for _, t := range sc.GetNodeTags() {
			if !slices.Contains(mine.GetNodeTags(), t) {
				return outside
			}
		}
	}
	if !sc.GetAllNodes() && !sc.GetNodesFollow() {
		for _, id := range sc.GetNodeIds() {
			if !x.nodeInScope(id) {
				return outside
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
	allNodes := sc.GetAllNodes() && !sc.GetNodesFollow()
	if err := s.Q.SetUserScope(ctx, store.SetUserScopeParams{AllProjects: flag(sc.GetAllProjects()), AllNodes: flag(allNodes), ID: id}); err != nil {
		return err
	}
	if err := s.Q.SetUserNodesFollow(ctx, id, sc.GetNodesFollow()); err != nil {
		return err
	}
	if err := s.Q.ClearUserProjects(ctx, id); err != nil {
		return err
	}
	if err := s.Q.ClearUserNodes(ctx, id); err != nil {
		return err
	}
	var groups, tags []string
	if !sc.GetAllProjects() {
		groups = sc.GetProjectGroups()
	}
	if !allNodes && !sc.GetNodesFollow() {
		tags = sc.GetNodeTags()
	}
	if err := s.Q.SetUserProjectGroups(ctx, id, groups); err != nil {
		return err
	}
	if err := s.Q.SetUserNodeTags(ctx, id, tags); err != nil {
		return err
	}
	if !sc.GetAllProjects() {
		for _, p := range sc.GetProjectIds() {
			if err := s.Q.AddUserProject(ctx, store.AddUserProjectParams{UserID: id, ProjectID: p}); err != nil {
				return err
			}
		}
	}
	if !allNodes && !sc.GetNodesFollow() {
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

// requestedRole 读出请求里的角色：role_id，或者旧版本的角色名。不是管理员的成员只能选权限不比自己多的角色。
func (s *settingsService) requestedRole(ctx context.Context, roleID int64, legacy string) (store.Role, error) {
	if roleID == 0 {
		roleID = auth.BuiltinRoleID(legacy)
	}
	r, err := s.Q.GetRole(ctx, roleID)
	if roleID == 0 || errors.Is(err, sql.ErrNoRows) {
		return r, connect.NewError(connect.CodeInvalidArgument, i18n.New("角色不存在"))
	} else if err != nil {
		return r, err
	}
	if u := auth.CurrentUser(ctx); !u.IsAdmin() && (r.Builtin == auth.RoleAdmin || rolePerms(r)&^u.Perms != 0) {
		return r, connect.NewError(connect.CodePermissionDenied, i18n.New("只能授予自己拥有的权限"))
	}
	return r, nil
}

// manageable：不是管理员的成员不能管理管理员，也不能管理权限或访问范围比自己大的成员：否则给他换成自己的角色、
// 重置密码再登录进去，就在更大的范围里得到自己的权限（2026-11-19-87c02b5#2）。
func (s *settingsService) manageable(ctx context.Context, target store.User) error {
	u := auth.CurrentUser(ctx)
	if u.IsAdmin() {
		return nil
	}
	if target.Role == auth.RoleAdmin {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("只有管理员能管理管理员"))
	}
	r, err := s.Q.GetRole(ctx, target.RoleID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if rolePerms(r)&^u.Perms != 0 {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("这个成员的权限比你多，不能管理"))
	}
	if target.ID == u.ID {
		return nil
	}
	sc, err := s.userScope(ctx, target)
	if err != nil {
		return err
	}
	if s.scopeWithin(ctx, sc) != nil {
		return connect.NewError(connect.CodePermissionDenied, i18n.New("这个成员的访问范围比你大，不能管理"))
	}
	return nil
}

func (s *settingsService) CreateUser(ctx context.Context, req *connect.Request[nagiv1.CreateUserRequest]) (*connect.Response[nagiv1.PanelUser], error) {
	m := req.Msg
	username := strings.TrimSpace(m.GetUsername())
	if username == "" || len([]rune(username)) > 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, i18n.New("用户名不能为空，且不超过 32 个字符"))
	}
	s.membersMu.Lock()
	defer s.membersMu.Unlock()
	role, err := s.requestedRole(ctx, m.GetRoleId(), m.GetRole())
	if err != nil {
		return nil, err
	}
	if err := checkNewPassword(m.GetPassword()); err != nil {
		return nil, err
	}
	scope := m.GetScope()
	if scope == nil {
		scope = &nagiv1.UserScope{AllProjects: true, NodesFollow: true}
	}
	if err := s.checkScope(ctx, scope); err != nil {
		return nil, err
	}
	if _, err := s.Q.GetUserByUsername(ctx, username); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, i18n.New("用户名已被使用"))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	id, err := s.Auth.CreateUser(ctx, username, m.GetPassword(), role.ID)
	if err != nil {
		return nil, err
	}
	if err := s.saveScope(ctx, id, scope); err != nil {
		return nil, err
	}
	return connect.NewResponse(&nagiv1.PanelUser{Id: id, Username: username, Role: auth.LegacyRole(role.Builtin == auth.RoleAdmin, rolePerms(role)),
		CreatedAt: timestamppb.Now(), Scope: scope, RoleId: role.ID, RoleName: role.Name, RoleBuiltin: role.Builtin}), nil
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
	s.membersMu.Lock()
	defer s.membersMu.Unlock()
	u, err := s.Q.GetUserByID(ctx, m.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("成员不存在"))
	}
	if err := s.manageable(ctx, u); err != nil {
		return nil, err
	}
	// 重置密码、清除两步验证和通行密钥是换掉登录凭据：自己的不能在这里改（改密码要旧密码，两步验证和通行密钥
	// 在账号安全里，各自要验证）；别人的要先用密码或验证码做高权限确认，手机上的确认不算。整个接口已经只认
	// 密码或验证码做的确认（auth.passwordElevation，访问令牌调不到），这里再查一次作为兜底。
	if m.GetPassword() != "" || m.GetClearTotp() || m.GetClearPasskeys() {
		cur := auth.CurrentUser(ctx)
		if u.ID == cur.ID {
			return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("自己的密码、两步验证和通行密钥请在「账号安全」里修改"))
		}
		if cur.TokenID == 0 && !(cur.Elevated() && cur.ElevatedBy == auth.ElevatedByPassword) {
			return nil, elevationError()
		}
	}
	// 降低角色、重置密码、修改访问范围都可能撤销这个成员已有的权限：之后换掉直连下载的密钥，
	// 让他已经拿到的链接立即失效（见 revokeDirectLinks）。中途出错时已经改了的部分也算。
	revoked, changed := false, false
	defer func() {
		if revoked {
			s.revokeDirectLinks()
		}
		// 角色、访问范围变了：打开着的订阅流马上按新的重新计算。中途出错时已经改了的部分也算。
		if changed && s.Nodes != nil {
			s.Nodes.Changes.Bump()
		}
	}()
	if sc := m.GetScope(); sc != nil {
		if err := s.checkScope(ctx, sc); err != nil {
			return nil, err
		}
	}
	if m.GetRoleId() != 0 || m.GetRole() != "" {
		role, err := s.requestedRole(ctx, m.GetRoleId(), m.GetRole())
		if err != nil {
			return nil, err
		}
		if role.ID != u.RoleID {
			if role.Builtin != auth.RoleAdmin {
				last, err := s.lastAdmin(ctx, u)
				if err != nil {
					return nil, err
				}
				if last {
					return nil, connect.NewError(connect.CodeFailedPrecondition, i18n.New("至少保留一个管理员"))
				}
			}
			old, err := s.Q.GetRole(ctx, u.RoleID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err := s.Q.SetUserRole(ctx, store.SetUserRoleParams{Role: auth.UsersRole(role.ID), RoleID: role.ID, ID: u.ID}); err != nil {
				return nil, err
			}
			changed = true
			oldPerms := rolePerms(old)
			if u.Role == auth.RoleAdmin {
				oldPerms = auth.AllPerms
			}
			revoked = oldPerms&^rolePerms(role) != 0
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
		revoked, changed = true, true
		// 别人重置了密码，这个用户已有的登录和访问令牌都要失效。
		if err := s.Q.DeleteUserSessions(ctx, u.ID); err != nil {
			return nil, err
		}
		if err := s.Q.DeleteUserAccessTokens(ctx, u.ID); err != nil {
			return nil, err
		}
		// 手机也一起退出（见 docs/android.md#管理和撤销）。
		if err := s.Auth.RemoveUserAppDevices(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if m.GetClearTotp() {
		if err := s.Q.ClearUserTOTP(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	// 通行密钥登录不要密码和验证码：别人重置密码、清除两步验证（通常是怀疑账号被盗）时一起删掉，
	// 否则攻击者事先加的通行密钥照样能登录。
	if m.GetPassword() != "" || m.GetClearTotp() || m.GetClearPasskeys() {
		if _, err := s.Q.DeleteUserPasskeys(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if sc := m.GetScope(); sc != nil {
		revoked, changed = true, true
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
	s.membersMu.Lock()
	defer s.membersMu.Unlock()
	u, err := s.Q.GetUserByID(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, i18n.New("成员不存在"))
	}
	if err := s.manageable(ctx, u); err != nil {
		return nil, err
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
	s.revokeDirectLinks()
	return connect.NewResponse(&nagiv1.DeleteUserResponse{}), nil
}
