package nodestore

import "embed"

// Migrations 是节点数据库的迁移文件。
//
//go:embed migrations/*.sql
var Migrations embed.FS
