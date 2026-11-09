package approvals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// fakeApprovals 是进程内审批仓储。
type fakeApprovals struct {
	mu    sync.Mutex
	items map[string]store.Approval
	next  int
}

func newFakeApprovals() *fakeApprovals {
	return &fakeApprovals{items: make(map[string]store.Approval)}
}

func (f *fakeApprovals) Create(_ context.Context, p store.CreateApprovalParams) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.items {
		if p.ToolCallID != nil && a.ToolCallID != nil && *a.ToolCallID == *p.ToolCallID {
			return store.Approval{}, store.ErrConflict
		}
	}
	f.next++
	ap := store.Approval{
		ID:             fmt.Sprintf("ap-%d", f.next),
		ConversationID: p.ConversationID,
		ToolCallID:     p.ToolCallID,
		// Source/RuleID/ApprovalMode 生产实现由仓储原样落库（postgres
		// approvals.Create）；测试桩此前漏拷 Source，导致 source=batch
		// 的审计路径无法在单测里覆盖（六期打磨 sprint 5 补齐）。
		Source:       p.Source,
		ServerID:     p.ServerID,
		ServerName:   p.ServerName,
		Command:      p.Command,
		RiskLevel:    p.RiskLevel,
		RuleID:       p.RuleID,
		Reason:       p.Reason,
		ApprovalMode: store.ApprovalMode(p.ApprovalMode),
		Status:       store.ApprovalPending,
		RequestedAt:  p.CreatedAt,
		ExpiresAt:    p.ExpiresAt,
	}
	f.items[ap.ID] = ap
	return ap, nil
}

func (f *fakeApprovals) Get(_ context.Context, id string) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.items[id]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	return ap, nil
}

func (f *fakeApprovals) List(_ context.Context, conversationID string, status store.ApprovalStatus, limit int) ([]store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Approval, 0, len(f.items))
	for _, ap := range f.items {
		// 四期 C1：console 审批 ConversationID 为 nil——按 conversation 过滤
		// 时（conversationID != ""）不匹配即排除（契约 3.4）。
		if conversationID != "" && (ap.ConversationID == nil || *ap.ConversationID != conversationID) {
			continue
		}
		if status != "" && ap.Status != status {
			continue
		}
		out = append(out, ap)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeApprovals) Decide(_ context.Context, p store.DecideApprovalParams) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.items[p.ID]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	if ap.Status != store.ApprovalPending {
		return store.Approval{}, store.ErrConflict
	}
	t := p.DecidedAt.UTC()
	ap.Status = p.Status
	ap.DecidedAt = &t
	ap.Note = p.Note
	if p.DecidedBy != "" {
		by := p.DecidedBy
		ap.DecidedBy = &by
	}
	ap.EditedCommand = p.EditedCommand
	ap.EditedRiskLevel = p.EditedRiskLevel
	ap.EditedRuleID = p.EditedRuleID
	f.items[p.ID] = ap
	return ap, nil
}

func (f *fakeApprovals) Expire(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, ap := range f.items {
		if ap.Status == store.ApprovalPending && !ap.ExpiresAt.After(now) {
			t := now.UTC()
			ap.Status = store.ApprovalExpired
			ap.DecidedAt = &t
			f.items[id] = ap
			n++
		}
	}
	return n, nil
}

type fakeStore struct {
	store.Store
	approvals *fakeApprovals
}

func (f *fakeStore) Approvals() store.ApprovalStore { return f.approvals }

func newTestService(t *testing.T, ttl time.Duration) (*Service, *fakeApprovals) {
	t.Helper()
	ap := newFakeApprovals()
	st := &fakeStore{approvals: ap}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := audit.NewWriter(nil, log, time.Now)
	svc := NewService(st, w, ttl, nil, log)
	return svc, ap
}

func createReq(toolCall string) CreateRequest {
	convID := "conv-1"
	return CreateRequest{
		ConversationID: &convID,
		ToolCallID:     &toolCall,
		ServerName:     "web-1",
		Command:        "systemctl restart nginx",
		RiskLevel:      "needs_approval",
		Reason:         "service change",
		RequestID:      "req-1",
	}
}

