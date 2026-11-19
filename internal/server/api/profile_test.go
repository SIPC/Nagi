package api

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/SIPC/nagi/internal/pkg/db"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

func TestUpdateProfile(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, created_at, role) VALUES (7, 'admin', 'x', 0, 'viewer')`); err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	s := &authService{&API{Q: q, Auth: auth.New(q, nil)}}
	ctx = auth.WithUser(ctx, auth.User{ID: 7, Name: "admin", Role: auth.RoleViewer})
	update := func(name string, avatar []byte) error {
		_, err := s.UpdateProfile(ctx, connect.NewRequest(&nagiv1.UpdateProfileRequest{DisplayName: name, Avatar: avatar}))
		return err
	}

	png := encodePNG(t, 1, 1)
	if err := update("  小明  ", png); err != nil {
		t.Fatal(err)
	}
	row, err := q.GetUserByID(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.DisplayName != "小明" || string(row.Avatar) != string(png) {
		t.Fatalf("没有保存：%q %d 字节", row.DisplayName, len(row.Avatar))
	}
	if row.Username != "admin" {
		t.Fatalf("用户名不应该变：%q", row.Username)
	}
	me, err := s.Me(ctx, connect.NewRequest(&nagiv1.MeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if me.Msg.GetDisplayName() != "小明" || me.Msg.GetAvatarType() != "image/png" {
		t.Fatalf("Me 返回不对：%q %q", me.Msg.GetDisplayName(), me.Msg.GetAvatarType())
	}

	// 不合法的内容不保存。SVG 能带脚本，不接受。
	for name, c := range map[string]struct {
		name   string
		avatar []byte
	}{
		"名称太长": {strings.Repeat("名", 33), nil},
		"头像太大": {"", append(append([]byte{}, png...), make([]byte, maxAvatarBytes)...)},
		"SVG":  {"", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		"不是图片": {"", []byte("hello")},
		// 只有文件头、读不出尺寸的也不接受。
		"PNG 文件头": {"", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)},
		// 文件小、尺寸大（v0.35.0#17）。
		"太宽":      {"", encodePNG(t, maxAvatarSide+1, 1)},
		"太高":      {"", encodePNG(t, 1, maxAvatarSide+1)},
		"WebP 太大": {"", webpVP8X(4000, 4000)},
	} {
		if err := update(c.name, c.avatar); err == nil {
			t.Errorf("%s：应该拒绝", name)
		}
	}
	if row, _ := q.GetUserByID(ctx, 7); row.DisplayName != "小明" || len(row.Avatar) != len(png) {
		t.Fatalf("被拒绝的请求改动了资料：%q %d 字节", row.DisplayName, len(row.Avatar))
	}

	// 清空名称和头像。
	if err := update("", nil); err != nil {
		t.Fatalf("清空应该成功：%v", err)
	}
	if row, _ := q.GetUserByID(ctx, 7); row.DisplayName != "" || len(row.Avatar) != 0 {
		t.Fatalf("没有清空：%q %d 字节", row.DisplayName, len(row.Avatar))
	}
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// webpVP8X 拼一个只有 VP8X 块的 WebP 文件头，画布是 w×h。
func webpVP8X(w, h int) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00")
	w, h = w-1, h-1
	return append(b, byte(w), byte(w>>8), byte(w>>16), byte(h), byte(h>>8), byte(h>>16))
}

func TestAvatarSize(t *testing.T) {
	// VP8：3 字节帧标记、起始码，再是宽 640、高 480。
	vp8 := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00"), 0, 0, 0, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01)
	// VP8L：签名 0x2f，宽减 1 = 99、高减 1 = 49，各 14 位。
	bits := uint32(99) | uint32(49)<<14
	vp8l := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8L\x00\x00\x00\x00"), 0x2f, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24), 0, 0, 0, 0, 0)
	for name, c := range map[string]struct {
		b    []byte
		w, h int
	}{
		"PNG":  {encodePNG(t, 300, 200), 300, 200},
		"VP8":  {vp8, 640, 480},
		"VP8L": {vp8l, 100, 50},
		"VP8X": {webpVP8X(5400, 5400), 5400, 5400},
	} {
		w, h, err := avatarSize(c.b)
		if err != nil || w != c.w || h != c.h {
			t.Errorf("%s：得到 %d×%d %v，应该是 %d×%d", name, w, h, err, c.w, c.h)
		}
	}
	for name, b := range map[string][]byte{
		"截断的 WebP":  []byte("RIFF\x00\x00\x00\x00WEBPVP8X"),
		"VP8 起始码不对": append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00"), make([]byte, 10)...),
		"不认识的块":     append([]byte("RIFF\x00\x00\x00\x00WEBPABCD\x00\x00\x00\x00"), make([]byte, 10)...),
	} {
		if _, _, err := avatarSize(b); err == nil {
			t.Errorf("%s：应该出错", name)
		}
	}
}
