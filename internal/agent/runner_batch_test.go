// runner_batch_test.go 覆盖 batch_execute 工具（G37 五期新工具的 M3 部分）：
// fake dispatcher 返回概要——断言工具不等待审批即返回；参数越界
// （server_ids 51 项）→TOOL_ARGS_INVALID；引擎未注入→INTERNAL。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aiops/internal/llm"
	"aiops/internal/store"
)

// fakeBatchDispatcher 记录 Dispatch 调用并返回固定 job 概要。
type fakeBatchDispatcher struct {
	called  int
	lastReq store.CreateBatchJobParams
	delay   time.Duration // 模拟引擎同步段耗时（工具不等待审批≠瞬时）
}

func (f *fakeBatchDispatcher) Dispatch(ctx context.Context, p store.CreateBatchJobParams) (store.BatchJob, error) {
	f.called++
	f.lastReq = p
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return store.BatchJob{}, ctx.Err()
		}
	}
	return store.BatchJob{
		ID: "job-1", Status: store.BatchRunning, TotalCount: 2, DoneCount: 1,
	}, nil
}

// batchFakeResults 提供 Get 装配的逐台结果（1 台 executed + 1 台审批中）。
type batchFakeResults struct {
	store.BatchJobStore
}

func (b *batchFakeResults) Get(ctx context.Context, id string) (store.BatchJob, []store.BatchResult, error) {
	return store.BatchJob{ID: id, Status: store.BatchRunning, TotalCount: 2, DoneCount: 1}, []store.BatchResult{
		{ID: "r1", BatchJobID: id, ServerID: "web-1", ServerName: "web-1", Status: store.BatchResultExecuted},
		{ID: "r2", BatchJobID: id, ServerID: "web-2", ServerName: "web-2", Status: store.BatchResultAwaitingApproval},
	}, nil
}

func (s *testStore) BatchJobs() store.BatchJobStore { return &batchFakeResults{} }

// TestRunBatchExecuteReturnsSummaryImmediately：工具调用后立即返回分派
// 概要（不产生 approval_required、不阻塞等待 decide——契约 2.10 约定 5）。
func TestRunBatchExecuteReturnsSummaryImmediately(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolBatchExecute, `{"server_ids":["web-1","web-2"],"command":"systemctl restart nginx","description":"重启所有 web 服务器"}`)},
		{events: finalEvents("已创建批量任务，1 台已执行，1 台等待审批", llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	dispatcher := &fakeBatchDispatcher{delay: 30 * time.Millisecond}
	r.SetBatchExecutor(dispatcher)

	var events []Event
	if err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if dispatcher.called != 1 {
		t.Fatalf("dispatch called = %d, want 1", dispatcher.called)
	}
	req := dispatcher.lastReq
	if req.Command != "systemctl restart nginx" || len(req.ServerIDs) != 2 || req.Source != store.BatchSourceChat {
		t.Fatalf("dispatch req = %+v", req)
	}
	if req.Description != "重启所有 web 服务器" {
		t.Fatalf("description = %q（自然语言意图应透传）", req.Description)
	}
	if req.ConversationID == nil || *req.ConversationID == "" {
		t.Fatal("conversation_id should be recorded on job")
	}

	var result *Event
	types := eventTypes(events)
	for i := range events {
		if events[i].Type == EventToolResult {
			result = &events[i]
		}
	}
	if result == nil || !result.ToolResult.OK {
		t.Fatalf("tool result missing or failed: %+v", result)
	}
	// 概要 schema（契约 7.1）。
	var payload map[string]any
	if err := json.Unmarshal(result.ToolResult.Result, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["batch_job_id"] != "job-1" || payload["total"] != float64(2) {
		t.Fatalf("payload = %v", payload)
	}
	if payload["awaiting_approval"] != float64(1) || payload["executed"] != float64(1) {
		t.Fatalf("status counts = %v", payload)
	}
	if note, _ := payload["note"].(string); !strings.Contains(note, "/api/batch/jobs/job-1") {
		t.Fatalf("note = %v", payload["note"])
	}
	// 关键断言：不在 chat 流内等待审批（无 approval_required 事件）。
	if indexOf(types, "approval_required") >= 0 {
		t.Fatalf("batch_execute must not wait for approval in-stream: %v", types)
	}
	// tool_call 落库成功。
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolSucceeded {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
	if st.toolCalls[0].RiskLevel != "needs_approval" {
		t.Fatalf("tool risk_level = %q, want needs_approval（工具级建模）", st.toolCalls[0].RiskLevel)
	}
}

// TestRunBatchExecuteTooManyTargets：51 项 server_ids →TOOL_ARGS_INVALID
// （G37 参数越界断言），引擎不被调。
func TestRunBatchExecuteTooManyTargets(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolBatchExecute, tooManyBatchArgs())},
		{events: finalEvents("目标过多", llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	dispatcher := &fakeBatchDispatcher{}
	r.SetBatchExecutor(dispatcher)

	if err := r.Run(context.Background(), runInput(), func(ev Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dispatcher.called != 0 {
		t.Fatalf("dispatch must not be called for invalid args, called = %d", dispatcher.called)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed || st.toolCalls[0].ErrorCode != "TOOL_ARGS_INVALID" {
		t.Fatalf("tool call = %+v", st.toolCalls)
	}
}

// TestRunBatchExecuteNotConfigured：引擎未注入（batch==nil）→INTERNAL，
// 不 panic。
func TestRunBatchExecuteNotConfigured(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolBatchExecute, `{"server_ids":["web-1"],"command":"ls"}`)},
		{events: finalEvents("批量功能未启用", llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})

	if err := r.Run(context.Background(), runInput(), func(ev Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed || st.toolCalls[0].ErrorCode != "INTERNAL" {
		t.Fatalf("tool call = %+v", st.toolCalls)
	}
}

func tooManyBatchArgs() string {
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = "srv"
	}
	raw, _ := json.Marshal(map[string]any{"server_ids": ids, "command": "ls"})
	return string(raw)
}
