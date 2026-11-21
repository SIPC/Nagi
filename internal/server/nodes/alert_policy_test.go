package nodes

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/alert"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/sealer"
	"github.com/SIPC/nagi/internal/server/store"
	"google.golang.org/protobuf/proto"
)

func newTestRegistry(t *testing.T) (*Registry, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	key, err := sealer.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	return NewRegistry(q, slog.Default(), key, nil), q
}

// 旧版本的节点、项目、容器规则升级后换成覆盖，每个容器的生效规则和升级前一样；之后容器没覆盖的字段跟随项目。
func TestMigrateAlertPolicy(t *testing.T) {
	ctx := context.Background()
	r, q := newTestRegistry(t)
	if err := q.CreateNode(ctx, store.CreateNodeParams{ID: "n1", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	b, _ := proto.Marshal(&nagiv1.AlertRules{DiskPercent: 80, Enabled: map[string]bool{"cpu": false}})
	if err := q.SetNodeAlertRules(ctx, store.SetNodeAlertRulesParams{ID: "n1", AlertRules: b}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertProject(ctx, store.InsertProjectParams{ID: "p", Name: "p", NetworkName: "p", Settings: []byte{}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"web", "same", "plain"} {
		if err := q.InsertContainer(ctx, store.InsertContainerParams{ProjectID: "p", Name: c, NodeID: "n1", Secrets: []byte{}, WebhookHash: "h-" + c}); err != nil {
			t.Fatal(err)
		}
	}
	project := &nagiv1.AlertRules{RestartCount: 5, Enabled: map[string]bool{"errors": false}}
	legacy := func(context.Context) (map[string]*nagiv1.AlertRules, []LegacyContainerRules, error) {
		return map[string]*nagiv1.AlertRules{"p": project}, []LegacyContainerRules{
			// 以前单独设置整组替换项目的：没写 errors 的开关就是开着，5xx 阈值 10。
			{ProjectID: "p", Container: "web", Project: project, Own: &nagiv1.AlertRules{RestartCount: 5, ErrorPercent: 10}},
			// 和项目完全一样：不需要覆盖。
			{ProjectID: "p", Container: "same", Project: project, Own: proto.CloneOf(project)},
		}, nil
	}
	if err := r.MigrateAlertPolicy(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	node, err := r.NodeAlertEffective(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if d := node.GetRules()[alert.RuleDisk]; d.GetThreshold() != 80 || d.GetRecoverThreshold() != 75 {
		t.Errorf("节点磁盘应为 80，恢复阈值按默认回差跟着降到 75：%v", d)
	}
	if alert.On(node.GetRules()[alert.RuleCPU]) || !alert.On(node.GetRules()[alert.RuleMem]) {
		t.Errorf("节点只关了 CPU：%v", node.GetRules())
	}

	eff := func(c string) map[string]*nagiv1.AlertRule { return r.ContainerAlertRules(ctx, "p", c).GetRules() }
	web, same, plain := eff("web"), eff("same"), eff("plain")
	if !alert.On(web[alert.RuleErrors]) || web[alert.RuleErrors].GetThreshold() != 10 || web[alert.RuleRestart].GetThreshold() != 5 {
		t.Errorf("web 的生效规则要和升级前一样（5xx 开着、阈值 10，重启 5）：%v %v", web[alert.RuleErrors], web[alert.RuleRestart])
	}
	for name, rules := range map[string]map[string]*nagiv1.AlertRule{"same": same, "plain": plain} {
		if alert.On(rules[alert.RuleErrors]) || rules[alert.RuleRestart].GetThreshold() != 5 {
			t.Errorf("%s 跟随项目：5xx 关、重启 5：%v %v", name, rules[alert.RuleErrors], rules[alert.RuleRestart])
		}
	}
	list, err := r.AlertOverrides(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]bool{}
	for _, o := range list {
		scopes[o.GetScope()+":"+o.GetContainer()] = true
	}
	if !scopes["node:"] || !scopes["project:"] || !scopes["container:web"] || scopes["container:same"] || len(list) != 3 {
		t.Errorf("覆盖应为节点、项目和 web 三条：%v", list)
	}
	// web 的覆盖里只有和项目不一样的字段：项目以后改重启次数，web 跟着变。
	cur, _ := q.GetAlertOverride(ctx, store.AlertOverride{Scope: "project", ProjectID: "p"})
	if _, err := r.SetAlertOverride(ctx, &nagiv1.AlertOverride{Scope: "project", ProjectId: "p", Rules: map[string]*nagiv1.AlertRule{
		alert.RuleRestart: {Threshold: proto.Float64(7)}, alert.RuleErrors: {Enabled: proto.Bool(false)}}}, cur.Revision); err != nil {
		t.Fatal(err)
	}
	if got := eff("web")[alert.RuleRestart].GetThreshold(); got != 7 {
		t.Errorf("web 没覆盖重启次数，应该跟随项目改成 7，得到 %v", got)
	}
	// 旧修订号提交被拒绝。
	if _, err := r.SetAlertOverride(ctx, &nagiv1.AlertOverride{Scope: "project", ProjectId: "p"}, cur.Revision); err != ErrAlertConflict {
		t.Errorf("过期的修订号应该冲突：%v", err)
	}
	// 只迁移一次：再改旧规则也不会覆盖新的。
	project.RestartCount = 9
	if err := r.MigrateAlertPolicy(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if got := eff("plain")[alert.RuleRestart].GetThreshold(); got != 7 {
		t.Errorf("第二次启动不应再迁移：%v", got)
	}
}

// 覆盖的检查：恢复阈值不能比触发阈值更糟（和下面各层合并以后），规则要放在对的层上。
func TestSetAlertOverrideValidation(t *testing.T) {
	ctx := context.Background()
	r, q := newTestRegistry(t)
	if err := q.CreateNode(ctx, store.CreateNodeParams{ID: "n1", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		rules map[string]*nagiv1.AlertRule
	}{
		{"触发阈值降到默认恢复阈值以下", map[string]*nagiv1.AlertRule{alert.RuleMem: {Threshold: proto.Float64(80)}}},
		{"容器规则放在节点上", map[string]*nagiv1.AlertRule{alert.RuleRestart: {Threshold: proto.Float64(2)}}},
		{"百分比超过 100", map[string]*nagiv1.AlertRule{alert.RuleDisk: {Threshold: proto.Float64(120)}}},
	} {
		if _, err := r.SetAlertOverride(ctx, &nagiv1.AlertOverride{Scope: "node", NodeId: "n1", Rules: c.rules}, 0); err == nil {
			t.Errorf("%s 应该被拒绝", c.name)
		}
	}
	if _, err := r.SetAlertOverride(ctx, &nagiv1.AlertOverride{Scope: "node", NodeId: "missing",
		Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {Threshold: proto.Float64(95)}}}, 0); err == nil {
		t.Error("不存在的节点应该被拒绝")
	}
	if _, err := r.SetAlertOverride(ctx, &nagiv1.AlertOverride{Scope: "node", NodeId: "n1",
		Rules: map[string]*nagiv1.AlertRule{alert.RuleMem: {Threshold: proto.Float64(80), RecoverThreshold: proto.Float64(70)}}}, 0); err != nil {
		t.Errorf("同时改恢复阈值就可以：%v", err)
	}
}

// 映射层的覆盖：只能放按映射判断的规则，只影响这条映射，盖在容器（和项目）之上；删掉映射时一起删掉。
func TestRouteAlertOverride(t *testing.T) {
	ctx := context.Background()
	r, q := newTestRegistry(t)
	if err := q.CreateNode(ctx, store.CreateNodeParams{ID: "n1", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertProject(ctx, store.InsertProjectParams{ID: "p", Name: "p", NetworkName: "p", Settings: []byte{}}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertContainer(ctx, store.InsertContainerParams{ProjectID: "p", Name: "web", NodeID: "n1", Secrets: []byte{}, WebhookHash: "h"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tcp", "other"} {
		if err := q.UpsertRoute(ctx, store.UpsertRouteParams{ProjectID: "p", Name: name, Container: "web", Target: 80, Protocol: "tcp", Domains: "[]", EntryNodes: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(scope, route string, rules map[string]*nagiv1.AlertRule) error {
		_, err := r.SetAlertOverride(ctx, &nagiv1.AlertOverride{Scope: scope, ProjectId: "p", Container: "web", Route: route, Rules: rules}, 0)
		return err
	}
	if err := set("route", "tcp", map[string]*nagiv1.AlertRule{alert.RuleRestart: {Threshold: proto.Float64(2)}}); err == nil {
		t.Error("容器规则不能放在映射上")
	}
	if err := set("route", "missing", map[string]*nagiv1.AlertRule{alert.RuleL4Fail: {Threshold: proto.Float64(30)}}); err == nil {
		t.Error("不存在的映射应该被拒绝")
	}
	// 容器把失败比例改成 40%（恢复 10%），映射 tcp 再改成 60%：other 跟随容器，tcp 用自己的阈值、容器的恢复阈值。
	if err := set("container", "", map[string]*nagiv1.AlertRule{alert.RuleL4Fail: {Threshold: proto.Float64(40), RecoverThreshold: proto.Float64(10)}}); err != nil {
		t.Fatal(err)
	}
	if err := set("route", "tcp", map[string]*nagiv1.AlertRule{alert.RuleL4Fail: {Threshold: proto.Float64(60)}}); err != nil {
		t.Fatal(err)
	}
	got := r.RouteAlertRules(ctx, "p", "web")
	if len(got) != 1 {
		t.Fatalf("只有有覆盖的映射单独下发：%v", got)
	}
	if f := got["tcp"].GetRules()[alert.RuleL4Fail]; f.GetThreshold() != 60 || f.GetRecoverThreshold() != 10 {
		t.Fatalf("映射应盖在容器之上：%v", f)
	}
	if err := q.DeleteRoute(ctx, store.DeleteRouteParams{ProjectID: "p", Name: "tcp"}); err != nil {
		t.Fatal(err)
	}
	if got := r.RouteAlertRules(ctx, "p", "web"); len(got) != 0 {
		t.Fatalf("删掉映射后覆盖应一起删掉：%v", got)
	}
}

// 事件通知的开关：版本不对时拒绝；关掉时 before 在保存之前调用；旧的三个登录安全开关升级时只迁移一次。
func TestAlertNotificationSwitches(t *testing.T) {
	ctx := context.Background()
	r, q := newTestRegistry(t)
	if err := q.SetSetting(ctx, store.SetSettingParams{Key: "alert_login_ratelimit", Value: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.MigrateAlertPolicy(ctx, func(context.Context) (map[string]*nagiv1.AlertRules, []LegacyContainerRules, error) { return nil, nil, nil }); err != nil {
		t.Fatal(err)
	}
	p, _ := r.AlertPolicy(ctx)
	if v, ok := p.GetNotifications()["login_ratelimit"]; !ok || v {
		t.Fatalf("关掉的登录限速应迁移成关掉的通知：%v", p.GetNotifications())
	}
	if !r.NoticeOn(ctx, "totp:alice") || r.NoticeOn(ctx, "ratelimit:hour") {
		t.Fatal("只有登录限速被关掉")
	}
	if _, err := r.SetAlertNotification(ctx, "takeover", false, p.GetRevision()-1, nil); err != ErrAlertConflict {
		t.Fatalf("旧版本应被拒绝：%v", err)
	}
	var savedBefore bool
	p, err := r.SetAlertNotification(ctx, "takeover", false, p.GetRevision(), func() {
		cur, _ := r.AlertPolicy(ctx)
		_, savedBefore = cur.GetNotifications()["takeover"]
	})
	if err != nil || savedBefore {
		t.Fatalf("before 应在保存之前调用：%v %v", savedBefore, err)
	}
	if eff := alert.Effective(p, true); eff.GetNotifications()["takeover"] || len(eff.GetNotifications()) != 1 {
		t.Fatalf("节点只收到关掉的节点通知（顶替），面板的登录限速不下发：%v", eff.GetNotifications())
	}
	// 用户之后重新打开登录限速：再次启动不应再按旧设置关掉。
	if p, err = r.SetAlertNotification(ctx, "login_ratelimit", true, p.GetRevision(), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.MigrateAlertPolicy(ctx, func(context.Context) (map[string]*nagiv1.AlertRules, []LegacyContainerRules, error) { return nil, nil, nil }); err != nil {
		t.Fatal(err)
	}
	if !r.NoticeOn(ctx, "ratelimit:hour") {
		t.Fatal("登录限速的迁移只做一次")
	}
}
