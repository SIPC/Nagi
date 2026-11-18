package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 账号、凭据和集群变更的告警（见 docs/audit.md#安全告警）：接口调用记进审计以后，按方法判断要不要通知。
// 这些都是攻击者拿到管理员后最先做的事（关掉告警、生成令牌留后门、关掉两步验证、加节点、部署特权容器），
// 一律通知，不能关。告警配置的修改在处理函数里先用旧配置通知（见 UpdateAlertConfig）。

// notifyChanges 在一次成功的接口调用以后，按方法发出通知。
func (a *API) notifyChanges(ctx context.Context, method string, req proto.Message) {
	if a.Nodes == nil || a.Panel == nil || a.Panel.Standby != nil {
		return
	}
	u := auth.CurrentUser(ctx)
	switch m := req.(type) {
	case *nagiv1.DisableTOTPRequest:
		a.notify(ctx, "2fa", i18n.Errorf("账号 %s 关闭了两步验证", u.Name).Error(), "")
	case *nagiv1.AddPasskeyRequest:
		a.notify(ctx, "passkey", i18n.Errorf("账号 %s 添加了通行密钥 %s", u.Name, m.GetName()).Error(), "")
	case *nagiv1.DeletePasskeyRequest:
		a.notify(ctx, "passkey", i18n.Errorf("账号 %s 删除了通行密钥", u.Name).Error(), "")
	case *nagiv1.ChangePasswordRequest:
		a.notify(ctx, "password", i18n.Errorf("账号 %s 修改了密码", u.Name).Error(), "")
	case *nagiv1.CreateUserRequest:
		if m.GetRoleId() == auth.AdminRoleID || m.GetRoleId() == 0 && m.GetRole() == auth.RoleAdmin {
			a.notify(ctx, "admin", i18n.Errorf("新增管理员 %s", m.GetUsername()).Error(), "")
		}
	case *nagiv1.UpdateUserRequest:
		name := a.userName(ctx, m.GetId())
		if m.GetPassword() != "" {
			a.notify(ctx, "reset-password", i18n.Errorf("成员 %s 的密码被重置", name).Error(), "")
		} else if m.GetClearPasskeys() {
			a.notify(ctx, "passkey", i18n.Errorf("成员 %s 的通行密钥被清除", name).Error(), "")
		}
		if m.GetRoleId() == auth.AdminRoleID || m.GetRoleId() == 0 && m.GetRole() == auth.RoleAdmin {
			a.notify(ctx, "admin", i18n.Errorf("成员 %s 的角色设为管理员", name).Error(), "")
		}
	case *nagiv1.CreateAccessTokenRequest:
		title := i18n.Errorf("账号 %s 生成了访问令牌 %s", u.Name, m.GetName()).Error()
		if m.GetAllowNodeExec() {
			title = i18n.Errorf("账号 %s 生成了可以使用节点终端的访问令牌 %s", u.Name, m.GetName()).Error()
		} else if m.GetAllowPanel() {
			title = i18n.Errorf("账号 %s 生成了可以管理面板的访问令牌 %s", u.Name, m.GetName()).Error()
		}
		a.notify(ctx, "token", title, "")
	case *nagiv1.ApproveOAuthRequest:
		if m.GetAllow() {
			title := i18n.Errorf("账号 %s 授权了 OAuth 客户端", u.Name).Error()
			if m.GetAllowNodeExec() {
				title = i18n.Errorf("账号 %s 授权了 OAuth 客户端，可以使用节点终端", u.Name).Error()
			} else if m.GetAllowPanel() {
				title = i18n.Errorf("账号 %s 授权了 OAuth 客户端，可以管理面板", u.Name).Error()
			}
			a.notify(ctx, "oauth", title, "")
		}
	case *nagiv1.AdminNetworks:
		if len(m.GetNetworks()) == 0 {
			a.notify(ctx, "admin-networks", i18n.Errorf("账号 %s 取消了管理员登录地址的限制", u.Name).Error(), "")
		} else {
			a.notify(ctx, "admin-networks", i18n.Errorf("账号 %s 修改了管理员登录地址：%s", u.Name, strings.Join(m.GetNetworks(), "、")).Error(), "")
		}
	case *nagiv1.SetEntranceRequest:
		a.notify(ctx, "entrance", i18n.Errorf("账号 %s 修改了安全入口", u.Name).Error(), "")
	case *nagiv1.ApproveNodeRequest:
		a.notify(ctx, "node-approve", i18n.Errorf("节点 %s 已批准加入", a.nodeName(ctx, m.GetNodeId())).Error(), "")
	case *nagiv1.HandoverNodesRequest:
		var names []string
		for _, id := range m.GetNodeIds() {
			names = append(names, a.nodeName(ctx, id))
		}
		a.notify(ctx, "node-handover", i18n.Errorf("节点 %s 已移交给其他面板", strings.Join(names, "、")).Error(), "")
	case *nagiv1.CreateStandbyCodeRequest:
		a.notify(ctx, "standby-code", i18n.New("生成了备用面板的配对码").Error(), "")
	case *nagiv1.PromoteStandbyRequest:
		a.notify(ctx, "standby-promote", i18n.New("备用面板已提升为主面板").Error(), "")
	case *nagiv1.SaveDraftRequest:
		a.notifyPrivileged(ctx, "save", m.GetProjectId(), m.GetContainer(), m.GetConfig())
	case *nagiv1.AddContainerRequest:
		a.notifyPrivileged(ctx, "save", m.GetProjectId(), m.GetName(), m.GetConfig())
	case *nagiv1.DeployRequest:
		for _, t := range m.GetTargets() {
			cfg, err := a.Projects.RunningConfig(ctx, m.GetProjectId(), t.GetContainer())
			if err == nil {
				a.notifyPrivileged(ctx, "deploy", m.GetProjectId(), t.GetContainer(), cfg)
			}
		}
	}
}

