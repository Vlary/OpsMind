// rbac_revoke_test.go 覆盖第 1 轮走查的高危项：
//
//  1. 停用/降级/吊销会话/重置密码必须收敛该账号的 pending 审批、
//     awaiting_approval tool_call 与已批准未消费的 terminal grant——
//     re-enable 后旧审批不得再被批准并触发执行（评审 high 实测 ssh_exec
//     走到了执行段）；
//  2. 登出必须关闭该 auth session 名下的终端/RDP WS；
//  3. 单管理员降级模式下审批者批准、开终端、放宽 policy 一律失败关闭；
//  4. 审批发起人被停用时的拒绝审计必须记在调用者（审批人）名下。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/authz"
	"aiops/internal/store"
)

// pendingApprovalFor 造一条指定发起人的 pending 审批（含 tool_call 反查用 ID）。
func pendingApprovalFor(t *testing.T, env *testEnv, requestedBy, toolCallID string) store.Approval {
	t.Helper()
	ap, err := env.apprSvc.Create(context.Background(), approvals.CreateRequest{
		ConversationID: testConvPtr("conv-1"), ToolCallID: testConvPtr(toolCallID),
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "SERVICE_CHANGE", Reason: "service state change",
		RequestedBy: requestedBy,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	return ap
}

// approvalStatus 读审批当前状态。
func approvalStatus(t *testing.T, env *testEnv, id string) store.ApprovalStatus {
	t.Helper()
	ap, err := env.st.Approvals().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get approval %s: %v", id, err)
	}
	return ap.Status
}

// TestDisableUserRevokesPendingApprovals：停用账号后，其 pending 审批必须
// 立即 expired；重新启用后 decide 也只能拿到 409（不能执行）。
func TestDisableUserRevokesPendingApprovals(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-revoke-1")
	if approvalStatus(t, env, ap.ID) != store.ApprovalPending {
		t.Fatalf("initial status = %s", approvalStatus(t, env, ap.ID))
	}

	// 停用发起人（requester-1）。
	rec := env.do(http.MethodPatch, "/api/users/"+testRequesterID, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := approvalStatus(t, env, ap.ID); got != store.ApprovalExpired {
		t.Fatalf("pending approval after disable = %s, want expired", got)
	}
	// 列表不再出现 pending 行（界面承诺「待处理审批会立即失效」）。
	rec = env.do(http.MethodGet, "/api/approvals?status=pending", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending list = %d", rec.Code)
	}
	var pending struct {
		Items []store.Approval `json:"items"`
	}
	decodeBody(t, rec, &pending)
	for _, item := range pending.Items {
		if item.ID == ap.ID {
			t.Fatalf("revoked approval still listed as pending: %+v", item)
		}
	}

	// 重新启用：旧审批仍然 expired，不能批准。
	rec = env.do(http.MethodPatch, "/api/users/"+testRequesterID, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decide after re-enable = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if got := approvalStatus(t, env, ap.ID); got != store.ApprovalExpired {
		t.Fatalf("approval after re-enable = %s, want expired", got)
	}
}

// TestDisableUserConvergesAwaitingToolCall：停用收敛同时把由该审批驱动的
// awaiting_approval tool_call 置 expired/APPROVAL_CANCELLED。
func TestDisableUserConvergesAwaitingToolCall(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	// 造一条 awaiting_approval tool_call（审批 tool_call_id 指向它）。
	env.st.mu.Lock()
	env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
		ID: "tc-converge", ConversationID: "conv-1", Seq: 1,
		Name: "ssh_exec", Status: store.ToolAwaitingApproval,
	})
	env.st.mu.Unlock()
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-converge")

	rec := env.do(http.MethodPatch, "/api/users/"+testRequesterID, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d body = %s", rec.Code, rec.Body.String())
	}
	tc, err := env.st.ToolCalls().ByID(context.Background(), "tc-converge")
	if err != nil {
		t.Fatalf("load tool call: %v", err)
	}
	if tc.Status != store.ToolExpired || tc.ErrorCode != "APPROVAL_CANCELLED" {
		t.Fatalf("tool call = %+v, want expired/APPROVAL_CANCELLED (approval %s)", tc, ap.ID)
	}
}

// TestRevokeSessionsExpiresApprovedGrant：吊销会话同样作废「已批准但未
// 消费」的 terminal grant（re-enable/重新登录后不得凭旧批准开 shell），
// 并在同一事务写 terminal.grant_expired 审计（走查第 3 轮 high）。
func TestRevokeSessionsExpiresApprovedGrant(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	serverID := "11111111-1111-4111-8111-111111111111"
	kind := "term"
	now := time.Now().UTC()
	expires := now.Add(30 * time.Minute)
	grantID := "22222222-2222-4222-8222-222222222222"
	env.st.mu.Lock()
	env.st.approvals[grantID] = store.Approval{
		ID: grantID, Source: "terminal", ServerID: &serverID, ServerName: "web-1",
		Status: store.ApprovalApproved, RequestedBy: testRequesterID,
		GrantKind: &kind, GrantExpiresAt: &expires, RequestedAt: now, ExpiresAt: expires,
	}
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/users/"+testRequesterID+"/revoke-sessions", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := approvalStatus(t, env, grantID); got != store.ApprovalExpired {
		t.Fatalf("approved unconsumed grant after revoke = %s, want expired", got)
	}
	// 审计：terminal 来源的授权走 terminal.grant_expired（与批准/消费同一
	// 生命周期），与业务行同事务（outbox）。命令类审批走 approval.expired，
	// 见 TestDisableUserAuditsExpiredApprovals。
	if n := outboxActionFor(t, env, "terminal.grant_expired", grantID); n != 1 {
		t.Fatalf("terminal.grant_expired rows for %s = %d, want 1", grantID, n)
	}
}

