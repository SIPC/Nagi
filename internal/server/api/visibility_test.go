package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

// memberViewer 是测试用的查看者：n1 看得到，n2 看不到。
func memberViewer(a *API, perms auth.Perms) auditViewer {
	ctx := context.Background()
	x := memberAccess(perms)
	v := auditViewer{x: x, viewer: !x.can(auth.PermConfig), other: "其他节点", scrub: func(s string) string { return a.scrubNodeIDs(ctx, x, s) }}
	v.refs = func(s string) string {
		return i18n.RenderRefs(s, func(r i18n.Ref) string {
			if r.Kind == "node" && !x.node(r.ID) {
				return v.other
			}
			return r.Name
		})
	}
	return v
}

func adminViewer() auditViewer {
	return auditViewer{admin: true, scrub: func(s string) string { return s }, refs: i18n.PlainRefs}
}

// 来自节点的文字里，看不到的节点改名前的名称、主机名也换成「其他节点」；看得到的节点的不换。
func TestScrubFormerNamesAndHostnames(t *testing.T) {
	info := func(host string) string {
		b, err := proto.Marshal(&nagiv1.NodeInfo{Hostname: host})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("x'%x'", b)
	}
	a, q := scopeTestAPI(t,
		`UPDATE nodes SET info = `+info("host-b")+` WHERE id = 'n2'`,
		`UPDATE nodes SET info = `+info("host-a")+` WHERE id = 'n1'`,
		`INSERT INTO nodes (id, name, created_at, mesh_key, info) VALUES ('n3', 'c', 0, x'03', `+info("localhost")+`)`)
	ctx := context.Background()
	a.rememberNodeName(ctx, "n2", "old-b")
	a.rememberNodeName(ctx, "n1", "old-a")
	a.rememberNodeName(ctx, "gone", "ghost")
	x := memberAccess(auth.PresetOperator)
	ev := &nagiv1.DeployEvent{Event: &nagiv1.DeployEvent_Output{Output: "sync to old-b failed, host-b unreachable, b down; old-a host-a a fine; localhost:80 ghost"}}
	a.filterReply(ctx, x, ev)
	if got, want := ev.GetOutput(), "sync to 其他节点 failed, 其他节点 unreachable, 其他节点 down; old-a host-a a fine; localhost:80 ghost"; got != want {
		t.Errorf("部署输出：%s", got)
	}
	// 顶替状态里记的是设置时的名称。
	st := &nagiv1.FailoverStatus{HolderName: "old-b"}
	a.cleanFailoverStatus(ctx, x, st)
	if st.HolderName != "" {
		t.Errorf("顶替的持有者：%q", st.HolderName)
	}
	// 同一个名称只记一次，最多记 maxFormerNames 个；已经移除的节点的记录清掉。
	for i := range maxFormerNames + 3 {
		a.rememberNodeName(ctx, "n2", fmt.Sprintf("name-%d", i))
	}
	a.rememberNodeName(ctx, "n2", "name-5")
	names := a.formerNodeNames(ctx)
	if len(names["n2"]) != maxFormerNames || names["n2"][maxFormerNames-1] != "name-5" || len(names["gone"]) != 0 || len(names["n1"]) != 1 {
		t.Errorf("记下的名称：%v", names)
	}
	_ = q
}

// webhook 令牌要「部署」权限：拿到它不用登录就能触发部署。
func TestCleanWebhookNeedsDeploy(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	p := richProject()
	a.filterReply(ctx, memberAccess(auth.PermsOf(auth.PermView, auth.PermConfig, auth.PermNodeView)), p)
	if c := p.Containers[0]; c.WebhookToken != "" || c.Config == nil || c.DeployKey != "k" {
		t.Errorf("只有查看配置：令牌 %q 配置 %v", c.WebhookToken, c.Config)
	}
	p = richProject()
	a.filterReply(ctx, memberAccess(auth.PresetOperator), p)
	if p.Containers[0].WebhookToken != "w" {
		t.Error("有部署权限应该拿到 webhook 令牌")
	}
}

