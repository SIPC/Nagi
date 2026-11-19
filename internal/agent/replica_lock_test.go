package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const lockTestPID = "fedcba9876543210"

// v0.35.0#20：没通过鉴权的副本请求不在 locks 里留下东西，随便填项目 ID 和容器名反复请求，节点内存不会一直涨。
func TestReplicaLocksReleasedAfterRejectedRequests(t *testing.T) {
	r := newReplicas(t.TempDir())
	s := &meshService{a: &agent{replicas: r}}
	for i := range 100 {
		pid := fmt.Sprintf("%016x", i)
		if _, err := s.ReplicaManifest(context.Background(), &nagiv1.ReplicaManifestRequest{ProjectId: pid, Container: "web"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("应该拒绝：%v", err)
		}
		if _, err := s.ReplicaCommit(context.Background(), &nagiv1.ReplicaCommitRequest{ProjectId: pid, Container: "web"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("应该拒绝：%v", err)
		}
	}
	if _, err := r.lockWithin(context.Background(), lockTestPID, "web", 0); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	n := len(r.locks)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("只应该留下还拿着的那一把锁，实际 %d", n)
	}
}

// 用完就删的锁照样互斥：拿着锁时别人等着，等着的请求也算用着，锁不会在中途被删掉换成新的。
func TestReplicaLockRefCount(t *testing.T) {
	r := &replicas{}
	unlock := r.lock(lockTestPID, "web")
	got := make(chan struct{})
	go func() {
		defer close(got)
		r.rlock(lockTestPID, "web")()
	}()
	// 别的副本不受影响。
	r.rlock(lockTestPID, "db")()
	select {
	case <-got:
		t.Fatal("写锁拿着时读锁应该等着")
	case <-time.After(100 * time.Millisecond):
	}
	r.mu.Lock()
	refs := r.locks[lockTestPID+"-web"].refs
	r.mu.Unlock()
	if refs != 2 {
		t.Fatalf("拿着的和等着的都算用着，实际 %d", refs)
	}
	unlock()
	<-got

	// 并发拿放以后全部删掉；lockWithin 拿不到时也不留下。
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			if i%2 == 0 {
				r.lock(lockTestPID, "web")()
			} else {
				r.rlock(lockTestPID, "web")()
			}
		})
	}
	wg.Wait()
	hold := r.rlock(lockTestPID, "web")
	if _, err := r.lockWithin(context.Background(), lockTestPID, "web", 0); status.Code(err) != codes.Aborted {
		t.Fatalf("拿不到时应该报同步进行中：%v", err)
	}
	hold()
	r.mu.Lock()
	n := len(r.locks)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("用完的锁应该删掉，还剩 %d", n)
	}
}