func TestCreatePending(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	now := time.Now()
	ap, err := svc.Create(context.Background(), createReq("tc-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ap.Status != store.ApprovalPending {
		t.Fatalf("status = %q", ap.Status)
	}
	if d := ap.ExpiresAt.Sub(ap.RequestedAt); d != time.Minute {
		t.Fatalf("ttl = %v, want 1m", d)
	}
	if ap.ExpiresAt.Before(now) {
		t.Fatal("expires_at should be in the future")
	}

	if _, err := svc.Create(context.Background(), createReq("tc-1")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate tool_call error = %v, want ErrConflict", err)
	}
}

func TestDecideApproveAndDeny(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()

	ap, _ := svc.Create(ctx, createReq("tc-a"))
	got, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1", Note: "looks good", RequestID: "req-2"})
	if err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	if got.Status != store.ApprovalApproved || got.DecidedBy == nil || *got.DecidedBy != "user-1" {
		t.Fatalf("approved = %+v", got)
	}
	if got.DecidedAt == nil || got.Note != "looks good" {
		t.Fatalf("decision metadata = %+v", got)
	}

	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: false, DecidedBy: "user-1", Note: "", RequestID: "req-3"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second decide error = %v, want ErrConflict", err)
	}
	if _, err := svc.Cancel(ctx, ap.ID, cancelReasonStream, "req-4"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cancel decided error = %v, want ErrConflict", err)
	}

	ap2, _ := svc.Create(ctx, createReq("tc-b"))
	denied, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap2.ID, Approve: false, DecidedBy: "user-1", Note: "no", RequestID: ""})
	if err != nil {
		t.Fatalf("Decide deny: %v", err)
	}
	if denied.Status != store.ApprovalDenied {
		t.Fatalf("denied status = %q", denied.Status)
	}
}

func TestWaitReceivesDecision(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()

	ap, _ := svc.Create(ctx, createReq("tc-wait"))

	results := make(chan Decision, 1)
	errs := make(chan error, 1)
	go func() {
		d, err := svc.Wait(ctx, ap.ID)
		results <- d
		errs <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1", Note: "ok", RequestID: ""}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	select {
	case d := <-results:
		if err := <-errs; err != nil {
			t.Fatalf("Wait error: %v", err)
		}
		if d.Status != store.ApprovalApproved {
			t.Fatalf("Wait status = %q", d.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Decide")
	}
}

// 同一审批被并发 Wait 两次时，Decide 必须通知所有等待者，
// 而不是只通知最后注册的 channel（回归：后注册者曾覆盖先注册者）。
func TestWaitConcurrentWaitersBothNotified(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()

	ap, _ := svc.Create(ctx, createReq("tc-multi"))

	const waiters = 3
	results := make(chan Decision, waiters)
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			d, err := svc.Wait(ctx, ap.ID)
			results <- d
			errs <- err
		}()
	}

	time.Sleep(50 * time.Millisecond)
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1", Note: "ok", RequestID: ""}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	for i := 0; i < waiters; i++ {
		select {
		case d := <-results:
			if err := <-errs; err != nil {
				t.Fatalf("Wait error: %v", err)
			}
			if d.Status != store.ApprovalApproved {
				t.Fatalf("waiter %d status = %q, want approved", i, d.Status)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d was not notified by Decide (waiter channel overwritten?)", i)
		}
	}
}

func TestWaitAfterDecisionReturnsImmediately(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()

	ap, _ := svc.Create(ctx, createReq("tc-late"))
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: false, DecidedBy: "user-1", Note: "", RequestID: ""}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	start := time.Now()
	d, err := svc.Wait(ctx, ap.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if d.Status != store.ApprovalDenied {
		t.Fatalf("status = %q, want denied", d.Status)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Wait took %v, should be immediate", time.Since(start))
	}
}