// 热备节点看不到时不给它上报的状态（有没有这份状态就是它在不在线）；等着确认顶替时保留。
func TestCleanStandbyStatus(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	ha := auth.PresetOperator | auth.PermsOf(auth.PermHA)
	project := func(node string, standby *nagiv1.FailoverStatus) *nagiv1.Project {
		return &nagiv1.Project{Id: "p1", Containers: []*nagiv1.Container{{Name: "web", NodeId: node,
			Failover:        &nagiv1.FailoverConfig{A: &nagiv1.FailoverParticipant{NodeId: "n1"}, B: &nagiv1.FailoverParticipant{NodeId: "n2"}},
			FailoverHolder:  &nagiv1.FailoverStatus{Role: "holder"},
			FailoverStandby: standby}}}
	}
	p := project("n1", &nagiv1.FailoverStatus{Role: "standby"})
	a.filterReply(ctx, memberAccess(ha), p)
	if c := p.Containers[0]; c.FailoverStandby != nil || c.FailoverHolder == nil {
		t.Errorf("热备 n2 看不到：%v", c)
	}
	p = project("n1", &nagiv1.FailoverStatus{Role: "standby", PendingConfirm: true})
	a.filterReply(ctx, memberAccess(ha), p)
	if !p.Containers[0].GetFailoverStandby().GetPendingConfirm() {
		t.Error("等着确认顶替时要保留")
	}
	p = project("n2", &nagiv1.FailoverStatus{Role: "standby"})
	a.filterReply(ctx, memberAccess(ha), p)
	if p.Containers[0].FailoverStandby == nil {
		t.Error("热备 n1 看得到，状态照常给")
	}
}

// 出站告警的标题和键里有容器尝试连接的 IP 和端口：没有「查看访问地址」的成员看不到，有的和管理员照旧。
func TestEgressAlertTarget(t *testing.T) {
	a := cleanTest(t)
	for _, target := range []string{"10.0.0.5:22", "[fe80::1]:8080"} {
		title := "容器 web 尝试连接 " + target + "，已拦截"
		e := audit.Event{Category: audit.Alert, Action: "firing", ProjectID: "p1", ActorType: "node", ActorID: "n1", ActorName: "a",
			TargetType: "container", TargetID: "web", TargetName: "web",
			Params: `{"title":"` + title + `","key":"egress:p1/web/` + target + `"}`}
		host := strings.Trim(target[:strings.LastIndex(target, ":")], "[]")
		for name, v := range map[string]auditViewer{
			"操作员": memberViewer(a, auth.PresetOperator),
			"只读":  memberViewer(a, auth.PresetViewer),
			"网络":  memberViewer(a, auth.PresetOperator|auth.PermsOf(auth.PermNetwork)),
		} {
			got := v.activityEntry(e, auditNames{})
			if got.GetDetail() != "容器 web 尝试连接 …，已拦截" {
				t.Errorf("%s 的事件标题：%q", name, got.GetDetail())
			}
			if p := v.entry(e, auditNames{}).GetParams(); strings.Contains(p, host) || strings.Contains(p, target) {
				t.Errorf("%s 的审计参数：%s", name, p)
			} else if !v.viewer && !strings.Contains(p, `"key":"egress:p1/web"`) {
				t.Errorf("%s 的审计参数里键只留到容器：%s", name, p)
			}
		}
		for name, v := range map[string]auditViewer{
			"有查看访问地址": memberViewer(a, auth.PresetOperator|auth.PermsOf(auth.PermNetwork, auth.PermAddresses)),
			"管理员":     adminViewer(),
		} {
			if got := v.activityEntry(e, auditNames{}); got.GetDetail() != title {
				t.Errorf("%s 的事件标题：%q", name, got.GetDetail())
			}
			if p := v.entry(e, auditNames{}).GetParams(); !strings.Contains(p, "egress:p1/web/"+target) {
				t.Errorf("%s 的审计参数：%s", name, p)
			}
		}
	}
	// 别的告警不动。
	other := audit.Event{Category: audit.Alert, ProjectID: "p1", Params: `{"title":"容器 web 重启 3 次","key":"health:p1/web/web"}`}
	if got := memberViewer(a, auth.PresetViewer).activityEntry(other, auditNames{}); got.GetDetail() != "容器 web 重启 3 次" {
		t.Errorf("别的告警：%q", got.GetDetail())
	}
}

