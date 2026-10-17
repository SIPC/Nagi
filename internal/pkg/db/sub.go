package db

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
)

// OpenMigrated 打开数据库，并执行 migrations 目录下的迁移。
// migrations 是 embed 进来的文件系统，迁移文件放在其中的 migrations/ 子目录。
func OpenMigrated(ctx context.Context, path string, migrations embed.FS) (*sql.DB, error) {
	d, err := Open(path)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		d.Close()
		return nil, err
	}
	if err := Migrate(ctx, d, sub); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}
