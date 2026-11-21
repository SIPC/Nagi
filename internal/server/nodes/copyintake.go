package nodes

import (
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
)

// copyWindow is how far back the receive duty cycle is measured.
const copyWindow = 10

// CopyIntake decides whether this panel accepts an extra catch-up batch.
// Normal batches are always accepted (their rate is already bounded by each
// node's schedule); catch-up is refused while recent-copy receipts already
// occupy Share of wall time across all nodes, while another catch-up batch is
// being written, while the log disk is low, or while the machine is under
// pressure. Refusal costs the node one cheap round trip; it retries later.
type CopyIntake struct {
	Share    float64 // fraction of wall time receipts may occupy before catch-up is refused
	Pressure float64 // PSI some avg10 percentage at which catch-up is refused
	Dir      string  // log directory checked with metrics.CheckDisk

	mu       sync.Mutex
	busy     [copyWindow]time.Duration
	second   [copyWindow]int64
	catching bool
	disk     error
	diskAt   time.Time
	psi      float64
	psiAt    time.Time
	now      func() time.Time
	// PSI samples machine pressure; replaceable for tests.
	PSI func() float64
}

func NewCopyIntake(sharePercent, pressurePercent int, dir string) *CopyIntake {
	return &CopyIntake{Share: float64(sharePercent) / 100, Pressure: float64(pressurePercent), Dir: dir, now: time.Now, PSI: metrics.Pressure}
}

// Begin admits one receipt. It returns false only for refused catch-up.
// Admitted callers must call the returned done when the receipt finishes.
func (c *CopyIntake) Begin(catchup bool) (done func(), ok bool) {
	if c == nil {
		return func() {}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if catchup {
		if c.catching || c.dutyLocked(now) >= c.Share || c.lowLocked(now) {
			return nil, false
		}
		c.catching = true
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		end := c.now()
		// Split the busy time across the seconds it covered.
		for t := now; t.Before(end); {
			next := t.Truncate(time.Second).Add(time.Second)
			if next.After(end) {
				next = end
			}
			s := t.Unix()
			i := s % copyWindow
			if c.second[i] != s {
				c.second[i], c.busy[i] = s, 0
			}
			c.busy[i] += next.Sub(t)
			t = next
		}
		if catchup {
			c.catching = false
		}
	}, true
}

// dutyLocked is the share of the last copyWindow whole seconds spent receiving.
// Concurrent receipts may add up past 1; that still means "saturated".
func (c *CopyIntake) dutyLocked(now time.Time) float64 {
	var total time.Duration
	cur := now.Unix()
	for i := range copyWindow {
		if s := c.second[i]; s < cur && s >= cur-copyWindow {
			total += c.busy[i]
		}
	}
	return total.Seconds() / copyWindow
}

func (c *CopyIntake) lowLocked(now time.Time) bool {
	if now.Sub(c.diskAt) >= 5*time.Second {
		c.disk, c.diskAt = metrics.CheckDisk(c.Dir, 0), now
	}
	if now.Sub(c.psiAt) >= time.Second {
		c.psi, c.psiAt = c.PSI(), now
	}
	return c.disk != nil || c.psi >= c.Pressure
}