// 记录的参数和失败详情：除了「查看配置」，还要做这个操作要的权限。执行的命令、文件路径、域名这些，
// 只有「查看配置」的成员在项目页上看不到，从记录里也看不到。
func TestAuditDetailNeedsActionPerm(t *testing.T) {
	a := cleanTest(t)
	config := auth.PermsOf(auth.PermView, auth.PermConfig, auth.PermNodeView)
	cases := []struct {
		name   string
		e      audit.Event
		perms  auth.Perms
		detail bool
	}{
		{"命令，只有查看配置", audit.Event{Category: audit.Session, Action: "ExecCommand", Params: `{"command":"mysql -psecret"}`, Error: "timeout"}, config, false},
		{"命令，有终端", audit.Event{Category: audit.Session, Action: "ExecCommand", Params: `{"command":"mysql -psecret"}`, Error: "timeout"}, config | auth.PermsOf(auth.PermTerminal), true},
		{"打开终端，只有查看配置", audit.Event{Category: audit.Session, Action: "OpenTerminal", Params: `{"command":["sh"]}`}, config, false},
		{"部署，只有查看配置", audit.Event{Category: audit.Operation, Action: "Deploy", Params: `{"targets":[{"container":"web"}]}`, Error: "compose 报错"}, config, true},
		{"控制，只有查看配置", audit.Event{Category: audit.Operation, Action: "ControlContainer", Params: `{"action":"restart"}`}, config, true},
		{"端口映射，只有查看配置", audit.Event{Category: audit.Operation, Action: "SaveRoute", Params: `{"route":{"domains":["a.example.com"]}}`}, config, false},
		{"端口映射，有网络和域名", audit.Event{Category: audit.Operation, Action: "SaveRoute", Params: `{"route":{"domains":["a.example.com"]}}`}, config | auth.PermsOf(auth.PermNetwork), true},
		{"项目设置，只有查看配置", audit.Event{Category: audit.Operation, Action: "UpdateProjectSettings", Params: `{"settings":{}}`}, config, false},
		{"镜像仓库，操作员", audit.Event{Category: audit.Operation, Action: "LoginRegistry", Params: `{"server":"r.example.com"}`}, auth.PresetOperator, false},
		{"备份密钥，有备份", audit.Event{Category: audit.Read, Action: "GetBackupKey", Params: `{"container":"web"}`}, config | auth.PermsOf(auth.PermBackup), true},
		{"认不出的动作", audit.Event{Category: audit.Operation, Action: "SomethingNew", Params: `{"x":1}`}, auth.PresetOperator, false},
		{"没有查看配置", audit.Event{Category: audit.Operation, Action: "Deploy", Params: `{"x":1}`, Error: "y"}, auth.PresetViewer, false},
		{"MCP 执行命令，只有查看配置", audit.Event{Category: audit.MCP, Action: "mcp:exec", Params: `{"arguments":{"command":["id"]},"perms":["project.view","project.terminal"]}`}, config, false},
		{"MCP 执行命令，有终端", audit.Event{Category: audit.MCP, Action: "mcp:exec", Params: `{"arguments":{"command":["id"]},"perms":["project.view","project.terminal"]}`}, auth.PresetOperator, true},
		{"MCP 只给管理员的工具", audit.Event{Category: audit.MCP, Action: "mcp:x", Params: `{"arguments":{},"perms":["admin"]}`}, auth.PresetOperator, false},
		{"以前的 MCP 记录", audit.Event{Category: audit.MCP, Action: "mcp:deploy", Params: `{"arguments":{"node":"b"}}`, Error: "节点 b 离线"}, auth.PresetOperator, false},
	}
	for _, c := range cases {
		c.e.ProjectID = "p1"
		v := memberViewer(a, c.perms)
		got := v.entry(c.e, auditNames{})
		if (got.GetParams() != "") != c.detail || c.e.Error != "" && (got.GetError() != "") != c.detail {
			t.Errorf("%s：参数 %q 失败详情 %q，应该%s", c.name, got.GetParams(), got.GetError(), map[bool]string{true: "给", false: "不给"}[c.detail])
		}
		if ev := v.activityEntry(c.e, auditNames{}); c.e.Error != "" && (ev.GetDetail() != "") != c.detail {
			t.Errorf("%s：事件详情 %q", c.name, ev.GetDetail())
		}
		if got := adminViewer().entry(c.e, auditNames{}); got.GetParams() != c.e.Params || got.GetError() != c.e.Error {
			t.Errorf("%s：管理员照常看到，得到 %q %q", c.name, got.GetParams(), got.GetError())
		}
	}
	// 文件操作：没有文件权限时路径也不给，只留容器。
	file := audit.Event{Category: audit.Read, Action: "DownloadVolumeFile", ProjectID: "p1", TargetType: "file", TargetID: "/data/secret.txt",
		TargetName: "web:/data/secret.txt", Params: `{"path":"/data/secret.txt"}`, Result: audit.ResultFailed, Error: "no such file"}
	v := memberViewer(a, config)
	if got := v.entry(file, auditNames{}); got.GetTargetName() != "web" || got.GetTargetId() != "" || got.GetParams() != "" {
		t.Errorf("没有文件权限：%q %q %q", got.GetTargetName(), got.GetTargetId(), got.GetParams())
	}
	if got := v.activityEntry(file, auditNames{}); got.GetDetail() != "" || got.GetTarget() != "web" {
		t.Errorf("没有文件权限的事件：%q %q", got.GetTarget(), got.GetDetail())
	}
	v = memberViewer(a, config|auth.PermsOf(auth.PermFilesRead))
	if got := v.entry(file, auditNames{}); got.GetTargetName() != "web:/data/secret.txt" || got.GetParams() == "" {
		t.Errorf("有文件查看：%q %q", got.GetTargetName(), got.GetParams())
	}
	if got := v.activityEntry(file, auditNames{}); got.GetDetail() != "/data/secret.txt: no such file" {
		t.Errorf("有文件查看的事件：%q", got.GetDetail())
	}
}

