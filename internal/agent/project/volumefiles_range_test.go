package project

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
)

// sendFile 按 offset、length 只发一段，第一条注明；普通文件（能 Seek）和快照里的文件（只能顺序读）结果一样。
func TestSendFileRange(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 60_000) // 比一块（256 KiB）大
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.Unix(1_700_000_000, 0)
	cases := []struct {
		desc           string
		offset, length int64
		want           []byte
		ranged         bool
	}{
		{"整个文件", 0, 0, data, false},
		{"中间一段", 300_000, 100, data[300_000:300_100], true},
		{"跨块到末尾", 200_000, 0, data[200_000:], true},
		{"最后几个字节", -5, 0, data[len(data)-5:], true},
		{"超出文件", int64(len(data)), 0, nil, true},
	}
	for _, c := range cases {
		for _, seekable := range []bool{true, false} {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			var r io.Reader = f
			if !seekable {
				r = struct{ io.Reader }{f}
			}
			var head *nagiv1.VolumeFileChunk
			var got []byte
			err = sendFile(r, "f", int64(len(data)), mod, c.offset, c.length, func(ch *nagiv1.VolumeFileChunk) error {
				if head == nil {
					head = ch
				} else {
					got = append(got, ch.GetData()...)
				}
				return nil
			})
			f.Close()
			if err != nil || !bytes.Equal(got, c.want) || head.GetRanged() != c.ranged || head.GetSize() != int64(len(data)) ||
				head.GetModifiedUnixNano() != mod.UnixNano() {
				t.Errorf("%s（seek=%v）：err=%v 收到 %d 字节，应为 %d；ranged=%v", c.desc, seekable, err, len(got), len(c.want), head.GetRanged())
			}
		}
	}
}
