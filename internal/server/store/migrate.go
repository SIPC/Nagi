package store

import "embed"

// Migrations 是面板数据库的迁移文件。
//
//go:embed migrations/*.sql
var Migrations embed.FS