// 按写入时处理的 MCP 记录：节点参数是 ID，失败详情带引用标记，看得到的节点照常显示，看不到的换掉；
// 不用再因为「有看不到的节点」就整个不给失败详情。
func TestMCPRecordByRef(t *testing.T) {
	a := cleanTest(t)
	fail := auditFailure(connect.NewError(connect.CodeUnavailable, nodes.NewOfflineError("n2", "b")))
	e := audit.Event{Category: audit.MCP, Action: "mcp:migrate_container", ProjectID: "p1", Result: audit.ResultFailed, Error: fail,
		TargetType: "node", TargetID: "n2", TargetName: "b",
		Params: `{"arguments":{"container":"web","node":"n2","entry_nodes":["n1","n2"]},"perms":["project.view","project.migrate"]}`}
	v := memberViewer(a, auth.PresetOperator|auth.PermsOf(auth.PermMigrate))
	got := v.entry(e, auditNames{})
	if got.GetError() != "节点 其他节点 离线" || got.GetTargetName() != "其他节点" || got.GetTargetId() != "" {
		t.Errorf("看不到的节点：%q %q", got.GetError(), got.GetTargetName())
	}
	if strings.Contains(got.GetParams(), "n2") || !strings.Contains(got.GetParams(), `"n1"`) || !strings.Contains(got.GetParams(), `"container":"web"`) {
		t.Errorf("参数：%s", got.GetParams())
	}
	if ev := v.activityEntry(e, auditNames{}); ev.GetDetail() != "节点 其他节点 离线" {
		t.Errorf("事件详情：%q", ev.GetDetail())
	}
	// 和节点无关的失败照常给，即使他有看不到的节点。
	plain := e
	plain.Error, plain.TargetType, plain.TargetID, plain.TargetName = "compose 第 3 行有误", "container", "web", "web"
	if got := v.entry(plain, auditNames{}); got.GetError() != "compose 第 3 行有误" {
		t.Errorf("和节点无关的失败：%q", got.GetError())
	}
	if got := adminViewer().entry(e, auditNames{}); got.GetError() != "节点 b 离线" || got.GetTargetName() != "b" {
		t.Errorf("管理员：%q %q", got.GetError(), got.GetTargetName())
	}
}

