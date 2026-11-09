package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"aiops/internal/store/migrations"
)

const defaultTestDatabaseURL = "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable"

// testDatabaseURL 返回测试库 DSN。集成测试在数据库不可达时必须失败，不得 skip。
func testDatabaseURL() string {
	if v := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")); v != "" {
		return v
	}
	return defaultTestDatabaseURL
}

func TestMigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	if err := testDB.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := testDB.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var tables int
	if err := testDB.pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN (
			'users','sessions','servers','databases','providers','conversations',
			'messages','tool_calls','approvals','audit_logs','settings')`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 11 {
		t.Fatalf("tables = %d, want 11", tables)
	}

	indexes := []string{
		"sessions_expires_at_idx", "sessions_user_id_idx",
		"servers_created_at_idx", "servers_tags_idx",
		"databases_created_at_idx", "providers_created_at_idx",
		"conversations_last_message_at_idx", "conversations_provider_id_idx",
		"messages_conversation_seq_idx", "messages_conversation_created_idx", "messages_turn_id_idx",
		"tool_calls_conversation_seq_idx", "tool_calls_turn_id_idx",
		"approvals_status_expires_idx", "approvals_conversation_requested_idx",
		"audit_logs_created_at_idx", "audit_logs_server_created_idx",
		"audit_logs_conversation_created_idx", "audit_logs_action_created_idx",
		"audit_logs_resource_created_idx",
	}
	for _, name := range indexes {
		var n int
		if err := testDB.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name).Scan(&n); err != nil {
			t.Fatalf("check index %s: %v", name, err)
		}
		if n != 1 {
			t.Fatalf("index %s is missing", name)
		}
	}

	var trigger int
	if err := testDB.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger WHERE tgname = 'audit_logs_no_update_delete'`).Scan(&trigger); err != nil {
		t.Fatalf("check trigger: %v", err)
	}
	if trigger != 1 {
		t.Fatal("audit_logs_no_update_delete trigger is missing")
	}
}

func TestMigrationDownThenUp(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", testDatabaseURL())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.DownToContext(ctx, db, ".", 0); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up after down: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='audit_logs'`).Scan(&n); err != nil {
		t.Fatalf("check audit_logs: %v", err)
	}
	if n != 1 {
		t.Fatal("audit_logs missing after down/up")
	}
}

func TestAuditLogsAreImmutable(t *testing.T) {
	ctx := context.Background()
	cleanDB(t)
	hash := strings.Repeat("a", 64)
	if _, err := testDB.pool.Exec(ctx, `
		INSERT INTO audit_logs (actor_type, action, prev_hash, hash)
		VALUES ('system', 'test.immutable', $1, $2)`,
		strings.Repeat("0", 64), hash); err != nil {
		t.Fatalf("insert audit row: %v", err)
	}
	if _, err := testDB.pool.Exec(ctx, `UPDATE audit_logs SET action = 'changed'`); err == nil {
		t.Fatal("UPDATE audit_logs succeeded; expected append-only error")
	}
	if _, err := testDB.pool.Exec(ctx, `DELETE FROM audit_logs`); err == nil {
		t.Fatal("DELETE audit_logs succeeded; expected append-only error")
	}
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	url := testDatabaseURL()

	lockDB, err := sql.Open("pgx", url)
	if err != nil {
		fail("open test database: %v", err)
	}
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		fail("connect test database: %v", err)
	}
	if err := conn.PingContext(ctx); err != nil {
		fail("ping test database (%s): %v", url, err)
	}
	const lockID int64 = 0x4f50534d
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		fail("acquire advisory lock: %v", err)
	}

	st, err := Open(ctx, url, slog.Default())
	if err != nil {
		fail("open store: %v", err)
	}
	testDB = st

	code := m.Run()

	_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID)
	_ = conn.Close()
	_ = lockDB.Close()
	st.Close()
	os.Exit(code)
}

func fail(format string, args ...any) {
	_, _ = os.Stderr.WriteString("store integration test setup failed: " + fmt.Sprintf(format, args...) + "\n")
	os.Exit(1)
}
