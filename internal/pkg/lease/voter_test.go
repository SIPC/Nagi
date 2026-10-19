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
