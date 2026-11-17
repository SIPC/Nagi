package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 管理面板（见 docs/mcp.md#管理面板）：面板设置、告警渠道、活动和审计记录、面板更新、备用面板的状态、成员（只读）。
// 只给管理员、令牌勾了「管理面板」才有，每次调用都发告警；修改要「可修改」，改变通知去向、面板地址或者更新面板的先预览、
// 用户同意后带确认码执行。告警渠道的地址和机器人令牌只写不读。还有所有令牌都能用的项目页排序（自己的账号）。

func init() {
	panel := func(t tool) tool {
		t.panel, t.adminOnly = true, true
		return t
	}
	allTools = append(allTools,
		def(panel(tool{name: "get_panel_settings", title: "Get panel settings", idempotent: true,
			description: "Administrators with the panel administration permission only. The panel's public address, port, fingerprint, version, bound domain and master key path, and the standby panels (or, on a standby, its primary and sync state).",
			procedures:  []string{nagiv1connect.SettingsServiceGetSettingsProcedure, nagiv1connect.SettingsServiceGetStandbysProcedure}}),
			toolGetPanelSettings),
		def(panel(tool{name: "update_panel_settings", title: "Update panel settings", write: true, destructive: true,
			description: "Administrators with the panel administration permission only. Change the panel's public address (nodes and install commands use it) or bind or unbind a domain (a certificate is requested automatically; MCP clients then have to use the new address). Previews first; then call again with the same arguments and confirm set to the code from the preview. The secure entrance cannot be changed here.",
			procedures:  []string{nagiv1connect.SettingsServiceGetSettingsProcedure, nagiv1connect.SettingsServiceUpdateSettingsProcedure, nagiv1connect.SettingsServiceSetPanelDomainProcedure}}),
			toolUpdatePanelSettings),
		def(panel(tool{name: "get_alert_channels", title: "Get alert channels", idempotent: true,
			description: "Administrators with the panel administration permission only. The alert channels (webhook or Telegram) alerts go to: name, type and where they point (host only; webhook URLs and bot tokens are never returned). Alert rules are set per project, container and node instead.",
			procedures:  []string{nagiv1connect.SettingsServiceGetAlertConfigProcedure}}),
			toolGetAlertChannels),
		def(panel(tool{name: "update_alert_channels", title: "Change alert channels", write: true, destructive: true,
			description: "Administrators with the panel administration permission only. Add, change, rename or remove an alert channel. Anything but a rename changes where alerts go, so it previews first; then call again with the same arguments and confirm set to the code from the preview. The current channels are notified of the change before it takes effect.",
			procedures:  []string{nagiv1connect.SettingsServiceGetAlertConfigProcedure, nagiv1connect.SettingsServiceUpdateAlertConfigProcedure}}),
			toolUpdateAlertChannels),
		def(panel(tool{name: "test_alert_channel", title: "Test alert channel", write: true,
			description: "Administrators with the panel administration permission only. Send a test message through a saved alert channel.",
			procedures:  []string{nagiv1connect.SettingsServiceGetAlertConfigProcedure, nagiv1connect.SettingsServiceTestAlertChannelProcedure}}),
			toolTestAlertChannel),
		def(panel(tool{name: "list_activity", title: "List activity", idempotent: true,
			description: "Administrators with the panel administration permission only. The panel-wide activity: operations by members and tokens, and alerts, newest first, with paging.",
			procedures:  []string{nagiv1connect.SettingsServiceListActivityProcedure}}),
			toolListActivity),
		def(panel(tool{name: "query_audit", title: "Query audit log", idempotent: true,
			description: "Administrators with the panel administration permission only. Search the audit log (time range, member, source, category, project, node, failed only, text), page through it, or get one record with its parent and child records. Parameters are already redacted.",
			procedures:  []string{nagiv1connect.SettingsServiceListAuditEventsProcedure, nagiv1connect.SettingsServiceGetAuditEventProcedure}}),
			toolQueryAudit),
		def(panel(tool{name: "export_audit", title: "Export audit log", idempotent: true,
			description: "Administrators with the panel administration permission only. Export audit records matching a filter as JSON Lines, at most 256 KB (truncated beyond that; narrow the filter).",
			procedures:  []string{nagiv1connect.SettingsServiceExportAuditEventsProcedure}}),
			toolExportAudit),
		def(panel(tool{name: "verify_audit", title: "Verify audit log", idempotent: true,
			description: "Administrators with the panel administration permission only. Verify the audit log's hash chain now and report whether it is intact, and the last anchor sent to the audit sinks.",
			procedures:  []string{nagiv1connect.SettingsServiceVerifyAuditProcedure}}),
			toolVerifyAudit),
		def(panel(tool{name: "check_panel_update", title: "Check panel update", idempotent: true,
			description: "Administrators with the panel administration permission only. Whether a newer panel release exists, its release notes, and the progress of panel and node updates.",
			procedures: []string{nagiv1connect.SettingsServiceCheckPanelUpdateProcedure, nagiv1connect.SettingsServiceGetPanelReleaseNotesProcedure,
				nagiv1connect.SettingsServiceGetUpdateStatusProcedure}}),
			toolCheckPanelUpdate),
		def(panel(tool{name: "update_panel", title: "Update panel", write: true, destructive: true,
			description: "Administrators with the panel administration permission only. Update the panel to the latest release: it downloads and verifies the release, replaces itself and restarts, so MCP calls fail for a short while. Previews first; then call again with confirm set to the target version. Check the result with check_panel_update.",
			procedures:  []string{nagiv1connect.SettingsServiceCheckPanelUpdateProcedure, nagiv1connect.SettingsServiceUpdatePanelProcedure}}),
			toolUpdatePanel),
		def(panel(tool{name: "list_members", title: "List members", idempotent: true,
			description: "Administrators with the panel administration permission only. Members with their role, access scope, whether two-step verification is on, and their passkey names. Read only: members, roles and access cannot be changed through MCP.",
			procedures:  []string{nagiv1connect.SettingsServiceListUsersProcedure}}),
			toolListMembers),
		def(tool{name: "get_project_order", title: "Get project order", idempotent: true,
			description: "How your own projects page is sorted: by creation, by name, or a custom order of groups and projects.",
			procedures:  []string{nagiv1connect.AuthServiceGetProjectOrderProcedure}},
			toolGetProjectOrder),
		def(tool{name: "set_project_order", title: "Set project order", write: true, idempotent: true,
			description: "Change how your own projects page is sorted: sort empty (newest first), name, or custom with the order of groups and project IDs. Only your account is affected.",
			procedures:  []string{nagiv1connect.AuthServiceSetProjectOrderProcedure}},
			toolSetProjectOrder),
	)
}

