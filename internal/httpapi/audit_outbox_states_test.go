// audit_outbox_states_test.go 验证「审批/策略/grant 状态变更审计走
// audit_outbox 同事务」（契约 12.3 第 19 条 / 12.4 第 6 条）。
//
// 评审 medium：此前审批/策略/终端 grant 的状态变更只在业务提交后进程内
// Enqueue，进程在提交与入队之间崩溃会永久丢这条审计。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aiops/internal/store"
)

// outboxActions 返回内存 outbox 里的 action 集合。
func outboxActions(t *testing.T, env *testEnv) map[string]int {
	t.Helper()
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	out := make(map[string]int, len(env.st.outbox))
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		out[payload.Action]++
	}
	return out
}

// TestApprovalDecideWritesOutbox：批准与拒绝的决策审计必须出现在
// audit_outbox（同一事务），而不是只走进程内 Enqueue。
func TestApprovalDecideWritesOutbox(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-outbox-approve")

	rec := env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide = %d body = %s", rec.Code, rec.Body.String())
	}
	actions := outboxActions(t, env)
	if actions["approval.approved"] != 1 {
		t.Fatalf("outbox approval.approved = %d, actions=%v", actions["approval.approved"], actions)
	}
	if actions["command.approved"] != 1 {
		t.Fatalf("outbox command.approved = %d, actions=%v", actions["command.approved"], actions)
	}

	deny := pendingApprovalFor(t, env, testRequesterID, "tc-outbox-deny")
	rec = env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+deny.ID+"/decide", `{"decision":"deny"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d body = %s", rec.Code, rec.Body.String())
	}
	actions = outboxActions(t, env)
	if actions["approval.denied"] != 1 {
		t.Fatalf("outbox approval.denied = %d, actions=%v", actions["approval.denied"], actions)
	}
	if actions["command.approved"] != 1 {
		t.Fatalf("deny must not add command.approved (actions=%v)", actions)
	}
}

// TestSettingsUpdateWritesOutbox：收紧门限的 settings.update 必须在
// outbox 中（与 settings 写入同事务）。
func TestSettingsUpdateWritesOutbox(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPut, "/api/settings", `{"approval_mode":"all"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tighten = %d body = %s", rec.Code, rec.Body.String())
	}
	actions := outboxActions(t, env)
	if actions["settings.update"] != 1 {
		t.Fatalf("outbox settings.update = %d, actions=%v", actions["settings.update"], actions)
	}
}

// TestPolicyDecideWritesOutbox：policy 批准的 settings 写入与
// policy.change_applied 审计同事务。
func TestPolicyDecideWritesOutbox(t *testing.T) {
	env := newTestEnv(t)
	// 放宽需要第二人：预置第二管理员已在 newTestEnv 中。
	rec := env.do(http.MethodPut, "/api/settings", `{"approval_mode":"high"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("widen = %d body = %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		PendingApproval store.Approval `json:"pending_approval"`
	}
	decodeBody(t, rec, &pending)
	if pending.PendingApproval.ID == "" {
		t.Fatal("pending policy approval missing")
	}
	// 第 4 轮走查 low：policy 的「申请」审计必须与「生效」同口径
	//（resource_type=setting、resource_id=approval.mode），否则审计页按
	//「目标类型=设置」过滤时看不到「谁申请了放宽审批门限」。
	env.st.mu.Lock()
	var requested *store.CreateAuditParams
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			env.st.mu.Unlock()
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action == "policy.change_requested" {
			p := payload
			requested = &p
		}
	}
	env.st.mu.Unlock()
	if requested == nil {
		t.Fatal("outbox policy.change_requested missing")
	}
	if requested.ResourceType != "setting" || requested.ResourceID != "approval.mode" {
		t.Fatalf("policy.change_requested resource = %s/%s, want setting/approval.mode",
			requested.ResourceType, requested.ResourceID)
	}
	// 发起人是 admin，审批人用第二管理员（同一人自审会被拒）。
	secondCookie := loginAs(t, env, testSecondAdminUsername, testPassword)
	rec = doAs(env, http.MethodPost, "/api/approvals/"+pending.PendingApproval.ID+"/decide",
		`{"decision":"approve"}`, secondCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve policy = %d body = %s", rec.Code, rec.Body.String())
	}
	actions := outboxActions(t, env)
	if actions["policy.change_applied"] != 1 {
		t.Fatalf("outbox policy.change_applied = %d, actions=%v", actions["policy.change_applied"], actions)
	}
	if actions["approval.approved"] != 1 {
		t.Fatalf("outbox approval.approved = %d, actions=%v", actions["approval.approved"], actions)
	}
}

// TestApprovalExpireWritesOutbox：第 4 轮走查 medium——pending 审批 TTL
// 过期必须与 approval.expired 审计同事务（UPDATE + audit_outbox），不得
// 先裸 UPDATE 再走进程内队列（进程在两步之间崩溃会留下「已过期但无审计」）。
func TestApprovalExpireWritesOutbox(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	ap := pendingApprovalFor(t, env, testRequesterID, "tc-outbox-expire")

	// 直接调用服务的过期扫描（sweeper 的 TTL 是 1h，这里用未来时间触发）。
	n, err := env.apprSvc.Expire(context.Background(), time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	stored, gerr := env.st.Approvals().Get(context.Background(), ap.ID)
	if gerr != nil || stored.Status != store.ApprovalExpired {
		t.Fatalf("approval status = %q err = %v, want expired", stored.Status, gerr)
	}

	env.st.mu.Lock()
	var perRow, aggregate int
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			env.st.mu.Unlock()
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action != "approval.expired" {
			continue
		}
		if payload.ResourceID == ap.ID {
			perRow++
		} else if payload.ResourceID == "" {
			aggregate++
		}
	}
	env.st.mu.Unlock()
	if perRow != 1 || aggregate != 1 {
		t.Fatalf("outbox approval.expired per-row=%d aggregate=%d, want 1/1", perRow, aggregate)
	}
}

// TestTerminalGrantConsumeWritesOutbox：grant 单次消费与
// terminal.grant_consumed 审计同事务（消费成功即出现在 outbox）。
func TestTerminalGrantConsumeWritesOutbox(t *testing.T) {
	env := newTermTestEnv(t)
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	grantID := env.grant(t, serverID)

	rec := env.do(http.MethodPost, "/api/term/sessions",
		`{"server_id":"`+serverID+`","grant_id":"`+grantID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session = %d body = %s", rec.Code, rec.Body.String())
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	found := false
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action == "terminal.grant_consumed" && payload.ResourceID == grantID {
			found = true
		}
	}
	if !found {
		t.Fatal("terminal.grant_consumed must be written to audit_outbox in the consume transaction")
	}
}
