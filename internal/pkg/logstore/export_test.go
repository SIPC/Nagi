package logstore

// SetLiveScan shrinks the Live page scan bound so tests cross it cheaply.
func SetLiveScan(n int64) (restore func()) {
	old := liveScan
	liveScan = n
	return func() { liveScan = old }
}

// HoldCopies simulates a slow recent.db writer by holding its gate until release.
func HoldCopies(c *Copies) (release func()) {
	c.s.gate <- struct{}{}
	return func() { <-c.s.gate }
}