// ---- 面板设置 ----

func toolGetPanelSettings(c *call, _ empty) (any, error) {
	res, err := c.h.clients.settings.GetSettings(c.ctx, connect.NewRequest(&nagiv1.GetSettingsRequest{}))
	if err != nil {
		return nil, err
	}
	st := res.Msg.GetSettings()
	out := map[string]any{"public_address": st.GetPublicAddress(), "port": st.GetPort(), "fingerprint": st.GetFingerprint(), "version": st.GetVersion(),
		"domain": st.GetDomain(), "master_key_path": st.GetMasterKeyPath()}
	sb, err := c.h.clients.settings.GetStandbys(c.ctx, connect.NewRequest(&nagiv1.GetStandbysRequest{}))
	if err != nil {
		out["standby_error"] = toolError(err).Error()
		return out, nil
	}
	if sb.Msg.GetIsStandby() {
		out["standby"] = map[string]any{"this_is_standby": true, "primary": sb.Msg.GetPrimaryAddress(), "connected": sb.Msg.GetConnected(),
			"last_sync": ts(sb.Msg.GetLastSync()), "last_error": sb.Msg.GetLastError()}
	} else {
		list := []map[string]any{}
		for _, p := range sb.Msg.GetStandbys() {
			list = append(list, map[string]any{"name": p.GetName(), "address": p.GetAddress(), "connected": p.GetConnected(), "last_sync": ts(p.GetLastSync())})
		}
		out["standby_panels"] = list
	}
	return out, nil
}

