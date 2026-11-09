//go:build tools

// Package deps 仅用于在骨架阶段锁定 go.mod 依赖版本。
//
// 该文件带 tools 构建标签，不参与正常构建；各领域包实现落地后应删除本文件，
// 由真实 import 维持依赖。
package deps

import (
	_ "github.com/auxten/postgresql-parser/pkg/sql/parser"
	_ "github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/google/uuid"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/pressly/goose/v3"
	_ "golang.org/x/crypto/ssh"
	_ "golang.org/x/sync/semaphore"
	_ "modernc.org/sqlite"
)
