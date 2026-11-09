// runner_phase6sprint6_test.go 覆盖六期打磨 sprint 6 的两处审计载荷补齐：
//
//  1. 对话拒绝命令的 `command.denied` detail 补 command / risk_level——
//     与批量路径（internal/batchx/execute.go）同口径，审计页「命令拒绝」
//     行的命令摘要不再恒「—」；
//  2. REST 容器直执行分支的 `container.action` detail 带 approval_mode
//     （见 httpapi/containers_test.go 的对应用例）。
package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/llm"
	"aiops/internal/store"
)

// TestRunDeniedCommandAuditCarriesCommandAndRisk 回归：对话链路拒绝命令时
// 审计 detail 必须带 command（审批创建时的原命令）与 risk_level。
func TestRunDeniedCommandAuditCarriesCommandAndRisk(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
		{events: finalEvents("denied", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalDenied}
	r, ssh, w := newPhase6Runner(t, st, client, ap)

	if err := r.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("denied command must not execute: %+v", ssh.calls)
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Flush(flushCtx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	var found bool
	for _, e := range st.auditLogs {
		if e.Action != "command.denied" {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal(e.Detail, &detail); err != nil {
			t.Fatalf("detail unmarshal: %v", err)
		}
		if detail["command"] != "systemctl restart nginx" {
			t.Fatalf("command.denied detail.command = %v（审计页命令摘要应可读）", detail["command"])
		}
		if detail["risk_level"] != "needs_approval" {
			t.Fatalf("command.denied detail.risk_level = %v", detail["risk_level"])
		}
		if detail["approval_id"] == "" || detail["approval_id"] == nil {
			t.Fatalf("command.denied detail.approval_id missing: %v", detail)
		}
		found = true
	}
	if !found {
		t.Fatalf("command.denied audit missing: %+v", st.auditLogs)
	}
}
