package agent

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// v0.35.0#19：同一个副本的 ReplicaPut 流共用一个磁盘检查，同时最多 replicaStreamsMax 条；别的副本不受影响，
// 流都结束以后不留下记录。
func TestReplicaOpenStream(t *testing.T) {
	r := &replicas{}
	const files = "/replicas/x/files"
	var dones []func()
	first, done, err := r.openStream(lockTestPID, "web", files)
	if err != nil {
		t.Fatal(err)
	}
	dones = append(dones, done)
	for range replicaStreamsMax - 1 {
		g, done, err := r.openStream(lockTestPID, "web", files)
		if err != nil {
			t.Fatal(err)
		}
		if g != first {
			t.Fatal("同一个副本的流应该共用一个磁盘检查")
		}
		dones = append(dones, done)
	}
	// 拒绝时回的错误要让源节点当成连接断开、稍后接着传，而不是放弃这次同步：
	// 源节点断开重连时旧流可能还没收尾，名额是暂时满的。
	if _, _, err := r.openStream(lockTestPID, "web", files); status.Code(err) != codes.Unavailable || !connectionLost(err) {
		t.Fatalf("超过 %d 条应该拒绝，并且源节点会重试：%v", replicaStreamsMax, err)
	}
	other, otherDone, err := r.openStream(lockTestPID, "db", "/replicas/y/files")
	if err != nil || other == first {
		t.Fatalf("别的副本不受影响、各用各的检查：%v", err)
	}
	otherDone()

	// 结束一条以后又能开；done 调用两次不多减。
	dones[0]()
	dones[0]()
	g, done, err := r.openStream(lockTestPID, "web", files)
	if err != nil || g != first {
		t.Fatalf("结束一条以后应该能再开，并且共用原来的检查：%v", err)
	}
	dones[0] = done
	for _, d := range dones {
		d()
	}
	r.mu.Lock()
	n := len(r.streams)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("流都结束以后不应该留下记录，还剩 %d", n)
	}

	// 副本移动以后目录变了，换一个新的检查。
	g, done, err = r.openStream(lockTestPID, "web", files)
	if err != nil {
		t.Fatal(err)
	}
	moved, done2, err := r.openStream(lockTestPID, "web", "/elsewhere/x/files")
	if err != nil || moved == g || moved.Dir != "/elsewhere/x/files" {
		t.Fatalf("目录变了应该换一个检查：%v", err)
	}
	done()
	done2()
}
