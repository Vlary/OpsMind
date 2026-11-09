// W4：rdp_sessions 表集成测试（契约 9.2 G22：CRUD、Close 幂等冲突、
// CountActive、服务器删除级联、迁移后约束生效）。使用真实 PostgreSQL
// （TEST_DATABASE_URL）；包级 TestMain（migrate_test.go）持有 advisory lock。
package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

// cleanRdpTables 截断本测试涉及的表（rdp_sessions 依赖 servers/users 级联）。
func cleanRdpTables(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE rdp_sessions, servers, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func mustRdpServer(t *testing.T, name string) store.Server {
	t.Helper()
	s, err := testDB.Servers().Create(context.Background(), store.CreateServerParams{
		Name:     name,
		OS:       store.ServerOSWindows,
		Host:     "127.0.0.1",
		Port:     3389,
		Username: "Administrator",
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

func TestRdpSessionsCRUDAndLifecycle(t *testing.T) {
	cleanRdpTables(t)
	ctx := context.Background()
	user := mustUser(t)
	server := mustRdpServer(t, "rdp-host")

	// Create：JOIN 装配 server_name；分辨率落列。
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, err := testDB.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID:  server.ID,
		UserID:    user.ID,
		Width:     1600,
		Height:    900,
		DPI:       120,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Status != store.SessionActive || created.EndedReason != "" || created.HasRecording {
		t.Fatalf("unexpected initial row: %+v", created)
	}
	if created.ServerName != "rdp-host" {
		t.Fatalf("JOIN field wrong: %+v", created)
	}
	if created.Width != 1600 || created.Height != 900 || created.DPI != 120 {
		t.Fatalf("resolution not stored: %+v", created)
	}

	// Get：存在 / 非法 uuid（ErrInvalid）/ 不存在（ErrNotFound）。
	got, err := testDB.RdpSessions().Get(ctx, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := testDB.RdpSessions().Get(ctx, "not-a-uuid"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Get invalid id = %v, want ErrInvalid", err)
	}
	if _, err := testDB.RdpSessions().Get(ctx, "0f0e0d0c-0b0a-4909-8807-060504030201"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}

	// CountActive / CountActiveByServer。
	if n, err := testDB.RdpSessions().CountActive(ctx); err != nil || n != 1 {
		t.Fatalf("CountActive = %d, %v", n, err)
	}
	if n, err := testDB.RdpSessions().CountActiveByServer(ctx, server.ID); err != nil || n != 1 {
		t.Fatalf("CountActiveByServer = %d, %v", n, err)
	}

	// TouchActivity：active 行生效。
	touched := now.Add(time.Minute)
	if err := testDB.RdpSessions().TouchActivity(ctx, created.ID, touched, 8192); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}
	got, _ = testDB.RdpSessions().Get(ctx, created.ID)
	if !got.LastActivityAt.Equal(touched) || got.BytesWritten != 8192 {
		t.Fatalf("TouchActivity not persisted: %+v", got)
	}

	// List：status 过滤 + JOIN。
	active, err := testDB.RdpSessions().List(ctx, store.InteractiveSessionFilter{
		Status: store.SessionActive, Limit: 50,
	})
	if err != nil || len(active) != 1 {
		t.Fatalf("List active = %d, %v", len(active), err)
	}
	if active[0].ServerName != "rdp-host" {
		t.Fatalf("List JOIN wrong: %+v", active[0])
	}

	// Close：active→closed；bytes>0 派生 recording_file=<id>.guac。
	ended := now.Add(2 * time.Minute)
	closed, err := testDB.RdpSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "client_closed", EndedAt: ended, BytesWritten: 8192,
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
	if !closed.HasRecording || closed.RecordingFile != created.ID+".guac" {
		t.Fatalf("recording_file derivation wrong: %+v", closed)
	}

	// Close 幂等冲突：再次 Close → ErrConflict；不存在 → ErrNotFound。
	if _, err := testDB.RdpSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "error", EndedAt: ended,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Close = %v, want ErrConflict", err)
	}
	if _, err := testDB.RdpSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: "0f0e0d0c-0b0a-4909-8807-060504030201", Reason: "error", EndedAt: ended,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Close missing = %v, want ErrNotFound", err)
	}

	// closed 行：CountActive 归零；TouchActivity 0 行不报错；List 过滤。
	if n, err := testDB.RdpSessions().CountActive(ctx); err != nil || n != 0 {
		t.Fatalf("CountActive after close = %d, %v", n, err)
	}
	if err := testDB.RdpSessions().TouchActivity(ctx, created.ID, ended, 1); err != nil {
		t.Fatalf("TouchActivity on closed should be no-op, got %v", err)
	}
	closedList, err := testDB.RdpSessions().List(ctx, store.InteractiveSessionFilter{
		Status: store.SessionClosed, Limit: 50,
	})
	if err != nil || len(closedList) != 1 {
		t.Fatalf("List closed = %d, %v", len(closedList), err)
	}

	// 服务器删除 → rdp_sessions 级联消失（外键 ON DELETE CASCADE 兜底）。
	if err := testDB.Servers().Delete(ctx, server.ID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	if _, err := testDB.RdpSessions().Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session row should be cascaded away: %v", err)
	}
}

func TestRdpSessionsZeroBytesKeepsRecordingEmpty(t *testing.T) {
	cleanRdpTables(t)
	ctx := context.Background()
	user := mustUser(t)
	server := mustRdpServer(t, "rdp-host-2")

	created, err := testDB.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID: server.ID, UserID: user.ID, Width: 1280, Height: 800, DPI: 96,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	closed, err := testDB.RdpSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "error", EndedAt: time.Now().UTC(), BytesWritten: 0,
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.HasRecording || closed.RecordingFile != "" {
		t.Fatalf("zero bytes must keep recording_file empty: %+v", closed)
	}
}

func TestRdpSessionsConstraints(t *testing.T) {
	cleanRdpTables(t)
	ctx := context.Background()
	user := mustUser(t)
	server := mustRdpServer(t, "rdp-host-3")

	// 分辨率越界（width<640）：CHECK 拒绝。
	if _, err := testDB.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID: server.ID, UserID: user.ID, Width: 100, Height: 800, DPI: 96,
		StartedAt: time.Now().UTC(),
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("width out of range = %v, want ErrInvalid", err)
	}
	// height>2160。
	if _, err := testDB.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID: server.ID, UserID: user.ID, Width: 1280, Height: 9999, DPI: 96,
		StartedAt: time.Now().UTC(),
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("height out of range = %v, want ErrInvalid", err)
	}
	// dpi>120。
	if _, err := testDB.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID: server.ID, UserID: user.ID, Width: 1280, Height: 800, DPI: 300,
		StartedAt: time.Now().UTC(),
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("dpi out of range = %v, want ErrInvalid", err)
	}
	// ended_reason 非法枚举：经 Close 更新路径写入 CHECK 拒绝。
	created, err := testDB.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID: server.ID, UserID: user.ID, Width: 1280, Height: 800, DPI: 96,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := testDB.RdpSessions().Close(ctx, store.CloseInteractiveSessionParams{
		ID: created.ID, Reason: "bogus_reason", EndedAt: time.Now().UTC(),
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("invalid ended_reason = %v, want ErrInvalid", err)
	}
}
