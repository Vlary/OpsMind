// service_test.go 覆盖契约 9.2 G36（批量编排）与任务单测要求：并行执行+
// 失败隔离、goroutine 池上限（fake 慢执行断言峰值并发）、per-server 审批
// 独立性（一台拒绝不影响其余）、取消语义、审批过期 sweeper 收敛、50 台
// 上限、输出截断与归并函数的 allocation budget（内存守卫）。
package batchx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeSSH 实现 sshx.Executor：可配置慢执行/失败注入/超大输出，跟踪峰值并发。
type fakeSSH struct {
	mu        sync.Mutex
	delay     time.Duration // 每次执行延迟（慢执行测池上限）
	output    string        // 模拟输出（超过 MaxOutputBytes 时截断）
	failIDs   map[string]error
	calls     []sshx.RunRequest
	inflight  atomic.Int64
	peak      atomic.Int64
	clockMsec int64 // 累计执行毫秒（clock 推进用）
}

func (f *fakeSSH) Test(ctx context.Context, target sshx.Target) (sshx.TestResult, error) {
	return sshx.TestResult{}, nil
}

func (f *fakeSSH) Execute(ctx context.Context, target sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	cur := f.inflight.Add(1)
	for {
		peak := f.peak.Load()
		if cur <= peak || f.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer f.inflight.Add(-1)

	f.mu.Lock()
	f.calls = append(f.calls, req)
	delay := f.delay
	output := f.output
	failErr := f.failIDs[target.ServerID]
	f.mu.Unlock()
	if failErr != nil {
		return sshx.RunResult{}, failErr
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return sshx.RunResult{}, ctx.Err()
		}
	}
	truncated := false
	if int64(len(output)) > req.MaxOutputBytes {
		output = output[:req.MaxOutputBytes]
		truncated = true
	}
	return sshx.RunResult{ExitCode: 0, Combined: output, Truncated: truncated, DurationMS: 12}, nil
}

func (f *fakeSSH) Invalidate(serverID string) {}
func (f *fakeSSH) Close() error               { return nil }

// commandsOf 返回已执行命令序列（线程安全）。
func (f *fakeSSH) commandsOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Command)
	}
	return out
}

// fakeApprovalsStore 是 approvals.Service 的内存审批仓储。
type fakeApprovalsStore struct {
	mu    sync.Mutex
	items map[string]store.Approval
	next  int
}

func newFakeApprovalsStore() *fakeApprovalsStore {
	return &fakeApprovalsStore{items: map[string]store.Approval{}}
}

func (f *fakeApprovalsStore) Create(_ context.Context, p store.CreateApprovalParams) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("ap-%d", f.next)
	ap := store.Approval{
		ID: id, ConversationID: p.ConversationID, ToolCallID: p.ToolCallID,
		Source: p.Source, ServerID: p.ServerID, ServerName: p.ServerName,
		Command: p.Command, RiskLevel: p.RiskLevel, RuleID: p.RuleID,
		Reason: p.Reason, Status: store.ApprovalPending,
		ApprovalMode: store.ApprovalMode(p.ApprovalMode),
		RequestedAt:  p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}
	f.items[id] = ap
	return ap, nil
}

func (f *fakeApprovalsStore) Get(_ context.Context, id string) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.items[id]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	return ap, nil
}

func (f *fakeApprovalsStore) List(_ context.Context, conversationID string, status store.ApprovalStatus, limit int) ([]store.Approval, error) {
	return nil, nil
}

func (f *fakeApprovalsStore) Decide(_ context.Context, p store.DecideApprovalParams) (store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.items[p.ID]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	if ap.Status != store.ApprovalPending {
		return store.Approval{}, store.ErrConflict
	}
	ap.Status = p.Status
	decidedAt := p.DecidedAt.UTC()
	ap.DecidedAt = &decidedAt
	ap.DecidedBy = &p.DecidedBy
	ap.Note = p.Note
	f.items[p.ID] = ap
	return ap, nil
}

func (f *fakeApprovalsStore) Expire(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, ap := range f.items {
		if ap.Status == store.ApprovalPending && !ap.ExpiresAt.After(now) {
			ap.Status = store.ApprovalExpired
			f.items[id] = ap
			n++
		}
	}
	return n, nil
}

// fakeBatchJobs 是 store.BatchJobStore 的内存实现（TotalCount/DoneCount
// 由 results 聚合派生，同 postgres 语义）。
type fakeBatchJobs struct {
	mu      sync.Mutex
	jobs    map[string]store.BatchJob
	results map[string][]store.BatchResult
	next    int
}

func newFakeBatchJobs() *fakeBatchJobs {
	return &fakeBatchJobs{jobs: map[string]store.BatchJob{}, results: map[string][]store.BatchResult{}}
}

func (f *fakeBatchJobs) Create(_ context.Context, p store.CreateBatchJobParams) (store.BatchJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("job-%d", f.next)
	job := store.BatchJob{
		ID: id, Description: p.Description, Command: p.Command,
		ServerIDs: append([]string{}, p.ServerIDs...), Source: p.Source,
		ConversationID: p.ConversationID, Status: store.BatchPending,
		TotalCount: len(p.ServerIDs), CreatedBy: p.CreatedBy,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	f.jobs[id] = job
	return job, nil
}

func (f *fakeBatchJobs) agg(jobID string) (store.BatchJob, []store.BatchResult) {
	job := f.jobs[jobID]
	results := f.results[jobID]
	job.TotalCount = len(results)
	job.DoneCount = 0
	for _, r := range results {
		switch r.Status {
		case store.BatchResultExecuted, store.BatchResultFailed, store.BatchResultDenied,
			store.BatchResultExpired, store.BatchResultSkipped, store.BatchResultCancelled:
			job.DoneCount++
		}
	}
	return job, results
}

func (f *fakeBatchJobs) Get(_ context.Context, id string) (store.BatchJob, []store.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[id]
	if !ok {
		return store.BatchJob{}, nil, store.ErrNotFound
	}
	job, results := f.agg(id)
	// 深拷贝：调用方在锁外遍历（mergeStatus），并发的 FinishResult 会写
	// 同一切片底层数组——共享引用会被 race 检测命中（postgres 实现返回
	// 行拷贝，无此问题）。
	out := make([]store.BatchResult, len(results))
	copy(out, results)
	return job, out, nil
}

func (f *fakeBatchJobs) List(_ context.Context, limit int) ([]store.BatchJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.BatchJob, 0, len(f.jobs))
	for id := range f.jobs {
		job, _ := f.agg(id)
		out = append(out, job)
	}
	// created_at 全同（time.Now 精度内），按 id 倒序近似 created_at DESC。
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].ID > out[i].ID {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeBatchJobs) SetStatus(_ context.Context, id string, status store.BatchStatus, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[id]
	if !ok {
		return store.ErrNotFound
	}
	job.Status = status
	job.UpdatedAt = at
	if isBatchTerminal(status) {
		fin := at
		job.FinishedAt = &fin
	}
	f.jobs[id] = job
	return nil
}

