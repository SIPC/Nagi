package api

import (
	"context"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/invite"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	"github.com/SIPC/nagi/internal/pkg/tlsutil"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// v0.35.0#12：节点提交的主机名按 RenameNode 的规则处理（最多 64 字、没有控制字符），重名时自动加后缀。
func TestEnrollName(t *testing.T) {
	names := map[string]bool{"web-1": true, "web-1-2": true}
	taken := func(s string) bool { return names[s] }
	long := strings.Repeat("长", 2_100_000)
	for _, c := range []struct{ in, want string }{
		{"db-1", "db-1"},
		{"  db-1 \n", "db-1"},
		{"web-1", "web-1-3"},
		{"we\x1b[31mb\r\n-2", "we[31mb-2"},
		{"", "abcd1234"},
		{"\x00\x07", "abcd1234"},
		{"bad\xffutf8", "badutf8"},
		{long, strings.Repeat("长", 64)},
	} {
		got := enrollName(c.in, "abcd1234", taken)
		if got != c.want {
			t.Errorf("enrollName(%.20q) = %.80q，想要 %.80q", c.in, got, c.want)
		}
	}
	// 64 个字符的名字重名时，加了后缀也不超过 64 个字符。
	full := strings.Repeat("a", 64)
	names[full] = true
	if got := enrollName(full, "id", taken); utf8.RuneCountInString(got) > maxNodeName || got != strings.Repeat("a", 62)+"-2" {
		t.Errorf("重名的长名字：%q", got)
	}
}

// v0.35.0#12：同一个接入码的加入通知合并：第一次立即发，窗口内的合成一条汇总；不同的接入码互不影响。
func TestEnrollAlertsCoalesce(t *testing.T) {
	e := &enrollAlerts{window: 50 * time.Millisecond}
	var mu sync.Mutex
	var flushed []int
	var listed []string
	flush := func(count int, names []string) {
		mu.Lock()
		defer mu.Unlock()
		flushed = append(flushed, count)
		listed = names
	}
	if !e.add("code-a", "n0", flush) {
		t.Fatal("第一次加入应该立即通知")
	}
	for i := range 25 {
		if e.add("code-a", "n"+string(rune('a'+i)), flush) {
			t.Fatal("窗口内的加入应该合并")
		}
	}
	if !e.add("code-b", "x", flush) {
		t.Fatal("别的接入码应该照常立即通知")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(flushed)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 1 || flushed[0] != 25 {
		t.Fatalf("应该只发一条汇总，合并 25 次，得到 %v", flushed)
	}
	if len(listed) != enrollAlertNames {
		t.Errorf("汇总里最多列 %d 个节点，得到 %d", enrollAlertNames, len(listed))
	}
	// 窗口结束后再加入：重新立即通知。
	if !e.add("code-a", "n9", func(int, []string) {}) {
		t.Error("窗口结束后应该重新立即通知")
	}
}

// v0.35.0#12：走一遍 Enroll：同一个接入码加入的同名、超长、带控制字符的主机名都变成合规且不重名的节点名；
// 一个接入码最多加入 inviteMaxUses 次，之后拒绝。
func TestEnrollInviteLimits(t *testing.T) {
	h := newAuditHarness(t)
	ctx := context.Background()
	ca, err := tlsutil.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	// 不挂面板：只测加入本身，不发通知。
	a := &API{Q: h.a.Q, Audit: h.a.Audit, CA: ca, Log: h.a.Log}
	svc := &enrollService{a}
	const token = "invite-token"
	if err := a.Q.CreateInvite(ctx, store.CreateInviteParams{TokenHash: auth.HashToken(token), CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	enroll := func(hostname string) (*nagiv1.EnrollResponse, error) {
		key, err := tlsutil.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := tlsutil.CreateCSR(key, "node")
		if err != nil {
			t.Fatal(err)
		}
		var priv mesh.Key
		_, _ = rand.Read(priv[:])
		pub := mesh.PublicKey(priv)
		res, err := svc.Enroll(ctx, connect.NewRequest(&nagiv1.EnrollRequest{Token: token, Hostname: hostname, Csr: csr, WireguardPublicKey: pub[:]}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	nameOf := func(id string) string {
		n, err := a.Q.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if n.Pending == 0 {
			t.Errorf("用接入码加入的节点 %s 应该待批准", id)
		}
		return n.Name
	}
	want := []string{"web-1", "web-1-2", strings.Repeat("x", 64), "evil[2Jname"}
	for i, host := range []string{"web-1", "web-1", strings.Repeat("x", 2_100_000), "evil\x1b[2J\nname"} {
		res, err := enroll(host)
		if err != nil {
			t.Fatalf("第 %d 次加入：%v", i+1, err)
		}
		if got := nameOf(res.GetNodeId()); got != want[i] {
			t.Errorf("第 %d 次加入的节点名 = %.80q，想要 %q", i+1, got, want[i])
		}
	}
	for i := len(want); i < inviteMaxUses; i++ {
		if _, err := enroll("n"); err != nil {
			t.Fatalf("第 %d 次加入：%v", i+1, err)
		}
	}
	_, err = enroll("one-more")
	if ce := new(connect.Error); !errors.As(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Errorf("超过 %d 次应该拒绝，得到 %v", inviteMaxUses, err)
	}
}

// v0.35.0#12：移交节点一次最多 inviteMaxUses 个（新面板的接入码最多加入这么多次），多了直接拒绝，不会移交到一半。
func TestHandoverBatchLimit(t *testing.T) {
	h := newAuditHarness(t)
	h.a.Panel.Fingerprint = "sha256//self="
	code := invite.Encode(invite.Invite{Address: "203.0.113.20:34567", Fingerprint: "sha256//other=", Token: "t"})
	ids := make([]string, inviteMaxUses+1)
	for i := range ids {
		ids[i] = "n" + strconv.Itoa(i)
	}
	_, err := (&nodeService{h.a}).HandoverNodes(context.Background(), connect.NewRequest(&nagiv1.HandoverNodesRequest{NodeIds: ids, Invite: code}))
	if ce := new(connect.Error); !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Errorf("一次移交 %d 个节点应该拒绝，得到 %v", len(ids), err)
	}
}