// MCP 工具拿到的报错只剩文字：接口这边记下「返回的文字 → 带引用的文字」，工具的记录按返回的文字找回带引用的那一份。
func TestFailureNotes(t *testing.T) {
	a := cleanTest(t)
	x := memberAccess(auth.PresetOperator)
	raw := connect.NewError(connect.CodeUnavailable, i18n.Errorf("同步失败：%w", nodes.NewOfflineError("n2", "b")))
	want := auditFailure(raw)
	if !strings.Contains(want, "\x02node:n2\x1fb\x03") {
		t.Fatalf("测试数据：%q", want)
	}

	// 成员调用：返回的报错里节点已经换成「其他节点」，记录里要的是带引用的。
	ctx, notes := WithFailureNotes(context.Background())
	returned := a.scrubError(ctx, x, raw)
	noteFailure(ctx, returned, raw)
	for _, lang := range []i18n.Lang{i18n.Zh, i18n.En} {
		text := errorText(lang, returned)
		if strings.Contains(text, "b ") && lang == i18n.Zh {
			t.Fatalf("返回给成员的不该有节点名：%q", text)
		}
		if got := notes.Mark(errors.New(text)); got != want {
			t.Errorf("%s：%q", lang, got)
		}
		if got := notes.MarkText("deploy failed: " + text); got != "deploy failed: "+want {
			t.Errorf("%s 写在结果里的失败：%q", lang, got)
		}
	}
	// 管理员调用：返回的文字里是名称，同样换成带引用的。
	ctx, notes = WithFailureNotes(context.Background())
	noteFailure(ctx, raw, raw)
	if got := notes.Mark(errors.New(errorText(i18n.Zh, raw))); got != want {
		t.Errorf("管理员：%q", got)
	}
	// 流式操作在结果里返回的失败。
	ctx, notes = WithFailureNotes(context.Background())
	noteFailureText(ctx, "节点 b 离线", nodes.NewOfflineError("n2", "b"))
	if got := notes.MarkText("节点 b 离线"); got != "节点 \x02node:n2\x1fb\x03 离线" {
		t.Errorf("结果里的失败：%q", got)
	}
	// 没有对上的文字原样记（标记字符去掉）：调用者自己写的词不会变成节点。
	if got := notes.MarkText("container b not found \x02node:n2\x1fb\x03"); got != "container b not found node:n2b" {
		t.Errorf("对不上的文字：%q", got)
	}
	// 没有引用的报错不用记；ctx 里没有收集器时什么都不做。
	noteFailure(ctx, errors.New("plain"), errors.New("plain"))
	noteFailure(context.Background(), raw, raw)
	if got := (*FailureNotes)(nil).MarkText("x"); got != "x" {
		t.Errorf("没有收集器：%q", got)
	}
	// 不走 Connect 的入口（scrubFor）也记。
	ctx, notes = WithFailureNotes(context.Background())
	out := a.scrubFor(ctx, auth.User{ID: 2, Perms: auth.PresetOperator, LimitProjects: true, LimitNodes: true}, i18n.Zh, raw)
	if got := notes.Mark(out); got != want {
		t.Errorf("scrubFor：%q（返回的是 %q）", got, errorText(i18n.Zh, out))
	}
}