func (f *fakeBatchJobs) CreateResult(_ context.Context, p store.CreateBatchResultParams) (store.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("res-%d", f.next)
	r := store.BatchResult{
		ID: id, BatchJobID: p.BatchJobID, ServerID: p.ServerID,
		RiskLevel: p.RiskLevel, RuleID: p.RuleID, ApprovalID: p.ApprovalID,
		Status: p.Status, ErrorCode: p.ErrorCode,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	f.results[p.BatchJobID] = append(f.results[p.BatchJobID], r)
	return r, nil
}

func (f *fakeBatchJobs) FinishResult(_ context.Context, p store.FinishBatchResultParams) (store.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for jobID := range f.results {
		for i := range f.results[jobID] {
			if f.results[jobID][i].ID == p.ID {
				r := &f.results[jobID][i]
				r.Status = p.Status
				r.ExitCode = p.ExitCode
				r.Output = p.Output
				r.Truncated = p.Truncated
				r.ErrorCode = p.ErrorCode
				r.DurationMS = p.DurationMS
				r.UpdatedAt = p.FinishedAt
				return *r, nil
			}
		}
	}
	return store.BatchResult{}, store.ErrNotFound
}

func (f *fakeBatchJobs) ByApproval(_ context.Context, approvalID string) (store.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, results := range f.results {
		for _, r := range results {
			if r.ApprovalID != nil && *r.ApprovalID == approvalID {
				return r, nil
			}
		}
	}
	return store.BatchResult{}, store.ErrNotFound
}

// fakeAuditStore 收集审计事件（audit.Writer 的落库端）。
type fakeAuditStore struct {
	mu    sync.Mutex
	logs  []store.AuditLog
	nextN int
}

func (f *fakeAuditStore) Append(_ context.Context, p store.CreateAuditParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextN++
	f.logs = append(f.logs, store.AuditLog{ID: int64(f.nextN), Action: p.Action, ResourceType: p.ResourceType, ResourceID: p.ResourceID, ActorType: p.ActorType, ActorID: p.ActorID, Detail: p.Detail})
	return nil
}

func (f *fakeAuditStore) List(_ context.Context, flt store.AuditFilter) ([]store.AuditLog, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]store.AuditLog{}, f.logs...)
	if len(out) > flt.Limit {
		out = out[:flt.Limit]
	}
	return out, false, nil
}

func (f *fakeAuditStore) LastHash(_ context.Context) (string, error) { return "", nil }

// fixture 装配被测 Service。
type fixture struct {
	st        *fakeStore
	ssh       *fakeSSH
	svc       *Service
	ap        *approvals.Service
	apStore   *fakeApprovalsStore
	auditSt   *fakeAuditStore
	writer    *audit.Writer
	cipher    *cryptox.Cipher
	servers   map[string]store.Server
	encSecret string
}

// fakeStore embed store.Store：未覆写的方法调用即 panic（测试不触达）。
type fakeStore struct {
	store.Store
	servers   map[string]store.Server
	convs     map[string]store.Conversation
	approvals *fakeApprovalsStore
	batch     *fakeBatchJobs
	auditLogs *fakeAuditStore
	settings  store.SettingStore
}

func (f *fakeStore) Settings() store.SettingStore { return f.settings }

// WithTx 内存版：无事务语义，直接执行回调（audit.Writer 的批量落库走此路径）。
func (f *fakeStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(f)
}

func (f *fakeStore) Servers() store.ServerStore { return &fakeServerStore{servers: f.servers} }
func (f *fakeStore) Conversations() store.ConversationStore {
	return &fakeConversationStore{convs: f.convs}
}
func (f *fakeStore) Approvals() store.ApprovalStore { return f.approvals }
func (f *fakeStore) BatchJobs() store.BatchJobStore { return f.batch }
func (f *fakeStore) AuditLogs() store.AuditStore    { return f.auditLogs }

// fakeConversationStore embed store.ConversationStore（nil）：批量门限解析只
// 读 Get，其余方法（创建/改标题/门限写入）不触达。
type fakeConversationStore struct {
	store.ConversationStore
	convs map[string]store.Conversation
}

func (c *fakeConversationStore) Get(_ context.Context, id string) (store.Conversation, error) {
	conv, ok := c.convs[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	return conv, nil
}

// fakeServerStore embed store.ServerStore（nil）：未覆写的方法触达即 panic
// （批量路径只读 Get）。
type fakeServerStore struct {
	store.ServerStore
	servers map[string]store.Server
}

func (s *fakeServerStore) Get(_ context.Context, id string) (store.Server, error) {
	srv, ok := s.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	return srv, nil
}

func newFixture(t *testing.T, cfg ServiceConfig) *fixture {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cipher, err := cryptox.New(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	enc, _ := cipher.Encrypt("secret")

	fx := &fixture{
		ssh:     &fakeSSH{failIDs: map[string]error{}},
		apStore: newFakeApprovalsStore(),
		servers: map[string]store.Server{},
		cipher:  cipher,
		auditSt: &fakeAuditStore{},
	}
	fx.st = &fakeStore{
		servers: fx.servers, convs: map[string]store.Conversation{},
		approvals: fx.apStore,
		batch:     newFakeBatchJobs(), auditLogs: fx.auditSt,
	}
	fx.writer = audit.NewWriter(fx.st, log, time.Now)
	fx.ap = approvals.NewService(fx.st, fx.writer, time.Minute, nil, log)
	fx.svc = NewService(cfg, fx.st, fx.ap, fx.ssh, nil, cipher, fx.writer, nil, log)
	fx.encSecret = enc
	return fx
}

// addServer 注册一台 Linux 服务器（password 认证、密文已加密）。
func (fx *fixture) addServer(id string, autoExec bool) store.Server {
	srv := store.Server{
		ID: id, Name: id, OS: store.ServerOSLinux, Host: "127.0.0.1", Port: 22,
		Username: "root", AuthMethod: store.AuthMethodPassword,
		PasswordEncrypted: fx.encSecret, AutoExecEnabled: autoExec,
	}
	fx.servers[id] = srv
	return srv
}

func (fx *fixture) startAudit(t *testing.T) {
	t.Helper()
	fx.writer.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = fx.writer.Close(ctx)
	})
}

