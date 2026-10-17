package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SIPC/nagi/internal/pkg/db"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// 面板本地命令（nagi reset ...）：忘记密码、丢了两步验证设备或者忘了安全入口时，在面板机器上用 root 执行。
// 能登录机器 root 的人本来就拥有这台机器，这条路不会降低安全性。直接修改面板数据库，面板运行中也能用。

func openPanelDB(ctx context.Context, dataDir string) (*store.Queries, func(), error) {
	path := filepath.Join(dataDir, "panel.db")
	if _, err := os.Stat(path); err != nil {
		return nil, nil, errors.New("找不到面板数据，请检查 --data-dir（默认 /var/lib/nagi，面板数据在它下面的 panel/）")
	}
	d, err := db.OpenMigrated(ctx, path, store.Migrations)
	if err != nil {
		return nil, nil, err
	}
	return store.New(d), func() { d.Close() }, nil
}

// ResetPassword 给用户设置一个新的随机密码，并让所有登录失效。username 为空且只有一个用户时就是这个用户。
func ResetPassword(ctx context.Context, dataDir, username string) (string, string, error) {
	q, done, err := openPanelDB(ctx, dataDir)
	if err != nil {
		return "", "", err
	}
	defer done()
	users, err := q.ListUsers(ctx)
	if err != nil {
		return "", "", err
	}
	var user *store.User
	for i := range users {
		if users[i].Username == username || (username == "" && len(users) == 1) {
			user = &users[i]
		}
	}
	if user == nil {
		return "", "", fmt.Errorf("找不到用户 %q", username)
	}
	token, err := auth.RandomToken()
	if err != nil {
		return "", "", err
	}
	password := token[:16]
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", "", err
	}
	if err := q.UpdatePassword(ctx, store.UpdatePasswordParams{PasswordHash: hash, ID: user.ID}); err != nil {
		return "", "", err
	}
	return user.Username, password, q.DeleteUserSessions(ctx, user.ID)
}

// ResetTOTP 关闭所有用户的两步验证。
func ResetTOTP(ctx context.Context, dataDir string) error {
	q, done, err := openPanelDB(ctx, dataDir)
	if err != nil {
		return err
	}
	defer done()
	users, err := q.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := q.SetTOTP(ctx, store.SetTOTPParams{TotpSecret: "", ID: u.ID}); err != nil {
			return err
		}
	}
	return nil
}

// ResetEntrance 关闭安全入口，返回原来的路径（方便用户想起来）。
func ResetEntrance(ctx context.Context, dataDir string) (string, error) {
	q, done, err := openPanelDB(ctx, dataDir)
	if err != nil {
		return "", err
	}
	defer done()
	old, _ := q.GetSetting(ctx, auth.SettingEntrance)
	return old, q.DeleteSetting(ctx, auth.SettingEntrance)
}
