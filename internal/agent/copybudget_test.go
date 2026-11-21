package agent

import (
	"testing"
	"time"
)

// Drives the budget like a saturated sender: one batch whenever ready, on a
// 10ms clock, and returns entries sent per class over the given duration.
func drainBudget(b *copyBudget, clock *time.Time, d time.Duration, ordinary, important bool) (sent [2]int) {
	for end := clock.Add(d); clock.Before(end); *clock = clock.Add(10 * time.Millisecond) {
		for i, want := range []bool{ordinary, important} {
			if want && b.ready(i == 1) {
				b.charge(i == 1, 100, 1000)
				sent[i] += 100
			}
		}
	}
	return sent
}

func TestCopyBudgetSharesAndBorrowing(t *testing.T) {
	clock := time.Unix(1000, 0)
	// 1000 entries/s total; bytes are not the binding limit here.
	b := newCopyBudget(1<<30, 1000, func() time.Time { return clock })
	// Alone, ordinary borrows the idle important share: 10 s at the full rate,
	// not its own half, plus the 1 s starting bucket and one overshooting batch.
	alone := drainBudget(b, &clock, 10*time.Second, true, false)
	if alone[0] < 10_000 || alone[0] > 11_100 {
		t.Fatalf("ordinary alone sent %d entries in 10s, want 10000-11100", alone[0])
	}
	// Once important is busy, neither class may take the other's half.
	both := drainBudget(b, &clock, 10*time.Second, true, true)
	for i, n := range both {
		if n < 4000 || n > 6000 {
			t.Fatalf("class %d sent %d entries in 10s while both busy, want about 5000: %v", i, n, both)
		}
	}
}
