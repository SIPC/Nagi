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

// 整个文件下载也只发 Lstat 看到的大小：文件变长了，或者读到的已经不是那个文件（例如设备文件），多出来的不发。
func TestSendFileLimitsToSize(t *testing.T) {
	data := bytes.Repeat([]byte("A"), 4096)
	r := io.MultiReader(bytes.NewReader(data), bytes.NewReader(make([]byte, 1<<20)))
	var got []byte
	err := sendFile(r, "f", int64(len(data)), time.Now(), 0, 0, func(ch *nagiv1.VolumeFileChunk) error {
		got = append(got, ch.GetData()...)
		return nil
	})
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("err=%v 收到 %d 字节，应为 %d", err, len(got), len(data))
	}
}

// ReadVolumeFile 下载整个文件时，在 Lstat 之后追加的内容不发。
func TestReadVolumeFileLimitsToLstatSize(t *testing.T) {
	f := newVolumeFixture(t)
	f.put(t, "log", "0123456789")
	ref := &nagiv1.VolumeFileRef{ProjectId: "p1", Container: "web", Volume: DataVolume, Path: "log"}
	var head *nagiv1.VolumeFileChunk
	var got []byte
	err := f.m.ReadVolumeFile(t.Context(), ref, func(ch *nagiv1.VolumeFileChunk) error {
		if head == nil {
			head = ch
			// 第一条已经带上了 Lstat 的大小，这时再往文件里追加。
			a, err := os.OpenFile(filepath.Join(f.data, "log"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, _ = a.Write(bytes.Repeat([]byte("x"), 1<<20))
			return a.Close()
		}
		got = append(got, ch.GetData()...)
		return nil
	})
	if err != nil || head.GetSize() != 10 || string(got) != "0123456789" {
		t.Errorf("err=%v size=%d 收到 %d 字节", err, head.GetSize(), len(got))
	}
}
