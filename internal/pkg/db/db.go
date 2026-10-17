// Package db 打开 SQLite 数据库并执行迁移。使用纯 Go 的 modernc.org/sqlite，不需要 CGO。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Open 打开（必要时创建）数据库文件，开启 WAL、外键和忙等待。
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 同一时间只允许一个写入者，单连接可以避免 "database is locked"。
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Migrate 按文件名顺序执行 migrations 目录下尚未执行的 SQL 文件。
// 文件名必须以数字开头，例如 0001_init.sql。已执行的版本记录在 user_version 里。
func Migrate(ctx context.Context, d *sql.DB, migrations fs.FS) error {
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("迁移文件名 %q 必须以数字开头", e.Name())
		}
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	var current int
	if err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		body, err := fs.ReadFile(migrations, m.name)
		if err != nil {
			return err
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("执行迁移 %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
