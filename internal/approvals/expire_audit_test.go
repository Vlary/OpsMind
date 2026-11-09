// expire_audit_test.go 覆盖审批 TTL 过期的逐条审计（六期打磨 sprint 6 走查
// low）：`Expire` 此前只写一行聚合审计（resource_id 空、detail={count}），
// 审计页「审批过期」筛选下目标与命令摘要恒「—」，无法定位具体哪条审批、
// 哪条命令过期。现在批量 UPDATE 前抓一份到期候选快照，UPDATE 后逐条
// emit（带 approval_id/command/source/risk_level/approval_mode），并保留
// 聚合行。
package approvals

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/store"
)

func detailMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("detail unmarshal: %v", err)
	}
	return detail
}

func TestExpireEmitsPerApprovalAudit(t *testing.T) {
	svc, repo, rec := newAuditService(t)
	ctx := context.Background()
	// 两个审批：一个即将过期、一个仍有效。带 server_id 与门限快照（审计
	// detail 要能自证「哪台、哪档门限」）。
	convID, toolID1, toolID2 := "conv-exp", "tc-exp-1", "tc-exp-2"
	serverID := "srv-fake"
	expiring, err := svc.Create(ctx, CreateRequest{
		ConversationID: &convID, ToolCallID: &toolID1, ServerID: &serverID,
		ServerName: "web-1", Command: "systemctl restart nginx",
		RiskLevel: "needs_approval", RuleID: "SERVICE_CHANGE", Reason: "service change",
		ApprovalMode: "high",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	alive, err := svc.Create(ctx, CreateRequest{
		ConversationID: &convID, ToolCallID: &toolID2, ServerID: &serverID,
		ServerName: "web-1", Command: "df -h",
		RiskLevel: "safe", RuleID: "READ_ONLY", Reason: "read only",
		ApprovalMode: "high",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 把第一个的到期时间挪到过去（Create 用服务 TTL=1m，不能等）。
	repo.mu.Lock()
	row := repo.items[expiring.ID]
	past := time.Now().Add(-time.Minute)
	row.ExpiresAt = past
	repo.items[expiring.ID] = row
	repo.mu.Unlock()

	n, err := svc.Expire(ctx, time.Now())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	flushAudit(t, svc)

	rows := rec.byAction(actionExpired)
	// 逐条行 + 聚合行（保留旧口径）。
	var perRow, aggregate *store.AuditLog
	for i := range rows {
		if rows[i].ResourceID == expiring.ID {
			perRow = &rows[i]
		} else if rows[i].ResourceID == "" {
			aggregate = &rows[i]
		}
	}
	if perRow == nil {
		t.Fatalf("missing per-approval approval.expired row（审计页「审批过期」目标恒「—」）: %+v", rows)
	}
	detail := detailMap(t, perRow.Detail)
	if detail["approval_id"] != expiring.ID {
		t.Fatalf("detail.approval_id = %v", detail["approval_id"])
	}
	if detail["command"] != "systemctl restart nginx" {
		t.Fatalf("detail.command = %v（审计页命令摘要应可读）", detail["command"])
	}
	if detail["risk_level"] != "needs_approval" {
		t.Fatalf("detail.risk_level = %v", detail["risk_level"])
	}
	if detail["source"] != "chat" {
		t.Fatalf("detail.source = %v", detail["source"])
	}
	if detail["approval_mode"] != "high" {
		t.Fatalf("detail.approval_mode = %v（门限快照）", detail["approval_mode"])
	}
	if detail["reason"] != "ttl_expired" {
		t.Fatalf("detail.reason = %v", detail["reason"])
	}
	if perRow.ResourceType != "approval" {
		t.Fatalf("resource_type = %q", perRow.ResourceType)
	}
	if perRow.ServerID == nil || *perRow.ServerID != "srv-fake" {
		t.Fatalf("server_id = %v（审批带服务器时应透传）", perRow.ServerID)
	}

	if aggregate == nil {
		t.Fatalf("missing aggregate approval.expired row（旧口径必须保留）: %+v", rows)
	}
	aggDetail := detailMap(t, aggregate.Detail)
	if aggDetail["count"] != float64(1) {
		t.Fatalf("aggregate count = %v, want 1", aggDetail["count"])
	}
	ids, _ := aggDetail["approval_ids"].([]any)
	if len(ids) != 1 || ids[0] != expiring.ID {
		t.Fatalf("aggregate approval_ids = %v", aggDetail["approval_ids"])
	}

	// 仍有效的审批不得落过期审计。
	for _, r := range rows {
		if r.ResourceID == alive.ID {
			t.Fatalf("alive approval must not be audited as expired: %+v", r)
		}
	}
}

// 并发 decide 抢先：快照里的行被批准后，不得被误报成「已过期」。
func TestExpireSkipsConcurrentlyDecidedApproval(t *testing.T) {
	svc, repo, rec := newAuditService(t)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-exp-race"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo.mu.Lock()
	row := repo.items[ap.ID]
	row.ExpiresAt = time.Now().Add(-time.Minute)
	repo.items[ap.ID] = row
	repo.mu.Unlock()

	// 模拟快照之后、UPDATE 之前的 decide 抢先（直接改 fake 行）。
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "u"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	n, err := svc.Expire(ctx, time.Now())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0（已批准的行不再过期）", n)
	}
	flushAudit(t, svc)
	// 只能有 decide 的审计行，不得有指向该审批的 approval.expired 明细。
	for _, r := range rec.byAction(actionExpired) {
		if r.ResourceID == ap.ID {
			t.Fatalf("approved approval must not be audited as expired: %+v", r)
		}
	}
}

// 无过期项：不发任何 approval.expired 审计（保持旧行为）。
func TestExpireNoCandidatesNoAudit(t *testing.T) {
	svc, _, rec := newAuditService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, createReq("tc-exp-live")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n, err := svc.Expire(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("Expire = %d, %v", n, err)
	}
	flushAudit(t, svc)
	if rows := rec.byAction(actionExpired); len(rows) != 0 {
		t.Fatalf("approval.expired rows = %d, want 0", len(rows))
	}
}
