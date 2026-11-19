package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg" // 注册 JPEG，image.DecodeConfig 才认得。
	_ "image/png"  // 注册 PNG。
)

// maxAvatarSide 是头像的边长上限（像素）。界面上传前会缩到 128；不限尺寸时，256 KB 的 PNG 能写成 5400×5400，
// 原样解出来上百 MB，App 打开「成员」页就崩溃（安全审查 v0.35.0#17）。
const maxAvatarSide = 1024

// avatarSize 只读头像的宽和高，不解码像素。PNG、JPEG 用标准库；标准库没有 WebP，按文件头读（VP8、VP8L、VP8X 三种）。
func avatarSize(b []byte) (int, int, error) {
	if len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return webpSize(b)
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, err
	}
	return c.Width, c.Height, nil
}

var errBadWebP = errors.New("webp: invalid header")

// webpSize 按 WebP 容器格式读第一个块的尺寸（https://developers.google.com/speed/webp/docs/riff_container）。
func webpSize(b []byte) (int, int, error) {
	if len(b) < 30 {
		return 0, 0, errBadWebP
	}
	d := b[20:]
	switch string(b[12:16]) {
	case "VP8 ": // 有损：3 字节帧标记、起始码 9d 01 2a，再是 14 位的宽和高。
		if d[3] != 0x9d || d[4] != 0x01 || d[5] != 0x2a {
			return 0, 0, errBadWebP
		}
		return int(binary.LittleEndian.Uint16(d[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(d[8:10]) & 0x3fff), nil
	case "VP8L": // 无损：签名 0x2f，再是各 14 位的宽减 1、高减 1。
		if d[0] != 0x2f {
			return 0, 0, errBadWebP
		}
		v := binary.LittleEndian.Uint32(d[1:5])
		return int(v&0x3fff) + 1, int(v>>14&0x3fff) + 1, nil
	case "VP8X": // 扩展：1 字节标志、3 字节保留，再是各 24 位的画布宽减 1、高减 1。
		w := int(d[4]) | int(d[5])<<8 | int(d[6])<<16
		h := int(d[7]) | int(d[8])<<8 | int(d[9])<<16
		return w + 1, h + 1, nil
	}
	return 0, 0, errBadWebP
}
