package project

import (
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// 定时快照失败以后逐级拉长重试间隔，不超过快照间隔；退避期间不算到期（见 docs/snapshot.md#失败与退避）。
func TestSnapshotBackoff(t *testing.T) {
	gap := 720 * time.Minute
	for i, want := range []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour} {
		if got := retryDelay(int64(i+1), gap); got != want {
			t.Errorf("failure %d: %s, want %s", i+1, got, want)
		}
	}
	if got := retryDelay(4, 15*time.Minute); got != 15*time.Minute {
		t.Errorf("short interval: %s, want 15m", got)
	}
	if got := retryDelay(3, time.Minute); got != 5*time.Minute {
		t.Errorf("tiny interval: %s, want 5m", got)
	}

	m := &Manager{}
	now := time.Now()
	c := &Cached{Spec: &nagiv1.ContainerSpec{SnapshotIntervalMinutes: 60}, LastSnapshot: now.Add(-2 * time.Hour)}
	if !m.snapshotDue(c, now) {
		t.Error("past the interval with no backoff should be due")
	}
	c.SnapshotRetryAt = now.Add(10 * time.Minute)
	if m.snapshotDue(c, now) {
		t.Error("inside the backoff should not be due")
	}
	if !m.snapshotDue(c, now.Add(11*time.Minute)) {
		t.Error("after the backoff should be due again")
	}
	c.SnapshotRetryAt, c.LastSnapshot = time.Time{}, now.Add(-30*time.Minute)
	if m.snapshotDue(c, now) {
		t.Error("before the interval should not be due")
	}
}
