// decide_audit_test.go 覆盖审批决策的审计载荷（契约 9.7 / 六期打磨 sprint 5）：
//
//  1. approval.approved / approval.denied 的 detail 无条件携带 command
//     （审计页「命令摘要」列与展开「完整命令」的唯一来源）；
//  2. 批准另写 command.approved（命令生命周期动作，此前后端 0 处写入，
//     审计页「命令批准」是死筛选项）；
//  3. source=batch 不在 approvals 侧写 command.approved（由 batchx 负责
//     补 batch_job_id，见 batchx/execute_test.go），一次批准只落一行。
package approvals

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// auditRecorder 是 audit.Writer 的落库端：把事件按入队顺序收进内存，
// 供断言 action/detail。
type auditRecorder struct {
	mu   sync.Mutex
	rows []store.AuditLog
	seq  int
}

func (r *auditRecorder) Append(_ context.Context, p store.CreateAuditParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.rows = append(r.rows, store.AuditLog{
		ID:           int64(r.seq),
		Action:       p.Action,
		ResourceType: p.ResourceType,
		ResourceID:   p.ResourceID,
		ServerID:     p.ServerID,
		Detail:       p.Detail,
	})
	return nil
}

func (r *auditRecorder) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.AuditLog{}, r.rows...), false, nil
}

func (r *auditRecorder) LastHash(context.Context) (string, error) { return "", nil }

func (r *auditRecorder) byAction(action string) []store.AuditLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.AuditLog, 0, len(r.rows))
	for _, row := range r.rows {
		if row.Action == action {
			out = append(out, row)
		}
	}
	return out
}

// auditStore 是带审计落库端的审批存储（WithTx 为内存直通）。
type auditStore struct {
	store.Store
	approvals *fakeApprovals
	recorder  *auditRecorder
}

func (s *auditStore) Approvals() store.ApprovalStore { return s.approvals }
func (s *auditStore) AuditLogs() store.AuditStore    { return s.recorder }
func (s *auditStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(s)
}

// newAuditService 构造带真实 audit.Writer（内存落库）的审批服务。
func newAuditService(t *testing.T) (*Service, *fakeApprovals, *auditRecorder) {
	t.Helper()
	repo := newFakeApprovals()
	rec := &auditRecorder{}
	st := &auditStore{approvals: repo, recorder: rec}
	w := audit.NewWriter(st, nil, time.Now)
	w.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = w.Close(ctx)
	})
	return NewService(st, w, time.Minute, nil, nil), repo, rec
}

// flushAudit 等待异步批量写入落库（writer 每 500ms 或满 100 条刷新；Flush
// 是同步屏障）。
func flushAudit(t *testing.T, svc *Service) {
	t.Helper()
	if svc.audit == nil {
		t.Fatal("audit writer must be configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.audit.Flush(ctx); err != nil {
		t.Fatalf("audit flush: %v", err)
	}
}

func detailString(t *testing.T, raw []byte, key string) string {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("detail unmarshal: %v", err)
	}
	value, _ := detail[key].(string)
	return value
}

func TestDecideApprovedWritesCommandToAudit(t *testing.T) {
	svc, _, rec := newAuditService(t)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-audit-approve"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1", Note: "ok", RequestID: "req-1",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	flushAudit(t, svc)

	approved := rec.byAction(actionApproved)
	if len(approved) != 1 {
		t.Fatalf("approval.approved rows = %d, want 1", len(approved))
	}
	if got := detailString(t, approved[0].Detail, "command"); got != "systemctl restart nginx" {
		t.Fatalf("approval.approved detail.command = %q", got)
	}

	// 契约 9.7：批准必须另有 command.approved（审计页「命令批准」项）。
	commandApproved := rec.byAction(actionCommandApproved)
	if len(commandApproved) != 1 {
		t.Fatalf("command.approved rows = %d, want 1（command.approved 此前后端 0 写入）", len(commandApproved))
	}
	if got := detailString(t, commandApproved[0].Detail, "command"); got != "systemctl restart nginx" {
		t.Fatalf("command.approved detail.command = %q", got)
	}
	if got := detailString(t, commandApproved[0].Detail, "approval_id"); got != ap.ID {
		t.Fatalf("command.approved detail.approval_id = %q, want %q", got, ap.ID)
	}
}

