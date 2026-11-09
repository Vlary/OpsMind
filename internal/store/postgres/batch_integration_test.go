// batch_integration_test.go 是五期 M3 的 store 集成测试（真实 PostgreSQL，
// 契约 9.2 G11/G36 的持久层部分）：batch_jobs/batch_results CRUD、聚合计数
// 派生（TotalCount/DoneCount 查询时 COUNT）、审批台 approval_id 反查回填、
// SetStatus 终态写 finished_at、服务器删除级联、ByApproval 0 行 ErrNotFound。
package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

func cleanBatchTables(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE servers, batch_jobs, batch_results, approvals
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate batch tables: %v", err)
	}
}

func TestBatchJobsCRUD(t *testing.T) {
	cleanBatchTables(t)
	ctx := context.Background()

	srv1, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "batch-web-1", Host: "10.0.0.1", Port: 22, Username: "root",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatalf("seed server 1: %v", err)
	}
	srv2, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "batch-web-2", Host: "10.0.0.2", Port: 22, Username: "root",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatalf("seed server 2: %v", err)
	}

	// Create：pending、TotalCount=server_ids 台数。
	created, err := testDB.BatchJobs().Create(ctx, store.CreateBatchJobParams{
		Description: "重启所有 web 服务器",
		Command:     "systemctl restart nginx",
		ServerIDs:   []string{srv1.ID, srv2.ID},
		Source:      store.BatchSourceChat,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if created.Status != store.BatchPending || created.TotalCount != 2 || created.Source != store.BatchSourceChat {
		t.Fatalf("created = %+v", created)
	}
	if len(created.ServerIDs) != 2 {
		t.Fatalf("server_ids = %v", created.ServerIDs)
	}

	// 审批台：先建 source=batch 审批（conversation/tool_call NULL）。
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "batch", ServerID: &srv1.ID, ServerName: srv1.Name,
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "DEFAULT_ASK", Reason: "批量操作",
		ExpiresAt: time.Now().Add(time.Minute), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	if ap.ConversationID != nil || ap.ToolCallID != nil {
		t.Fatalf("batch approval must have NULL context: %+v", ap)
	}

	// CreateResult：审批台（带 approval_id）+ 跳过台（带 error_code）。
	awaiting, err := testDB.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
		BatchJobID: created.ID, ServerID: srv1.ID, RiskLevel: "needs_approval",
		RuleID: "DEFAULT_ASK", Status: store.BatchResultAwaitingApproval, ApprovalID: &ap.ID,
	})
	if err != nil {
		t.Fatalf("create awaiting result: %v", err)
	}
	if _, err := testDB.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
		BatchJobID: created.ID, ServerID: srv2.ID, RiskLevel: "forbidden",
		RuleID: "FS_DESTROY", Status: store.BatchResultSkipped, ErrorCode: "COMMAND_FORBIDDEN",
	}); err != nil {
		t.Fatalf("create skipped result: %v", err)
	}

	// Get：聚合计数派生（skipped 属终态 → DoneCount=1）；results 按输入顺序。
	job, results, err := testDB.BatchJobs().Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.TotalCount != 2 || job.DoneCount != 1 {
		t.Fatalf("agg counts = %d/%d, want 2/1", job.TotalCount, job.DoneCount)
	}
	if len(results) != 2 || results[0].ServerID != srv1.ID || results[1].ErrorCode != "COMMAND_FORBIDDEN" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].ServerName != "batch-web-1" {
		t.Fatalf("server_name join = %q", results[0].ServerName)
	}
	if results[0].ApprovalID == nil || *results[0].ApprovalID != ap.ID {
		t.Fatalf("approval_id = %v", results[0].ApprovalID)
	}

	// ByApproval：decide 回填入口定位。
	got, err := testDB.BatchJobs().ByApproval(ctx, ap.ID)
	if err != nil || got.ID != awaiting.ID {
		t.Fatalf("ByApproval = %+v err=%v", got, err)
	}

	// FinishResult：执行后回填 exit_code/output/duration。
	exit := 0
	finished, err := testDB.BatchJobs().FinishResult(ctx, store.FinishBatchResultParams{
		ID: awaiting.ID, Status: store.BatchResultExecuted, ExitCode: &exit,
		Output: "restarted", DurationMS: 812, FinishedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("finish result: %v", err)
	}
	if finished.Status != store.BatchResultExecuted || finished.Output != "restarted" || finished.ExitCode == nil {
		t.Fatalf("finished = %+v", finished)
	}
	// 回填后聚合：两台均终态 → DoneCount=2。
	_, results, _ = testDB.BatchJobs().Get(ctx, created.ID)
	_ = results

	// SetStatus 终态 → finished_at 写入。
	now := time.Now().UTC()
	if err := testDB.BatchJobs().SetStatus(ctx, created.ID, store.BatchPartial, now); err != nil {
		t.Fatalf("set status: %v", err)
	}
	job, _, err = testDB.BatchJobs().Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after status: %v", err)
	}
	if job.Status != store.BatchPartial || job.FinishedAt == nil {
		t.Fatalf("job = %+v, want partial with finished_at", job)
	}
	if job.DoneCount != 2 {
		t.Fatalf("done_count = %d, want 2", job.DoneCount)
	}

	// List：最近 job 可见（created_at DESC）。
	jobs, err := testDB.BatchJobs().List(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list = %d err=%v", len(jobs), err)
	}

	// 服务器删除 → batch_results 级联删除（FK ON DELETE CASCADE，契约 4.26）。
	if err := testDB.Servers().Delete(ctx, srv2.ID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	_, results, _ = testDB.BatchJobs().Get(ctx, created.ID)
	if len(results) != 1 {
		t.Fatalf("results after server delete = %d, want 1 (CASCADE)", len(results))
	}

	// ByApproval 0 行 → ErrNotFound（契约 3.4）。
	if _, err := testDB.BatchJobs().ByApproval(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByApproval missing = %v, want ErrNotFound", err)
	}
	// SetStatus 不存在 → ErrNotFound。
	if err := testDB.BatchJobs().SetStatus(ctx, "00000000-0000-0000-0000-000000000000", store.BatchDone, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetStatus missing = %v, want ErrNotFound", err)
	}
	// List limit 非法 → ErrInvalid。
	if _, err := testDB.BatchJobs().List(ctx, 0); err == nil {
		// limit=0 走缺省 50（pageLimit 语义），不是错误——仅断言 201 越界。
		_, err = testDB.BatchJobs().List(ctx, 201)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("List 201 = %v, want ErrInvalid", err)
		}
	}
}

