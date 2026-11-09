package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/store"
)

func TestHashChain(t *testing.T) {
	st := newMemStore()
	w := NewWriter(st, testLogger(), fixedClock())
	w.Start(context.Background())

	const n = 150
	for i := 0; i < n; i++ {
		if err := w.Enqueue(context.Background(), Event{
			ActorType:    "user",
			ActorID:      "user-1",
			Action:       "server.create",
			ResourceType: "server",
			ResourceID:   "srv-1",
			RequestID:    "req-1",
			IP:           "127.0.0.1",
			Detail:       map[string]any{"index": i},
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := st.rowsSnapshot()
	if len(rows) != n {
		t.Fatalf("rows = %d, want %d", len(rows), n)
	}
	prev := zeroHash
	for i, row := range rows {
		if row.PrevHash != prev {
			t.Fatalf("row %d prev_hash = %q, want %q", i, row.PrevHash, prev)
		}
		recomputed := computeHash(store.CreateAuditParams{
			CreatedAt:      row.CreatedAt,
			ActorType:      row.ActorType,
			ActorID:        row.ActorID,
			Action:         row.Action,
			ResourceType:   row.ResourceType,
			ResourceID:     row.ResourceID,
			RequestID:      row.RequestID,
			ConversationID: row.ConversationID,
			ServerID:       row.ServerID,
			IP:             row.IP,
			Detail:         row.Detail,
			PrevHash:       row.PrevHash,
		})
		if recomputed != row.Hash {
			t.Fatalf("row %d hash mismatch: %q vs %q", i, recomputed, row.Hash)
		}
		prev = row.Hash
	}
}

func TestWriterResumesChainAfterRestart(t *testing.T) {
	st := newMemStore()
	first := NewWriter(st, testLogger(), fixedClock())
	first.Start(context.Background())
	if err := first.Enqueue(context.Background(), Event{Action: "auth.login_succeeded", Detail: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := st.rowsSnapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}

	second := NewWriter(st, testLogger(), fixedClock())
	second.Start(context.Background())
	if err := second.Enqueue(context.Background(), Event{Action: "auth.logout", Detail: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows = st.rowsSnapshot()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[1].PrevHash != rows[0].Hash {
		t.Fatalf("chain did not resume: prev = %q, want %q", rows[1].PrevHash, rows[0].Hash)
	}
}

func TestDetailRedaction(t *testing.T) {
	st := newMemStore()
	w := NewWriter(st, testLogger(), fixedClock())
	w.Start(context.Background())
	err := w.Enqueue(context.Background(), Event{
		Action: "server.update",
		Detail: map[string]any{
			"password": "super-secret",
			"nested": map[string]any{
				"api_key": "sk-live-123",
			},
			"note": "token=abc123 ok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := st.rowsSnapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	detail := string(rows[0].Detail)
	for _, secret := range []string{"super-secret", "sk-live-123", "token=abc123"} {
		if strings.Contains(detail, secret) {
			t.Fatalf("detail leaked %q: %s", secret, detail)
		}
	}
	if !strings.Contains(detail, "[REDACTED]") {
		t.Fatalf("detail was not redacted: %s", detail)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rows[0].Detail, &parsed); err != nil {
		t.Fatalf("detail is not valid JSON: %v", err)
	}
}

func TestDetailTruncation(t *testing.T) {
	st := newMemStore()
	w := NewWriter(st, testLogger(), fixedClock())
	w.Start(context.Background())
	if err := w.Enqueue(context.Background(), Event{
		Action: "chat.finished",
		Detail: map[string]any{"blob": strings.Repeat("x", 100*1024)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := st.rowsSnapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if len(rows[0].Detail) > maxDetailBytes {
		t.Fatalf("detail size = %d, want <= %d", len(rows[0].Detail), maxDetailBytes)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rows[0].Detail, &parsed); err != nil {
		t.Fatalf("truncated detail is not valid JSON: %v", err)
	}
}

func TestRetryKeepsBatch(t *testing.T) {
	st := newMemStore()
	st.failNext = 1
	w := NewWriter(st, testLogger(), fixedClock())
	w.Start(context.Background())
	if err := w.Enqueue(context.Background(), Event{Action: "auth.logout", Detail: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("Flush after transient failure: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(st.rowsSnapshot()) != 1 {
		t.Fatalf("rows = %d, want 1", len(st.rowsSnapshot()))
	}
}

func TestBatchTriggers(t *testing.T) {
	st := newMemStore()
	w := NewWriter(st, testLogger(), fixedClock())
	w.Start(context.Background())
	defer func() { _ = w.Close(context.Background()) }()

	// 100 条应立即触发一次批量写入，不依赖 500ms 定时器。
	for i := 0; i < 100; i++ {
		if err := w.Enqueue(context.Background(), Event{Action: "tool.call", Detail: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	waitForRows(t, st, 100, 2*time.Second)

	// 单条事件由 500ms 定时器落库。
	if err := w.Enqueue(context.Background(), Event{Action: "tool.result"}); err != nil {
		t.Fatal(err)
	}
	waitForRows(t, st, 101, 2*time.Second)
}

func waitForRows(t *testing.T, st *memStore, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(st.rowsSnapshot()) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("rows = %d after %s, want %d", len(st.rowsSnapshot()), timeout, want)
}

func TestEnqueueAfterClose(t *testing.T) {
	st := newMemStore()
	w := NewWriter(st, testLogger(), fixedClock())
	w.Start(context.Background())
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Enqueue(context.Background(), Event{Action: "auth.logout"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after Close = %v, want ErrClosed", err)
	}
}

func TestLastHashEmpty(t *testing.T) {
	st := newMemStore()
	got, err := st.AuditLogs().LastHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != zeroHash {
		t.Fatalf("LastHash = %q, want 64 zeros", got)
	}
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---- in-memory audit store ------------------------------------------------

type memStore struct {
	store.Store
	mu       sync.Mutex
	rows     []store.AuditLog
	nextID   int64
	failNext int
}

func newMemStore() *memStore { return &memStore{} }

func (m *memStore) WithTx(_ context.Context, fn func(store.Store) error) error { return fn(m) }
func (m *memStore) AuditLogs() store.AuditStore                                { return memAudit{m} }

func (m *memStore) rowsSnapshot() []store.AuditLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.AuditLog, len(m.rows))
	copy(out, m.rows)
	return out
}

type memAudit struct{ m *memStore }

func (a memAudit) Append(_ context.Context, p store.CreateAuditParams) error {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	if a.m.failNext > 0 {
		a.m.failNext--
		return errors.New("injected append failure")
	}
	a.m.nextID++
	a.m.rows = append(a.m.rows, store.AuditLog{
		ID:             a.m.nextID,
		CreatedAt:      p.CreatedAt,
		ActorType:      p.ActorType,
		ActorID:        p.ActorID,
		Action:         p.Action,
		ResourceType:   p.ResourceType,
		ResourceID:     p.ResourceID,
		RequestID:      p.RequestID,
		ConversationID: p.ConversationID,
		ServerID:       p.ServerID,
		IP:             p.IP,
		Detail:         p.Detail,
		PrevHash:       p.PrevHash,
		Hash:           p.Hash,
	})
	return nil
}

func (a memAudit) LastHash(context.Context) (string, error) {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	if len(a.m.rows) == 0 {
		return zeroHash, nil
	}
	return a.m.rows[len(a.m.rows)-1].Hash, nil
}

func (a memAudit) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	return nil, false, nil
}
