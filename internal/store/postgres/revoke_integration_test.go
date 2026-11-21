package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/rbacops"
	"aiops/internal/store"
)

// TestRevokePendingByUserIntegration 覆盖契约 12.3 第 15/22 条的 SQL 路径：
// 停用/吊销账号时，pending 审批 → expired、approved-but-unconsumed grant →
// expired、awaiting_approval tool_call → expired/APPROVAL_CANCELLED、
// batch awaiting_approval → cancelled 并重算 job。
//
// 这是评审 high 的核心回归：修复前账号重新启用后旧审批仍可被批准并触发
// 执行（实测 ssh_exec 走到了执行段）。
func TestRevokePendingByUserIntegration(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	approver, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "rbac-revoke-approver", PasswordHash: "$2a$12$rbacapprover",
		Role: store.RoleApprover, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-revoke", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	convID := mustConversation(t, owner)
	now := time.Now().UTC()

	// 1) chat 审批 + awaiting_approval tool_call。
	turnID := uuid.NewString()
	tc, err := testDB.ToolCalls().Create(ctx, store.CreateToolCallParams{
		ConversationID: convID, TurnID: turnID, ProviderCallID: "call-1",
		Name: "ssh_exec", Arguments: []byte(`{"command":"systemctl restart nginx"}`),
		RiskLevel: "needs_approval", Status: store.ToolAwaitingApproval, StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingChat, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		ConversationID: &convID, ToolCallID: &tc.ID, Source: "chat",
		ServerID: &server.ID, ServerName: server.Name, Command: "systemctl restart nginx",
		RiskLevel: "needs_approval", ExpiresAt: now.Add(time.Hour), CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2) 已批准但未消费的 terminal grant。
	kind := "term"
	expires := now.Add(30 * time.Minute)
	grant, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "terminal", ServerID: &server.ID, ServerName: server.Name,
		Command: "", RiskLevel: "safe", GrantKind: &kind, GrantExpiresAt: &expires,
		ExpiresAt: expires, CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: grant.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// 3) batch awaiting_approval 台。
	job, err := testDB.BatchJobs().Create(ctx, store.CreateBatchJobParams{
		Description: "revoke", Command: "uptime", ServerIDs: []string{server.ID},
		Source: store.BatchSourceConsole, CreatedBy: &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	batchApproval, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "batch", ServerID: &server.ID, ServerName: server.Name,
		Command: "uptime", RiskLevel: "needs_approval",
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
		BatchJobID: job.ID, ServerID: server.ID, RiskLevel: "needs_approval",
		ApprovalID: &batchApproval.ID, Status: store.BatchResultAwaitingApproval,
	}); err != nil {
		t.Fatal(err)
	}

	// 执行吊销收敛（HTTP handler 在同一事务内调用的同一函数）。
	if err := testDB.WithTx(ctx, func(tx store.Store) error {
		return rbacops.RevokePending(ctx, tx, owner, now)
	}); err != nil {
		t.Fatalf("RevokePending: %v", err)
	}

	got, err := testDB.Approvals().Get(ctx, pendingChat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ApprovalExpired || got.Note != rbacops.NoteUserRevoked {
		t.Fatalf("chat approval = %s / %q, want expired / %s", got.Status, got.Note, rbacops.NoteUserRevoked)
	}
	// DB 层拦截：重新启用后旧审批也不能被批准。
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: pendingChat.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: now,
	}); err == nil {
		t.Fatal("revoked approval must not be decidable")
	}
	tcRow, err := testDB.ToolCalls().ByID(ctx, tc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tcRow.Status != store.ToolExpired || tcRow.ErrorCode != "APPROVAL_CANCELLED" {
		t.Fatalf("tool call = %s / %s, want expired / APPROVAL_CANCELLED", tcRow.Status, tcRow.ErrorCode)
	}
	grantRow, err := testDB.Approvals().Get(ctx, grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grantRow.Status != store.ApprovalExpired {
		t.Fatalf("approved unconsumed grant = %s, want expired", grantRow.Status)
	}
	// 已批准 grant 不再可消费。
	if _, err := store.GrantStoreOf(testDB).ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: grant.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now,
	}); err == nil {
		t.Fatal("expired grant must not be consumable")
	}

	batchJob, results, err := testDB.BatchJobs().Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != store.BatchResultCancelled {
		t.Fatalf("batch results = %+v, want cancelled", results)
	}
	if batchJob.Status != store.BatchPartial {
		t.Fatalf("batch job status = %s, want partial", batchJob.Status)
	}
	// 再次收敛必须幂等（无待处理行时返回 0 且不报错）。
	if err := testDB.WithTx(ctx, func(tx store.Store) error {
		return rbacops.RevokePending(ctx, tx, owner, now)
	}); err != nil {
		t.Fatalf("second RevokePending: %v", err)
	}

	// 走查第 3 轮 high：收敛动作必须留下审计，审计员才能证明授权是被系统
	// 作废而不是丢失。outbox 由独立 worker 追加，直接读 outbox 载荷断言。
	approvalExpired := outboxPayloadActions(t, "approval.expired")
	if approvalExpired[pendingChat.ID] != 1 {
		t.Fatalf("approval.expired rows for %s = %d, want 1 (outbox=%v)",
			pendingChat.ID, approvalExpired[pendingChat.ID], approvalExpired)
	}
	grantExpired := outboxPayloadActions(t, "terminal.grant_expired")
	// 已批准未消费的 grant 与 pending 的 grant 各一行（本用例只造了 approved 那条）。
	if grantExpired[grant.ID] != 1 {
		t.Fatalf("terminal.grant_expired rows for %s = %d, want 1 (outbox=%v)",
			grant.ID, grantExpired[grant.ID], grantExpired)
	}
}