// TestBatchResultsCarryEditedCommand 六期打磨 sprint 5：批量结果查询必须带出
// 审批的用户改写命令（batch_results.approval_id → approvals.edited_command）。
//
// 背景：用户在批量审批行把命令改为 uptime 并批准后，执行的是 uptime；此前
// GET /api/batch/jobs/:id 的 results 完全没有改写线索（前端只能显示 job 的
// 原文命令，页面与事实背离）。逐行回取审批在 50 台 job 上要打 50 次 HTTP，
// 因此在 store 层 LEFT JOIN approvals 一次读出。
func TestBatchResultsCarryEditedCommand(t *testing.T) {
	cleanBatchTables(t)
	ctx := context.Background()

	srv, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "batch-edit-1", Host: "10.0.0.9", Port: 22, Username: "root",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	// users 表 CHECK (username = 'admin')：全库只有一个管理员账号，且
	// cleanBatchTables 不 TRUNCATE users（其他用例共用）——先查后建。
	decider, err := testDB.Users().ByUsername(ctx, "admin")
	if err != nil {
		decider, err = testDB.Users().Create(ctx, store.CreateUserParams{
			Username: "admin", PasswordHash: "$2a$12$abcdefghijklmnopqrstuv",
		})
		if err != nil {
			t.Fatalf("seed decider: %v", err)
		}
	}
	job, err := testDB.BatchJobs().Create(ctx, store.CreateBatchJobParams{
		Command: "rm -rf /tmp/x", ServerIDs: []string{srv.ID}, Source: store.BatchSourceConsole,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "batch", ServerID: &srv.ID, ServerName: srv.Name,
		Command: "rm -rf /tmp/x", RiskLevel: "needs_approval",
		RuleID: "RM_ANY", Reason: "dangerous",
		ExpiresAt: time.Now().Add(time.Minute), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: ap.ID, Status: store.ApprovalApproved, DecidedBy: decider.ID,
		EditedCommand: "uptime", EditedRiskLevel: "safe", EditedRuleID: "READ_ONLY",
		DecidedAt: time.Now(),
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	created, err := testDB.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
		BatchJobID: job.ID, ServerID: srv.ID, RiskLevel: "needs_approval",
		RuleID: "RM_ANY", Status: store.BatchResultAwaitingApproval, ApprovalID: &ap.ID,
	})
	if err != nil {
		t.Fatalf("create result: %v", err)
	}
	// 三处读取路径都要带 edited_command：CreateResult 返回、Get、ByApproval。
	check := func(label string, got store.BatchResult) {
		t.Helper()
		if got.EditedCommand != "uptime" {
			t.Fatalf("%s edited_command = %q, want uptime", label, got.EditedCommand)
		}
	}
	check("CreateResult", created)

	_, results, err := testDB.BatchJobs().Get(ctx, job.ID)
	if err != nil || len(results) != 1 {
		t.Fatalf("get = %d results, err=%v", len(results), err)
	}
	check("Get", results[0])

	exit := 0
	finished, err := testDB.BatchJobs().FinishResult(ctx, store.FinishBatchResultParams{
		ID: created.ID, Status: store.BatchResultExecuted, ExitCode: &exit,
		Output: "up 3 days", FinishedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	check("FinishResult", finished)

	byApproval, err := testDB.BatchJobs().ByApproval(ctx, ap.ID)
	if err != nil {
		t.Fatalf("ByApproval: %v", err)
	}
	check("ByApproval", byApproval)

	// 未改写的审批（edited_command 空串）→ 空串，不是脏数据。
	ap2, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "batch", ServerID: &srv.ID, ServerName: srv.Name,
		Command: "df -h", RiskLevel: "needs_approval", RuleID: "DEFAULT_ASK",
		ExpiresAt: time.Now().Add(time.Minute), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create approval 2: %v", err)
	}
	plain, err := testDB.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
		BatchJobID: job.ID, ServerID: srv.ID, RiskLevel: "needs_approval",
		RuleID: "DEFAULT_ASK", Status: store.BatchResultAwaitingApproval, ApprovalID: &ap2.ID,
	})
	if err != nil {
		t.Fatalf("create result 2: %v", err)
	}
	if plain.EditedCommand != "" {
		t.Fatalf("未改写 edited_command = %q, want empty", plain.EditedCommand)
	}
}
