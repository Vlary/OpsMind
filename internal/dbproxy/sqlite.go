package dbproxy

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"aiops/internal/store"
)

// sqliteBackend 是 SQLite 只读连接（modernc，无 cgo，单连接）。
type sqliteBackend struct {
	db *sql.DB
}

func newSQLiteBackend(cfg Config, db store.Database) (*sqliteBackend, error) {
	path := strings.TrimSpace(db.SQLitePath)
	if path == "" {
		return nil, fmt.Errorf("dbproxy: sqlite requires sqlite_path")
	}
	if strings.ContainsRune(path, 0) {
		return nil, fmt.Errorf("dbproxy: sqlite_path contains NUL")
	}
	if path == ":memory:" || strings.HasPrefix(path, ":memory:") {
		return nil, fmt.Errorf("dbproxy: in-memory sqlite is not allowed")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("dbproxy: sqlite_path must be absolute")
	}
	path = filepath.Clean(path)

	dsn := "file:" + path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbproxy: sqlite open: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxIdleTime(cfg.IdleTimeout)
	return &sqliteBackend{db: sqlDB}, nil
}

// Version 读取 sqlite_version()。
func (b *sqliteBackend) Version(ctx context.Context) (string, error) {
	var version string
	if err := b.db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

// Query 在 query_only 连接上执行一条 SELECT。
func (b *sqliteBackend) Query(ctx context.Context, sqlText string, maxRows int, maxBytes int64) (QueryResult, error) {
	// 单连接 + query_only(1)：任何写语句会被 SQLite 拒绝。
	rows, err := b.db.QueryContext(ctx, sqlText)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()
	return scanSQLRows(rows, maxRows, maxBytes)
}

func (b *sqliteBackend) Close() {
	if b.db != nil {
		_ = b.db.Close()
	}
}
