package dbproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

func TestNormalizeValue(t *testing.T) {
	ts := time.Date(2026, 10, 23, 12, 34, 56, 789000000, time.UTC)
	cases := []struct {
		in   any
		want any
	}{
		{nil, nil},
		{[]byte("abc"), "abc"},
		{ts, "2026-10-23T12:34:56.789Z"},
		{int64(42), int64(42)},
		{"x", "x"},
		{true, true},
		{float64(1.5), float64(1.5)},
	}
	for i, tc := range cases {
		if got := normalizeValue(tc.in); got != tc.want {
			t.Errorf("case %d: normalizeValue(%v) = %#v, want %#v", i, tc.in, got, tc.want)
		}
	}
}

func TestMySQLTLSConfig(t *testing.T) {
	cases := map[store.SSLMode]string{
		store.SSLModeDisable:    "false",
		store.SSLModePrefer:     "skip-verify",
		store.SSLModeRequire:    "skip-verify",
		store.SSLModeVerifyCA:   "skip-verify",
		store.SSLModeVerifyFull: "true",
		"":                      "false",
	}
	for mode, want := range cases {
		if got := mysqlTLSConfig(mode); got != want {
			t.Errorf("mysqlTLSConfig(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestSignatureIncludesPasswordAndHost(t *testing.T) {
	db := store.Database{ID: "db1", Engine: store.EngineMySQL, Host: "h", Port: 3306, DatabaseName: "d", Username: "u"}
	s1 := signature(db, "pw1")
	if s1 == signature(db, "pw2") {
		t.Fatal("password change must change the pool signature")
	}
	db2 := db
	db2.Host = "other"
	if signature(db2, "pw1") == s1 {
		t.Fatal("host change must change the pool signature")
	}
}

func setupSQLite(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	stmts := []string{
		"CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL, amount REAL)",
		"INSERT INTO items (id, name, amount) VALUES (1, 'alpha', 1.5)",
		"INSERT INTO items (id, name, amount) VALUES (2, 'beta', 2.5)",
		"INSERT INTO items (id, name, amount) VALUES (3, 'gamma', 3.5)",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
	return path
}

func TestSQLiteTestAndQuery(t *testing.T) {
	path := setupSQLite(t)
	db := store.Database{ID: "db1", Name: "local", Engine: store.EngineSQLite, SQLitePath: path, SSLMode: store.SSLModeDisable}

	p := New(Config{}, nil)
	defer p.Close()

	ctx := context.Background()
	res, err := p.Test(ctx, db, "")
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.ServerVersion == "" {
		t.Fatal("server_version is empty")
	}

	qr, err := p.Query(ctx, db, "", QueryRequest{SQL: "SELECT id, name FROM items ORDER BY id"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(qr.Columns) != 2 || qr.Columns[0].Name != "id" || qr.Columns[1].Name != "name" {
		t.Fatalf("columns = %+v", qr.Columns)
	}
	if qr.Columns[1].Type == "" {
		t.Fatal("column type should come from driver metadata")
	}
	if qr.RowCount != 3 || len(qr.Rows) != 3 {
		t.Fatalf("row_count = %d rows = %d", qr.RowCount, len(qr.Rows))
	}
	if fmt.Sprint(qr.Rows[0][0]) != "1" || qr.Rows[0][1] != "alpha" {
		t.Fatalf("first row = %#v", qr.Rows[0])
	}
	if qr.Truncated {
		t.Fatal("should not be truncated")
	}
	if qr.DurationMS < 0 {
		t.Fatal("negative duration")
	}

	// 行数上限
	qr, err = p.Query(ctx, db, "", QueryRequest{SQL: "SELECT id FROM items ORDER BY id", MaxRows: 1})
	if err != nil {
		t.Fatalf("Query maxRows: %v", err)
	}
	if !qr.Truncated || qr.RowCount != 1 {
		t.Fatalf("maxRows: truncated=%v rows=%d", qr.Truncated, qr.RowCount)
	}

	// 字节上限
	qr, err = p.Query(ctx, db, "", QueryRequest{SQL: "SELECT id, name FROM items", MaxBytes: 5})
	if err != nil {
		t.Fatalf("Query maxBytes: %v", err)
	}
	if !qr.Truncated || qr.RowCount != 0 {
		t.Fatalf("maxBytes: truncated=%v rows=%d", qr.Truncated, qr.RowCount)
	}
}

func TestQueryRejectsWrites(t *testing.T) {
	path := setupSQLite(t)
	db := store.Database{ID: "db1", Engine: store.EngineSQLite, SQLitePath: path}

	p := New(Config{}, nil)
	defer p.Close()

	for _, sqlText := range []string{
		"DELETE FROM items",
		"UPDATE items SET name = 'x'",
		"INSERT INTO items (id, name) VALUES (4, 'delta')",
		"DROP TABLE items",
		"SELECT 1; DELETE FROM items",
	} {
		_, err := p.Query(context.Background(), db, "", QueryRequest{SQL: sqlText})
		if !errors.Is(err, ErrNotReadOnly) {
			t.Fatalf("Query(%q) error = %v, want ErrNotReadOnly", sqlText, err)
		}
		var nre *NotReadOnlyError
		if !errors.As(err, &nre) || nre.Result.RuleID == "" {
			t.Fatalf("Query(%q) should carry the guard result", sqlText)
		}
	}

	// 第三道闸：即使绕过 sqlguard，query_only 连接也拒绝写入。
	b, err := newSQLiteBackend(Config{IdleTimeout: time.Minute}, db)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer b.Close()
	if _, err := b.Query(context.Background(), "DELETE FROM items", 10, 1024); err == nil {
		t.Fatal("query_only sqlite connection must reject writes")
	}
}

func TestSQLiteRejectsUnsafePaths(t *testing.T) {
	p := New(Config{}, nil)
	defer p.Close()

	for _, path := range []string{"", ":memory:", "relative.db", "file::memory:"} {
		db := store.Database{ID: "db1", Engine: store.EngineSQLite, SQLitePath: path}
		if _, err := p.Test(context.Background(), db, ""); err == nil {
			t.Fatalf("sqlite_path %q should be rejected", path)
		}
	}
}

func TestSQLiteMissingFileFails(t *testing.T) {
	db := store.Database{
		ID:         "db1",
		Engine:     store.EngineSQLite,
		SQLitePath: filepath.Join(t.TempDir(), "missing.db"),
	}
	p := New(Config{}, nil)
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := p.Test(ctx, db, ""); err == nil {
		t.Fatal("reading a missing sqlite file should fail")
	}
}

func TestInvalidateClosesPool(t *testing.T) {
	path := setupSQLite(t)
	db := store.Database{ID: "db1", Engine: store.EngineSQLite, SQLitePath: path}
	p := New(Config{}, nil)
	defer p.Close()

	if _, err := p.Test(context.Background(), db, ""); err != nil {
		t.Fatalf("Test: %v", err)
	}
	p.Invalidate("db1")
	p.mu.Lock()
	_, exists := p.pools["db1"]
	p.mu.Unlock()
	if exists {
		t.Fatal("Invalidate should remove the pool")
	}

	// 失效后仍可重建并查询。
	if _, err := p.Query(context.Background(), db, "", QueryRequest{SQL: "SELECT 1"}); err != nil {
		t.Fatalf("Query after invalidate: %v", err)
	}
}

func TestQueryAfterClose(t *testing.T) {
	path := setupSQLite(t)
	db := store.Database{ID: "db1", Engine: store.EngineSQLite, SQLitePath: path}
	p := New(Config{}, nil)
	_ = p.Close()

	_, err := p.Query(context.Background(), db, "", QueryRequest{SQL: "SELECT 1"})
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("error = %v, want ErrPoolClosed", err)
	}
}

func TestSQLiteQueryError(t *testing.T) {
	path := setupSQLite(t)
	db := store.Database{ID: "db1", Engine: store.EngineSQLite, SQLitePath: path}
	p := New(Config{}, nil)
	defer p.Close()

	_, err := p.Query(context.Background(), db, "", QueryRequest{SQL: "SELECT * FROM no_such_table"})
	if err == nil || errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("error = %v, want execution error", err)
	}
}

func TestUnsupportedEngine(t *testing.T) {
	p := New(Config{}, nil)
	defer p.Close()
	_, err := p.Test(context.Background(), store.Database{ID: "x", Engine: "oracle"}, "")
	if err == nil || !strings.Contains(err.Error(), "unsupported engine") {
		t.Fatalf("error = %v", err)
	}
}
