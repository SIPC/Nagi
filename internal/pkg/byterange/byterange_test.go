package byterange

import (
	"io"
	"strings"
	"testing"
)

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

// Section 不读出超过 size 的部分，整个文件也一样（文件变长了，或者读的已经不是看到的那个文件）。
func TestSectionLimitsToSize(t *testing.T) {
	data := strings.Repeat("A", 100)
	cases := []struct {
		offset, length, size int64
		want                 string
		ok                   bool
	}{
		{0, 0, 10, data[:10], true},
		{0, 0, 0, "", true},
		{0, 0, 1000, data, true},
		{5, 0, 10, data[5:10], true},
		{-3, 0, 10, data[7:10], true},
		{2, 50, 10, data[2:10], true},
		{10, 0, 10, "", false},
	}
	for _, c := range cases {
		r, ok, err := Section(strings.NewReader(data), c.offset, c.length, c.size)
		if err != nil || ok != c.ok {
			t.Errorf("%d+%d / %d：ok=%v err=%v", c.offset, c.length, c.size, ok, err)
			continue
		}
		if !ok {
			continue
		}
		got, _ := io.ReadAll(r)
		if string(got) != c.want {
			t.Errorf("%d+%d / %d：读出 %d 字节，应为 %d", c.offset, c.length, c.size, len(got), len(c.want))
		}
	}
}
