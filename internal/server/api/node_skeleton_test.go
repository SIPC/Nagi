package api

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/i18n"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/audit"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/nodes"
	"github.com/SIPC/nagi/internal/server/store"
)

// 连续的空格、各种空格混用、截断留下的末尾空格都不能让名字绕过重名检查：网页上连续的空格显示成一个。
func TestEnrollNameCollapsesSpaces(t *testing.T) {
	long := strings.Repeat("a", 63)
	names := nodeNames{}
	for _, n := range []string{"web 1", long} {
		names.add(n)
	}
	for _, c := range []struct{ in, want string }{
		{"web  1", "web 1-2"},
		{"web\u00a0 1", "web 1-2"},
		{"web\u3000\u30001", "web 1-2"},
		{" web \u2003 1 ", "web 1-2"},
		{"web \u200b 1", "web 1-2"}, // 两个空格中间夹着不可见字符
		{long + " b", strings.Repeat("a", 62) + "-2"},
		{"db   2", "db 2"},
	} {
		if got := enrollName(c.in, "id", names.taken); got != c.want {
			t.Errorf("enrollName(%+q) = %+q，想要 %+q", c.in, got, c.want)
		}
	}
	// 截断以后末尾不留空格。
	if got := enrollName(strings.Repeat("b", 63)+" c", "id", func(string) bool { return false }); strings.HasSuffix(got, " ") {
		t.Errorf("截断后末尾有空格：%q", got)
	}
}

