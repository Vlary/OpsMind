package approvals

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// faultApprovals 在 fakeApprovals 上注入故障：Expire/List/Decide 可按需返回错误。
type faultApprovals struct {
	*fakeApprovals

	mu        sync.Mutex
	expireErr error
	listErr   error
	decideErr map[string]error
}

func (f *faultApprovals) Expire(ctx context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	err := f.expireErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return f.fakeApprovals.Expire(ctx, now)
}

func (f *faultApprovals) List(ctx context.Context, conversationID string, status store.ApprovalStatus, limit int) ([]store.Approval, error) {
	f.mu.Lock()
	err := f.listErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.fakeApprovals.List(ctx, conversationID, status, limit)
}

func (f *faultApprovals) Decide(ctx context.Context, p store.DecideApprovalParams) (store.Approval, error) {
	f.mu.Lock()
	err := f.decideErr[p.ID]
	f.mu.Unlock()
	if err != nil {
		return store.Approval{}, err
	}
	return f.fakeApprovals.Decide(ctx, p)
}

// faultStore 让 Approvals() 返回注入了故障的实现。
type faultStore struct {
	store.Store
	ap store.ApprovalStore
}

func (f *faultStore) Approvals() store.ApprovalStore { return f.ap }

func newFaultService(t *testing.T, base *fakeApprovals, w *audit.Writer, log *slog.Logger) *Service {
	t.Helper()
	if base == nil {
		base = newFakeApprovals()
	}
	return NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: base}, w, time.Minute, nil, log)
}

// logCapture 收集 Warn 级别以上的日志消息，便于断言错误路径确实走到了日志分支。
type logCapture struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	seen []string
}

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := r.Message
	c.seen = append(c.seen, msg)
	c.buf.WriteString(msg + "\n")
	return nil
}

func (c *logCapture) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (c *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler     { return c }
func (c *logCapture) WithGroup(name string) slog.Handler           { return c }
func (c *logCapture) has(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.buf.String(), msg)
}

func TestNewServiceDefaults(t *testing.T) {
	svc := NewService(nil, nil, 0, nil, nil)
	if svc.ttl != defaultApprovalTTL {
		t.Fatalf("ttl = %v, want default %v", svc.ttl, defaultApprovalTTL)
	}
	if svc.clock == nil {
		t.Fatal("clock must default to time.Now")
	}
	if svc.log == nil {
		t.Fatal("log must default to slog.Default()")
	}
}

// 六个入口在 store 为 nil 时必须返回 ErrStoreUnavailable，而不是 panic。
func TestStoreUnavailableOnEveryEntrypoint(t *testing.T) {
	svc := NewService(nil, nil, time.Minute, nil, nil)
	ctx := context.Background()

	if _, err := svc.Create(ctx, createReq("tc-nil")); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Create error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := svc.Wait(ctx, "ap-1"); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Wait error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: "ap-1", Approve: true, DecidedBy: "u", Note: "", RequestID: ""}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Decide error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := svc.Cancel(ctx, "ap-1", "reason", ""); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Cancel error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := svc.Expire(ctx, time.Now()); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Expire error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := svc.Get(ctx, "ap-1"); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Get error = %v, want ErrStoreUnavailable", err)
	}
}

func TestCreateRejectsMissingIdentity(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()

	noConv := createReq("tc-nc")
	noConv.ConversationID = nil
	if _, err := svc.Create(ctx, noConv); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("nil conversation_id error = %v, want ErrInvalid", err)
	}

	noTool := createReq("")
	convID := "conv-1"
	noTool.ConversationID = &convID
	if _, err := svc.Create(ctx, noTool); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("nil tool_call_id error = %v, want ErrInvalid", err)
	}
}

func TestWaitEmptyApprovalID(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	if _, err := svc.Wait(context.Background(), ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// 审批到期但落库失败（store 故障）时，Wait 必须仍返回 expired 决策，
// 不让调用方挂在已超时的审批上；落库由 sweeper 重试。
func TestWaitTimeoutWhenExpireWriteFails(t *testing.T) {
	base := newFakeApprovals()
	fault := &faultApprovals{fakeApprovals: base, expireErr: errors.New("db down")}
	capLog := &logCapture{}
	log := slog.New(capLog)
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: fault}, nil, 20*time.Millisecond, nil, log)

	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-flaky"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	d, err := svc.Wait(ctx, ap.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if d.Status != store.ApprovalExpired {
		t.Fatalf("status = %q, want expired even when store write fails", d.Status)
	}
	if d.ApprovalID != ap.ID {
		t.Fatalf("decision approval_id = %q, want %q", d.ApprovalID, ap.ID)
	}
	if !capLog.has("approval_expire_failed") {
		t.Fatal("expected approval_expire_failed warn log")
	}
	stored, _ := base.Get(ctx, ap.ID)
	if stored.Status != store.ApprovalPending {
		t.Fatalf("store status = %q, want still pending (write failed)", stored.Status)
	}
}

// Start 必须幂等：第二次调用不得再起一个 sweeper goroutine。
func TestStartIdempotent(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx)
	svc.Start(ctx) // 第二次调用应直接返回
	svc.Stop()     // 若起了两个 sweeper，wg 计数不会归零或泄漏 goroutine
}

// sweeper 周期扫描时 store 报错只记日志，不退出扫描循环。
func TestSweeperLogsExpireFailure(t *testing.T) {
	base := newFakeApprovals()
	capLog := &logCapture{}
	log := slog.New(capLog)
	fault := &faultApprovals{fakeApprovals: base, expireErr: errors.New("db down")}
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: fault}, nil, time.Minute, nil, log)

	svc.Start(context.Background())
	defer svc.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if capLog.has("approval_expire_failed") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("sweeper did not log expire failure")
}