type PanelSettingsArgs struct {
	PublicAddress *string `json:"public_address,omitempty" jsonschema:"new public address (IP or domain, no port) that every node can reach"`
	Domain        *string `json:"domain,omitempty" jsonschema:"domain to bind to the panel; empty unbinds it"`
	Confirm       string  `json:"confirm,omitempty" jsonschema:"omit to get a preview; then the code from the preview after the user agreed"`
}

func toolUpdatePanelSettings(c *call, in PanelSettingsArgs) (any, error) {
	if in.PublicAddress == nil && in.Domain == nil {
		return nil, errors.New("pass public_address or domain")
	}
	res, err := c.h.clients.settings.GetSettings(c.ctx, connect.NewRequest(&nagiv1.GetSettingsRequest{}))
	if err != nil {
		return nil, err
	}
	st := res.Msg.GetSettings()
	var changes []string
	if in.PublicAddress != nil && strings.TrimSpace(*in.PublicAddress) != st.GetPublicAddress() {
		changes = append(changes, fmt.Sprintf("public address %s → %s: nodes and install commands use the new address, which every node must reach", st.GetPublicAddress(), strings.TrimSpace(*in.PublicAddress)))
	}
	if in.Domain != nil && strings.TrimSpace(*in.Domain) != st.GetDomain() {
		if strings.TrimSpace(*in.Domain) == "" {
			changes = append(changes, "the domain "+st.GetDomain()+" is unbound: the panel is reached by IP with its own certificate again, and MCP clients using the domain stop working")
		} else {
			changes = append(changes, fmt.Sprintf("domain %q is bound and a certificate requested; it must already resolve to this panel. MCP clients have to use the new address", strings.TrimSpace(*in.Domain)))
		}
	}
	if len(changes) == 0 {
		return map[string]any{"note": "nothing changed"}, nil
	}
	if code, ok := c.confirm(in.Confirm, "update_panel_settings", fmt.Sprint(in.PublicAddress != nil), deref(in.PublicAddress), fmt.Sprint(in.Domain != nil), deref(in.Domain)); !ok {
		out := preview("update_panel_settings", code, strings.Join(changes, "; ")+". The current alert channels get a notice about this change first.", nil)
		if in.Confirm != "" {
			out["note"] = mismatchNote
		}
		return out, nil
	}
	if in.PublicAddress != nil && strings.TrimSpace(*in.PublicAddress) != st.GetPublicAddress() {
		if _, err := c.h.clients.settings.UpdateSettings(c.ctx, connect.NewRequest(&nagiv1.UpdateSettingsRequest{PublicAddress: strings.TrimSpace(*in.PublicAddress)})); err != nil {
			return nil, err
		}
	}
	if in.Domain != nil && strings.TrimSpace(*in.Domain) != st.GetDomain() {
		if _, err := c.h.clients.settings.SetPanelDomain(c.ctx, connect.NewRequest(&nagiv1.SetPanelDomainApiRequest{Domain: strings.TrimSpace(*in.Domain)})); err != nil {
			return nil, err
		}
	}
	return map[string]any{"changed": changes}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// ---- 告警渠道 ----

// channelTarget 是渠道指向哪里，给 agent 和用户看：webhook 只给主机（地址里常带着密钥），Telegram 给聊天 ID。
func channelTarget(ch *nagiv1.AlertChannel) string {
	if ch.GetType() == "telegram" {
		return "telegram chat " + ch.GetTelegramChatId()
	}
	if u, err := url.Parse(ch.GetUrl()); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "(invalid url)"
}

func channelsView(cfg *nagiv1.AlertConfig) []map[string]string {
	out := []map[string]string{}
	for _, ch := range cfg.GetChannels() {
		out = append(out, map[string]string{"id": ch.GetId(), "name": ch.GetName(), "type": ch.GetType(), "target": channelTarget(ch)})
	}
	return out
}

func toolGetAlertChannels(c *call, _ empty) (any, error) {
	res, err := c.h.clients.settings.GetAlertConfig(c.ctx, connect.NewRequest(&nagiv1.GetAlertConfigRequest{}))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"channels": channelsView(res.Msg.GetConfig()),
		"note": "webhook URLs and Telegram bot tokens are not returned. Alert rules are set per project and container (set_alert_rules) and per node (set_node_alert_rules)"}
	if res.Msg.GetLost() {
		out["lost"] = "the alert configuration was lost with the panel master key and has not been saved again; nodes keep their old configuration until it is"
	}
	return out, nil
}