// outboxActionFor 统计 outbox 中指定 action + resource_id 的行数。
func outboxActionFor(t *testing.T, env *testEnv, action, resourceID string) int {
	t.Helper()
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	n := 0
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action == action && payload.ResourceID == resourceID {
			n++
		}
	}
	return n
}

// TestDisableUserAuditsExpiredApprovals：停用/吊销收敛 pending 审批时必须
// 写 approval.expired（reason=rbac:user_revoked），否则审计页只有
// approval.created，审计员无法证明审批是被系统作废还是丢失
// （走查第 3 轮 high）。
func TestDisableUserAuditsExpiredApprovals(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-revoke-audit")

	rec := env.do(http.MethodPatch, "/api/users/"+testRequesterID, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := approvalStatus(t, env, ap.ID); got != store.ApprovalExpired {
		t.Fatalf("approval = %s, want expired", got)
	}
	if n := outboxActionFor(t, env, "approval.expired", ap.ID); n != 1 {
		t.Fatalf("approval.expired rows for %s = %d, want 1（审计缺失=无法证明系统作废）", ap.ID, n)
	}
	// detail 必须带 reason 与命令，审计页才能定位「哪条审批、哪条命令」。
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	found := false
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action != "approval.expired" || payload.ResourceID != ap.ID {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal(payload.Detail, &detail); err != nil {
			t.Fatalf("detail: %v", err)
		}
		if detail["reason"] != "rbac:user_revoked" {
			t.Fatalf("detail.reason = %v, want rbac:user_revoked", detail["reason"])
		}
		if detail["command"] != "systemctl restart nginx" {
			t.Fatalf("detail.command = %v", detail["command"])
		}
		if detail["requested_by"] != testRequesterID {
			t.Fatalf("detail.requested_by = %v", detail["requested_by"])
		}
		found = true
	}
	if !found {
		t.Fatalf("approval.expired payload for %s not found in outbox", ap.ID)
	}
}

// TestBatchAwaitingCancelledOnDisable：批量审批台在发起人被停用后置
// cancelled，job 不再停留在 running/awaiting。
func TestBatchAwaitingCancelledOnDisable(t *testing.T) {
	resetBatchState()
	env := newTestEnv(t)
	env.ensureRequester(t)
	now := time.Now().UTC()
	approvalID := "33333333-3333-4333-8333-333333333333"
	createdBy := testRequesterID
	batchState.mu.Lock()
	batchState.jobs["job-revoke"] = store.BatchJob{
		ID: "job-revoke", Command: "uptime", ServerIDs: []string{"srv-1"},
		Source: store.BatchSourceConsole, Status: store.BatchRunning,
		CreatedBy: &createdBy, CreatedAt: now, UpdatedAt: now,
	}
	batchState.results["job-revoke"] = []store.BatchResult{{
		ID: "res-revoke", BatchJobID: "job-revoke", ServerID: "srv-1",
		RiskLevel: "needs_approval", ApprovalID: &approvalID,
		Status: store.BatchResultAwaitingApproval, CreatedAt: now, UpdatedAt: now,
	}}
	batchState.mu.Unlock()

	rec := env.do(http.MethodPatch, "/api/users/"+testRequesterID, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d body = %s", rec.Code, rec.Body.String())
	}
	job, results, err := env.st.BatchJobs().Get(context.Background(), "job-revoke")
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if len(results) != 1 || results[0].Status != store.BatchResultCancelled {
		t.Fatalf("batch results = %+v, want cancelled", results)
	}
	if job.Status != store.BatchPartial {
		t.Fatalf("job status = %s, want partial (cancelled is a bad terminal)", job.Status)
	}
}

// TestRequesterDeniedAuditRecordsCaller：审批发起人被停用后，审批人 decide
// 得到 403，且审计 actor 必须是审批人（不是被停用的发起人）。
func TestRequesterDeniedAuditRecordsCaller(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-denied-audit")
	disableUserForTest(t, env, testRequesterUsername)

	rec := env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("decide with disabled requester = %d body = %s", rec.Code, rec.Body.String())
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var row *store.AuditLog
	for i := range env.st.auditLogs {
		if env.st.auditLogs[i].Action == "approval.requester_denied" {
			row = &env.st.auditLogs[i]
		}
	}
	if row == nil {
		t.Fatalf("approval.requester_denied audit missing; rows=%+v", env.st.auditLogs)
	}
	// 审批人会话的 user_id 由登录时写入 store 的用户 ID 决定。
	var approverID string
	for _, u := range env.st.users {
		if u.Username == testReviewerUsername {
			approverID = u.ID
		}
	}
	if row.ActorID != approverID {
		t.Fatalf("audit actor_id = %q, want caller %q (never the disabled requester %q)",
			row.ActorID, approverID, testRequesterID)
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["requested_by"] != testRequesterID || detail["reason"] != "requester_disabled" {
		t.Fatalf("detail = %v", detail)
	}
}

// TestSingleAdminDegradedMode：没有任何可承担第二人职责的启用账号
// （admin/approver 合计 <= 1）时，审批批准、终端/RDP 授权申请与会话创建、
// policy 放宽全部 409 NO_SECOND_APPROVER。
//
// 判定口径是 CountEnabledApprovers（与启动告警、README 一致）。因此这条
// 用例不能再用「启用 approver 去批准」来触达降级门——那正是修复前
// 1 admin + 1 approver 被误判为降级的路径（见
// TestApproverCountsAsSecondApprover）。降级下唯一能通过 approvals:decide
// 权限守卫的调用者就是那个唯一管理员。
func TestSingleAdminDegradedMode(t *testing.T) {
	env := newTestEnv(t)
	// 构造「只有 admin 一个启用管理员、且没有启用审批者」：停用预置的
	// 第二管理员，并且不创建 approver。
	disableUserForTest(t, env, testSecondAdminUsername)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-degraded")

	// 1) 审批批准 fail-closed。
	rec := env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decide in degraded mode = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "NO_SECOND_APPROVER" {
		t.Fatalf("code = %s", code)
	}
	if got := approvalStatus(t, env, ap.ID); got != store.ApprovalPending {
		t.Fatalf("approval must stay pending, got %s", got)
	}

	// 2) 终端 grant 申请 fail-closed（先放一台服务器，让降级门成为
	// 唯一的拒绝原因，避免把 404 误当成通过）。
	serverID := addTestServer(t, env)
	rec = env.do(http.MethodPost, "/api/term/grants", `{"server_id":"`+serverID+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("term grant in degraded mode = %d body = %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "NO_SECOND_APPROVER" {
		t.Fatalf("code = %s", code)
	}

	// 3) policy 放宽 fail-closed（既有 NO_SECOND_APPROVER 路径，此处确认
	// 与审批/终端同一判定口径）。
	rec = env.do(http.MethodPut, "/api/settings", `{"approval_mode":"all"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tighten in degraded mode = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPut, "/api/settings", `{"approval_mode":"high"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("widen in degraded mode = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestApproverCountsAsSecondApprover 是走查第 3 轮 high 的核心回归：
// 「1 个启用管理员 + 1 个启用审批者」是 README/CONTRACT 承诺的合法解除
// 方式，此时审批人批准任何业务审批、管理员申请终端 grant 都必须放行；
// 判定口径必须与启动告警一致（CountEnabledApprovers），不能只看
// CountEnabledAdmins。
func TestApproverCountsAsSecondApprover(t *testing.T) {
	env := newTestEnv(t)
	// 只留 admin + approver：停用预置的第二管理员。
	disableUserForTest(t, env, testSecondAdminUsername)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-second-approver")

	// 审批者批准业务审批：放行（不能 409 NO_SECOND_APPROVER）。
	rec := env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide with approver present = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := approvalStatus(t, env, ap.ID); got != store.ApprovalApproved {
		t.Fatalf("approval status = %s, want approved", got)
	}

	// 管理员申请终端 grant：放行（创建 pending 审批，202）。
	serverID := addTestServer(t, env)
	rec = env.do(http.MethodPost, "/api/term/grants", `{"server_id":"`+serverID+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("term grant with approver present = %d body = %s, want 202", rec.Code, rec.Body.String())
	}

	// policy 放宽：创建 source=policy 审批（202），不再直接 409。
	rec = env.do(http.MethodPut, "/api/settings", `{"approval_mode":"all"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tighten = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPut, "/api/settings", `{"approval_mode":"high"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("widen with approver present = %d body = %s, want 202", rec.Code, rec.Body.String())
	}
}

// TestDegradedModeWhenApproverDisabled：审批者被停用后，可承担第二人职责的
// 账号只剩 admin 一个，降级门重新生效（与启动告警同口径）。
func TestDegradedModeWhenApproverDisabled(t *testing.T) {
	env := newTestEnv(t)
	disableUserForTest(t, env, testSecondAdminUsername)
	env.ensureRequester(t)
	// 先确保 approver 存在并通过一次批准（非降级）。
	approverCookie := env.approverCookie(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-approver-off")
	if rec := env.doAs(approverCookie, http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`); rec.Code != http.StatusOK {
		t.Fatalf("baseline approve = %d body = %s", rec.Code, rec.Body.String())
	}
	disableUserForTest(t, env, testReviewerUsername)

	ap2 := pendingApprovalFor(t, env, testRequesterID, "tc-approver-off-2")
	rec := env.do(http.MethodPost, "/api/approvals/"+ap2.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decide after approver disabled = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "NO_SECOND_APPROVER" {
		t.Fatalf("code = %s, want NO_SECOND_APPROVER", code)
	}
}

// resetDeniedAuditLimiter 清空 authz.denied 限速表（测试隔离用：限速表
// 是进程级的，前置用例可能已在 30s 窗口内写过同一 actor+permission）。
func resetDeniedAuditLimiter() {
	deniedLimiter.Lock()
	deniedLimiter.last = make(map[string]time.Time, 64)
	deniedLimiter.Unlock()
}

// TestDegradedDenialAuditDetailAndRateLimit 覆盖第 4 轮走查 low：降级模式
// 拒绝「批准」写出的 authz.denied 必须与普通 403 同口径——detail 含
// permission/method/path/ip（以及 action/reason），且按 actor+permission
// 限速：连续点 5 次「批准」只落 1 条审计，而不是按请求数刷。
func TestDegradedDenialAuditDetailAndRateLimit(t *testing.T) {
	env := newTestEnv(t)
	// 降级：停用第二管理员，可审批账号只剩 admin。
	disableUserForTest(t, env, testSecondAdminUsername)
	env.ensureRequester(t)
	resetDeniedAuditLimiter()
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-degraded-denied-audit")

	for i := 0; i < 5; i++ {
		rec := env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("degraded approve #%d = %d body = %s, want 409", i+1, rec.Code, rec.Body.String())
		}
		if code, _ := errorBody(t, rec); code != "NO_SECOND_APPROVER" {
			t.Fatalf("degraded approve #%d code = %s, want NO_SECOND_APPROVER", i+1, code)
		}
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}

	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	rows := 0
	var detail map[string]any
	for _, row := range env.st.auditLogs {
		if row.Action != "authz.denied" || row.ResourceType != "permission" ||
			row.ResourceID != string(authz.PermApprovalDecide) {
			continue
		}
		rows++
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			t.Fatalf("audit detail: %v", err)
		}
	}
	if rows != 1 {
		t.Fatalf("authz.denied rows for approval:decide = %d after 5 attempts, want 1 (rate limit)", rows)
	}
	if detail["permission"] != string(authz.PermApprovalDecide) {
		t.Fatalf("detail.permission = %v", detail["permission"])
	}
	if detail["method"] != http.MethodPost {
		t.Fatalf("detail.method = %v", detail["method"])
	}
	if detail["path"] != "/api/approvals/:id/decide" {
		t.Fatalf("detail.path = %v", detail["path"])
	}
	if ip, _ := detail["ip"].(string); ip == "" {
		t.Fatalf("detail.ip missing: %v", detail)
	}
	if detail["action"] != "approval.approve" || detail["reason"] != "single_admin_degraded" {
		t.Fatalf("detail action/reason = %v/%v", detail["action"], detail["reason"])
	}
}

// TestSettingsExposeDegradedState 是走查第 3 轮 medium 的后端部分：
// GET /api/settings 必须公开只读降级态（single_admin_degraded +
// enabled_approvers），前端据此在点「批准」前渲染中文说明并置灰按钮，
// 而不是让审批人撞一次英文 409。口径与启动告警一致（CountEnabledApprovers）。
func TestSettingsExposeDegradedState(t *testing.T) {
	env := newTestEnv(t)
	type settingsResp struct {
		ApprovalMode        string `json:"approval_mode"`
		SingleAdminDegraded *bool  `json:"single_admin_degraded"`
		EnabledApprovers    *int   `json:"enabled_approvers"`
	}
	decode := func() settingsResp {
		t.Helper()
		rec := env.do(http.MethodGet, "/api/settings", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d body = %s", rec.Code, rec.Body.String())
		}
		var out settingsResp
		decodeBody(t, rec, &out)
		return out
	}

	// 默认测试环境：admin + admin-second 两个启用管理员 → 非降级。
	normal := decode()
	if normal.SingleAdminDegraded == nil || *normal.SingleAdminDegraded {
		t.Fatalf("single_admin_degraded = %v, want false", normal.SingleAdminDegraded)
	}
	if normal.EnabledApprovers == nil || *normal.EnabledApprovers != 2 {
		t.Fatalf("enabled_approvers = %v, want 2", normal.EnabledApprovers)
	}

	// 停用第二管理员 → 只剩 1 个可审批账号 → 降级。
	disableUserForTest(t, env, testSecondAdminUsername)
	degraded := decode()
	if degraded.SingleAdminDegraded == nil || !*degraded.SingleAdminDegraded {
		t.Fatalf("single_admin_degraded = %v, want true", degraded.SingleAdminDegraded)
	}
	if degraded.EnabledApprovers == nil || *degraded.EnabledApprovers != 1 {
		t.Fatalf("enabled_approvers = %v, want 1", degraded.EnabledApprovers)
	}

	// 补一个启用 approver → 解除降级（README 承诺的恢复方式）。
	addTestUser(t, env, "approver-degrade", testPassword, store.RoleApprover, true)
	recovered := decode()
	if recovered.SingleAdminDegraded == nil || *recovered.SingleAdminDegraded {
		t.Fatalf("single_admin_degraded = %v after approver added, want false", recovered.SingleAdminDegraded)
	}
	if recovered.EnabledApprovers == nil || *recovered.EnabledApprovers != 2 {
		t.Fatalf("enabled_approvers = %v, want 2", recovered.EnabledApprovers)
	}
}

// TestRequesterDeniedKeepsPending：拒绝路径不让审批状态变化（决策未发生）。
func TestRequesterDeniedKeepsPending(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-keep-pending")
	disableUserForTest(t, env, testRequesterUsername)
	_ = env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if got := approvalStatus(t, env, ap.ID); got != store.ApprovalPending {
		t.Fatalf("status = %s, want pending", got)
	}
}

// addTestServer 向内存 store 注入一台 Linux password 服务器并返回其 UUID。
func addTestServer(t *testing.T, env *testEnv) string {
	t.Helper()
	enc, err := env.cipher.Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	id := "44444444-4444-4444-8444-444444444444"
	env.st.mu.Lock()
	env.st.servers[id] = store.Server{
		ID: id, Name: "degraded-host", OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root",
		AuthMethod: store.AuthMethodPassword, PasswordEncrypted: enc,
	}
	env.st.mu.Unlock()
	return id
}
