// offline_test.go 覆盖主机侧离线恢复（契约 12.3 第 11/25 条）：
//
//   - ResetAdmin 置 must_change_password=true、吊销全部会话、收敛待处理
//     审批，并写 admin.offline_reset 审计；
//   - BreakGlassApprove 只批准 pending 审批、置 break_glass=true，且重复
//     批准返回 ErrConflict；
//   - 审计载荷含 reason/host_user。
package offline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/rbacops"
	"aiops/internal/store"
)

// fakeStore 是离线路径需要的最小存储（用户/会话/审批/审计/outbox）。
type fakeStore struct {
	store.Store
	mu        sync.Mutex
	users     map[string]store.User
	sessions  map[string]store.Session
	approvals map[string]store.Approval
	audits    []store.CreateAuditParams
	outbox    [][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:     map[string]store.User{},
		sessions:  map[string]store.Session{},
		approvals: map[string]store.Approval{},
	}
}

func (f *fakeStore) WithTx(_ context.Context, fn func(store.Store) error) error {
	return fn(f)
}
func (f *fakeStore) Users() store.UserStore       { return fakeUsers{f: f} }
func (f *fakeStore) Sessions() store.SessionStore { return fakeSessions{f: f} }
func (f *fakeStore) Approvals() store.ApprovalStore {
	return fakeApprovals{f: f}
}
func (f *fakeStore) AuditLogs() store.AuditStore { return fakeAudit{f: f} }

// BatchJobs 显式返回 nil：离线路径的 rbacops 会探测可选批量收敛能力，
// 嵌入的 store.Store 为 nil 时直接调用提升方法会 panic。
func (f *fakeStore) BatchJobs() store.BatchJobStore { return nil }

func (f *fakeStore) EnqueueAuditOutbox(_ context.Context, payload []byte, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outbox = append(f.outbox, append([]byte(nil), payload...))
	return nil
}
func (f *fakeStore) PendingAuditOutbox(context.Context, int) ([]store.AuditOutboxEntry, error) {
	return nil, nil
}
func (f *fakeStore) ProcessAuditOutbox(context.Context, []int64, time.Time) error { return nil }
func (f *fakeStore) LockAuditChain(context.Context) error                         { return nil }

type fakeUsers struct {
	store.UserStore
	f *fakeStore
}

func (u fakeUsers) ByUsername(_ context.Context, username string) (store.User, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	v, ok := u.f.users[username]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return v, nil
}
func (u fakeUsers) SetMustChangePassword(_ context.Context, id string, mustChange bool, at time.Time) error {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	for name, v := range u.f.users {
		if v.ID == id {
			v.MustChangePassword = mustChange
			v.UpdatedAt = at
			u.f.users[name] = v
			return nil
		}
	}
	return store.ErrNotFound
}

type fakeSessions struct {
	store.SessionStore
	f *fakeStore
}

func (s fakeSessions) DeleteByUser(_ context.Context, userID string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for id, v := range s.f.sessions {
		if v.UserID == userID {
			delete(s.f.sessions, id)
		}
	}
	return nil
}

type fakeApprovals struct {
	store.ApprovalStore
	f *fakeStore
}

func (a fakeApprovals) ExpirePendingByUser(_ context.Context, userID, note string, at time.Time) ([]store.Approval, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	expired := make([]store.Approval, 0, len(a.f.approvals))
	for id, v := range a.f.approvals {
		if v.RequestedBy == userID && v.Status == store.ApprovalPending {
			v.Status = store.ApprovalExpired
			v.Note = note
			t := at
			v.DecidedAt = &t
			a.f.approvals[id] = v
			expired = append(expired, v)
		}
	}
	return expired, nil
}
func (a fakeApprovals) ExpireApprovedGrantsByUser(context.Context, string, string, time.Time) ([]store.Approval, error) {
	return nil, nil
}
func (a fakeApprovals) ApproveBreakGlass(_ context.Context, id, note string, at time.Time) (store.Approval, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	v, ok := a.f.approvals[id]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	if v.Status != store.ApprovalPending {
		return store.Approval{}, store.ErrConflict
	}
	v.Status = store.ApprovalApproved
	v.Note = note
	v.BreakGlass = true
	t := at
	v.DecidedAt = &t
	a.f.approvals[id] = v
	return v, nil
}

type fakeAudit struct {
	store.AuditStore
	f *fakeStore
}

func (a fakeAudit) Append(_ context.Context, p store.CreateAuditParams) error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	a.f.audits = append(a.f.audits, p)
	return nil
}
func (a fakeAudit) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	return nil, false, nil
}
func (a fakeAudit) LastHash(context.Context) (string, error) { return audit.ZeroHash, nil }

func seedUser(f *fakeStore, id, username string) {
	f.users[username] = store.User{ID: id, Username: username, Role: store.RoleAdmin, Enabled: true}
	f.sessions["sess-1"] = store.Session{ID: "sess-1", UserID: id}
	f.sessions["sess-2"] = store.Session{ID: "sess-2", UserID: id}
}