// notifyPrivileged 在保存或部署带特权内容的配置时通知：项目、容器、特权原因、操作人。
func (a *API) notifyPrivileged(ctx context.Context, what, projectID, container string, cfg *nagiv1.ContainerConfig) {
	if cfg == nil {
		return
	}
	reasons, _ := spec.PrivilegeReasons(cfg, container)
	if len(reasons) == 0 {
		return
	}
	project := projectID
	if p, err := a.Q.GetProject(ctx, projectID); err == nil {
		project = p.Name
	}
	title := i18n.Errorf("保存了带特权内容的配置：%s / %s", project, container).Error()
	if what == "deploy" {
		title = i18n.Errorf("部署了带特权内容的配置：%s / %s", project, container).Error()
	}
	a.notify(ctx, "privileged", title, i18n.Errorf("特权原因：%s", privilegeReasons(reasons).String()).Error())
}

func (a *API) userName(ctx context.Context, id int64) string {
	if u, err := a.Q.GetUserByID(ctx, id); err == nil {
		return u.Username
	}
	return strconv.FormatInt(id, 10)
}

// notify 发一条一次性的安全通知：标题，后面附上操作人和来源。发送在后台进行，不挡接口返回。
// AlertPanelTool 在访问令牌用 MCP 的管理面板工具时发一条告警（见 docs/mcp.md#管理面板），和节点命令一样：
// 令牌泄露时能马上知道。查看类的工具也发。
func (a *API) AlertPanelTool(ctx context.Context, u auth.User, tool string) {
	if a.Nodes == nil || a.Panel == nil || a.Panel.Standby != nil {
		return
	}
	a.notify(auth.WithUser(ctx, u), "panel-admin", i18n.Errorf("访问令牌使用了管理面板的工具 %s", tool).Error(), i18n.Errorf("令牌：%s", strings.TrimPrefix(u.Via, "mcp:")).Error())
}

func (a *API) notify(ctx context.Context, key, title, detail string) {
	u := auth.CurrentUser(ctx)
	m := auth.RequestMeta(ctx)
	msg := i18n.Errorf("成员：%s；来源：%s", u.Name, m.IP).Error()
	if u.TokenID != 0 {
		msg += "（MCP）"
	}
	if detail != "" {
		msg = detail + "\n" + msg
	}
	ctx = context.WithoutCancel(ctx)
	go a.Nodes.PanelAlert(ctx, &nagiv1.Alert{Key: fmt.Sprintf("security:%s:%d", key, time.Now().UnixNano()), Title: title, Message: msg, Time: timestamppb.Now()})
}