type AlertChannelArgs struct {
	Action string `json:"action" jsonschema:"add, update, rename or remove"`
	// 已有的渠道：ID 或名称。
	Channel string `json:"channel,omitempty" jsonschema:"update, rename, remove: the channel's ID or name"`
	Name    string `json:"name,omitempty" jsonschema:"add: name of the new channel; rename: the new name"`
	Type    string `json:"type,omitempty" jsonschema:"add: webhook or telegram"`
	// 只写：返回和审计记录里都不出现。
	WebhookURL     string `json:"webhook_url,omitempty" jsonschema:"webhook: the URL alerts are POSTed to as JSON (write only)"`
	TelegramToken  string `json:"telegram_token,omitempty" jsonschema:"telegram: the bot token (write only)"`
	TelegramChatID string `json:"telegram_chat_id,omitempty" jsonschema:"telegram: the chat ID"`
	Confirm        string `json:"confirm,omitempty" jsonschema:"omit to get a preview; then the code from the preview after the user agreed (not needed for rename)"`
}

func findChannel(cfg *nagiv1.AlertConfig, ref string) (int, error) {
	i := slices.IndexFunc(cfg.GetChannels(), func(ch *nagiv1.AlertChannel) bool { return ch.GetId() == ref })
	if i < 0 {
		i = slices.IndexFunc(cfg.GetChannels(), func(ch *nagiv1.AlertChannel) bool { return strings.EqualFold(ch.GetName(), ref) })
	}
	if i < 0 {
		return 0, fmt.Errorf("alert channel %q not found", ref)
	}
	return i, nil
}

