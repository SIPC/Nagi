package api

import (
	"context"
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

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
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