func (fx *fixture) auditActions() []string {
	fx.auditSt.mu.Lock()
	defer fx.auditSt.mu.Unlock()
	actions := make([]string, 0, len(fx.auditSt.logs))
	for _, l := range fx.auditSt.logs {
		actions = append(actions, l.Action)
	}
	return actions
}

// resultsOf 读 job 全部逐台结果。
func (fx *fixture) resultsOf(t *testing.T, jobID string) []store.BatchResult {
	t.Helper()
	_, results, err := fx.st.BatchJobs().Get(context.Background(), jobID)
	if err != nil {
		t.Fatalf("Get job: %v", err)
	}
	return results
}

// waitResultStatus 轮询等待某台 result 到达期望状态（Dispatch 自五期
// 评审起异步收敛直执行台，不再同步等待执行完成）。
func (fx *fixture) waitResultStatus(t *testing.T, jobID, serverID string, want store.BatchResultStatus) store.BatchResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range fx.resultsOf(t, jobID) {
			if r.ServerID == serverID && r.Status == want {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var cur *store.BatchResult
	for _, r := range fx.resultsOf(t, jobID) {
		if r.ServerID == serverID {
			cur = &r
		}
	}
	t.Fatalf("result %s/%s not %s, got %+v", jobID, serverID, want, cur)
	return store.BatchResult{}
}

func resultByServer(results []store.BatchResult, serverID string) *store.BatchResult {
	for i := range results {
		if results[i].ServerID == serverID {
			return &results[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// G36 主线：逐台分派
// ---------------------------------------------------------------------------

// TestDispatchPerServerRouting 覆盖 G36 分派三分支（同一命令）：safe+
// auto_exec 直执行、safe 未开 auto_exec 建 source=batch 审批、server 不
// 存在 skipped。forbidden 分支见 TestDispatchForbiddenSkips（批量命令统一，
// forbidden 命令全台跳过）。
func TestDispatchPerServerRouting(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "ok"
	fx.addServer("srv-direct", true)   // ls safe + auto_exec → 直接执行
	fx.addServer("srv-approve", false) // safe 但未开 auto_exec → 审批
	fx.startAudit(t)

	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Description: "对所有 web 机器执行 df -h",
		Command:     "ls -la",
		ServerIDs:   []string{"srv-direct", "srv-approve", "srv-gone"},
		Source:      store.BatchSourceConsole,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if job.TotalCount != 3 {
		t.Fatalf("total = %d, want 3", job.TotalCount)
	}

	results := fx.resultsOf(t, job.ID)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}

	// safe+auto_exec：executed，命令逐字节一致。
	fx.waitResultStatus(t, job.ID, "srv-direct", store.BatchResultExecuted)
	results = fx.resultsOf(t, job.ID)
	direct := resultByServer(results, "srv-direct")
	if direct == nil {
		t.Fatal("srv-direct missing")
	}
	if direct.Output != "ok" || direct.ExitCode == nil || *direct.ExitCode != 0 {
		t.Fatalf("srv-direct output/exit = %+v", direct)
	}
	found := false
	fx.ssh.mu.Lock()
	for _, c := range fx.ssh.calls {
		if c.Command == "ls -la" {
			found = true
		}
	}
	fx.ssh.mu.Unlock()
	if !found {
		t.Fatal("fake ssh did not receive the exact command")
	}

	// safe 未开 auto_exec：source=batch 审批（conversation/tool_call NULL、
	// command 逐字节一致）+ result awaiting_approval。
	approve := resultByServer(results, "srv-approve")
	if approve == nil || approve.Status != store.BatchResultAwaitingApproval || approve.ApprovalID == nil {
		t.Fatalf("srv-approve = %+v, want awaiting_approval with approval", approve)
	}
	ap, err := fx.ap.Get(ctx, *approve.ApprovalID)
	if err != nil {
		t.Fatalf("approval get: %v", err)
	}
	if ap.Source != "batch" || ap.Command != "ls -la" {
		t.Fatalf("approval = %+v", ap)
	}
	if ap.ConversationID != nil || ap.ToolCallID != nil {
		t.Fatalf("batch approval must have NULL conversation/tool_call: %+v", ap)
	}
	if !strings.Contains(ap.Reason, "对所有 web 机器执行 df -h") {
		t.Fatalf("approval reason should carry description context: %q", ap.Reason)
	}

	// 已删除 server：skipped(SERVER_NOT_FOUND)。
	gone := resultByServer(results, "srv-gone")
	if gone == nil || gone.Status != store.BatchResultSkipped || gone.ErrorCode != errCodeServerNotFound {
		t.Fatalf("srv-gone = %+v, want skipped(SERVER_NOT_FOUND)", gone)
	}

	// job 仍有审批台未终态 → running。
	if job.Status != store.BatchRunning {
		t.Fatalf("job status = %q, want running", job.Status)
	}

	// 审计：batch.created + 直执行台 command.executed（forbidden 审计见
	// TestDispatchForbiddenSkips）。
	deadline := time.Now().Add(3 * time.Second)
	var actions []string
	for {
		actions = fx.auditActions()
		if hasAll(actions, actionBatchCreated, actionCmdExecuted) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hasAll(actions, actionBatchCreated, actionCmdExecuted) {
		t.Fatalf("audit actions = %v", actions)
	}
}

// TestDispatchForbiddenSkips：`rm -rf /` 全台 forbidden → skipped
// (COMMAND_FORBIDDEN)+审计 command.forbidden，不建审批、不执行（G36）。
func TestDispatchForbiddenSkips(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.addServer("srv-evil", true) // 即便 auto_exec=true，forbidden 也不执行
	fx.startAudit(t)
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "rm -rf /", ServerIDs: []string{"srv-evil"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	r := resultByServer(fx.resultsOf(t, job.ID), "srv-evil")
	if r == nil || r.Status != store.BatchResultSkipped || r.ErrorCode != errCodeCommandForbidden {
		t.Fatalf("result = %+v, want skipped(COMMAND_FORBIDDEN)", r)
	}
	if r.RiskLevel != "forbidden" {
		t.Fatalf("risk_level = %q, want forbidden", r.RiskLevel)
	}
	if len(fx.ssh.calls) != 0 {
		t.Fatal("forbidden command must not execute")
	}
	// 全部台终态（skipped）→ partial（含跳过台）。
	final, _ := fx.svc.RecalcJobStatus(ctx, job.ID)
	if final.Status != store.BatchPartial {
		t.Fatalf("job = %q, want partial", final.Status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if hasAll(fx.auditActions(), actionCmdForbidden) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit actions = %v, want command.forbidden", fx.auditActions())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hasAll(actions []string, want ...string) bool {
	set := map[string]bool{}
	for _, a := range actions {
		set[a] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 审批闭环：approve 执行回填、deny、per-server 独立性
// ---------------------------------------------------------------------------

func TestApprovalDecideApproveExecutesAndBackfills(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "df output"
	fx.addServer("web-1", false)
	ctx := context.Background()

	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "df -h", ServerIDs: []string{"web-1"}, Source: store.BatchSourceConsole,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	results := fx.resultsOf(t, job.ID)
	apID := resultByServer(results, "web-1").ApprovalID

	// decide approve → OnApprovalDecided 执行 → FinishResult executed。
	ap, err := fx.ap.Decide(ctx, approvals.DecideRequest{ApprovalID: *apID, Approve: true, DecidedBy: "user-1", Note: "", RequestID: "req-1"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := fx.svc.OnApprovalDecided(ctx, ap, true); err != nil {
		t.Fatalf("OnApprovalDecided: %v", err)
	}

	// ByApproval 回填可见（G36 断言）。
	backfilled, err := fx.st.BatchJobs().ByApproval(ctx, *apID)
	if err != nil {
		t.Fatalf("ByApproval: %v", err)
	}
	if backfilled.Status != store.BatchResultExecuted || backfilled.Output != "df output" {
		t.Fatalf("backfilled = %+v", backfilled)
	}

	// RecalcJobStatus：全部台终态且无失败 → done。
	final, err := fx.svc.RecalcJobStatus(ctx, job.ID)
	if err != nil {
		t.Fatalf("Recalc: %v", err)
	}
	if final.Status != store.BatchDone {
		t.Fatalf("status = %q, want done", final.Status)
	}
	if final.FinishedAt == nil {
		t.Fatal("finished_at should be set on terminal status")
	}
}

func TestApprovalDecideDeny(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.addServer("web-1", false)
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "df -h", ServerIDs: []string{"web-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	apID := resultByServer(fx.resultsOf(t, job.ID), "web-1").ApprovalID
	ap, err := fx.ap.Decide(ctx, approvals.DecideRequest{ApprovalID: *apID, Approve: false, DecidedBy: "user-1", Note: "", RequestID: "req-1"})
	if err != nil {
		t.Fatalf("Decide deny: %v", err)
	}
	if err := fx.svc.OnApprovalDecided(ctx, ap, false); err != nil {
		t.Fatalf("OnApprovalDecided deny: %v", err)
	}
	results := fx.resultsOf(t, job.ID)
	if r := resultByServer(results, "web-1"); r.Status != store.BatchResultDenied {
		t.Fatalf("status = %q, want denied", r.Status)
	}
	final, _ := fx.svc.RecalcJobStatus(ctx, job.ID)
	if final.Status != store.BatchPartial {
		t.Fatalf("job = %q, want partial（denied 台）", final.Status)
	}
	// deny 不执行命令。
	if len(fx.ssh.calls) != 0 {
		t.Fatalf("deny must not execute, calls = %d", len(fx.ssh.calls))
	}
}

// TestApprovalIndependence：两台审批台一台 deny 一台 approve——互不影响，
// 各自按自身决定执行/拒绝。
func TestApprovalIndependence(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "ok"
	fx.addServer("web-1", false)
	fx.addServer("web-2", false)
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "systemctl status nginx", ServerIDs: []string{"web-1", "web-2"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	results := fx.resultsOf(t, job.ID)
	ap1 := resultByServer(results, "web-1").ApprovalID
	ap2 := resultByServer(results, "web-2").ApprovalID
	if *ap1 == *ap2 {
		t.Fatal("two servers must get independent approvals")
	}

	decided1, _ := fx.ap.Decide(ctx, approvals.DecideRequest{ApprovalID: *ap1, Approve: false, DecidedBy: "user-1", Note: "", RequestID: "r"})
	if err := fx.svc.OnApprovalDecided(ctx, decided1, false); err != nil {
		t.Fatalf("deny web-1: %v", err)
	}
	decided2, _ := fx.ap.Decide(ctx, approvals.DecideRequest{ApprovalID: *ap2, Approve: true, DecidedBy: "user-1", Note: "", RequestID: "r"})
	if err := fx.svc.OnApprovalDecided(ctx, decided2, true); err != nil {
		t.Fatalf("approve web-2: %v", err)
	}

	results = fx.resultsOf(t, job.ID)
	if r := resultByServer(results, "web-1"); r.Status != store.BatchResultDenied {
		t.Fatalf("web-1 = %q, want denied", r.Status)
	}
	if r := resultByServer(results, "web-2"); r.Status != store.BatchResultExecuted {
		t.Fatalf("web-2 = %q, want executed（一台拒绝不影响其余）", r.Status)
	}
	final, _ := fx.svc.RecalcJobStatus(ctx, job.ID)
	if final.Status != store.BatchPartial {
		t.Fatalf("job = %q, want partial", final.Status)
	}
}

// ---------------------------------------------------------------------------
// 审批过期 sweeper → result expired（G36）
// ---------------------------------------------------------------------------

func TestExpireScanConvergesExpiredApprovals(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.addServer("web-1", false)
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "df -h", ServerIDs: []string{"web-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// approvals sweeper 把 pending 审批置 expired。
	if _, err := fx.ap.Expire(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if err := fx.svc.expireScanOnce(ctx); err != nil {
		t.Fatalf("expireScanOnce: %v", err)
	}

	results := fx.resultsOf(t, job.ID)
	if r := resultByServer(results, "web-1"); r.Status != store.BatchResultExpired {
		t.Fatalf("status = %q, want expired", r.Status)
	}
	final, _ := fx.svc.RecalcJobStatus(ctx, job.ID)
	if final.Status != store.BatchPartial {
		t.Fatalf("job = %q, want partial", final.Status)
	}
}

// ---------------------------------------------------------------------------
// 池上限（G36：fake 慢执行断言峰值并发）+ 并行 + 失败隔离
// ---------------------------------------------------------------------------

func TestWorkerPoolLimit(t *testing.T) {
	fx := newFixture(t, ServiceConfig{}) // 池固定 8
	fx.ssh.delay = 60 * time.Millisecond
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = fmt.Sprintf("srv-%02d", i)
		fx.addServer(ids[i], true)
	}
	start := time.Now()
	job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "ls", ServerIDs: ids,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// Dispatch 自五期评审起不等待直执行台（异步收敛）：16 台 × 60ms 的
	// 执行窗口内必须已返回（同步等待实现会 ≥960ms）。
	if elapsed > 700*time.Millisecond {
		t.Fatalf("Dispatch elapsed = %v, must return before direct executions finish", elapsed)
	}
	// 等待全部直执行台收敛后断言终态与并发上限。
	for _, id := range ids {
		fx.waitResultStatus(t, job.ID, id, store.BatchResultExecuted)
	}
	finJob, _, gerr := fx.st.BatchJobs().Get(context.Background(), job.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if finJob.Status != store.BatchDone {
		t.Fatalf("status = %q, want done", finJob.Status)
	}
	if peak := fx.ssh.peak.Load(); peak > 8 {
		t.Fatalf("peak concurrency = %d, want ≤ 8（池上限）", peak)
	}
	if peak := fx.ssh.peak.Load(); peak < 2 {
		t.Fatalf("peak concurrency = %d, want parallel (>1)", peak)
	}
}

func TestFailureIsolation(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.failIDs["srv-bad"] = errors.New("dial tcp: connection refused")
	fx.addServer("srv-ok-1", true)
	fx.addServer("srv-bad", true)
	fx.addServer("srv-ok-2", true)
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "ls", ServerIDs: []string{"srv-ok-1", "srv-bad", "srv-ok-2"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err) // 单台失败不中断编排
	}
	fx.waitResultStatus(t, job.ID, "srv-bad", store.BatchResultFailed)
	fx.waitResultStatus(t, job.ID, "srv-ok-1", store.BatchResultExecuted)
	fx.waitResultStatus(t, job.ID, "srv-ok-2", store.BatchResultExecuted)
	results := fx.resultsOf(t, job.ID)
	if r := resultByServer(results, "srv-bad"); r == nil || r.Status != store.BatchResultFailed || r.ErrorCode != errCodeSSHExecFailed {
		t.Fatalf("srv-bad = %+v, want failed(SSH_EXEC_FAILED)", r)
	}
	finJob, _, gerr := fx.st.BatchJobs().Get(ctx, job.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if finJob.Status != store.BatchPartial {
		t.Fatalf("job = %q, want partial", finJob.Status)
	}
}

// ---------------------------------------------------------------------------
// 取消语义
// ---------------------------------------------------------------------------

func TestCancelAwaitingApproval(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.addServer("web-1", false)
	fx.addServer("web-2", false)
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "df -h", ServerIDs: []string{"web-1", "web-2"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	apID := resultByServer(fx.resultsOf(t, job.ID), "web-1").ApprovalID

	cancelled, err := fx.svc.Cancel(ctx, job.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.Status != store.BatchPartial {
		t.Fatalf("job = %q, want partial", cancelled.Status)
	}
	for _, r := range fx.resultsOf(t, job.ID) {
		if r.Status != store.BatchResultCancelled {
			t.Fatalf("result %s = %q, want cancelled", r.ServerID, r.Status)
		}
	}
	// 取消后审批已作废：decide 返回冲突，不可再批准执行。
	if _, err := fx.ap.Decide(ctx, approvals.DecideRequest{ApprovalID: *apID, Approve: true, DecidedBy: "user-1", Note: "", RequestID: "r"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("decide after cancel = %v, want ErrConflict", err)
	}
	// 取消后的台不再执行（即使拿到已决定的审批对象也不会跑命令）。
	ap, _ := fx.apStore.Get(ctx, *apID)
	if err := fx.svc.OnApprovalDecided(ctx, ap, true); err != nil {
		t.Fatalf("OnApprovalDecided on cancelled: %v", err)
	}
	if len(fx.ssh.calls) != 0 {
		t.Fatal("cancelled result must not execute")
	}
}

func TestCancelJobNotFound(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	if _, err := fx.svc.Cancel(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// 50 台上限（G36）
// ---------------------------------------------------------------------------

func TestMaxTargets(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = fmt.Sprintf("srv-%02d", i)
		fx.addServer(ids[i], true)
	}
	if _, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "ls", ServerIDs: ids,
	}); !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("51 targets err = %v, want ErrTooManyTargets", err)
	}

	// 50 台（合法上界）：Dispatch 异步分派后等待全部直执行台收敛 → done。
	job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "ls", ServerIDs: ids[:50],
	})
	if err != nil {
		t.Fatalf("50 targets: %v", err)
	}
	if job.TotalCount != 50 {
		t.Fatalf("job total = %d, want 50", job.TotalCount)
	}
	for _, id := range ids[:50] {
		fx.waitResultStatus(t, job.ID, id, store.BatchResultExecuted)
	}
	finJob, _, gerr := fx.st.BatchJobs().Get(context.Background(), job.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if finJob.Status != store.BatchDone {
		t.Fatalf("job = %q, want done", finJob.Status)
	}
}

// ---------------------------------------------------------------------------
// 内存守卫：输出截断（256KB 环形上限）与归并函数 allocation budget
// ---------------------------------------------------------------------------

func TestOutputTruncation(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = strings.Repeat("x", 1024*1024) // 1MB 原始输出
	fx.addServer("srv-1", true)
	job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "ls", ServerIDs: []string{"srv-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	r := fx.waitResultStatus(t, job.ID, "srv-1", store.BatchResultExecuted)
	if int64(len(r.Output)) != 262144 {
		t.Fatalf("output = %d bytes, want 262144 (256KB cap)", len(r.Output))
	}
	if !r.Truncated {
		t.Fatal("truncated flag must be set")
	}
}

// TestMergeStatusAllocBudget 是核心归并逻辑的内存守卫单测（防 OOM 铁律 6）：
// 状态归并是每台执行完成后的热路径，分配必须是有界的固定开销。
func TestMergeStatusAllocBudget(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	results := make([]store.BatchResult, 50)
	for i := range results {
		results[i] = store.BatchResult{Status: store.BatchResultExecuted}
	}
	allocs := testing.AllocsPerRun(200, func() {
		_ = fx.svc.mergeStatus(results)
	})
	if allocs > 8 {
		t.Fatalf("mergeStatus allocs = %.0f per run, want ≤ 8（固定开销）", allocs)
	}
	if got := fx.svc.mergeStatus(results); got != store.BatchDone {
		t.Fatalf("merge = %q, want done", got)
	}
}

// TestDedupeAllocBudget：目标去重的分配预算（≤50 台输入，常数级开销）。
func TestDedupeAllocBudget(t *testing.T) {
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("srv-%02d", i)
	}
	allocs := testing.AllocsPerRun(200, func() {
		_ = dedupeKeepOrder(ids)
	})
	if allocs > 16 {
		t.Fatalf("dedupeKeepOrder allocs = %.0f per run, want ≤ 16", allocs)
	}
	if got := dedupeKeepOrder([]string{"a", "b", "a"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("dedupe = %v", got)
	}
}

// ---------------------------------------------------------------------------
// 网络设备：经注入的 ExecFunc 分类路由（safe+auto_exec 才直执行）
// ---------------------------------------------------------------------------

func TestNetworkDeviceViaInjectedExecFunc(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.servers["net-1"] = store.Server{
		ID: "net-1", Name: "net-1", DeviceKind: store.DeviceKindNetworkDevice,
		OS: store.ServerOSNetwork, NetdevBrand: store.NetdevBrandCisco,
		AccessProto: store.AccessProtoSSH, Host: "127.0.0.1", Port: 22,
		Username: "admin", AuthMethod: store.AuthMethodPassword,
		PasswordEncrypted: fx.encSecret, AutoExecEnabled: true,
	}

	var gotCommand string
	fx.svc.SetNetExecFactory(func(srv store.Server) (ExecFunc, error) {
		return func(ctx context.Context, command string, timeout time.Duration) (ExecResult, error) {
			gotCommand = command
			return ExecResult{ExitCode: 0, Combined: "Cisco IOS"}, nil
		}, nil
	})

	job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "show version", ServerIDs: []string{"net-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	r := fx.waitResultStatus(t, job.ID, "net-1", store.BatchResultExecuted)
	if gotCommand != "show version" {
		t.Fatalf("netdev command = %q", gotCommand)
	}
	if r.Status != store.BatchResultExecuted || r.Output != "Cisco IOS" {
		t.Fatalf("net-1 = %+v", r)
	}
}

// TestNetdevConfigChangeNeedsApproval：网络设备 needs_approval 命令
// （write memory）走审批，不经 netExec。
func TestNetdevConfigChangeNeedsApproval(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.servers["net-1"] = store.Server{
		ID: "net-1", DeviceKind: store.DeviceKindNetworkDevice,
		OS: store.ServerOSNetwork, NetdevBrand: store.NetdevBrandCisco,
		AccessProto: store.AccessProtoSSH, Host: "127.0.0.1", Port: 22,
		Username: "admin", AuthMethod: store.AuthMethodPassword,
		PasswordEncrypted: fx.encSecret, AutoExecEnabled: true,
	}
	called := false
	fx.svc.SetNetExecFactory(func(srv store.Server) (ExecFunc, error) {
		return func(ctx context.Context, command string, timeout time.Duration) (ExecResult, error) {
			called = true
			return ExecResult{}, nil
		}, nil
	})

	job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "write memory", ServerIDs: []string{"net-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if called {
		t.Fatal("needs_approval netdev command must not execute directly")
	}
	r := resultByServer(fx.resultsOf(t, job.ID), "net-1")
	if r.Status != store.BatchResultAwaitingApproval {
		t.Fatalf("status = %q, want awaiting_approval", r.Status)
	}
}

// ---------------------------------------------------------------------------
// goroutine 退出路径：Stop 后扫描退出（防泄漏）
// ---------------------------------------------------------------------------

func TestStopTerminatesExpireLoop(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.svc.Start(context.Background())
	fx.svc.Stop() // Start 后立即 Stop：ticker goroutine 必须退出
	// Stop 幂等。
	fx.svc.Stop()
}

type stubSettings struct {
	store.SettingStore
	mode string
}

// Get 只实现 approval.mode 键（其余走嵌入 nil）。
func (s stubSettings) Get(_ context.Context, key string) (store.Setting, error) {
	if s.mode == "" || key != "approval.mode" {
		return store.Setting{}, store.ErrNotFound
	}
	return store.Setting{Key: key, Value: json.RawMessage(`"` + s.mode + `"`)}, nil
}

// Test2_11BatchPolicyIntegration 六期 BE-P（契约 2.11 约定 2 路径③）回归：
// ① standard（缺省）safe+auto_exec 免审直执行，审计携带 approval_mode；
// ② 注入全局门限=all 后 safe+auto_exec 也不得直执行——逐台落审批且
// 携带 ApprovalMode=all 快照（五期内联判定曾绕过全量审批）。
func Test2_11BatchPolicyIntegration(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "ok"
	fx.addServer("srv-1", true)
	ctx := context.Background()

	fx.startAudit(t)
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "ls -la", ServerIDs: []string{"srv-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	fx.waitResultStatus(t, job.ID, "srv-1", store.BatchResultExecuted)
	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = fx.writer.Flush(flushCtx)
	fx.auditSt.mu.Lock()
	foundMode := false
	for _, l := range fx.auditSt.logs {
		if l.Action == "command.executed" && strings.Contains(string(l.Detail), "approval_mode") {
			foundMode = true
		}
	}
	fx.auditSt.mu.Unlock()
	if !foundMode {
		t.Fatal("直执行审计 detail 缺 approval_mode")
	}

	fx.st.settings = stubSettings{mode: string(store.ApprovalModeAll)}
	job2, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "cat /etc/hostname", ServerIDs: []string{"srv-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch(all): %v", err)
	}
	results := fx.resultsOf(t, job2.ID)
	if len(results) != 1 || results[0].Status != store.BatchResultAwaitingApproval {
		t.Fatalf("all 档 result = %+v, want awaiting_approval（safe 不得直执行）", results)
	}
	if results[0].ApprovalID == nil {
		t.Fatal("approval id missing")
	}
	fx.apStore.mu.Lock()
	ap := fx.apStore.items[*results[0].ApprovalID]
	fx.apStore.mu.Unlock()
	if string(ap.ApprovalMode) != string(store.ApprovalModeAll) {
		t.Fatalf("approval.ApprovalMode = %q, want all（门限快照）", ap.ApprovalMode)
	}
}

// Test2_11ChatBatchHonoursGlobalMode 六期打磨 sprint 1 回归（对话内批量门限）：
// source=chat 的 Dispatch 必须先取全局门限再叠加会话覆盖。此前 chat 分支
// 把兜底值 all 当作「全局」传给 ResolveMode——会话无覆盖时有效门限恒为
// all，全局 standard 下 safe+auto_exec 仍逐台造审批、none 档同样造审批，
// 与「免审批档全部自动执行」直接矛盾。
//
// 断言矩阵（同一台 auto_exec_enabled=true 的 Linux 服务器 + safe 命令）：
//   - standard：safe+auto_exec 直执行，不落审批；
//   - none：safe 直执行，不落审批（免审批档全部自动执行）；
//   - 会话覆盖 all：即便全局 none 也逐台审批（会话覆盖优先）。
func Test2_11ChatBatchHonoursGlobalMode(t *testing.T) {
	convID := "conv-1"
	cases := []struct {
		name       string
		global     store.ApprovalMode
		convMode   *store.ApprovalMode
		wantStatus store.BatchResultStatus
		wantAppro  bool
	}{
		{
			name: "standard", global: store.ApprovalModeStandard,
			wantStatus: store.BatchResultExecuted,
		},
		{
			name: "none", global: store.ApprovalModeNone,
			wantStatus: store.BatchResultExecuted,
		},
		{
			name: "会话覆盖 all 优先于全局 none", global: store.ApprovalModeNone,
			convMode:   ptrApprovalMode(store.ApprovalModeAll),
			wantStatus: store.BatchResultAwaitingApproval, wantAppro: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, ServiceConfig{})
			fx.ssh.output = "ok"
			fx.addServer("srv-1", true) // auto_exec_enabled=true
			fx.st.settings = stubSettings{mode: string(tc.global)}
			fx.st.convs[convID] = store.Conversation{
				ID: convID, Title: "c", ApprovalMode: tc.convMode,
			}
			fx.startAudit(t)

			id := convID
			job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
				Command: "uptime", ServerIDs: []string{"srv-1"},
				Source: store.BatchSourceChat, ConversationID: &id,
			})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if !tc.wantAppro {
				fx.waitResultStatus(t, job.ID, "srv-1", tc.wantStatus)
				results := fx.resultsOf(t, job.ID)
				if len(results) != 1 || results[0].ApprovalID != nil {
					t.Fatalf("results = %+v, want 无审批直执行（全局 %s）", results, tc.global)
				}
				return
			}
			results := fx.resultsOf(t, job.ID)
			if len(results) != 1 || results[0].Status != tc.wantStatus {
				t.Fatalf("results = %+v, want %s", results, tc.wantStatus)
			}
			if results[0].ApprovalID == nil {
				t.Fatal("approval id missing（会话覆盖 all 必须逐台审批）")
			}
			fx.apStore.mu.Lock()
			ap := fx.apStore.items[*results[0].ApprovalID]
			fx.apStore.mu.Unlock()
			if string(ap.ApprovalMode) != string(store.ApprovalModeAll) {
				t.Fatalf("approval.ApprovalMode = %q, want all（会话覆盖快照）", ap.ApprovalMode)
			}
		})
	}
}

// ptrApprovalMode 便捷取址。
func ptrApprovalMode(m store.ApprovalMode) *store.ApprovalMode { return &m }

// TestBatchApproveExecutesEditedCommand 契约 2.11 约定 3：批量审批批准时
// 逐字节执行改写后命令（六期评审发现曾忽略用户改写、执行原始命令）。
func TestBatchApproveExecutesEditedCommand(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.addServer("web-1", false) // needs_approval：逐台审批
	ctx := context.Background()
	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "systemctl restart nginx", ServerIDs: []string{"web-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	results := fx.resultsOf(t, job.ID)
	if len(results) != 1 || results[0].ApprovalID == nil {
		t.Fatalf("results = %+v, want awaiting approval", results)
	}
	fx.apStore.mu.Lock()
	ap := fx.apStore.items[*results[0].ApprovalID]
	fx.apStore.mu.Unlock()

	ap.EditedCommand = "echo EDITED_ACTUAL"
	ap.Status = store.ApprovalApproved
	if err := fx.svc.OnApprovalDecided(ctx, ap, true); err != nil {
		t.Fatalf("OnApprovalDecided: %v", err)
	}
	fx.waitResultStatus(t, job.ID, "web-1", store.BatchResultExecuted)
	commands := fx.ssh.commandsOf()
	if len(commands) == 0 || commands[len(commands)-1] != "echo EDITED_ACTUAL" {
		t.Fatalf("executed = %v, want edited command（逐字节）", commands)
	}
}

// TestBatchDirectExecutionCarriesClassifiedRiskLevel 六期打磨 sprint 3（走查
// high）：直执行台的 risk_level 必须是逐台分类快照，不是写死的 "safe"。
//
// none 档 + auto_exec_enabled=true 时 `rm -rf /tmp/x`（RM_ANY，needs_approval）
// 免审批直执行——此前 CreateResult 与 command.executed 审计都写死 "safe"，
// 于是 rm -rf 在批量详情里被标成「安全」，落库与审计同错。这里同时断言
// 落库结果与审计 detail 的 risk_level 均为 needs_approval，且审计带上了
// 实际执行命令（契约 9.7，走查 medium）。
func TestBatchDirectExecutionCarriesClassifiedRiskLevel(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "removed"
	fx.addServer("wt-host", true) // auto_exec_enabled=true
	fx.st.settings = stubSettings{mode: string(store.ApprovalModeNone)}
	fx.startAudit(t)

	job, err := fx.svc.Dispatch(context.Background(), store.CreateBatchJobParams{
		Command: "rm -rf /tmp/aiops-riskcol", ServerIDs: []string{"wt-host"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	res := fx.waitResultStatus(t, job.ID, "wt-host", store.BatchResultExecuted)
	if res.RiskLevel != string(risk.LevelNeedsApproval) {
		t.Fatalf("result.risk_level = %q, want needs_approval（逐台分类快照）", res.RiskLevel)
	}
	if res.RuleID != "RM_ANY" {
		t.Fatalf("result.rule_id = %q, want RM_ANY", res.RuleID)
	}
	if len(fx.ssh.calls) != 1 {
		t.Fatalf("executed = %d, want 1（none 档直执行）", len(fx.ssh.calls))
	}

	// 审计：command.executed 的 risk_level 与 result 同口径，并携带实际命令。
	deadline := time.Now().Add(3 * time.Second)
	var detail map[string]any
	for time.Now().Before(deadline) {
		fx.auditSt.mu.Lock()
		for _, l := range fx.auditSt.logs {
			if l.Action == actionCmdExecuted {
				var parsed map[string]any
				if err := json.Unmarshal(l.Detail, &parsed); err == nil {
					detail = parsed
				}
			}
		}
		fx.auditSt.mu.Unlock()
		if detail != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if detail == nil {
		t.Fatalf("audit actions = %v, want command.executed", fx.auditActions())
	}
	if detail["risk_level"] != string(risk.LevelNeedsApproval) {
		t.Fatalf("audit risk_level = %v, want needs_approval（不得写死 safe）", detail["risk_level"])
	}
	if detail["command"] != "rm -rf /tmp/aiops-riskcol" {
		t.Fatalf("audit command = %v, want 实际执行命令（契约 9.7）", detail["command"])
	}
	if detail["approval_mode"] != string(store.ApprovalModeNone) {
		t.Fatalf("audit approval_mode = %v, want none", detail["approval_mode"])
	}
}

// TestBatchApprovalExecutionAuditCarriesCommand 契约 9.7（六期打磨 sprint 3）：
// 审批执行路径的 command.executed 也必须带实际执行命令文本——改写时是
// 改写后命令。审计页「命令摘要」列据此从恒「—」变为可读。
func TestBatchApprovalExecutionAuditCarriesCommand(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "WT-EXEC-RECEIVED"
	fx.addServer("web-1", false) // needs_approval：逐台审批
	fx.startAudit(t)
	ctx := context.Background()

	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "systemctl restart nginx", ServerIDs: []string{"web-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	results := fx.resultsOf(t, job.ID)
	if len(results) != 1 || results[0].ApprovalID == nil {
		t.Fatalf("results = %+v, want awaiting approval", results)
	}
	fx.apStore.mu.Lock()
	ap := fx.apStore.items[*results[0].ApprovalID]
	fx.apStore.mu.Unlock()
	ap.EditedCommand = "uptime"
	ap.Status = store.ApprovalApproved
	if err := fx.svc.OnApprovalDecided(ctx, ap, true); err != nil {
		t.Fatalf("OnApprovalDecided: %v", err)
	}
	fx.waitResultStatus(t, job.ID, "web-1", store.BatchResultExecuted)

	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = fx.writer.Flush(flushCtx)
	var detail map[string]any
	fx.auditSt.mu.Lock()
	for _, l := range fx.auditSt.logs {
		if l.Action != actionCmdExecuted {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal(l.Detail, &parsed); err == nil {
			detail = parsed
		}
	}
	fx.auditSt.mu.Unlock()
	if detail == nil {
		t.Fatalf("audit actions = %v, want command.executed", fx.auditActions())
	}
	if detail["command"] != "uptime" {
		t.Fatalf("audit command = %v, want uptime（改写后实际执行）", detail["command"])
	}
	if detail["risk_level"] != string(risk.LevelNeedsApproval) {
		t.Fatalf("audit risk_level = %v, want needs_approval", detail["risk_level"])
	}
}

// TestBatchApprovalAuditCarriesCommandAndApproved 契约 9.7（六期打磨 sprint 5）：
// 批量审批的两条审计行都必须能在审计页复原命令——
//   - command.approved：此前后端 0 处写入（审计页「命令批准」是死筛选项）；
//     批量路径由 OnApprovalDecided 写，detail 带 batch_job_id 与命令全文；
//   - command.denied：此前只有 approval_id + batch_job_id，「命令摘要」列
//     恒「—」，拒绝后完全看不出拒的是哪条命令。
func TestBatchApprovalAuditCarriesCommandAndApproved(t *testing.T) {
	fx := newFixture(t, ServiceConfig{})
	fx.ssh.output = "ok"
	fx.addServer("web-approve", false)
	fx.addServer("web-deny", false)
	fx.startAudit(t)
	ctx := context.Background()

	job, err := fx.svc.Dispatch(ctx, store.CreateBatchJobParams{
		Command: "rm -rf /tmp/audit5/sandbox/batchdeny", ServerIDs: []string{"web-approve", "web-deny"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	results := fx.resultsOf(t, job.ID)

	lookup := func(serverID string) store.Approval {
		t.Helper()
		apID := resultByServer(results, serverID).ApprovalID
		if apID == nil {
			t.Fatalf("%s has no approval", serverID)
		}
		fx.apStore.mu.Lock()
		defer fx.apStore.mu.Unlock()
		return fx.apStore.items[*apID]
	}

	approved := lookup("web-approve")
	if err := fx.svc.OnApprovalDecided(ctx, approved, true); err != nil {
		t.Fatalf("OnApprovalDecided approve: %v", err)
	}
	denied := lookup("web-deny")
	if err := fx.svc.OnApprovalDecided(ctx, denied, false); err != nil {
		t.Fatalf("OnApprovalDecided deny: %v", err)
	}

	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = fx.writer.Flush(flushCtx)

	find := func(action string) map[string]any {
		t.Helper()
		fx.auditSt.mu.Lock()
		defer fx.auditSt.mu.Unlock()
		for _, l := range fx.auditSt.logs {
			if l.Action != action {
				continue
			}
			var parsed map[string]any
			if err := json.Unmarshal(l.Detail, &parsed); err == nil {
				return parsed
			}
		}
		return nil
	}

	approvedDetail := find(actionCmdApproved)
	if approvedDetail == nil {
		t.Fatalf("audit actions = %v, want command.approved", fx.auditActions())
	}
	if approvedDetail["command"] != "rm -rf /tmp/audit5/sandbox/batchdeny" {
		t.Fatalf("command.approved detail.command = %v", approvedDetail["command"])
	}
	if approvedDetail["batch_job_id"] != job.ID {
		t.Fatalf("command.approved detail.batch_job_id = %v, want %s", approvedDetail["batch_job_id"], job.ID)
	}

	deniedDetail := find(actionCmdDenied)
	if deniedDetail == nil {
		t.Fatalf("audit actions = %v, want command.denied", fx.auditActions())
	}
	if deniedDetail["command"] != "rm -rf /tmp/audit5/sandbox/batchdeny" {
		t.Fatalf("command.denied detail.command = %v，审计页「命令摘要」列依赖该键", deniedDetail["command"])
	}
	if deniedDetail["batch_job_id"] != job.ID {
		t.Fatalf("command.denied detail.batch_job_id = %v, want %s", deniedDetail["batch_job_id"], job.ID)
	}

	// 一次批准只落一行 command.approved（approvals 侧对 source=batch 跳过）。
	fx.auditSt.mu.Lock()
	approvedRows := 0
	for _, l := range fx.auditSt.logs {
		if l.Action == actionCmdApproved {
			approvedRows++
		}
	}
	fx.auditSt.mu.Unlock()
	if approvedRows != 1 {
		t.Fatalf("command.approved rows = %d, want 1", approvedRows)
	}
}
