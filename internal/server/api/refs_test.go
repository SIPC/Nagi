package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/spec"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
)

// 报错里提到节点时要用引用（i18n.NodeRef、nodeRef、NodeRef）：直接拼名称的不会按查看的人换，
// 成员会看到范围外节点的名称。这里扫面板的代码，「节点 %s」开头的格式串对应的参数不是引用就失败。
// 确实不返回给成员的（告警标题、只有管理员能调的接口、节点加入）登记在 plainNodeNames 里。
func TestNodeNamesInErrorsAreRefs(t *testing.T) {
	plainNodeNames := []string{
		"已从 %s 加入",          // 告警标题
		"用接入码加入，等待批准",       // 告警标题
		"已被隔离",              // 告警标题
		"已解除隔离",             // 告警标题
		"已批准加入",             // 告警标题
		"已移交给其他面板",          // 告警标题
		"已删除\", a.nodeName", // 告警标题
		"移除了一个面板",           // 告警标题
		"的网络设置",             // 告警标题
		"中转池",               // 告警标题
		"越权",                // 告警标题
		"疑似被攻破",             // 告警标题
		"该 WireGuard 公钥已属于", // 节点加入时的报错，不是成员调的
		"还需要一起移交",           // 移交节点，只有管理员
	}
	call := regexp.MustCompile(`(Errorf|invalid|Invalid)\("[^"]*节点 %s[^"]*"(.*)`)
	root := filepath.Join("..", "..", "server")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
	lines:
		for i, line := range strings.Split(string(b), "\n") {
			m := call.FindStringSubmatch(line)
			if m == nil || strings.Contains(m[2], "Ref(") {
				continue
			}
			for _, ok := range plainNodeNames {
				if strings.Contains(line, ok) {
					continue lines
				}
			}
			t.Errorf("%s:%d 报错里的节点要用引用（NodeRef）：%s", path, i+1, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 审计的失败详情存的是带引用标记的文字，显示时按查看的人换：管理员看到名称，看不到这台节点的成员看到
// 「其他节点」。成员写进报错的文字（包括仿造的标记）不会被换。
func TestAuditFailureByRef(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	x := memberAccess(auth.PresetOperator)
	member := auditViewer{x: x, other: "其他节点", scrub: func(s string) string { return a.scrubNodeIDs(ctx, x, s) }}
	member.refs = func(s string) string {
		return i18n.RenderRefs(s, func(r i18n.Ref) string {
			if r.Kind == "node" && !x.node(r.ID) {
				return member.other
			}
			return r.Name
		})
	}
	admin := auditViewer{admin: true, scrub: func(s string) string { return s }, refs: i18n.PlainRefs}

	off := connect.NewError(connect.CodeUnavailable, i18n.Errorf("同步失败：%w", nodes.NewOfflineError("n2", "b")))
	stored := auditFailure(off)
	if strings.Contains(i18n.PlainRefs(stored), "\x02") || i18n.PlainRefs(stored) != "同步失败：节点 b 离线" {
		t.Errorf("管理员看到的：%q", i18n.PlainRefs(stored))
	}
	e := audit.Event{Category: audit.Operation, Action: "Deploy", ProjectID: "p1", Error: stored, TargetType: "container", TargetName: "b"}
	if got := member.entry(e, auditNames{}); got.Error != "同步失败：节点 其他节点 离线" || got.TargetName != "b" {
		t.Errorf("成员看到的：%q 对象 %q", got.Error, got.TargetName)
	}
	if got := admin.entry(e, auditNames{}); got.Error != "同步失败：节点 b 离线" {
		t.Errorf("管理员看到的：%q", got.Error)
	}
	// 项目事件：对象名称是成员自己写的，和节点同名也不换；对象是看不到的节点时按类型换。
	if got := member.activityEntry(e, auditNames{}); got.Target != "b" || got.Detail != "同步失败：节点 其他节点 离线" {
		t.Errorf("事件：对象 %q 详情 %q", got.Target, got.Detail)
	}
	node := audit.Event{ProjectID: "p1", ActorType: "node", ActorID: "n2", ActorName: "b", TargetType: "node", TargetID: "n2", TargetName: "b"}
	if got := member.activityEntry(node, auditNames{}); got.Actor != "其他节点" || got.Target != "其他节点" {
		t.Errorf("事件里的节点：%q %q", got.Actor, got.Target)
	}
	if got := admin.activityEntry(node, auditNames{}); got.Actor != "b" || got.Target != "b" {
		t.Errorf("管理员看事件里的节点：%q %q", got.Actor, got.Target)
	}
	// 看得到的节点照常显示名称。
	seen := auditFailure(connect.NewError(connect.CodeUnavailable, nodes.NewOfflineError("n1", "a")))
	if got := member.refs(seen); got != "节点 a 离线" {
		t.Errorf("看得到的节点：%q", got)
	}
	// 成员写进报错的文字：猜的名称、仿造的标记都原样记下（标记字符去掉），显示时不换。
	for _, typed := range []string{"b", "x\nb", "\x02node:n2\x1fb\x03"} {
		stored := auditFailure(connect.NewError(connect.CodeInvalidArgument, spec.Invalid("权限 %s 不存在", typed)))
		want := "权限 " + strings.NewReplacer("\x02", "", "\x1f", "", "\x03", "").Replace(typed) + " 不存在"
		if got := member.refs(stored); got != want {
			t.Errorf("成员写的 %q：%q", typed, got)
		}
	}
	// 截断的标记连同后面的内容去掉，不露出名称。
	cut := stored[:strings.Index(stored, "\x1f")+1]
	if got := member.refs(cut + "b"); got != "同步失败：节点 " {
		t.Errorf("截断的标记：%q", got)
	}
}

// 换过名称以后，错误的类型、错误码、errors.Join 连起来的错误都保留。
func TestRedactKeepsShape(t *testing.T) {
	a := cleanTest(t)
	ctx := context.Background()
	x := memberAccess(auth.PresetOperator)
	ref := i18n.NodeRef("n2", "b")
	got := a.scrubError(ctx, x, connect.NewError(connect.CodeInvalidArgument, spec.Invalid("节点 %s 没有启动 Caddy，无法作为入口节点", ref)))
	if !errors.Is(got, spec.ErrInvalid) || connect.CodeOf(got) != connect.CodeInvalidArgument || errorText(i18n.Zh, got) != "节点 其他节点 没有启动 Caddy，无法作为入口节点" {
		t.Errorf("参数错误：%v", got)
	}
	got = a.scrubError(ctx, x, &sourceError{503, i18n.Errorf("节点 %s 还没有加入节点网络", ref)})
	if sourceCode(got) != 503 || errorText(i18n.Zh, got) != "节点 其他节点 还没有加入节点网络" {
		t.Errorf("sourceError：%v", got)
	}
	got = a.scrubError(ctx, x, errors.Join(nodes.NewOfflineError("n2", "b"), errors.New("y")))
	if !errors.Is(got, nodes.ErrOffline) || strings.Contains(got.Error(), "节点 b") {
		t.Errorf("errors.Join：%v", got)
	}
}