// 安装脚本只给带着钥匙的请求：钥匙对应一个还能用的一次性加入令牌。接入码不算，用过、过期的令牌不算。
func TestInstallScriptNeedsKey(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Panel.Fingerprint, h.a.Panel.Port = "sha256//abc=", 34567
	now := time.Now()
	add := func(token string, expires time.Time) {
		t.Helper()
		if err := h.a.Q.CreateEnrollToken(ctx, store.CreateEnrollTokenParams{TokenHash: auth.HashToken(token), CreatedAt: now.Unix(), ExpiresAt: expires.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(query string) int {
		w := httptest.NewRecorder()
		h.a.InstallHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/install.sh"+query, nil))
		if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), `FINGERPRINT="sha256//abc="`) {
			t.Errorf("脚本里应该有指纹")
		}
		return w.Code
	}
	key := func(token string) string { return "?k=" + installKey(auth.HashToken(token)) }
	if get(key("tok")) != http.StatusNotFound {
		t.Error("没有令牌时不提供")
	}
	add("tok", now.Add(time.Hour))
	add("old", now.Add(-time.Minute))
	if err := h.a.Q.CreateInvite(ctx, store.CreateInviteParams{TokenHash: auth.HashToken("invite"), CreatedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		"":                              http.StatusNotFound,
		"?k=":                           http.StatusNotFound,
		"?k=" + strings.Repeat("0", 32): http.StatusNotFound,
		"?k=tok":                        http.StatusNotFound,
		"?k=" + auth.HashToken("tok"):   http.StatusNotFound,
		key("old"):                      http.StatusNotFound,
		key("invite"):                   http.StatusNotFound,
		key("tok"):                      http.StatusOK,
	} {
		if got := get(query); got != want {
			t.Errorf("GET /install.sh%s = %d，想要 %d", query, got, want)
		}
	}
	// 生成的安装命令带着钥匙，取得到脚本；钥匙不是令牌。
	h.a.Panel.NewEnrollToken = func(context.Context) (string, time.Time, error) {
		add("tok2", now.Add(time.Hour))
		return "tok2", now.Add(time.Hour), nil
	}
	res, err := (&nodeService{h.a}).CreateEnrollCommand(ctx, connect.NewRequest(&nagiv1.CreateEnrollCommandRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	cmd := res.Msg.GetCommand()
	i, j := strings.Index(cmd, "/install.sh?k="), strings.Index(cmd, `" | NAGI_TOKEN=`)
	if i < 0 || j < i {
		t.Fatalf("安装命令：%s", cmd)
	}
	k := cmd[i+len("/install.sh?k=") : j]
	if k == "tok2" || k != installKey(auth.HashToken("tok2")) || get("?k="+k) != http.StatusOK {
		t.Errorf("安装命令里的钥匙：%s", cmd)
	}
	// 令牌用掉以后不再提供。
	if _, err := h.db.ExecContext(ctx, `UPDATE enroll_tokens SET used_at = 1 WHERE token_hash = ?`, auth.HashToken("tok2")); err != nil {
		t.Fatal(err)
	}
	if get("?k="+k) != http.StatusNotFound {
		t.Error("令牌用掉以后不该再提供脚本")
	}
}

// 从节点缓存收进容器：没有「特权配置」权限的成员不能收进带特权内容的配置；以节点为准重建项目整个要这项权限。
func TestAdoptNeedsPrivileged(t *testing.T) {
	a := cleanTest(t)
	member := auth.WithUser(context.Background(), auth.MemberWith(2, auth.PresetOperator|auth.PermsOf(auth.PermNodeManage)))
	privileged := auth.WithUser(context.Background(), auth.MemberWith(3, auth.PresetOperator|auth.PermsOf(auth.PermNodeManage, auth.PermPrivileged)))
	admin := auth.WithUser(context.Background(), auth.PresetUser(auth.RoleAdmin))
	if a.adoptCheck(privileged, "n1") != nil || a.adoptCheck(admin, "n1") != nil {
		t.Error("有特权配置权限时不用检查")
	}
	check := a.adoptCheck(member, "n1")
	if check == nil {
		t.Fatal("没有特权配置权限时要检查")
	}
	plain := &nagiv1.ContainerSpec{ProjectId: "p1", Name: "web", Config: &nagiv1.ContainerConfig{Compose: "services:\n  web:\n    image: nginx\n"}}
	if err := check(plain); err != nil {
		t.Errorf("普通的配置可以收进来：%v", err)
	}
	for name, compose := range map[string]string{
		"privileged": "services:\n  web:\n    image: nginx\n    privileged: true\n",
		"主机目录":       "services:\n  web:\n    image: nginx\n    volumes:\n      - /:/host\n",
		"主机网络":       "services:\n  web:\n    image: nginx\n    network_mode: host\n",
	} {
		sp := &nagiv1.ContainerSpec{ProjectId: "p1", Name: "web", Config: &nagiv1.ContainerConfig{Compose: compose}}
		if err := check(sp); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s：应该要特权配置权限，得到 %v", name, err)
		}
	}
	// 以节点为准重建项目：在动项目之前就拒绝（这里没有接 Projects，走到那一步会崩）。
	req := connect.NewRequest(&nagiv1.AdoptStaleProjectRequest{NodeId: "n1", ProjectId: "p1"})
	if _, err := (&nodeService{a}).AdoptStaleProject(member, req); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("以节点为准重建项目：%v", err)
	}
}
