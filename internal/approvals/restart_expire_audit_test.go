// restart_expire_audit_test.go 覆盖走查第 3 轮 high 的重启路径：进程启动把
// 遗留 pending 审批收敛为 expired(server_shutting_down) 时，必须写
// approval.expired 审计，否则审计员只看到 approval.created，无法区分
// 「系统作废」与「记录丢失」。
package approvals

import (
	"context"
	"testing"
	"time"

	"aiops/internal/store"
)

func TestRestartExpiryWritesAudit(t *testing.T) {
	svc, _, rec := newAuditService(t)
	ctx := context.Background()
	repo := svc.st.Approvals().(*fakeApprovals)
	convID := "conv-restart"
	repo.mu.Lock()
	repo.items["ap-restart"] = store.Approval{
		ID: "ap-restart", ConversationID: &convID, Source: "chat",
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		Status: store.ApprovalPending, RequestedBy: "requester-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	repo.items["ap-restart-policy"] = store.Approval{
		ID: "ap-restart-policy", Source: "policy", Command: "",
		RiskLevel: "safe", Status: store.ApprovalPending, RequestedBy: "admin-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	repo.mu.Unlock()

	svc.expirePendingAfterRestart(ctx)
	flushAudit(t, svc)

	rows := rec.byAction(actionExpired)
	byID := map[string]bool{}
	for _, row := range rows {
		if detailString(t, row.Detail, "reason") != cancelReasonShutdown {
			t.Fatalf("reason = %q, want %s", detailString(t, row.Detail, "reason"), cancelReasonShutdown)
		}
		byID[row.ResourceID] = true
	}
	for _, id := range []string{"ap-restart", "ap-restart-policy"} {
		if !byID[id] {
			t.Fatalf("approval.expired missing for %s（审计页只有 approval.created 无法证明审批被系统作废）: %+v", id, rows)
		}
	}
	// 命令类审批的 detail 必须能定位「哪条命令被作废」。
	for _, row := range rows {
		if row.ResourceID == "ap-restart" && detailString(t, row.Detail, "command") != "systemctl restart nginx" {
			t.Fatalf("detail.command = %q", detailString(t, row.Detail, "command"))
		}
	}
	// 已无 pending：再跑一轮不产生新行。
	svc.expirePendingAfterRestart(ctx)
	flushAudit(t, svc)
	if got := len(rec.byAction(actionExpired)); got != len(rows) {
		t.Fatalf("second restart sweep added rows: %d -> %d", len(rows), got)
	}
}
