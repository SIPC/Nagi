package metrics

// Pressure returns the highest 10-second Linux PSI stall percentage: CPU
// "some", memory and I/O "full". It is 0 when PSI is unavailable, so
// background work that yields on pressure never stops just because the
// kernel lacks PSI.
func Pressure() float64 { return pressure() }
