// Package migrations 内嵌 goose SQL 迁移。
package migrations

import "embed"

// FS 包含全部 *.sql 迁移文件。
//
//go:embed *.sql
var FS embed.FS
