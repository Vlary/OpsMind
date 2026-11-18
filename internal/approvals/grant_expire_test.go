// grant_expire_test.go 覆盖走查第 3 轮 medium：已批准但已过 grant_expires_at
// 的终端/RDP 授权必须在 sweeper 里收敛为 expired，并写
// terminal.grant_expired 审计（此前只有前端按时间戳临时推算「已过期」，
// 库里状态永远是 approved，审计页该筛选恒空）。
package approvals

import (
	"context"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// grantExpiryRepo 在 fakeApprovals 上补 GrantExpiryStore：只收敛
// approved + 未消费 + grant_expires_at <= now 的 terminal 行。
type grantExpiryRepo struct {
	*fakeApprovals
}

func (g grantExpiryRepo) ExpireStaleGrants(_ context.Context, now time.Time, limit int) ([]store.Approval, error) {
	if limit <= 0 {
		limit = 200
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]store.Approval, 0, limit)
	for id, ap := range g.items {
		if len(out) >= limit {
			break
		}
		if ap.Source != "terminal" || ap.Status != store.ApprovalApproved || ap.ConsumedAt != nil {
			continue
		}
		if ap.GrantExpiresAt == nil || ap.GrantExpiresAt.After(now) {
			continue
		}
		t := now.UTC()
		ap.Status = store.ApprovalExpired
		ap.Note = "grant_expired"
		ap.DecidedAt = &t
		g.items[id] = ap
		out = append(out, ap)
	}
	return out, nil
}

// grantStore 让 ApprovalStore 同时具备 GrantExpiryStore。
type grantStore struct {
	store.Store
	approvals *grantExpiryRepo
	recorder  *auditRecorder
}

func (s *grantStore) Approvals() store.ApprovalStore { return s.approvals }
func (s *grantStore) AuditLogs() store.AuditStore    { return s.recorder }
func (s *grantStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(s)
}

func newGrantService(t *testing.T) (*Service, *grantExpiryRepo, *auditRecorder) {
	t.Helper()
	repo := grantExpiryRepo{newFakeApprovals()}
	rec := &auditRecorder{}
	st := &grantStore{approvals: &repo, recorder: rec}
	w := audit.NewWriter(st, nil, time.Now)
	w.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = w.Close(ctx)
	})
	return NewService(st, w, time.Minute, nil, nil), &repo, rec
}

func TestExpireStaleGrantsWritesAudit(t *testing.T) {
	svc, repo, rec := newGrantService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	kind := "term"
	repo.mu.Lock()
	repo.items["ap-stale"] = store.Approval{
		ID: "ap-stale", Source: "terminal", Status: store.ApprovalApproved,
		GrantKind: &kind, GrantExpiresAt: &past, RequestedBy: "requester-1",
	}
	repo.items["ap-fresh"] = store.Approval{
		ID: "ap-fresh", Source: "terminal", Status: store.ApprovalApproved,
		GrantKind: &kind, GrantExpiresAt: &future, RequestedBy: "requester-1",
	}
	repo.mu.Unlock()

	if _, err := svc.Expire(ctx, now); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	flushAudit(t, svc)

	cur, err := svc.Get(ctx, "ap-stale")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != store.ApprovalExpired || cur.Note != "grant_expired" {
		t.Fatalf("stale grant = %s / %q, want expired / grant_expired", cur.Status, cur.Note)
	}
	fresh, err := svc.Get(ctx, "ap-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != store.ApprovalApproved {
		t.Fatalf("fresh grant = %s, want approved", fresh.Status)
	}

	rows := rec.byAction(actionGrantExpired)
	if len(rows) != 1 {
		t.Fatalf("terminal.grant_expired rows = %d, want 1", len(rows))
	}
	if rows[0].ResourceID != "ap-stale" || rows[0].ResourceType != "approval" {
		t.Fatalf("grant expired row = %+v", rows[0])
	}
	detail := detailMap(t, rows[0].Detail)
	if detail["approval_id"] != "ap-stale" || detail["reason"] != "grant_expired" {
		t.Fatalf("detail = %v", detail)
	}
	if detail["grant_expires_at"] == "" {
		t.Fatalf("detail must carry grant_expires_at: %v", detail)
	}
	// 第二轮幂等：没有新的过期行。
	if _, err := svc.Expire(ctx, now); err != nil {
		t.Fatalf("second Expire: %v", err)
	}
	flushAudit(t, svc)
	if rows := rec.byAction(actionGrantExpired); len(rows) != 1 {
		t.Fatalf("second sweep wrote %d grant_expired rows, want 1 total", len(rows))
	}
}
