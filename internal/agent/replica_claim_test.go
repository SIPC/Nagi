package agent

import (
	"testing"
	"time"
)

// L12：多源副本（设置了顶替时两方都能同步）之前不限定持有租约的一方：两个源交替 Put 和 Commit，A 的 Commit 会删掉
// B 刚写入的文件；平时不运行容器的另一方被攻破，提交一个空列表就能清空备份节点上的副本。
func TestReplicaClaim(t *testing.T) {
	r := &replicas{}
	a, b := []byte("source-a"), []byte("source-b")
	now := time.Now()
	if err := r.claim("p1", "db", a, now); err != nil {
		t.Fatal(err)
	}
	if err := r.claim("p1", "db", b, now.Add(time.Second)); err == nil {
		t.Fatal("A 正在同步时 B 不能开始")
	}
	if err := r.claim("p1", "web", b, now); err != nil {
		t.Fatalf("别的副本不受影响：%v", err)
	}
	// A 提交以后 B 可以开始。
	r.release("p1", "db", a)
	if err := r.claim("p1", "db", b, now.Add(2*time.Second)); err != nil {
		t.Fatalf("A 提交以后 B 应该能同步：%v", err)
	}
	// B 中途断了：2 分钟没有请求以后 A 可以开始。
	if err := r.claim("p1", "db", a, now.Add(time.Minute)); err == nil {
		t.Fatal("B 的同步还没过期")
	}
	if err := r.claim("p1", "db", a, now.Add(2*time.Second+replicaSyncIdle)); err != nil {
		t.Fatalf("过期以后应该能接手：%v", err)
	}
}

func TestReplicaMassDelete(t *testing.T) {
	for _, c := range []struct {
		existing, removed int
		want              bool
	}{
		{100, 100, true}, // 提交一个空列表
		{100, 60, true},
		{100, 10, false}, // 维护回收的数据包
		{30, 20, false},  // 文件少时不算
		{0, 0, false},
	} {
		if got := massDelete(c.existing, c.removed); got != c.want {
			t.Errorf("massDelete(%d, %d) = %v", c.existing, c.removed, got)
		}
	}
}
