package standby

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/store"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := db.OpenMigrated(context.Background(), path, store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func mustExec(t *testing.T, d *sql.DB, q string) {
	t.Helper()
	if _, err := d.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// 主面板改了密码：备用面板上这个账号的会话作废。两步验证用过的时间步取两边大的。
func TestApplySessionsAndTOTP(t *testing.T) {
	dir := t.TempDir()
	local := openTestDB(t, filepath.Join(dir, "panel.db"))
	mustExec(t, local, `INSERT INTO users (id, username, password_hash, created_at, totp_last_step) VALUES (1, 'admin', 'old', 0, 500), (2, 'ops', 'same', 0, 0)`)
	mustExec(t, local, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('s1', 1, 0, 9999999999), ('s2', 2, 0, 9999999999)`)

	primary := openTestDB(t, filepath.Join(t.TempDir(), "primary.db"))
	mustExec(t, primary, `INSERT INTO users (id, username, password_hash, created_at, totp_last_step) VALUES (1, 'admin', 'new', 0, 400), (2, 'ops', 'same', 0, 0)`)
	snap, err := snapshot(context.Background(), primary, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	c := &Client{DataDir: dir, DB: local, Log: slog.Default()}
	if err := c.apply(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = local.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = 1`).Scan(&n)
	if n != 0 {
		t.Fatal("改了密码的账号的会话应作废")
	}
	_ = local.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = 2`).Scan(&n)
	if n != 1 {
		t.Fatal("没改的账号的会话应保留")
	}
	var step int64
	var hash string
	_ = local.QueryRow(`SELECT totp_last_step, password_hash FROM users WHERE id = 1`).Scan(&step, &hash)
	if step != 500 || hash != "new" {
		t.Fatalf("totp_last_step 应取大的 500、密码用主面板的：%d %s", step, hash)
	}
}
