package agent

import (
	"sync"
	"time"
)

// copyIdle is how long a class must have sent nothing before the other class
// may borrow its unused share.
const copyIdle = 2 * time.Second

// copyBudget is this node's bytes-per-second and entries-per-second budget
// for recent-copy catch-up, shared by every target panel. Important and
// ordinary categories each own half; a class borrows the other's unused share
// only while the other has sent nothing for copyIdle. Each class holds at most
// one second of tokens. A batch is charged after it is built, so a balance may
// go negative by one batch; the class then waits until it is positive again.
type copyBudget struct {
	mu     sync.Mutex
	rate   [2][2]float64 // [class][bytes, entries] per second
	tokens [2][2]float64
	active [2]time.Time
	at     time.Time
	now    func() time.Time
}

func newCopyBudget(bytesPerSecond, entriesPerSecond int, now func() time.Time) *copyBudget {
	half := [2]float64{float64(bytesPerSecond) / 2, float64(entriesPerSecond) / 2}
	return &copyBudget{rate: [2][2]float64{half, half}, tokens: [2][2]float64{half, half}, now: now, at: now()}
}

func copyClass(important bool) (own, other int) {
	if important {
		return 1, 0
	}
	return 0, 1
}

func (b *copyBudget) refill() time.Time {
	now := b.now()
	if dt := now.Sub(b.at).Seconds(); dt > 0 {
		for c := range 2 {
			for d := range 2 {
				b.tokens[c][d] = min(b.rate[c][d], b.tokens[c][d]+b.rate[c][d]*dt)
			}
		}
		b.at = now
	}
	return now
}

// ready reports whether the class may send another catch-up batch now.
func (b *copyBudget) ready(important bool) bool {
	own, other := copyClass(important)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.refill()
	borrow := now.Sub(b.active[other]) >= copyIdle
	for d := range 2 {
		avail := b.tokens[own][d]
		if borrow {
			avail += max(b.tokens[other][d], 0)
		}
		if avail <= 0 {
			return false
		}
	}
	return true
}

// charge records a sent batch, taking from an idle class's share first.
func (b *copyBudget) charge(important bool, entries, bytes int) {
	own, other := copyClass(important)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.refill()
	borrow := now.Sub(b.active[other]) >= copyIdle
	for d, cost := range [2]float64{float64(bytes), float64(entries)} {
		if need := cost - max(b.tokens[own][d], 0); need > 0 && borrow && b.tokens[other][d] > 0 {
			lend := min(need, b.tokens[other][d])
			b.tokens[other][d] -= lend
			cost -= lend
		}
		b.tokens[own][d] -= cost
	}
	if entries > 0 {
		b.active[own] = now
	}
}
