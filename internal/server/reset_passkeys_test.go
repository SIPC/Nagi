package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/store"
)

// M5：nagi reset password 同时删除这个成员的通行密钥，别人的不动。
func TestResetPasswordClearsPasskeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(d)
	var ids []int64
	for _, name := range []string{"admin", "bob"} {
		id, err := q.CreateUser(ctx, store.CreateUserParams{Username: name, PasswordHash: "x", Role: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if _, err := q.AddPasskey(ctx, store.Passkey{UserID: id, CredentialID: []byte(name), PublicKey: []byte("k"), Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	if _, _, err := ResetPassword(ctx, dir, "admin"); err != nil {
		t.Fatal(err)
	}
	d, _ = db.OpenMigrated(ctx, filepath.Join(dir, "panel.db"), store.Migrations)
	defer d.Close()
	q = store.New(d)
	for i, want := range []int{0, 1} {
		list, _ := q.UserPasskeys(ctx, ids[i])
		if len(list) != want {
			t.Errorf("用户 %d 剩 %d 个通行密钥，应该是 %d", ids[i], len(list), want)
		}
	}
}