func TestWaitTimeoutExpires(t *testing.T) {
	svc, apStore := newTestService(t, 40*time.Millisecond)
	ctx := context.Background()

	ap, _ := svc.Create(ctx, createReq("tc-timeout"))
	d, err := svc.Wait(ctx, ap.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if d.Status != store.ApprovalExpired {
		t.Fatalf("status = %q, want expired", d.Status)
	}
	stored, _ := apStore.Get(ctx, ap.ID)
	if stored.Status != store.ApprovalExpired {
		t.Fatalf("stored status = %q, want expired", stored.Status)
	}
}

func TestWaitContextCancelKeepsPending(t *testing.T) {
	svc, apStore := newTestService(t, time.Minute)

	ap, _ := svc.Create(context.Background(), createReq("tc-cancel"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := svc.Wait(ctx, ap.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error = %v, want DeadlineExceeded", err)
	}
	stored, _ := apStore.Get(context.Background(), ap.ID)
	if stored.Status != store.ApprovalPending {
		t.Fatalf("ctx cancel must not change DB status, got %q", stored.Status)
	}
}

func TestCancelNotifiesWaiter(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()

	ap, _ := svc.Create(ctx, createReq("tc-stream"))

	ch := make(chan Decision, 1)
	go func() {
		d, _ := svc.Wait(ctx, ap.ID)
		ch <- d
	}()
	time.Sleep(20 * time.Millisecond)

	cancelled, err := svc.Cancel(ctx, ap.ID, cancelReasonStream, "req-x")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.Status != store.ApprovalExpired || cancelled.Note != cancelReasonStream {
		t.Fatalf("cancelled = %+v", cancelled)
	}

	select {
	case d := <-ch:
		if d.Status != store.ApprovalExpired || d.Note != cancelReasonStream {
			t.Fatalf("waiter decision = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not notified by Cancel")
	}

	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1", Note: "", RequestID: ""}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("decide after cancel error = %v, want ErrConflict", err)
	}
}

func TestExpireBulk(t *testing.T) {
	svc, apStore := newTestService(t, time.Minute)
	ctx := context.Background()

	a1, _ := svc.Create(ctx, createReq("tc-e1"))
	a2, _ := svc.Create(ctx, createReq("tc-e2"))

	n, err := svc.Expire(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 2 {
		t.Fatalf("expired = %d, want 2", n)
	}
	for _, id := range []string{a1.ID, a2.ID} {
		ap, _ := apStore.Get(ctx, id)
		if ap.Status != store.ApprovalExpired {
			t.Fatalf("%s status = %q", id, ap.Status)
		}
	}
}

func TestStartExpiresPendingFromPreviousRun(t *testing.T) {
	svc, apStore := newTestService(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ap, err := svc.Create(ctx, createReq("tc-restart"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc.Start(ctx)
	defer svc.Stop()

	stored, _ := apStore.Get(ctx, ap.ID)
	if stored.Status != store.ApprovalExpired {
		t.Fatalf("pending from previous run = %q, want expired", stored.Status)
	}
}

func TestStartSweeperExpires(t *testing.T) {
	svc, apStore := newTestService(t, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx)
	defer svc.Stop()

	ap, err := svc.Create(ctx, createReq("tc-sweep"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stored, _ := apStore.Get(ctx, ap.ID)
		if stored.Status == store.ApprovalExpired {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("sweeper did not expire the approval")
}

func TestGetAndNotFound(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	if _, err := svc.Get(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Wait(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Wait missing error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Decide(context.Background(), DecideRequest{ApprovalID: "missing", Approve: true, DecidedBy: "u"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Decide missing error = %v, want ErrNotFound", err)
	}
}

// sweeper 的生命周期由 Stop() 控制，不随调用方 ctx 取消：SIGTERM 后 HTTP 还有
// 排空窗口，sweeper 提前退出会让排空期内的 pending 审批失去超时。
func TestSweeperSurvivesCallerContextCancel(t *testing.T) {
	svc, apStore := newTestService(t, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	svc.Start(ctx)
	defer svc.Stop()
	cancel() // 模拟 signal ctx 结束：sweeper 必须继续工作。

	ap, err := svc.Create(context.Background(), createReq("tc-cancel"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stored, _ := apStore.Get(context.Background(), ap.ID)
		if stored.Status == store.ApprovalExpired {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("sweeper stopped after caller context cancel")
}
