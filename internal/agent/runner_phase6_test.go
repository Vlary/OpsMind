// runner_phase6_test.go 覆盖六期 BE-P 的 agent 侧门限与改写（契约 2.11
// 约定 1/2/3、5.8 第 9 条）：会话覆盖门限直执行（none+auto_exec=false 的
// needs_approval(high) 不建审批、仍落审计、tool_result 携带 approval_mode）、
// standard 审批路径创建审批带门限快照、批准且改写时执行改写后命令
// （result.command、SSE approval_resolved.edited_command、审计
// original_command）。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/llm"
	"aiops/internal/store"
)

// newPhase6Runner 构造带已启动审计 writer 的 Runner（六期测试需要断言
// 异步审计内容——newRunnerForTest 不暴露 writer）。
func newPhase6Runner(t *testing.T, st *testStore, client llm.Client, ap ApprovalService) (*Runner, *fakeSSH, *audit.Writer) {
	t.Helper()
	ssh := &fakeSSH{}
	w := audit.NewWriter(st, testLogger(), time.Now)
	w.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = w.Close(ctx)
	})
	r := NewRunner(st, nil, ssh, nil, nil, w, nil, RunnerConfig{}, time.Now, testLogger())
	r.SetLLMClient(client)
	if ap != nil {
		r.SetApprovalService(ap)
	}
	return r, ssh, w
}

func TestRunPhase6NoneOverrideExecutesDirectly(t *testing.T) {
	st := seed(false)
	// 会话覆盖 none + 服务器 auto_exec=false + needs_approval(high) 命令：
	// 契约 2.11 约定 2 矩阵 → none 档直执行（硬保护只作用于 auto=false
	// 的 safe 档以下……实际矩阵写死：needs_approval(high)+auto=false 在
	// none 档仍为「审批」——本用例换 needs_approval 不适用；改用矩阵中
	// none 档直执行的合法格：safe+auto=false。
	conv := st.conversations["c1"]
	none := store.ApprovalModeNone
	conv.ApprovalMode = &none
	st.conversations["c1"] = conv

	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, ssh, w := newPhase6Runner(t, st, client, ap)

	var results []*ToolResultPayload
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		if ev.Type == EventToolResult && ev.ToolResult != nil {
			tr := *ev.ToolResult
			results = append(results, &tr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// none 档 + safe + auto_exec=false → 直执行：不建审批。
	if len(ap.created) != 0 {
		t.Fatalf("approvals created = %d, want 0 (none override should execute directly)", len(ap.created))
	}
	if len(ssh.calls) == 0 || ssh.calls[0].Command != "ls -la" {
		t.Fatalf("ssh commands = %v", ssh.calls)
	}
	// tool_result 携带有效门限。
	if len(results) == 0 || results[0].ApprovalMode != string(store.ApprovalModeNone) {
		t.Fatalf("tool_result approval_mode = %+v", results)
	}
	// 六期打磨 sprint 1：门限快照随 tool_calls 行落库（迁移 00010）——
	// 刷新/重进会话后工具卡的门限徽标据此恢复，不再只存在于 SSE 流内。
	assertToolCallApprovalMode(t, st, ToolSSHExec, string(store.ApprovalModeNone))
	// 直执行仍落审计 command.executed（detail 带 approval_mode）；writer
	// 异步——flush 后同步断言。
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	if err := w.Flush(flushCtx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}
	assertAuditDetail(t, st, "command.executed", func(detail map[string]any) error {
		if detail["approval_mode"] != string(store.ApprovalModeNone) {
			return fmt.Errorf("audit approval_mode = %v", detail["approval_mode"])
		}
		return nil
	})
}

func TestRunPhase6HardProtectionStillApproves(t *testing.T) {
	st := seed(false)
	// 会话覆盖 none + auto_exec=false + needs_approval(high)：矩阵写死
	// none 档此格为「审批」（服务器级硬保护：非只读命令无论门限一律审批）。
	conv := st.conversations["c1"]
	none := store.ApprovalModeNone
	conv.ApprovalMode = &none
	st.conversations["c1"] = conv

	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	err := r.Run(context.Background(), runInput(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ap.created) != 1 {
		t.Fatalf("approvals created = %d, want 1 (hard protection)", len(ap.created))
	}
	if ap.created[0].ApprovalMode != string(store.ApprovalModeNone) {
		t.Fatalf("approval snapshot mode = %q, want none", ap.created[0].ApprovalMode)
	}
}

func TestRunPhase6EditedCommandExecution(t *testing.T) {
	st := seed(false)
	// 无覆盖 + settings 未接线（GlobalMode 兜底 standard）+ auto_exec=false
	// + needs_approval 命令 → 审批；批准且携带改写 → 执行改写后命令。
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved, waitEdited: "systemctl restart nginx --no-block"}
	r, ssh, w := newPhase6Runner(t, st, client, ap)

	var resolved *ApprovalResolvedPayload
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		if ev.Type == EventApprovalResolved && ev.ApprovalResolved != nil {
			res := *ev.ApprovalResolved
			resolved = &res
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ap.created) != 1 {
		t.Fatalf("approvals created = %d, want 1", len(ap.created))
	}
	if ap.created[0].ApprovalMode != string(store.ApprovalModeStandard) {
		t.Fatalf("approval mode snapshot = %q, want standard", ap.created[0].ApprovalMode)
	}
	// 工具调用行同样冻结门限快照（用 tool_result/审计之外的第三条持久化路径）。
	assertToolCallApprovalMode(t, st, ToolSSHExec, string(store.ApprovalModeStandard))
	// 执行的是改写后命令（逐字节）。
	if len(ssh.calls) != 1 || ssh.calls[0].Command != "systemctl restart nginx --no-block" {
		t.Fatalf("ssh commands = %v, want edited", ssh.calls)
	}
	// SSE approval_resolved 携带改写命令。
	if resolved == nil || resolved.EditedCommand != "systemctl restart nginx --no-block" {
		t.Fatalf("approval_resolved = %+v", resolved)
	}
	// tool_calls.result 记录实际执行（command=改写后）。
	var resultJSON json.RawMessage
	st.mu.Lock()
	for _, tc := range st.toolCalls {
		if tc.Name == ToolSSHExec {
			resultJSON = tc.Result
		}
	}
	st.mu.Unlock()
	var result struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("result json: %v (%s)", err, resultJSON)
	}
	if result.Command != "systemctl restart nginx --no-block" {
		t.Fatalf("result.command = %q, want edited", result.Command)
	}
	// 审计 command.executed 带 original_command（改写时）与 approval_mode，
	// 以及实际执行的命令文本（六期打磨 sprint 1：免审批档事后从审计复原
	// 「跑的是什么」）。
	flushCtx2, flushCancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel2()
	if err := w.Flush(flushCtx2); err != nil {
		t.Fatalf("flush audit: %v", err)
	}
	assertAuditDetail(t, st, "command.executed", func(detail map[string]any) error {
		if detail["original_command"] != "systemctl restart nginx" {
			return fmt.Errorf("original_command = %v", detail["original_command"])
		}
		if detail["approval_mode"] != string(store.ApprovalModeStandard) {
			return fmt.Errorf("audit approval_mode = %v", detail["approval_mode"])
		}
		if detail["command"] != "systemctl restart nginx --no-block" {
			return fmt.Errorf("audit command = %v", detail["command"])
		}
		return nil
	})
}

// assertToolCallApprovalMode 断言指定工具名的最新落库工具调用带期望的门限
// 快照（六期打磨 sprint 1，tool_calls.approval_mode）。
func assertToolCallApprovalMode(t *testing.T, st *testStore, name, want string) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, tc := range st.toolCalls {
		if tc.Name != name {
			continue
		}
		if tc.ApprovalMode != want {
			t.Fatalf("tool_calls[%s].approval_mode = %q, want %q", name, tc.ApprovalMode, want)
		}
		return
	}
	t.Fatalf("tool call %q not found", name)
}

