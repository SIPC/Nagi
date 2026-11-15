package byterange

import "testing"

func TestParseResolve(t *testing.T) {
	cases := []struct {
		header      string
		ok          bool
		size        int64
		start, n    int64
		satisfiable bool
	}{
		{"bytes=0-99", true, 1000, 0, 100, true},
		{"bytes=500-", true, 1000, 500, 500, true},
		{"bytes=-100", true, 1000, 900, 100, true},
		{"bytes=-5000", true, 1000, 0, 1000, true},
		{"bytes=900-5000", true, 1000, 900, 100, true},
		{"bytes=1000-", true, 1000, 1000, 0, false},
		{"bytes=0-", true, 0, 0, 0, false},
		{"bytes=0-1,5-6", false, 0, 0, 0, false},
		{"bytes=5-1", false, 0, 0, 0, false},
		{"items=0-1", false, 0, 0, 0, false},
		{"bytes=-0", false, 0, 0, 0, false},
		{"bytes=abc-", false, 0, 0, 0, false},
	}
	for _, c := range cases {
		off, length, ok := Parse(c.header)
		if ok != c.ok {
			t.Errorf("Parse(%q) ok = %v", c.header, ok)
			continue
		}
		if !ok {
			continue
		}
		start, n, sat := Resolve(off, length, c.size)
		if sat != c.satisfiable || (sat && (start != c.start || n != c.n)) {
			t.Errorf("%q / %d：得到 %d+%d %v，应为 %d+%d %v", c.header, c.size, start, n, sat, c.start, c.n, c.satisfiable)
		}
	}
}