func TestExpireStoreError(t *testing.T) {
	base := newFakeApprovals()
	fault := &faultApprovals{fakeApprovals: base, expireErr: errors.New("boom")}
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: fault}, nil, time.Minute, nil, nil)

	if _, err := svc.Expire(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Expire error = %v, want injected boom", err)
	}
}

// Expire 没有过期项时返回 0 且不发审计。
func TestExpireZeroNoAudit(t *testing.T) {
	base := newFakeApprovals()
	w := audit.NewWriter(nil, slog.Default(), time.Now)
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: base}, w, time.Minute, nil, nil)

	n, err := svc.Expire(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0", n)
	}
}

// 重启清理：List 报错时直接放弃，不影响 sweeper 启动。
func TestRestartCleanupListError(t *testing.T) {
	base := newFakeApprovals()
	if _, err := base.Create(context.Background(), store.CreateApprovalParams{
		ConversationID: ptr("c1"), ToolCallID: ptr("tc-x"), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	capLog := &logCapture{}
	log := slog.New(capLog)
	fault := &faultApprovals{fakeApprovals: base, listErr: errors.New("list boom")}
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: fault}, nil, time.Minute, nil, log)

	svc.Start(context.Background())
	defer svc.Stop()

	if !capLog.has("approval_restart_expire_failed") {
		t.Fatal("expected approval_restart_expire_failed warn log")
	}
	ctx := context.Background()
	stored, _ := base.Get(ctx, "ap-1")
	if stored.Status != store.ApprovalPending {
		t.Fatalf("status = %q, want still pending after list failure", stored.Status)
	}
}

// 重启清理：单条 Decide 失败（冲突/不存在/其他错误）跳过该条并继续处理其余条目。
func TestRestartCleanupSkipsFailedDecides(t *testing.T) {
	base := newFakeApprovals()
	ctx := context.Background()
	ids := make([]string, 3)
	for i := range ids {
		tcID := fmt.Sprintf("tc-r%d", i)
		ap, err := base.Create(ctx, store.CreateApprovalParams{
			ConversationID: ptr("c1"),
			ToolCallID:     &tcID,
			ExpiresAt:      time.Now().Add(time.Hour),
			CreatedAt:      time.Now(),
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids[i] = ap.ID
	}
	capLog := &logCapture{}
	log := slog.New(capLog)
	fault := &faultApprovals{
		fakeApprovals: base,
		decideErr: map[string]error{
			ids[0]: store.ErrConflict,
			ids[1]: errors.New("write boom"),
		},
	}
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: fault}, nil, time.Minute, nil, log)

	svc.Start(context.Background())
	defer svc.Stop()

	for i, id := range ids {
		stored, err := base.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if i == 2 && stored.Status != store.ApprovalExpired {
			t.Fatalf("healthy item %d status = %q, want expired", i, stored.Status)
		}
		if i != 2 && stored.Status != store.ApprovalPending {
			t.Fatalf("failed item %d status = %q, want pending (skipped)", i, stored.Status)
		}
	}
	if !capLog.has("approval_restart_expire_failed") {
		t.Fatal("expected warn log for the non-conflict decide failure")
	}
}

// 审计 writer 不可用（nil）时审批流程不受影响。
func TestAuditWriterNilStillWorks(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	svc.audit = nil
	ctx := context.Background()

	ap, err := svc.Create(ctx, createReq("tc-noaudit"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "u", Note: "ok", RequestID: ""}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
}

// 审计 writer 已关闭时 Enqueue 失败只记 Warn，不回滚审批决策。
func TestAuditEnqueueFailureDoesNotRollback(t *testing.T) {
	capLog := &logCapture{}
	log := slog.New(capLog)
	w := audit.NewWriter(nil, log, time.Now)
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("audit close: %v", err)
	}
	base := newFakeApprovals()
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: base}, w, time.Minute, nil, log)

	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-closed"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	approved, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "u", Note: "ok", RequestID: ""})
	if err != nil {
		t.Fatalf("Decide must not fail because audit enqueue failed: %v", err)
	}
	if approved.Status != store.ApprovalApproved {
		t.Fatalf("status = %q, want approved", approved.Status)
	}
	if !capLog.has("approval_audit_enqueue_failed") {
		t.Fatal("expected approval_audit_enqueue_failed warn log")
	}
}

// ConversationID 为空的审批（历史数据）决策时审计里 conversation 字段应为 nil 而非空串指针。
func TestDecideEmptyConversationNilPointer(t *testing.T) {
	base := newFakeApprovals()
	svc := NewService(&faultStore{Store: &fakeStore{approvals: base}, ap: base}, nil, time.Minute, nil, nil)
	ctx := context.Background()

	orphanTool := "tc-orphan"
	orphan := store.Approval{
		ID:          "ap-orphan",
		ToolCallID:  &orphanTool,
		Status:      store.ApprovalPending,
		RequestedAt: time.Now(),
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	base.items[orphan.ID] = orphan

	got, err := svc.Decide(ctx, DecideRequest{ApprovalID: orphan.ID, Approve: false, DecidedBy: "u", Note: "n", RequestID: ""})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Status != store.ApprovalDenied {
		t.Fatalf("status = %q, want denied", got.Status)
	}
}

// ptr 是测试辅助：字符串转指针。
func ptr(s string) *string { return &s }