func TestDecideDeniedWritesCommandToAudit(t *testing.T) {
	svc, _, rec := newAuditService(t)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-audit-deny"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: false, DecidedBy: "user-1", Note: "no", RequestID: "req-2",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	flushAudit(t, svc)

	denied := rec.byAction(actionDenied)
	if len(denied) != 1 {
		t.Fatalf("approval.denied rows = %d, want 1", len(denied))
	}
	if got := detailString(t, denied[0].Detail, "command"); got != "systemctl restart nginx" {
		t.Fatalf("approval.denied detail.command = %q", got)
	}
	// 拒绝不产生 command.approved。
	if rows := rec.byAction(actionCommandApproved); len(rows) != 0 {
		t.Fatalf("command.approved rows = %d, want 0 on deny", len(rows))
	}
}

// 批量审批（source=batch）的 command.approved 由 batchx 写（detail 带
// batch_job_id）：approvals 侧不得重复写第二行。
func TestDecideBatchSourceSkipsCommandApproved(t *testing.T) {
	svc, _, rec := newAuditService(t)
	ctx := context.Background()
	ap, err := svc.Create(ctx, CreateRequest{
		Source:    "batch",
		ServerID:  ptr("srv-batch"),
		Command:   "rm -rf /tmp/x",
		RiskLevel: "needs_approval",
		RuleID:    "RM_ANY",
		Reason:    "dangerous",
	})
	if err != nil {
		t.Fatalf("Create batch approval: %v", err)
	}
	if _, err := svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1", RequestID: "req-3",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	flushAudit(t, svc)

	if rows := rec.byAction(actionCommandApproved); len(rows) != 0 {
		t.Fatalf("command.approved rows = %d, want 0 (batch path owns this action)", len(rows))
	}
	approved := rec.byAction(actionApproved)
	if len(approved) != 1 {
		t.Fatalf("approval.approved rows = %d, want 1", len(approved))
	}
	if got := detailString(t, approved[0].Detail, "command"); got != "rm -rf /tmp/x" {
		t.Fatalf("approval.approved detail.command = %q", got)
	}
}

// 改写批准：approval.approved 与 command.approved 都带原命令与改写命令。
func TestDecideEditedWritesBothCommands(t *testing.T) {
	svc, _, rec := newAuditService(t)
	ctx := context.Background()
	// fakeServerStore 在 decide 改写路径提供重分类目标。
	servers := &fakeServerStore{servers: map[string]store.Server{
		"srv-1": {ID: "srv-1", OS: store.ServerOSLinux},
	}}
	svc.st = &editAuditStore{Store: svc.st, servers: servers}

	ap := createEditApproval(t, svc, "srv-1")
	if _, err := svc.Decide(ctx, editReq(ap.ID, true, "uptime")); err != nil {
		t.Fatalf("Decide with edit: %v", err)
	}
	flushAudit(t, svc)

	approved := rec.byAction(actionApproved)
	if len(approved) != 1 {
		t.Fatalf("approval.approved rows = %d, want 1", len(approved))
	}
	if got := detailString(t, approved[0].Detail, "original_command"); got != "systemctl restart nginx" {
		t.Fatalf("approval.approved detail.original_command = %q", got)
	}
	if got := detailString(t, approved[0].Detail, "edited_command"); got != "uptime" {
		t.Fatalf("approval.approved detail.edited_command = %q", got)
	}

	commandApproved := rec.byAction(actionCommandApproved)
	if len(commandApproved) != 1 {
		t.Fatalf("command.approved rows = %d, want 1", len(commandApproved))
	}
	if got := detailString(t, commandApproved[0].Detail, "edited_command"); got != "uptime" {
		t.Fatalf("command.approved detail.edited_command = %q", got)
	}
	if got := detailString(t, commandApproved[0].Detail, "original_command"); got != "systemctl restart nginx" {
		t.Fatalf("command.approved detail.original_command = %q", got)
	}
}

// editAuditStore 在 auditStore 之上叠加 Servers()（改写重分类需要）。
type editAuditStore struct {
	store.Store
	servers *fakeServerStore
}

func (s *editAuditStore) Servers() store.ServerStore { return s.servers }