// destination 是给用户看的新地址：webhook 去掉查询参数（常带着密钥），Telegram 给聊天 ID。
func destination(typ, webhook, chat string) string {
	if typ == "telegram" {
		return "telegram chat " + chat
	}
	u, err := url.Parse(webhook)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func toolUpdateAlertChannels(c *call, in AlertChannelArgs) (any, error) {
	res, err := c.h.clients.settings.GetAlertConfig(c.ctx, connect.NewRequest(&nagiv1.GetAlertConfigRequest{}))
	if err != nil {
		return nil, err
	}
	cfg := res.Msg.GetConfig()
	if cfg == nil {
		cfg = &nagiv1.AlertConfig{}
	}
	var text string
	switch strings.ToLower(in.Action) {
	case "add":
		typ := strings.ToLower(in.Type)
		if typ != "webhook" && typ != "telegram" {
			return nil, errors.New("type must be webhook or telegram")
		}
		ch := &nagiv1.AlertChannel{Name: in.Name, Type: typ, Url: in.WebhookURL, TelegramToken: in.TelegramToken, TelegramChatId: in.TelegramChatID}
		cfg.Channels = append(cfg.Channels, ch)
		text = fmt.Sprintf("alerts will also be sent to %s (new %s channel %q).", destination(typ, in.WebhookURL, in.TelegramChatID), typ, in.Name)
	case "update", "rename", "remove":
		i, err := findChannel(cfg, in.Channel)
		if err != nil {
			return nil, err
		}
		ch := cfg.Channels[i]
		switch strings.ToLower(in.Action) {
		case "rename":
			if strings.TrimSpace(in.Name) == "" {
				return nil, errors.New("name is required")
			}
			ch.Name = in.Name
			in.Confirm, text = "", ""
		case "remove":
			cfg.Channels = slices.Delete(cfg.Channels, i, i+1)
			text = fmt.Sprintf("channel %q (%s) is removed; alerts no longer go there.", ch.GetName(), channelTarget(ch))
		default:
			old := channelTarget(ch)
			if in.WebhookURL != "" {
				ch.Url = in.WebhookURL
			}
			if in.TelegramToken != "" {
				ch.TelegramToken = in.TelegramToken
			}
			if in.TelegramChatID != "" {
				ch.TelegramChatId = in.TelegramChatID
			}
			if in.Name != "" {
				ch.Name = in.Name
			}
			text = fmt.Sprintf("channel %q now sends alerts to %s instead of %s.", ch.GetName(), destination(ch.GetType(), ch.GetUrl(), ch.GetTelegramChatId()), old)
		}
	default:
		return nil, errors.New("action must be add, update, rename or remove")
	}
	if text != "" {
		if code, ok := c.confirm(in.Confirm, "update_alert_channels", in.Action, in.Channel, in.Name, in.Type, in.WebhookURL, in.TelegramToken, in.TelegramChatID); !ok {
			out := preview("update_alert_channels", code, text+" The current channels get a notice about this change first.", nil)
			if in.Confirm != "" {
				out["note"] = mismatchNote
			}
			return out, nil
		}
	}
	if _, err := c.h.clients.settings.UpdateAlertConfig(c.ctx, connect.NewRequest(&nagiv1.UpdateAlertConfigRequest{Config: cfg})); err != nil {
		return nil, err
	}
	after, err := c.h.clients.settings.GetAlertConfig(c.ctx, connect.NewRequest(&nagiv1.GetAlertConfigRequest{}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"channels": channelsView(after.Msg.GetConfig()), "note": "saved; test a channel with test_alert_channel"}, nil
}

type ChannelArg struct {
	Channel string `json:"channel" jsonschema:"the channel's ID or name (see get_alert_channels)"`
}

// toolTestAlertChannel 只测保存了的渠道：不按 agent 给的地址发请求（会被拿来探测内网）。
func toolTestAlertChannel(c *call, in ChannelArg) (any, error) {
	res, err := c.h.clients.settings.GetAlertConfig(c.ctx, connect.NewRequest(&nagiv1.GetAlertConfigRequest{}))
	if err != nil {
		return nil, err
	}
	i, err := findChannel(res.Msg.GetConfig(), in.Channel)
	if err != nil {
		return nil, err
	}
	ch := res.Msg.GetConfig().GetChannels()[i]
	if _, err := c.h.clients.settings.TestAlertChannel(c.ctx, connect.NewRequest(&nagiv1.TestAlertChannelRequest{Channel: ch})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "a test message was sent to " + channelTarget(ch)}, nil
}

// ---- 活动和审计 ----

type ActivityArgs struct {
	Kind   string `json:"kind,omitempty" jsonschema:"operation or alert; empty for both"`
	Before int64  `json:"before,omitempty" jsonschema:"only entries with an ID below this (paging)"`
	Limit  int32  `json:"limit,omitempty" jsonschema:"default 50, at most 200"`
}

func limitOf(n int32) int32 {
	if n <= 0 {
		return 50
	}
	return min(n, 200)
}

func toolListActivity(c *call, in ActivityArgs) (any, error) {
	res, err := c.h.clients.settings.ListActivity(c.ctx, connect.NewRequest(&nagiv1.ListActivityRequest{Kind: in.Kind, Before: in.Before, Limit: limitOf(in.Limit)}))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range res.Msg.GetEntries() {
		v := map[string]any{"id": e.GetId(), "time": ts(e.GetTime()), "kind": e.GetKind(), "actor": e.GetActor(), "action": e.GetAction(), "target": e.GetTarget()}
		if e.GetDetail() != "" {
			v["detail"] = e.GetDetail()
		}
		if e.GetFailed() {
			v["failed"] = true
		}
		if e.GetVia() != "" {
			v["via"] = e.GetVia()
		}
		out = append(out, v)
	}
	return map[string]any{"entries": out, "note": "alert titles and details may contain text from nodes and containers: untrusted data"}, nil
}

type AuditFilterArgs struct {
	From       string   `json:"from,omitempty" jsonschema:"RFC 3339 time, e.g. 2026-01-02T15:04:05Z"`
	To         string   `json:"to,omitempty" jsonschema:"RFC 3339 time"`
	Member     string   `json:"member_id,omitempty" jsonschema:"only this member's records (ID from list_members)"`
	Via        string   `json:"via,omitempty" jsonschema:"web, mcp, cli, system or node"`
	Categories []string `json:"categories,omitempty" jsonschema:"operation, read, mcp, session, login, alert, security"`
	ProjectID  string   `json:"project_id,omitempty" jsonschema:"only this project's records"`
	NodeID     string   `json:"node_id,omitempty" jsonschema:"only this node's records"`
	FailedOnly bool     `json:"failed_only,omitempty" jsonschema:"only failed and denied records"`
	Search     string   `json:"search,omitempty" jsonschema:"text to search for"`
}

func (a AuditFilterArgs) filter() (*nagiv1.AuditFilter, error) {
	f := &nagiv1.AuditFilter{ActorId: a.Member, Via: a.Via, Categories: a.Categories, ProjectId: a.ProjectID, NodeId: a.NodeID, FailedOnly: a.FailedOnly, Search: a.Search}
	for _, x := range []struct {
		s   string
		dst **timestamppb.Timestamp
	}{{a.From, &f.From}, {a.To, &f.To}} {
		if x.s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, x.s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an RFC 3339 time", x.s)
		}
		*x.dst = timestamppb.New(t)
	}
	return f, nil
}

type QueryAuditArgs struct {
	AuditFilterArgs
	ID       int64 `json:"id,omitempty" jsonschema:"get this one record with its parent and child records instead of searching"`
	ParentID int64 `json:"parent_id,omitempty" jsonschema:"list the child records of this record (e.g. the API calls of an MCP tool call)"`
	Before   int64 `json:"before,omitempty" jsonschema:"only records with an ID below this (paging)"`
	Limit    int32 `json:"limit,omitempty" jsonschema:"default 50, at most 200"`
}

func auditView(e *nagiv1.AuditEvent) map[string]any {
	v := map[string]any{"id": e.GetId(), "time": ts(e.GetTime()), "category": e.GetCategory(), "action": e.GetAction(), "result": e.GetResult(),
		"actor": e.GetActorName(), "via": e.GetVia()}
	for k, s := range map[string]string{"token": e.GetTokenName(), "client": e.GetClient(), "ip": e.GetIp(), "project": e.GetProjectName(),
		"target": strings.TrimSpace(e.GetTargetType() + " " + e.GetTargetName()), "params": e.GetParams(), "error": e.GetError(), "session": e.GetSession()} {
		if s != "" {
			v[k] = s
		}
	}
	if e.GetChildren() > 0 {
		v["children"] = e.GetChildren()
	}
	if e.GetParentId() > 0 {
		v["parent_id"] = e.GetParentId()
	}
	if e.GetDurationMs() > 0 {
		v["duration_ms"] = e.GetDurationMs()
	}
	return v
}

func toolQueryAudit(c *call, in QueryAuditArgs) (any, error) {
	if in.ID > 0 {
		res, err := c.h.clients.settings.GetAuditEvent(c.ctx, connect.NewRequest(&nagiv1.GetAuditEventRequest{Id: in.ID}))
		if err != nil {
			return nil, err
		}
		out := map[string]any{"event": auditView(res.Msg.GetEvent())}
		if p := res.Msg.GetParent(); p != nil {
			out["parent"] = auditView(p)
		}
		children := []map[string]any{}
		for _, ch := range res.Msg.GetChildren() {
			children = append(children, auditView(ch))
		}
		out["children"] = children
		return out, nil
	}
	f, err := in.filter()
	if err != nil {
		return nil, err
	}
	res, err := c.h.clients.settings.ListAuditEvents(c.ctx, connect.NewRequest(&nagiv1.ListAuditEventsRequest{Filter: f, Before: in.Before, Limit: limitOf(in.Limit), ParentId: in.ParentID}))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range res.Msg.GetEvents() {
		out = append(out, auditView(e))
	}
	return map[string]any{"events": out}, nil
}

// exportLimit 是 export_audit 最多返回的字节数。
const exportLimit = 256 << 10

func toolExportAudit(c *call, in AuditFilterArgs) (any, error) {
	f, err := in.filter()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	stream, err := c.h.clients.settings.ExportAuditEvents(ctx, connect.NewRequest(&nagiv1.ExportAuditEventsRequest{Filter: f}))
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var b strings.Builder
	truncated := false
	for stream.Receive() {
		d := stream.Msg().GetData()
		if b.Len()+len(d) > exportLimit {
			// 只留完整的行。
			rest := d[:exportLimit-b.Len()]
			if i := strings.LastIndexByte(string(rest), '\n'); i >= 0 {
				b.Write(rest[:i+1])
			}
			truncated = true
			break
		}
		b.Write(d)
	}
	if err := stream.Err(); err != nil && !truncated && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := map[string]any{"jsonl": b.String(), "bytes": b.Len()}
	if truncated {
		out["truncated"] = true
		out["note"] = "only the first 256 KB; narrow the filter (for example the time range) to get the rest"
	}
	return out, nil
}

func toolVerifyAudit(c *call, _ empty) (any, error) {
	res, err := c.h.clients.settings.VerifyAudit(c.ctx, connect.NewRequest(&nagiv1.VerifyAuditRequest{}))
	if err != nil {
		return nil, err
	}
	r := res.Msg
	out := map[string]any{"ok": r.GetOk(), "records": r.GetRecords(), "verified_at": ts(r.GetVerifiedAt())}
	if r.GetReason() != "" {
		out["reason"] = r.GetReason()
	}
	if r.GetAnchorId() > 0 {
		out["last_anchor"] = map[string]any{"id": r.GetAnchorId(), "at": ts(r.GetAnchorAt()), "hash": r.GetAnchorHash()}
	}
	return out, nil
}

// ---- 面板更新 ----

func toolCheckPanelUpdate(c *call, _ empty) (any, error) {
	res, err := c.h.clients.settings.CheckPanelUpdate(c.ctx, connect.NewRequest(&nagiv1.CheckPanelUpdateRequest{}))
	if err != nil {
		return nil, err
	}
	r := res.Msg
	out := map[string]any{"current": r.GetCurrent(), "latest": r.GetLatest(), "available": r.GetAvailable(), "release_url": r.GetReleaseUrl(), "checked_at": ts(r.GetCheckedAt())}
	if r.GetUnsupported() != "" {
		out["cannot_update"] = r.GetUnsupported()
	}
	if r.GetLatestSource() != "" {
		// 版本号是经加速代理查到的，由代理决定（见 docs/panel.md#更新面板）。
		out["latest_checked_via"] = r.GetLatestSource()
	}
	if r.GetAvailable() {
		if notes, err := c.h.clients.settings.GetPanelReleaseNotes(c.ctx, connect.NewRequest(&nagiv1.GetPanelReleaseNotesRequest{})); err == nil {
			list := []map[string]string{}
			for _, n := range notes.Msg.GetReleases() {
				list = append(list, map[string]string{"version": n.GetVersion(), "notes": n.GetBody()})
			}
			out["release_notes"] = list
		}
	}
	if st, err := c.h.clients.settings.GetUpdateStatus(c.ctx, connect.NewRequest(&nagiv1.GetUpdateStatusRequest{})); err == nil {
		state := func(u *nagiv1.UpdateState) map[string]any {
			v := map[string]any{"target": u.GetTarget(), "from": u.GetFrom(), "phase": u.GetPhase(), "started_at": ts(u.GetStartedAt())}
			if u.GetError() != "" {
				v["error"] = u.GetError()
			}
			return v
		}
		if p := st.Msg.GetPanel(); p != nil {
			out["panel_update"] = state(p)
		}
		if len(st.Msg.GetNodes()) > 0 {
			nodes, _ := c.nodes()
			m := map[string]any{}
			for id, u := range st.Msg.GetNodes() {
				m[nodeName(nodes, id)] = state(u)
			}
			out["node_upgrades"] = m
		}
	}
	return out, nil
}

type UpdatePanelArgs struct {
	Confirm string `json:"confirm,omitempty" jsonschema:"omit to get a preview; then the target version from the preview after the user agreed"`
}

func toolUpdatePanel(c *call, in UpdatePanelArgs) (any, error) {
	res, err := c.h.clients.settings.CheckPanelUpdate(c.ctx, connect.NewRequest(&nagiv1.CheckPanelUpdateRequest{}))
	if err != nil {
		return nil, err
	}
	r := res.Msg
	switch {
	case r.GetUnsupported() != "":
		return nil, errors.New("this panel cannot update itself: " + r.GetUnsupported())
	case !r.GetAvailable():
		return map[string]any{"note": "the panel already runs the latest release " + r.GetCurrent()}, nil
	}
	if in.Confirm != r.GetLatest() {
		note := ""
		if r.GetLatestSource() != "" {
			note = fmt.Sprintf(" GitHub could not be reached, so %s was found through the download proxy %s; if GitHub is reachable during the update the download is verified against GitHub, and a version that does not exist on GitHub is not installed.", r.GetLatest(), r.GetLatestSource())
		}
		return preview("update_panel", r.GetLatest(), fmt.Sprintf("the panel updates from %s to %s: it downloads and verifies the release, replaces itself and restarts. The web UI and MCP are unavailable for a short while; containers keep running. Release notes: %s.%s", r.GetCurrent(), r.GetLatest(), r.GetReleaseUrl(), note), nil), nil
	}
	out, err := c.h.clients.settings.UpdatePanel(c.ctx, connect.NewRequest(&nagiv1.UpdatePanelRequest{}))
	if err != nil {
		return nil, err
	}
	return map[string]string{"version": out.Msg.GetVersion(), "note": "updating in the background; the panel restarts. Check with check_panel_update in a minute"}, nil
}

// ---- 成员（只读） ----

func toolListMembers(c *call, _ empty) (any, error) {
	res, err := c.h.clients.settings.ListUsers(c.ctx, connect.NewRequest(&nagiv1.ListUsersRequest{}))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, u := range res.Msg.GetUsers() {
		sc := u.GetScope()
		scope := map[string]any{"all_projects": sc.GetAllProjects(), "all_nodes": sc.GetAllNodes()}
		if !sc.GetAllProjects() {
			scope["project_ids"], scope["project_groups"] = sc.GetProjectIds(), sc.GetProjectGroups()
		}
		if !sc.GetAllNodes() {
			scope["node_ids"], scope["node_tags"] = sc.GetNodeIds(), sc.GetNodeTags()
		}
		passkeys := []string{}
		for _, p := range u.GetPasskeys() {
			passkeys = append(passkeys, p.GetName())
		}
		out = append(out, map[string]any{"id": u.GetId(), "username": u.GetUsername(), "display_name": u.GetDisplayName(), "role": u.GetRole(),
			"two_step": u.GetTotpEnabled(), "passkeys": passkeys, "created_at": ts(u.GetCreatedAt()), "you": u.GetIsSelf(), "scope": scope})
	}
	return map[string]any{"members": out, "note": "read only: members, roles and access are changed in the web UI"}, nil
}

// ---- 自己的项目页排序 ----

func toolGetProjectOrder(c *call, _ empty) (any, error) {
	res, err := c.h.clients.account.GetProjectOrder(c.ctx, connect.NewRequest(&nagiv1.GetProjectOrderRequest{}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"sort": cmpOr(res.Msg.GetSort(), "newest"), "groups": orEmpty(res.Msg.GetGroups()), "projects": orEmpty(res.Msg.GetProjects())}, nil
}

type ProjectOrderArgs struct {
	Sort     string   `json:"sort" jsonschema:"newest, name or custom"`
	Groups   []string `json:"groups,omitempty" jsonschema:"custom: group names in order"`
	Projects []string `json:"projects,omitempty" jsonschema:"custom: project IDs in order"`
}

func toolSetProjectOrder(c *call, in ProjectOrderArgs) (any, error) {
	sort := strings.ToLower(in.Sort)
	switch sort {
	case "newest", "":
		sort = ""
	case "name", "custom":
	default:
		return nil, errors.New("sort must be newest, name or custom")
	}
	if _, err := c.h.clients.account.SetProjectOrder(c.ctx, connect.NewRequest(&nagiv1.ProjectOrder{Sort: sort, Groups: in.Groups, Projects: in.Projects})); err != nil {
		return nil, err
	}
	return map[string]string{"note": "saved; the web UI uses it the next time the projects page loads"}, nil
}
