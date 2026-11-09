// batch_handlers_test.go 覆盖 M3 批量任务 REST（契约 2.5/9.2）与 decide
// handler 的 batch 审批回填分支：创建→逐台分派（真 batchx.Service + 真
// approvals.Service + memStore/fakeSSH）、列表/详情轮询、取消、404/超限
// 错误映射。同时提供 memStore.BatchJobs() 的真实 fake（替换 M1 占位桩）。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/approvals"
	"aiops/internal/batchx"
	"aiops/internal/logx"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// memStore 的 BatchJobStore fake（状态挂包级 batchState，M1 monState 模式）
// ---------------------------------------------------------------------------

type batchFakeState struct {
	mu      sync.Mutex
	jobs    map[string]store.BatchJob
	results map[string][]store.BatchResult
	next    int
}

var batchState = &batchFakeState{jobs: map[string]store.BatchJob{}, results: map[string][]store.BatchResult{}}

func resetBatchState() {
	batchState = &batchFakeState{jobs: map[string]store.BatchJob{}, results: map[string][]store.BatchResult{}}
}

func (b *batchFakeState) agg(jobID string) (store.BatchJob, []store.BatchResult) {
	job := b.jobs[jobID]
	results := b.results[jobID]
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

func (b *batchFakeState) Create(_ context.Context, p store.CreateBatchJobParams) (store.BatchJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := fmt.Sprintf("job-%d", b.next)
	job := store.BatchJob{
		ID: id, Description: p.Description, Command: p.Command,
		ServerIDs: append([]string{}, p.ServerIDs...), Source: p.Source,
		ConversationID: p.ConversationID, Status: store.BatchPending,
		TotalCount: len(p.ServerIDs), CreatedBy: p.CreatedBy,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	b.jobs[id] = job
	return job, nil
}

func (b *batchFakeState) Get(_ context.Context, id string) (store.BatchJob, []store.BatchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	job, ok := b.jobs[id]
	if !ok {
		return store.BatchJob{}, nil, store.ErrNotFound
	}
	job, results := b.agg(id)
	// 深拷贝（防共享底层数组与并发 FinishResult 的数据竞争，同 batchx fake）。
	out := make([]store.BatchResult, len(results))
	copy(out, results)
	return job, out, nil
}

func (b *batchFakeState) List(_ context.Context, limit int) ([]store.BatchJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]store.BatchJob, 0, len(b.jobs))
	for id := range b.jobs {
		job, _ := b.agg(id)
		out = append(out, job)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (b *batchFakeState) SetStatus(_ context.Context, id string, status store.BatchStatus, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	job, ok := b.jobs[id]
	if !ok {
		return store.ErrNotFound
	}
	job.Status = status
	job.UpdatedAt = at
	if status == store.BatchDone || status == store.BatchPartial || status == store.BatchFailed || status == store.BatchCancelled {
		fin := at
		job.FinishedAt = &fin
	}
	b.jobs[id] = job
	return nil
}

func (b *batchFakeState) CreateResult(_ context.Context, p store.CreateBatchResultParams) (store.BatchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := fmt.Sprintf("res-%d", b.next)
	r := store.BatchResult{
		ID: id, BatchJobID: p.BatchJobID, ServerID: p.ServerID,
		RiskLevel: p.RiskLevel, RuleID: p.RuleID, ApprovalID: p.ApprovalID,
		Status: p.Status, ErrorCode: p.ErrorCode,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	b.results[p.BatchJobID] = append(b.results[p.BatchJobID], r)
	return r, nil
}

func (b *batchFakeState) FinishResult(_ context.Context, p store.FinishBatchResultParams) (store.BatchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for jobID := range b.results {
		for i := range b.results[jobID] {
			if b.results[jobID][i].ID == p.ID {
				r := &b.results[jobID][i]
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

func (b *batchFakeState) ByApproval(_ context.Context, approvalID string) (store.BatchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, results := range b.results {
		for _, r := range results {
			if r.ApprovalID != nil && *r.ApprovalID == approvalID {
				return r, nil
			}
		}
	}
	return store.BatchResult{}, store.ErrNotFound
}

// memStore 的 BatchJobs 装配（M1 占位桩的真实替换）。
func (m *memStore) BatchJobs() store.BatchJobStore { return batchState }

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

func newBatchTestEnv(t *testing.T) (*gin.Engine, *Deps, *memStore, *fakeSSHExec) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetBatchState()
	st := newMemStore()
	cipher := testCipherForMonitor(t)
	ssh := &fakeSSHExec{}
	log := logx.New(slog.LevelError)
	apSvc := approvals.NewService(st, nil, time.Minute, time.Now, log)
	batchSvc := batchx.NewService(batchx.ServiceConfig{}, st, apSvc, ssh, nil, cipher, nil, time.Now, log)
	d := &Deps{
		Store: st, Cipher: cipher, Approvals: apSvc, Batch: batchSvc, SSH: ssh,
		Clock: func() time.Time { return time.Now().UTC() },
	}
	r := gin.New()
	protected := r.Group("/api")
	d.RegisterBatchRoutes(protected)
	return r, d, st, ssh
}

// addBatchServer 注册一台 Linux password 服务器（密文密码）。
// 复用 testCipherForMonitor 的固定密钥（与 Deps.Cipher 同源）。
func addBatchServer(t *testing.T, st *memStore, id string, autoExec bool) {
	t.Helper()
	enc, err := testCipherForMonitor(t).Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	st.mu.Lock()
	st.servers[id] = store.Server{
		ID: id, Name: id, OS: store.ServerOSLinux, Host: "127.0.0.1", Port: 22,
		Username: "root", AuthMethod: store.AuthMethodPassword,
		PasswordEncrypted: enc, AutoExecEnabled: autoExec,
	}
	st.mu.Unlock()
}

func postJSON(r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func getJSON(r *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// handler 测试
// ---------------------------------------------------------------------------

func TestBatchJobLifecycle(t *testing.T) {
	r, d, st, ssh := newBatchTestEnv(t)
	addBatchServer(t, st, "web-1", true)  // safe+auto_exec → 直接执行
	addBatchServer(t, st, "web-2", false) // safe 未开 auto_exec → 审批

	// 创建：POST /api/batch/jobs。
	w := postJSON(r, "/api/batch/jobs", createBatchJobRequest{
		Description: "批量清理日志",
		Command:     "ls -la",
		ServerIDs:   []string{"web-1", "web-2"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var job store.BatchJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatalf("job json: %v", err)
	}
	if job.TotalCount != 2 {
		t.Fatalf("total = %d", job.TotalCount)
	}
	// Dispatch 异步执行直执行台（五期评审：不再同步等待，REST 立即返回
	// 分派中间态）→ running；轮询等待 web-1 的直执行命令真实发出。
	// fakeSSHExec.calls 非并发安全但本用例单直执行台，读取前等待即可。
	deadline := time.Now().Add(5 * time.Second)
	calls := 0
	for time.Now().Before(deadline) {
		calls = ssh.callCount()
		if calls >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("ssh calls = %d, want 1（仅 auto_exec 台直执行）", calls)
	}

	// 列表。
	w = getJSON(r, "/api/batch/jobs")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	var list struct {
		Items []store.BatchJob `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %s err=%v", w.Body.String(), err)
	}

	// 详情：逐台 results 可见（前端轮询入口）。
	w = getJSON(r, "/api/batch/jobs/"+job.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	var detail struct {
		Job     store.BatchJob      `json:"job"`
		Results []store.BatchResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail json: %v", err)
	}
	if len(detail.Results) != 2 {
		t.Fatalf("results = %d", len(detail.Results))
	}
	var awaiting *store.BatchResult
	for i := range detail.Results {
		if detail.Results[i].Status == store.BatchResultAwaitingApproval {
			awaiting = &detail.Results[i]
		}
	}
	if awaiting == nil || awaiting.ApprovalID == nil {
		t.Fatalf("no awaiting_approval result: %+v", detail.Results)
	}

	// decide approve（batch 分支）：执行并回填该台。
	ap, err := d.Approvals.Decide(context.Background(), approvals.DecideRequest{ApprovalID: *awaiting.ApprovalID, Approve: true, DecidedBy: "user-1", Note: "", RequestID: "req"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if err := d.Batch.OnApprovalDecided(context.Background(), ap, true); err != nil {
		t.Fatalf("OnApprovalDecided: %v", err)
	}
	w = getJSON(r, "/api/batch/jobs/"+job.ID)
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	if detail.Job.Status != store.BatchDone {
		t.Fatalf("after approve job = %q, want done", detail.Job.Status)
	}
	if ssh.callCount() != 2 {
		t.Fatalf("ssh calls = %d, want 2（审批台执行）", ssh.callCount())
	}
}

func TestBatchJobCancelAwaitingApproval(t *testing.T) {
	r, _, st, _ := newBatchTestEnv(t)
	addBatchServer(t, st, "web-9", false)

	w := postJSON(r, "/api/batch/jobs", createBatchJobRequest{
		Command: "df -h", ServerIDs: []string{"web-9"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	var job store.BatchJob
	_ = json.Unmarshal(w.Body.Bytes(), &job)

	w = postJSON(r, "/api/batch/jobs/"+job.ID+"/cancel", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	var cancelled store.BatchJob
	_ = json.Unmarshal(w.Body.Bytes(), &cancelled)
	if cancelled.Status != store.BatchPartial {
		t.Fatalf("status = %q, want partial", cancelled.Status)
	}
	w = getJSON(r, "/api/batch/jobs/"+job.ID)
	var detail struct {
		Results []store.BatchResult `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	if detail.Results[0].Status != store.BatchResultCancelled {
		t.Fatalf("result = %q, want cancelled", detail.Results[0].Status)
	}
}

func TestBatchJobErrors(t *testing.T) {
	r, _, st, _ := newBatchTestEnv(t)
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = "srv"
		addBatchServer(t, st, fmt.Sprintf("srv-%02d", i), true)
	}
	// 51 台 → 400 BATCH_TOO_MANY_TARGETS。
	w := postJSON(r, "/api/batch/jobs", createBatchJobRequest{Command: "ls", ServerIDs: ids})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("51 targets = %d", w.Code)
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "BATCH_TOO_MANY_TARGETS" {
		t.Fatalf("code = %q", e.Error.Code)
	}
	// 空 server_ids → 400 VALIDATION_FAILED。
	w = postJSON(r, "/api/batch/jobs", createBatchJobRequest{Command: "ls"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty ids = %d", w.Code)
	}
	// 不存在的 job → 404 BATCH_JOB_NOT_FOUND。
	w = getJSON(r, "/api/batch/jobs/job-999")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "BATCH_JOB_NOT_FOUND" {
		t.Fatalf("code = %q", e.Error.Code)
	}
}

// TestBatchApprovalExecutionExitCode 走查回归（五期 Sprint5）：批量审批
// approve 后 decide 响应 execution.exit_code 必须始终存在（契约 8.9
// ApprovalExecution.exit_code 必填；通道失败约定 -1，对齐 console 路径
// containers_handlers 的口径）。此前通道失败时回填 ExitCode=nil 使键被
// 省略，前端 toast 插值出「退出码 undefined」（/batch 详情抽屉与对话
// batch_execute 深链两处复现）。
func TestBatchApprovalExecutionExitCode(t *testing.T) {
	// decide 经完整 handler 链路（Approvals.Decide → OnApprovalDecided
	// 同步执行 → batchApprovalExecution 读回填），与生产路径逐字节一致。
	type executionSummary struct {
		ExitCode  *int   `json:"exit_code"`
		ErrorCode string `json:"error_code"`
		Status    string `json:"status"`
	}
	runDecide := func(t *testing.T, execFn func(context.Context, sshx.Target, sshx.RunRequest) (sshx.RunResult, error)) executionSummary {
		t.Helper()
		r, d, st, ssh := newBatchTestEnv(t)
		addBatchServer(t, st, "web-1", false) // 未开 auto_exec → 审批
		ssh.execFn = execFn
		r.POST("/api/approvals/:id/decide", d.handleDecideApproval)

		w := postJSON(r, "/api/batch/jobs", createBatchJobRequest{
			Command: "ls -la", ServerIDs: []string{"web-1"},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
		var job store.BatchJob
		_ = json.Unmarshal(w.Body.Bytes(), &job)

		// 轮询异步 Dispatch 回填出 awaiting_approval（5s 截止防死循环）。
		var approvalID string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			w = getJSON(r, "/api/batch/jobs/"+job.ID)
			var detail struct {
				Results []store.BatchResult `json:"results"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &detail)
			if len(detail.Results) == 1 && detail.Results[0].ApprovalID != nil {
				approvalID = *detail.Results[0].ApprovalID
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if approvalID == "" {
			t.Fatalf("5s 内未回填 awaiting_approval result")
		}

		w = postJSON(r, "/api/approvals/"+approvalID+"/decide", map[string]string{"decision": "approve"})
		if w.Code != http.StatusOK {
			t.Fatalf("decide = %d: %s", w.Code, w.Body.String())
		}
		var res struct {
			Execution executionSummary `json:"execution"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("decide json: %v", err)
		}
		return res.Execution
	}

	t.Run("通道失败 exit_code=-1（键不得缺省）", func(t *testing.T) {
		// 目标 SSH 不可达：Execute 返回连接错误 → 回填 ExitCode=nil。
		exec := runDecide(t, func(context.Context, sshx.Target, sshx.RunRequest) (sshx.RunResult, error) {
			return sshx.RunResult{}, errors.New("connection refused")
		})
		if exec.ExitCode == nil {
			t.Fatalf("execution.exit_code 键缺省（回归：前端显示「退出码 undefined」）: %+v", exec)
		}
		if *exec.ExitCode != -1 {
			t.Fatalf("execution.exit_code = %d, want -1（通道失败口径）", *exec.ExitCode)
		}
		if exec.ErrorCode != "SSH_EXEC_FAILED" {
			t.Fatalf("execution.error_code = %q, want SSH_EXEC_FAILED", exec.ErrorCode)
		}
		if exec.Status != "failed" {
			t.Fatalf("execution.status = %q, want failed", exec.Status)
		}
	})

	t.Run("执行成功 exit_code 透传", func(t *testing.T) {
		exec := runDecide(t, func(context.Context, sshx.Target, sshx.RunRequest) (sshx.RunResult, error) {
			return sshx.RunResult{ExitCode: 3, Combined: "done"}, nil // 非 0 退出码更具区分度
		})
		if exec.ExitCode == nil || *exec.ExitCode != 3 {
			t.Fatalf("execution.exit_code = %v, want 3", exec.ExitCode)
		}
		if exec.ErrorCode != "" {
			t.Fatalf("execution.error_code = %q, want 空", exec.ErrorCode)
		}
		if exec.Status != "executed" {
			t.Fatalf("execution.status = %q, want executed", exec.Status)
		}
	})
}