// 没分配的码位和非字符和不可见字符一样：加入时去掉，改名时拒绝。
func TestNodeNameUnassignedRunes(t *testing.T) {
	names := nodeNames{}
	names.add("web-1")
	inputs := []string{"web-1\u0378", "web-1\ufdd0", "web-1\ufffe", "web-1\U0001fffe", "web-1\U0010ffff", "web-1\U000e0080"}
	for _, in := range inputs {
		if got := enrollName(in, "id", names.taken); got != "web-1-2" {
			t.Errorf("enrollName(%+q) = %+q，想要 \"web-1-2\"", in, got)
		}
	}
	svc := &nodeService{&API{}}
	for _, name := range inputs {
		_, err := svc.RenameNode(context.Background(), connect.NewRequest(&nagiv1.RenameNodeRequest{NodeId: "n", Name: name}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("RenameNode(%+q) 应拒绝：%v", name, err)
		}
	}
	// 已分配的字符（中文、表情、带附加符号的字母）照常可以用。
	for _, ok := range []rune{'长', '\u00e9', '\U0001f600', '\u0436', '\u0301'} {
		if invisibleRune(ok) {
			t.Errorf("%+q 不应算不可见字符", ok)
		}
	}
}

// 形近字按骨架比较：西里尔、希腊字母里和拉丁字母长得一样的，全角、数学字母，组合方式不同的带附加符号的字母，
// 和已有的名称看起来一样就算重名；加后缀时保留原来的写法。
func TestEnrollNameConfusables(t *testing.T) {
	names := nodeNames{}
	for _, n := range []string{"web-1", "Admin", "caf\u00e9", "PROD", "db.local"} {
		names.add(n)
	}
	for _, c := range []struct{ in, want string }{
		{"w\u0435b-1", "w\u0435b-1-2"},                               // 西里尔字母 е
		{"\u0410dmin", "\u0410dmin-2"},                               // 西里尔字母 А
		{"\u0391dmin", "\u0391dmin-2"},                               // 希腊字母 Α
		{"Adm\u0456n", "Adm\u0456n-2"},                               // 乌克兰字母 і
		{"\uff57\uff45\uff42-\uff11", "\uff57\uff45\uff42-\uff11-2"}, // 全角
		{"\U0001d5d0\U0001d5be\U0001d5bb-\U0001d7e3", "\U0001d5d0\U0001d5be\U0001d5bb-\U0001d7e3-2"}, // 数学无衬线字母
		{"web\u20101", "web\u20101-2"},                           // 连字符 U+2010
		{"web\u20111", "web\u20111-2"},                           // 不断行连字符
		{"web\u22121", "web\u22121-2"},                           // 减号
		{"cafe\u0301", "cafe\u0301-2"},                           // 分解的 é
		{"caf\u0435\u0301", "caf\u0435\u0301-2"},                 // 西里尔 е 加组合符号
		{"\u03a1\u03a1\u039f\u0414", "\u03a1\u03a1\u039f\u0414"}, // ΡΡΟД：Д 没有形近的拉丁字母，不算重名
		{"\u0420R\u041eD", "\u0420R\u041eD-2"},                   // РRОD
		{"db\u2024local", "db\u2024local-2"},                     // 单点前导符
		// 不算重名的。
		{"web-2", "web-2"},
		{"admin", "admin"},
		{"cafe", "cafe"},
		{"\u0441\u0435\u0440\u0432\u0435\u0440", "\u0441\u0435\u0440\u0432\u0435\u0440"},
	} {
		if got := enrollName(c.in, "id", names.taken); got != c.want {
			t.Errorf("enrollName(%+q) = %+q，想要 %+q", c.in, got, c.want)
		}
	}
	// 后缀本身也按骨架比较：已有 web-1-2 时，形近的 web-1 拿到 -3。
	names.add("web-1-2")
	if got := enrollName("w\u0435b-1", "id", names.taken); got != "w\u0435b-1-3" {
		t.Errorf("后缀也要按骨架比较：%+q", got)
	}
}

// 改名：连续的空格合成一个；和看得到的节点形近也算重名，报错里的节点用引用；看不到的节点不比。
func TestRenameNodeSkeleton(t *testing.T) {
	a, q := scopeTestAPI(t, `INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('n3', 'web 1', 0, x'03')`)
	a.Nodes = nodes.NewRegistry(q, slog.New(slog.DiscardHandler), nil, nil)
	a.Log = slog.New(slog.DiscardHandler)
	svc := &nodeService{a}
	ctx := context.Background()
	rename := func(ctx context.Context, id, name string) error {
		_, err := svc.RenameNode(ctx, connect.NewRequest(&nagiv1.RenameNodeRequest{NodeId: id, Name: name}))
		return err
	}
	nameOf := func(id string) string {
		n, err := q.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Name
	}
	for _, name := range []string{"web  1", "web\u00a0\u00a01", "w\u0435b 1", "\uff57\uff45\uff42 \uff11", "\u0430"} {
		target := "n1"
		if name == "\u0430" {
			target = "n2" // 和 n1 的 a 形近
		}
		err := rename(ctx, target, name)
		if connect.CodeOf(err) != connect.CodeAlreadyExists {
			t.Errorf("RenameNode(%+q) 应报重名：%v", name, err)
		}
		if name == "\u0430" {
			ce := new(connect.Error)
			hide := func(r i18n.Ref) (string, bool) { return "其他节点", r.Kind == "node" && r.ID == "n1" }
			if !errors.As(err, &ce) {
				t.Fatal(err)
			}
			if _, ok := i18n.Redact(ce.Unwrap(), hide); !ok {
				t.Errorf("形近的报错里的节点要用引用：%v", err)
			}
		}
	}
	if err := rename(ctx, "n1", "  db   main "); err != nil {
		t.Fatal(err)
	}
	if got := nameOf("n1"); got != "db main" {
		t.Errorf("连续的空格应合成一个：%q", got)
	}
	// 改成和自己形近的名称照常可以。
	if err := rename(ctx, "n1", "db m\u0430in"); err != nil {
		t.Errorf("和自己形近不算重名：%v", err)
	}
	// 成员只和看得到的节点比：n2（b）、n3 在他的范围外。
	member := auth.WithUser(ctx, auth.User{ID: 2, Role: auth.RoleOperator, Perms: auth.PresetOperator, LimitProjects: true, LimitNodes: true})
	if err := rename(member, "n1", "w\u0435b 1"); err != nil {
		t.Errorf("范围外的节点不比：%v", err)
	}
}

// 升级前存下的名称在启动时整理一次：去掉不可见字符、合并空格、截短；整理后重名的加后缀，已经合规的优先保留；
// 改了的记审计；再启动一次不再改。
func TestNormalizeNodeNames(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	h.a.Nodes = nodes.NewRegistry(h.a.Q, slog.New(slog.DiscardHandler), nil, nil)
	rows := []struct{ id, name, want string }{
		{"n1", "web\u200b-1", "web-1-2"}, // 比 n2 早加入，但 n2 的名称本来就合规，保留 n2 的
		{"n2", "web-1", "web-1"},
		{"n3", "db  main", "db main"},
		{"n4", strings.Repeat("x", 70), strings.Repeat("x", 64)},
		{"n5", "\u202e\u200b", "n5"},
		{"n6", "w\u0435b-1", "w\u0435b-1"}, // 没有不可见字符的旧名称不动，即使和别的形近
		{"n7", "ok\ufdd0", "ok"},
	}
	for i, r := range rows {
		if _, err := h.db.ExecContext(ctx, `INSERT INTO nodes (id, name, created_at, mesh_key) VALUES (?, ?, ?, ?)`, r.id, r.name, i, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	countRenames := func() int {
		list, err := h.a.Audit.List(ctx, audit.Filter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range list {
			if e.Action == "RenameNode" && e.ActorType == audit.ActorSystem {
				n++
			}
		}
		return n
	}
	h.a.NormalizeNodeNames(ctx)
	for _, r := range rows {
		n, err := h.a.Q.GetNode(ctx, r.id)
		if err != nil {
			t.Fatal(err)
		}
		if n.Name != r.want {
			t.Errorf("%s：%+q 整理成 %+q，想要 %+q", r.id, r.name, n.Name, r.want)
		}
	}
	if n := countRenames(); n != 5 {
		t.Errorf("改了 5 个节点，审计记了 %d 条", n)
	}
	h.a.NormalizeNodeNames(ctx)
	if n := countRenames(); n != 5 {
		t.Errorf("再启动一次不应再改：审计共 %d 条", n)
	}
}

// 移除过的节点重新加入时，原来的名称按加入的规则再处理一次：移除期间别的节点用了同样（或形近）的名称时加后缀。
func TestReenrollRevokedNodeName(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	ca, err := tlsutil.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Q: h.a.Q, Audit: h.a.Audit, CA: ca, Log: h.a.Log}
	svc := &enrollService{a}
	key, err := tlsutil.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(token string) string {
		if err := a.Q.CreateEnrollToken(ctx, store.CreateEnrollTokenParams{TokenHash: auth.HashToken(token), CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
		csr, err := tlsutil.CreateCSR(key, "node")
		if err != nil {
			t.Fatal(err)
		}
		var priv mesh.Key
		_, _ = rand.Read(priv[:])
		pub := mesh.PublicKey(priv)
		res, err := svc.Enroll(ctx, connect.NewRequest(&nagiv1.EnrollRequest{Token: token, Hostname: "db", Csr: csr, WireguardPublicKey: pub[:]}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetNodeId()
	}
	id := enroll("t1")
	if err := a.Q.RevokeNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	// 移除期间加入了一台形近的 db（西里尔字母 ԁ）。
	if _, err := h.db.ExecContext(ctx, `INSERT INTO nodes (id, name, created_at, mesh_key) VALUES ('other', ?, 1, x'09')`, "\u0501b"); err != nil {
		t.Fatal(err)
	}
	if got := enroll("t2"); got != id {
		t.Fatalf("节点 ID 变了：%s", got)
	}
	n, err := a.Q.GetNode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "db-2" {
		t.Errorf("重新加入的节点名 = %q，想要 db-2", n.Name)
	}
}