// assertAuditDetail 同步断言已落库审计中存在动作与 detail 均匹配的事件。
func assertAuditDetail(t *testing.T, st *testStore, action string, check func(map[string]any) error) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.auditLogs {
		if e.Action != action {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal(e.Detail, &detail); err != nil {
			continue
		}
		if err := check(detail); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("audit %s not found", action)
}

// TestRunPhase6ContainerActionHighDirect：container_action(stop=needs_approval
// CONTAINER_CHANGE=high) 在 high+auto_exec 下仍审批（矩阵写死），验证
// runner_container 走统一策略函数而非旧内联判定。
func TestRunPhase6ContainerActionHighStillApproves(t *testing.T) {
	st := seed(false)
	conv := st.conversations["c1"]
	high := store.ApprovalModeHigh
	conv.ApprovalMode = &high
	st.conversations["c1"] = conv
	srv := st.servers["s1"]
	srv.AutoExecEnabled = true
	srv.ContainerRuntime = store.ContainerRuntimeDocker
	st.servers["s1"] = srv

	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolContainerAction, `{"server_id":"s1","container_id":"9f2c6f8f7c21","action":"stop"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _, _ := newPhase6Runner(t, st, client, ap)

	_ = r.Run(context.Background(), runInput(), func(Event) error { return nil })
	if len(ap.created) != 1 {
		t.Fatalf("approvals created = %d, want 1 (CONTAINER_CHANGE is high severity)", len(ap.created))
	}
	if ap.created[0].ApprovalMode != string(store.ApprovalModeHigh) {
		t.Fatalf("approval mode = %q, want high", ap.created[0].ApprovalMode)
	}
}