func TestResetAdminForcesPasswordChangeAndRevokes(t *testing.T) {
	st := newFakeStore()
	seedUser(st, "user-1", "admin")
	st.approvals["ap-1"] = store.Approval{ID: "ap-1", RequestedBy: "user-1", Status: store.ApprovalPending}

	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	u, err := ResetAdmin(context.Background(), st, "admin", "admin locked out", now)
	if err != nil {
		t.Fatalf("ResetAdmin: %v", err)
	}
	if u.ID != "user-1" || !u.MustChangePassword {
		t.Fatalf("returned user = %+v", u)
	}
	if !st.users["admin"].MustChangePassword {
		t.Fatal("must_change_password was not set")
	}
	if len(st.sessions) != 0 {
		t.Fatalf("sessions = %d, want 0", len(st.sessions))
	}
	if st.approvals["ap-1"].Status != store.ApprovalExpired {
		t.Fatalf("pending approval = %s, want expired", st.approvals["ap-1"].Status)
	}
	// 离线路径直接追加 audit_logs 哈希链（不走 outbox：CLI 没有 worker，
	// 写 outbox 会让审计在库里一直缺席）。
	//
	// RBAC 第 3 轮 high：收敛该用户的 pending 审批也必须留痕——离线 reset
	// 与 HTTP 停用走同一套 rbacops 收敛，审计同口径为 approval.expired
	//（reason=rbac:user_revoked），因此这里是两行：
	// approval.expired + admin.offline_reset。
	if len(st.audits) != 2 {
		t.Fatalf("audit rows = %d, want 2 (approval.expired + admin.offline_reset): %+v", len(st.audits), st.audits)
	}
	var payload store.CreateAuditParams
	foundExpired := false
	for _, row := range st.audits {
		switch row.Action {
		case "approval.expired":
			foundExpired = true
			if row.ResourceID != "ap-1" || row.ActorType != "system" {
				t.Fatalf("approval.expired row = %+v", row)
			}
			var detail map[string]any
			_ = json.Unmarshal(row.Detail, &detail)
			if detail["reason"] != rbacops.NoteUserRevoked {
				t.Fatalf("approval.expired detail.reason = %v, want %s", detail["reason"], rbacops.NoteUserRevoked)
			}
		case ActionOfflineReset:
			payload = row
		}
	}
	if !foundExpired {
		t.Fatalf("approval.expired audit missing (离线收敛必须留痕): %+v", st.audits)
	}
	if payload.Action != ActionOfflineReset || payload.ActorType != "system" || payload.ResourceID != "user-1" {
		t.Fatalf("audit payload = %+v", payload)
	}
	if payload.Hash == "" || payload.PrevHash != audit.ZeroHash {
		t.Fatalf("audit row must carry hash chain fields: %+v", payload)
	}
	var detail map[string]any
	_ = json.Unmarshal(payload.Detail, &detail)
	if detail["reason"] != "admin locked out" || detail["host_user"] == "" {
		t.Fatalf("audit detail = %v", detail)
	}
}

func TestResetAdminUnknownUser(t *testing.T) {
	st := newFakeStore()
	if _, err := ResetAdmin(context.Background(), st, "ghost", "x", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestBreakGlassApproveOnce(t *testing.T) {
	st := newFakeStore()
	st.approvals["ap-1"] = store.Approval{ID: "ap-1", Status: store.ApprovalPending, Source: "terminal"}
	now := time.Now().UTC()

	ap, err := BreakGlassApprove(context.Background(), st, "ap-1", "single admin recovery", now)
	if err != nil {
		t.Fatalf("BreakGlassApprove: %v", err)
	}
	if ap.Status != store.ApprovalApproved || !ap.BreakGlass {
		t.Fatalf("approved = %+v", ap)
	}
	if _, err := BreakGlassApprove(context.Background(), st, "ap-1", "again", now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second break-glass = %v, want ErrConflict", err)
	}
	if len(st.audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(st.audits))
	}
	if st.audits[0].Action != ActionBreakGlass {
		t.Fatalf("action = %s", st.audits[0].Action)
	}
}

func TestBreakGlassRequiresReason(t *testing.T) {
	st := newFakeStore()
	st.approvals["ap-1"] = store.Approval{ID: "ap-1", Status: store.ApprovalPending}
	if _, err := BreakGlassApprove(context.Background(), st, "ap-1", "  ", time.Now()); err == nil {
		t.Fatal("missing reason must be rejected")
	}
}

// anchorStore 提供升序审计扫描（anchor 导出用）。
type anchorStore struct {
	store.Store
	rows []store.AuditLog
}

func (a *anchorStore) AuditLogs() store.AuditStore { return anchorAudit{s: a} }

type anchorAudit struct {
	store.AuditStore
	s *anchorStore
}

func (a anchorAudit) LastHash(context.Context) (string, error) { return "", nil }

func (a anchorAudit) ListAuditAsc(_ context.Context, afterID int64, limit int) ([]store.AuditLog, error) {
	out := make([]store.AuditLog, 0, limit)
	for _, row := range a.s.rows {
		if row.ID <= afterID {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func TestExportAnchorHead(t *testing.T) {
	st := &anchorStore{rows: []store.AuditLog{
		{ID: 1, Hash: "h1"},
		{ID: 2, Hash: "h2", LegacyHash: true},
		{ID: 3, Hash: "h3"},
	}}
	rec, err := ExportAnchor(context.Background(), st)
	if err != nil {
		t.Fatalf("ExportAnchor: %v", err)
	}
	if rec.HeadID != 3 || rec.HeadHash != "h3" || rec.Rows != 3 || rec.Legacy != 1 {
		t.Fatalf("anchor = %+v", rec)
	}
}

func TestWriteAnchorAppendsAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/anchor.jsonl"
	if err := WriteAnchor(out, AnchorRecord{HeadID: 1, HeadHash: "h1", Rows: 1}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteAnchor(out, AnchorRecord{HeadID: 2, HeadHash: "h2", Rows: 2}); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (append-only)", len(lines))
	}
	var rec AnchorRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.HeadID != 2 {
		t.Fatalf("second record = %+v", rec)
	}
	// 符号链接拒绝写入：避免被引导覆盖任意文件。
	link := dir + "/link.jsonl"
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnchor(link, AnchorRecord{HeadID: 3}); err == nil {
		t.Fatal("writing through a symlink must be rejected")
	}
}