// TestRevokePendingDirectAuditAppendsChain 覆盖主机侧离线路径：CLI 没有
// 常驻 worker，若只写 outbox，`aiops admin reset` 返回后审计页仍看不到
// 收敛记录。RevokePendingDirectAudit 必须在同一事务内直接追加
// audit_logs 哈希链（契约 12.3 第 19 条「命令返回时审计已可查」）。
func TestRevokePendingDirectAuditAppendsChain(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	now := time.Now().UTC()
	convID := mustConversation(t, owner)
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		ConversationID: &convID, Source: "console", Command: "systemctl restart nginx",
		RiskLevel: "needs_approval", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	kind := "term"
	expires := now.Add(30 * time.Minute)
	grant, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "terminal", ServerName: "web-1",
		RiskLevel: "safe", GrantKind: &kind, GrantExpiresAt: &expires,
		ExpiresAt: expires, CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := testDB.WithTx(ctx, func(tx store.Store) error {
		return rbacops.RevokeSessionsAndAuthorizations(ctx, tx, owner, now)
	}); err != nil {
		t.Fatalf("RevokeSessionsAndAuthorizations: %v", err)
	}

	// 审计必须立即可查（不依赖 worker）。
	rows, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{Action: "approval.expired", Limit: 10})
	if err != nil {
		t.Fatalf("list approval.expired: %v", err)
	}
	if len(rows) != 1 || rows[0].ResourceID != ap.ID {
		t.Fatalf("approval.expired rows = %+v, want one for %s", rows, ap.ID)
	}
	var detail map[string]any
	if err := json.Unmarshal(rows[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["reason"] != rbacops.NoteUserRevoked {
		t.Fatalf("detail.reason = %v, want %s", detail["reason"], rbacops.NoteUserRevoked)
	}
	if rows[0].Hash == "" || rows[0].PrevHash == "" {
		t.Fatalf("audit row must carry hash chain fields: %+v", rows[0])
	}

	grantRows, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{Action: "terminal.grant_expired", Limit: 10})
	if err != nil {
		t.Fatalf("list terminal.grant_expired: %v", err)
	}
	if len(grantRows) != 1 || grantRows[0].ResourceID != grant.ID {
		t.Fatalf("terminal.grant_expired rows = %+v, want one for %s", grantRows, grant.ID)
	}
	// 收敛审计不得走 outbox（离线路径直接追加哈希链，没有 worker 消费）。
	// 注意 Create 本身仍会写 outbox（approval.created / grant_requested），
	// 同一库里也可能有其它用例的残留行——只断言本次两条审批 ID。
	if got := outboxPayloadActions(t, "approval.expired")[ap.ID]; got != 0 {
		t.Fatalf("offline path must not enqueue approval.expired outbox row for %s, got %d", ap.ID, got)
	}
	if got := outboxPayloadActions(t, "terminal.grant_expired")[grant.ID]; got != 0 {
		t.Fatalf("offline path must not enqueue terminal.grant_expired outbox row for %s, got %d", grant.ID, got)
	}
}

// outboxPayloadActions 按 action 统计 audit_outbox 中未处理载荷的 resource_id。
// 与 rbacops 的事务内写入同源（未处理即代表本次事务已提交）。
func outboxPayloadActions(t *testing.T, action string) map[string]int {
	t.Helper()
	entries, err := testDB.PendingAuditOutbox(context.Background(), 500)
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	out := map[string]int{}
	for _, e := range entries {
		// 载荷是 store.CreateAuditParams 的 JSON（字段名即 Go 字段名）。
		var payload store.CreateAuditParams
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action == action {
			out[payload.ResourceID]++
		}
	}
	return out
}

