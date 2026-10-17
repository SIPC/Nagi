package projectstore

import "embed"

// Migrations 是项目数据库的迁移文件。
//
//go:embed migrations/*.sql
var Migrations embed.FS
