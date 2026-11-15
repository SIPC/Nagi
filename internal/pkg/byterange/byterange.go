// Package byterange 处理下载时只取文件一段（HTTP Range 请求）：面板解析请求头，节点按同样的规则算出要读的部分。
// 支持断点续传和多线程下载工具；一次只支持一段，多段的请求按整个文件回答（HTTP 允许忽略 Range）。
package byterange

import (
	"io"
	"strconv"
	"strings"
)

// Parse 解析 Range 请求头，返回 offset 和 length（含义见 Resolve）。不是单段的 bytes 范围时 ok 为 false。
func Parse(header string) (offset, length int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return -n, 0, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if last == "" {
		return start, 0, true
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end - start + 1, true
}

// Resolve 按文件大小算出要读的起点和长度。offset 小于 0 表示最后 -offset 个字节，length 为 0 表示读到末尾，
// 超出文件的部分截掉。起点不在文件里时 ok 为 false（HTTP 416）。
func Resolve(offset, length, size int64) (start, n int64, ok bool) {
	if offset < 0 {
		start = max(0, size+offset)
	} else {
		start = offset
	}
	if start >= size {
		return start, 0, false
	}
	n = size - start
	if length > 0 && length < n {
		n = length
	}
	return start, n, true
}

// Skip 把 r 跳到 start：能 Seek 的直接跳（普通文件），不能的读掉前面的部分（例如快照里的文件）。
func Skip(r io.Reader, start int64) error {
	if start == 0 {
		return nil
	}
	if s, ok := r.(io.Seeker); ok {
		_, err := s.Seek(start, io.SeekStart)
		return err
	}
	_, err := io.CopyN(io.Discard, r, start)
	return err
}

// Section 返回 r（从文件开头读）里 offset、length 指定的那一段，以及这一段是否存在；offset、length 都是 0 时是整个文件。
func Section(r io.Reader, offset, length, size int64) (io.Reader, bool, error) {
	if offset == 0 && length == 0 {
		return r, true, nil
	}
	start, n, ok := Resolve(offset, length, size)
	if !ok {
		return nil, false, nil
	}
	if err := Skip(r, start); err != nil {
		return nil, false, err
	}
	return io.LimitReader(r, n), true, nil
}
