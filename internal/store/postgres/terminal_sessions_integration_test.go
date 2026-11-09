// W3：terminal_sessions 表集成测试（契约 9.2 G22：CRUD、Close 幂等冲突、
// CountActive、服务器删除级联、约束生效）。使用真实 PostgreSQL
// （TEST_DATABASE_URL）；包级 TestMain（migrate_test.go）持有 advisory lock。
package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

// cleanTermTables 截断本测试涉及的表（terminal_sessions 依赖 servers/users
// 级联）。不修改共享的 cleanDB（BE1 文件）。
func cleanTermTables(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE terminal_sessions, servers, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func mustTermServer(t *testing.T, name string) store.Server {
	t.Helper()
	s, err := testDB.Servers().Create(context.Background(), store.CreateServerParams{
		Name:     name,
		OS:       store.ServerOSLinux,
		Host:     "127.0.0.1",
		Port:     22,
		Username: "root",
		Credentials: store.ServerCredentials{
			AuthMethod:        store.AuthMethodPassword,
			PasswordEncrypted: "v1:test",
		},
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	return s
}

func TestTerminalSessionsCRUDAndLifecycle(t *testing.T) {
	cleanTermTables(t)
	ctx := context.Background()
	user := mustUser(t)
	server := mustTermServer(t, "term-host")

	// Create：JOIN 装配 server_name/os/username。
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, err := testDB.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
		ServerID:  server.ID,
		UserID:    user.ID,
		Cols:      132,
		Rows:      43,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Status != store.SessionActive || created.EndedReason != "" || created.HasRecording {
		t.Fatalf("unexpected initial row: %+v", created)
	}
	if created.ServerName != "term-host" || created.ServerOS != store.ServerOSLinux || created.Username != "admin" {
		t.Fatalf("JOIN fields wrong: %+v", created)
	}
	if created.Cols != 132 || created.Rows != 43 {
		t.Fatalf("cols/rows not stored: %+v", created)
	}

	// Get。
	got, err := testDB.TerminalSessions().Get(ctx, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	// 非法 uuid：mapError 映射为 ErrInvalid（调用方均已先做 uuid 校验）。
	if _, err := testDB.TerminalSessions().Get(ctx, "not-a-uuid"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Get invalid id = %v, want ErrInvalid", err)
	}
	// 合法但不存在的 uuid：ErrNotFound。
	if _, err := testDB.TerminalSessions().Get(ctx, "0f0e0d0c-0b0a-4909-8807-060504030201"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}

	// CountActive / CountActiveByServer。
	if n, err := testDB.TerminalSessions().CountActive(ctx); err != nil || n != 1 {
		t.Fatalf("CountActive = %d, %v", n, err)
	}
	if n, err := testDB.TerminalSessions().CountActiveByServer(ctx, server.ID); err != nil || n != 1 {
		t.Fatalf("CountActiveByServer = %d, %v", n, err)
	}

	// TouchActivity：active 行生效；closed 行 0 行不报错（收尾后验证）。
	touched := now.Add(time.Minute)
	if err := testDB.TerminalSessions().TouchActivity(ctx, created.ID, touched, 4096); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}
	got, _ = testDB.TerminalSessions().Get(ctx, created.ID)
	if !got.LastActivityAt.Equal(touched) || got.BytesWritten != 4096 {
		t.Fatalf("TouchActivity not persisted: %+v", got)
	}

	// List：status 过滤 + JOIN。
	active, err := testDB.TerminalSessions().List(ctx, store.InteractiveSessionFilter{
		Status: store.SessionActive, Limit: 50,
	})
	if err != nil || len(active) != 1 {
		t.Fatalf("List active = %d, %v", len(active), err)
	}
	if active[0].ServerName != "term-host" || active[0].Username != "admin" {
		t.Fatalf("List JOIN wrong: %+v", active[0])
	}

	// Close：active→closed；bytes>0 派生 recording_file=<id>.cast。
	ended := now.Add(2 * time.Minute)
	closed, err := testDB.TerminalSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "client_closed", EndedAt: ended, BytesWritten: 4096,
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.Status != store.SessionClosed || closed.EndedReason != "client_closed" {
		t.Fatalf("unexpected closed row: %+v", closed)
	}
	if closed.EndedAt == nil || !closed.EndedAt.Equal(ended) {
		t.Fatalf("ended_at wrong: %+v", closed)
	}
	if !closed.HasRecording || closed.RecordingFile != created.ID+".cast" {
		t.Fatalf("recording_file derivation wrong: %+v", closed)
	}

	// Close 幂等冲突：再次 Close → ErrConflict；不存在 → ErrNotFound。
	if _, err := testDB.TerminalSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "error", EndedAt: ended,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Close = %v, want ErrConflict", err)
	}
	if _, err := testDB.TerminalSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: "0f0e0d0c-0b0a-4909-8807-060504030201", Reason: "error", EndedAt: ended,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Close missing = %v, want ErrNotFound", err)
	}

	// closed 行：CountActive 归零；TouchActivity 0 行不报错；List 过滤。
	if n, err := testDB.TerminalSessions().CountActive(ctx); err != nil || n != 0 {
		t.Fatalf("CountActive after close = %d, %v", n, err)
	}
	if err := testDB.TerminalSessions().TouchActivity(ctx, created.ID, ended, 1); err != nil {
		t.Fatalf("TouchActivity on closed should be no-op, got %v", err)
	}
	closedList, err := testDB.TerminalSessions().List(ctx, store.InteractiveSessionFilter{
		Status: store.SessionClosed, Limit: 50,
	})
	if err != nil || len(closedList) != 1 {
		t.Fatalf("List closed = %d, %v", len(closedList), err)
	}

	// 服务器删除 → terminal_sessions 级联消失（外键 ON DELETE CASCADE 兜底）。
	if err := testDB.Servers().Delete(ctx, server.ID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	if _, err := testDB.TerminalSessions().Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session row should be cascaded away: %v", err)
	}
}

func TestTerminalSessionsZeroBytesKeepsRecordingEmpty(t *testing.T) {
	cleanTermTables(t)
	ctx := context.Background()
	user := mustUser(t)
	server := mustTermServer(t, "term-host-2")

	created, err := testDB.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
		ServerID: server.ID, UserID: user.ID, Cols: 120, Rows: 40,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	closed, err := testDB.TerminalSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "error", EndedAt: time.Now().UTC(), BytesWritten: 0,
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.HasRecording || closed.RecordingFile != "" {
		t.Fatalf("zero bytes must keep recording_file empty: %+v", closed)
	}
}

func TestTerminalSessionsConstraints(t *testing.T) {
	cleanTermTables(t)
	ctx := context.Background()
	user := mustUser(t)
	server := mustTermServer(t, "term-host-3")

	// cols 越界（<10）：CHECK 拒绝。
	if _, err := testDB.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
		ServerID: server.ID, UserID: user.ID, Cols: 9, Rows: 40,
	}); err == nil {
		t.Fatal("cols=9 should violate CHECK constraint")
	}
	// 不存在的 server FK。
	if _, err := testDB.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
		ServerID: "0f0e0d0c-0b0a-4909-8807-060504030201", UserID: user.ID, Cols: 120, Rows: 40,
	}); err == nil {
		t.Fatal("nonexistent server should violate FK")
	}
	// 非法 ended_reason。
	created, err := testDB.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
		ServerID: server.ID, UserID: user.ID, Cols: 120, Rows: 40,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := testDB.pool.Exec(ctx,
		`UPDATE terminal_sessions SET ended_reason='bogus' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("invalid ended_reason should violate CHECK constraint")
	}
}