// TestExpireStaleGrantsIntegration 覆盖走查第 3 轮 medium：已批准但已过
// grant_expires_at 的终端授权必须收敛为 expired（note=grant_expired），
// 而不是永远停在 approved 由前端临时推算。
func TestExpireStaleGrantsIntegration(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	approver, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "rbac-grant-approver", PasswordHash: "$2a$12$rbacgrant",
		Role: store.RoleApprover, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-stale-grant", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mkGrant := func(kind string, expires time.Time) store.Approval {
		grant, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
			Source: "terminal", ServerID: &server.ID, ServerName: server.Name,
			Command: "", RiskLevel: "safe", GrantKind: &kind, GrantExpiresAt: &expires,
			ExpiresAt: expires, CreatedAt: now, RequestedBy: owner,
		})
		if err != nil {
			t.Fatal(err)
		}
		decidedAt := now
		if expires.After(now) {
			decidedAt = now
		} else {
			// 第 11 轮走查 high：当前时刻批准过期授权会被 TTL 兜底拒绝；
			// 构造「批准后已过期」的历史行时把决策时刻放在 expires_at 之前。
			decidedAt = expires.Add(-time.Second)
		}
		if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
			ID: grant.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: decidedAt,
		}); err != nil {
			t.Fatal(err)
		}
		return grant
	}
	// 开发库上可能同时跑着 app 实例（后台 grant 收敛器按真实时钟扫描）。
	// stale 行若直接用「过去」的 grant_expires_at，会在本测试读取前被外部
	// 收敛器置为 expired，ExpireStaleGrants 便返回 0 行（实测偶发）。
	// 改用「真实时钟仍在未来、但传给函数的 now 已过」的 expires_at 隔离外部
	// 扫描器：被测语义（grant_expires_at <= now 才收敛）不变。
	staleExpires := now.Add(time.Hour)
	expireNow := staleExpires.Add(time.Minute)
	stale := mkGrant("term", staleExpires)
	fresh := mkGrant("rdp", now.Add(2*time.Hour))

	expiry := store.GrantExpiryStoreOf(testDB)
	if expiry == nil {
		t.Fatal("postgres approval store does not implement GrantExpiryStore")
	}
	var expired []store.Approval
	if err := testDB.WithTx(ctx, func(tx store.Store) error {
		rows, err := store.GrantExpiryStoreOf(tx).ExpireStaleGrants(ctx, expireNow, 200)
		expired = rows
		return err
	}); err != nil {
		t.Fatalf("ExpireStaleGrants: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != stale.ID {
		t.Fatalf("expired rows = %+v, want only %s", expired, stale.ID)
	}

	got, err := testDB.Approvals().Get(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ApprovalExpired || got.Note != "grant_expired" {
		t.Fatalf("stale grant = %s / %q, want expired / grant_expired", got.Status, got.Note)
	}
	freshRow, err := testDB.Approvals().Get(ctx, fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if freshRow.Status != store.ApprovalApproved {
		t.Fatalf("fresh grant = %s, want approved (未到期不得收敛)", freshRow.Status)
	}
	// 过期后不可再消费。
	if _, err := store.GrantStoreOf(testDB).ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: stale.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now,
	}); err == nil {
		t.Fatal("expired stale grant must not be consumable")
	}
	// 幂等：第二轮无待收敛行。
	var again []store.Approval
	if err := testDB.WithTx(ctx, func(tx store.Store) error {
		rows, err := store.GrantExpiryStoreOf(tx).ExpireStaleGrants(ctx, expireNow, 200)
		again = rows
		return err
	}); err != nil {
		t.Fatalf("second ExpireStaleGrants: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second sweep expired %d rows, want 0", len(again))
	}
}

// TestApproveBreakGlassIntegration：主机侧 break-glass 只批准 pending 行、
// 置 break_glass=true、decided_by 保持 NULL，重复批准返回 ErrConflict。
func TestApproveBreakGlassIntegration(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-bg", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "console", ServerID: &server.ID, ServerName: server.Name,
		Command: "uptime", RiskLevel: "safe", ExpiresAt: now.Add(time.Hour),
		CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	bg := store.BreakGlassStoreOf(testDB)
	if bg == nil {
		t.Fatal("postgres approval store does not implement BreakGlassStore")
	}
	approved, err := bg.ApproveBreakGlass(ctx, ap.ID, "single admin recovery", now)
	if err != nil {
		t.Fatalf("ApproveBreakGlass: %v", err)
	}
	if approved.Status != store.ApprovalApproved || !approved.BreakGlass || approved.DecidedBy != nil {
		t.Fatalf("approved = %+v", approved)
	}
	if approved.Note != "single admin recovery" {
		t.Fatalf("note = %q", approved.Note)
	}
	if _, err := bg.ApproveBreakGlass(ctx, ap.ID, "again", now); err != store.ErrConflict {
		t.Fatalf("second break-glass = %v, want ErrConflict", err)
	}
}

// mustConversation 创建一个归属 owner 的会话（approvals/tool_calls 的 FK）。
func mustConversation(t *testing.T, owner string) string {
	t.Helper()
	conv, err := testDB.Conversations().Create(context.Background(), store.CreateConversationParams{
		Title: "revoke-conv", UserID: owner,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	return conv.ID
}
