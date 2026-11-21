package logstore

// HoldCopies simulates a slow recent.db writer by holding its gate until release.
func HoldCopies(c *Copies) (release func()) {
	c.s.gate <- struct{}{}
	return func() { <-c.s.gate }
}
