package lease

import (
	"testing"
	"time"
)

var (
	a = []byte("node-a")
	b = []byte("node-b")
	c = []byte("node-c")
	m = [][]byte{a, b}
)

func TestVoteRejectsEpochPoisoning(t *testing.T) {
	v := NewVoter(t.TempDir())
	if err := v.Set("p", a, 5, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, epoch := range []uint64{^uint64(0) - 1, ^uint64(0)} {
		if ok, _, got := v.Vote("p", m, a, epoch, time.Minute); ok || got != 5 {
			t.Fatalf("accepted excessive epoch %d: %v %d", epoch, ok, got)
		}
	}
	if ok, _, epoch := v.Vote("p", m, a, 1029, time.Minute); !ok || epoch != 1029 {
		t.Fatalf("bounded advance rejected: %v %d", ok, epoch)
	}
	if err := v.Set("p", a, ^uint64(0), time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, _, epoch := v.Handover("p", m, a, b, 0, time.Minute); ok || epoch != ^uint64(0) {
		t.Fatalf("handover wrapped: %v %d", ok, epoch)
	}
}

func TestVoteCatchesUpWithoutPrematureGrant(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "renew", true: "takeover"}[expired], func(t *testing.T) {
			dir := t.TempDir()
			v := NewVoter(dir)
			L, candidate := time.Minute, a
			if expired {
				L, candidate = -time.Second, b
			}
			if err := v.Set("p", a, 1, L); err != nil {
				t.Fatal(err)
			}
			if ok, h, epoch := v.Vote("p", m, candidate, 2050, time.Minute); ok || string(h) != string(a) || epoch != 1025 {
				t.Fatalf("catch-up must not grant target epoch: %v %s %d", ok, h, epoch)
			}
			if h, e, gone := v.State("p", time.Minute); string(h) != string(a) || e != 1025 || gone != expired {
				t.Fatalf("catch-up changed lease: %s %d %v", h, e, gone)
			}
			if ok, _, epoch := v.Vote("p", m, candidate, 2050, time.Minute); ok || epoch != 2049 {
				t.Fatalf("second catch-up step: %v %d", ok, epoch)
			}
			if ok, h, epoch := v.Vote("p", m, candidate, 2050, time.Minute); !ok || string(h) != string(candidate) || epoch != 2050 {
				t.Fatalf("caught-up vote rejected: %v %s %d", ok, h, epoch)
			}
			v = NewVoter(dir)
			if ok, _, epoch := v.Vote("p", m, candidate, 2050, time.Minute); !ok || epoch != 2050 {
				t.Fatalf("caught-up vote lost after restart: %v %d", ok, epoch)
			}
		})
	}
	// 追赶提示和对方的大跨度请求都不能打断仍有效的租约。
	v := NewVoter(t.TempDir())
	if err := v.Set("p", a, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	v.CatchUp("p", 5000, time.Minute)
	if ok, _, e := v.Vote("p", m, b, 5000, time.Minute); ok || e != 1 {
		t.Fatalf("active lease advanced by other candidate: %v %d", ok, e)
	}
	if err := v.Set("p", a, 1, -time.Second); err != nil {
		t.Fatal(err)
	}
	v.CatchUp("p", 5000, time.Minute)
	if h, e, gone := v.State("p", time.Minute); string(h) != string(a) || e != 1025 || !gone {
		t.Fatalf("hint granted or renewed lease: %s %d %v", h, e, gone)
	}
}

func TestVote(t *testing.T) {
	dir := t.TempDir()
	v := NewVoter(dir)
	const L = 200 * time.Millisecond
	if err := v.Set("p", a, 1, L); err != nil {
		t.Fatal(err)
	}
	// 持有者续约
	if ok, _, _ := v.Vote("p", m, a, 1, L); !ok {
		t.Fatal("A 续约应该成功")
	}
	// 票还有效时，B 要不到下一轮
	if ok, h, e := v.Vote("p", m, b, 2, L); ok || string(h) != "node-a" || e != 1 {
		t.Fatalf("A 的票还有效，不应该投给 B：%v %s %d", ok, h, e)
	}
	// 不在设置里的一方要不到票
	if ok, _, _ := v.Vote("p", m, c, 9, L); ok {
		t.Fatal("C 不是参与者")
	}
	time.Sleep(L + 50*time.Millisecond)
	// 票作废后，B 拿到第 2 轮
	if ok, _, e := v.Vote("p", m, b, 2, L); !ok || e != 2 {
		t.Fatalf("A 的票作废后应该投给 B：%v %d", ok, e)
	}
	// A 再来续第 1 轮：被拒绝，并告诉它现在是 B、第 2 轮
	if ok, h, e := v.Vote("p", m, a, 1, L); ok || string(h) != "node-b" || e != 2 {
		t.Fatalf("旧持有者应该被拒绝：%v %s %d", ok, h, e)
	}

	// 投票者重启：一个租约时长内只给原持有者续约。
	v2 := NewVoter(dir)
	if ok, _, _ := v2.Vote("p", m, a, 3, L); ok {
		t.Fatal("重启后不能立刻投给别人")
	}
	if ok, _, _ := v2.Vote("p", m, b, 2, L); !ok {
		t.Fatal("重启后应该照常给原持有者续约")
	}
	if h, e, expired := v2.State("p", L); string(h) != "node-b" || e != 2 || expired {
		t.Fatalf("状态不对：%s %d %v", h, e, expired)
	}
}

func TestHandover(t *testing.T) {
	v := NewVoter(t.TempDir())
	a, b, c := []byte("a"), []byte("b"), []byte("c")
	parties := [][]byte{a, b}
	if err := v.Set("p", b, 2, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := v.Handover("p", parties, a, a, 3, time.Minute); ok {
		t.Fatal("不是持有者不能转让")
	}
	if ok, _, _ := v.Handover("p", parties, b, c, 3, time.Minute); ok {
		t.Fatal("不能转给顶替设置以外的一方")
	}
	if ok, _, _ := v.Handover("p", parties, b, a, 4, time.Minute); ok {
		t.Fatal("轮次必须正好加一")
	}
	if ok, h, e := v.Handover("p", parties, b, a, 3, time.Minute); !ok || string(h) != "a" || e != 3 {
		t.Fatalf("持有者转让应该成功：%v %s %d", ok, h, e)
	}
	if ok, _, _ := v.Handover("p", parties, b, a, 3, time.Minute); !ok {
		t.Fatal("重试同一个转让应该返回成功")
	}
	if ok, _, _ := v.Vote("p", parties, b, 3, time.Minute); ok {
		t.Fatal("转让以后原持有者不能再续约")
	}
}

// 票现在投给的一方可以把自己的轮次调高；别人不行（票还有效时）。
func TestVoteRaiseOwnEpoch(t *testing.T) {
	v := NewVoter(t.TempDir())
	const L = time.Minute
	if err := v.Set("p", a, 1, L); err != nil {
		t.Fatal(err)
	}
	if ok, h, e := v.Vote("p", m, a, 5, L); !ok || string(h) != "node-a" || e != 5 {
		t.Fatalf("A 应能把自己的轮次调到 5：%v %s %d", ok, h, e)
	}
	if ok, _, e := v.Vote("p", m, b, 6, L); ok || e != 5 {
		t.Fatalf("A 的票还有效，B 不应拿到：%v %d", ok, e)
	}
	if ok, _, _ := v.Vote("p", m, a, 5, L); !ok {
		t.Fatal("A 按新的轮次续约应该成功")
	}
}
