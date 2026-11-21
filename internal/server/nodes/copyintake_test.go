package nodes

import (
	"testing"
	"time"
)

func TestCopyIntakeRefusesCatchupWhenReceiptsSaturate(t *testing.T) {
	clock := time.Unix(1000, 0)
	c := NewCopyIntake(50, 40, t.TempDir())
	c.now = func() time.Time { return clock }
	c.PSI = func() float64 { return 0 }
	// Ten seconds of normal receipts each busy for `busy` of every second.
	receive := func(busy time.Duration) {
		for range 10 {
			done, ok := c.Begin(false)
			if !ok {
				t.Fatal("normal batch refused")
			}
			clock = clock.Add(busy)
			done()
			clock = clock.Add(time.Second - busy)
		}
	}
	receive(400 * time.Millisecond) // 40% < 50%
	done, ok := c.Begin(true)
	if !ok {
		t.Fatal("catch-up refused below the receive share")
	}
	// Only one catch-up batch is written at a time.
	if _, ok := c.Begin(true); ok {
		t.Fatal("second concurrent catch-up admitted")
	}
	if d, ok := c.Begin(false); !ok {
		t.Fatal("normal batch refused during catch-up")
	} else {
		d()
	}
	done()
	receive(600 * time.Millisecond) // 60% > 50%
	if _, ok := c.Begin(true); ok {
		t.Fatal("catch-up admitted while receipts used 60% of the panel")
	}
	// The window forgets old busy time.
	clock = clock.Add(11 * time.Second)
	if d, ok := c.Begin(true); !ok {
		t.Fatal("catch-up still refused after the window passed")
	} else {
		d()
	}
	c.PSI = func() float64 { return 45 }
	clock = clock.Add(time.Second)
	if _, ok := c.Begin(true); ok {
		t.Fatal("catch-up admitted under machine pressure")
	}
}
