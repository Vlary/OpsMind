// replay_visibility_test.go 覆盖八期「AI 操作全量可见」（契约 2.12 约定 3 /
// 7.8 / G41）：11 个动作类工具各生成一条 execution 步、7 个本地工具不生成、
// 全部 18 个工具 tool_call 步带 result_summary、output 按工具固定键取值、
// 整包 64KB 截断包装降级、duration_ms 恒非 null、exit_code 只在两个命令类
// 工具出现、affected_servers 装配与已删资产占位、资源句柄字段、payload 预算。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aiops/internal/agent"
	"aiops/internal/store"
)

// replayVisibilityResult 为各工具构造逐字贴近 agent 侧 result 的样例。
func replayVisibilityResult(name string) json.RawMessage {
	switch name {
	case agent.ToolSSHExec:
		return json.RawMessage(`{"server_id":"srv-1","command":"uptime","exit_code":0,"output":"up 3 days","truncated":true,"duration_ms":88,"risk_level":"safe","approval_id":null}`)
	case agent.ToolServerFacts:
		return json.RawMessage(`{"server_id":"srv-1","facts":{"hostname":"web-1"},"raw_output":"hostname=web-1\n","duration_ms":300}`)
	case agent.ToolContainerAction:
		return json.RawMessage(`{"server_id":"srv-1","container_id":"9f2c","action":"restart","command":"docker restart 9f2c","exit_code":0,"output":"9f2c","truncated":false,"duration_ms":420}`)
	case agent.ToolContainerList:
		return json.RawMessage(`{"server_id":"srv-1","runtime":"podman","containers":[{"id":"a","name":"pg","state":"running"},{"id":"b","name":"redis","state":"exited"}]}`)
	case agent.ToolContainerInspect:
		return json.RawMessage(`{"server_id":"srv-1","container_id":"9f2c","container":{"id":"9f2cabcdef","name":"pg","state":"running"}}`)
	case agent.ToolContainerLogs:
		return json.RawMessage(`{"server_id":"srv-1","container_id":"9f2c","tail":50,"output":"a\nb\nc\n","truncated":true}`)
	case agent.ToolImageList:
		return json.RawMessage(`{"server_id":"srv-1","runtime":"docker","images":[{"repo":"pg"},{"repo":"nginx"}]}`)
	case agent.ToolDBQuery:
		return json.RawMessage(`{"database_id":"db-1","columns":[{"name":"id"}],"rows":[[1]],"row_count":1,"truncated":false,"duration_ms":9}`)
	case agent.ToolBatchExecute:
		return json.RawMessage(`{"batch_job_id":"BATCH_JOB_ID","status":"running","total":3,"executed":1,"awaiting_approval":1,"skipped":1}`)
	case agent.ToolRunDiagnosis:
		return json.RawMessage(`{"diagnosis_id":"diag-1","server_id":"srv-1","server_name":"web-1","status":"awaiting_fix","evidence_count":3,"suggestion_count":2,"severity":"warning","report":{"summary":"s"}}`)
	case agent.ToolRunInspection:
		return json.RawMessage(`{"report_id":"rep-1","trigger":"chat","template":"full","server_count":2,"avg_score":71,"anomaly_count":1,"items":[{"server_id":"srv-1","score":80,"truncated":false},{"server_id":"srv-2","score":62,"truncated":true}]}`)
	case agent.ToolListServers:
		return json.RawMessage(`{"servers":[{"id":"srv-1"},{"id":"srv-2"}]}`)
	case agent.ToolListDatabases:
		return json.RawMessage(`{"databases":[{"id":"db-1"}]}`)
	case agent.ToolGetMetrics:
		return json.RawMessage(`{"server_id":"srv-1","minutes":15,"points":[{"cpu_pct":1},{"cpu_pct":2}],"truncated":false}`)
	case agent.ToolListAlerts:
		return json.RawMessage(`{"alerts":[{"id":1}],"has_more":true}`)
	case agent.ToolAuditSearch:
		return json.RawMessage(`{"items":[{"id":1},{"id":2}],"has_more":false}`)
	case agent.ToolGetInspectionReport:
		return json.RawMessage(`{"report":{"report_id":"rep-9","server_count":2,"avg_score":88},"items":[]}`)
	case agent.ToolAskUser:
		return json.RawMessage(`{"question":"继续吗","status":"awaiting_user"}`)
	}
	return json.RawMessage(`{}`)
}

// seedAllToolsConversation 为 18 个工具各造一条成功的 tool_call（plus 两条
// 服务器资产），返回会话 ID。
func seedAllToolsConversation(t *testing.T, env *testEnv) string {
	t.Helper()
	const convID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	// batch_execute 的真实执行面来自 batch_jobs.server_ids（契约 7.8）：
	// 造一个真实 job，且目标与模型参数**故意不同**（参数 srv-1+srv-2，
	// 真实目标只有 srv-2），断言 affected_servers 以真实目标为准。
	resetBatchState()
	job, err := batchState.Create(context.Background(), store.CreateBatchJobParams{
		Command: "systemctl restart nginx", ServerIDs: []string{"srv-2"},
		Source: store.BatchSourceChat, ConversationID: ptrTo(convID),
	})
	if err != nil {
		t.Fatalf("create batch job: %v", err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "全工具", LastMessageAt: base}
	env.st.servers["srv-1"] = store.Server{ID: "srv-1", Name: "web-1", OS: store.ServerOSLinux, ContainerRuntime: store.ContainerRuntimePodman}
	env.st.servers["srv-2"] = store.Server{ID: "srv-2", Name: "web-2", OS: store.ServerOSWindows}

	argsFor := map[string]string{
		agent.ToolSSHExec:             `{"server_id":"srv-1","command":"uptime"}`,
		agent.ToolServerFacts:         `{"server_id":"srv-1"}`,
		agent.ToolContainerAction:     `{"server_id":"srv-1","container_id":"9f2c","action":"restart"}`,
		agent.ToolContainerList:       `{"server_id":"srv-1"}`,
		agent.ToolContainerInspect:    `{"server_id":"srv-1","container_id":"9f2c"}`,
		agent.ToolContainerLogs:       `{"server_id":"srv-1","container_id":"9f2c","tail":50}`,
		agent.ToolImageList:           `{"server_id":"srv-1"}`,
		agent.ToolDBQuery:             `{"database_id":"db-1","sql":"select 1"}`,
		agent.ToolBatchExecute:        `{"server_ids":["srv-1","srv-2"],"command":"systemctl restart nginx"}`,
		agent.ToolRunDiagnosis:        `{"server_id":"srv-1"}`,
		agent.ToolRunInspection:       `{"server_ids":["srv-1","srv-2"],"template":"full"}`,
		agent.ToolListServers:         `{}`,
		agent.ToolListDatabases:       `{}`,
		agent.ToolGetMetrics:          `{"server_id":"srv-1","minutes":15}`,
		agent.ToolListAlerts:          `{"limit":1}`,
		agent.ToolAuditSearch:         `{"limit":2}`,
		agent.ToolGetInspectionReport: `{"report_id":"rep-9"}`,
		agent.ToolAskUser:             `{"question":"继续吗"}`,
	}
	all := append(agent.ReplayActionTools(),
		"list_servers", "list_databases", "get_metrics", "list_alerts",
		"audit_search", "get_inspection_report", "ask_user")
	for i, name := range all {
		seq := int64(i + 1)
		started := base.Add(time.Duration(i) * time.Second)
		finished := started.Add(time.Second)
		result := replayVisibilityResult(name)
		if name == agent.ToolBatchExecute {
			result = json.RawMessage(strings.ReplaceAll(string(result), "BATCH_JOB_ID", job.ID))
		}
		env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
			ID: fmt.Sprintf("tc-all-%02d", seq), Seq: seq, ConversationID: convID,
			TurnID: "turn-1", ProviderCallID: fmt.Sprintf("call-%02d", seq), Name: name,
			Arguments: json.RawMessage(argsFor[name]), Result: result,
			RiskLevel: "safe", Status: store.ToolSucceeded,
			StartedAt: started, FinishedAt: &finished,
		})
	}
	return convID
}

func fetchReplay(t *testing.T, env *testEnv, convID string) ([]replayStep, bool, string) {
	t.Helper()
	rec := env.do(http.MethodGet, "/api/conversations/"+convID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Steps           []replayStep `json:"steps"`
		Truncated       bool         `json:"truncated"`
		TruncatedReason string       `json:"truncated_reason"`
	}
	decodeBody(t, rec, &resp)
	return resp.Steps, resp.Truncated, resp.TruncatedReason
}

func findReplayStep(steps []replayStep, kind, toolCallID string) *replayStep {
	for i := range steps {
		if steps[i].Kind == kind && steps[i].ToolCallID != nil && *steps[i].ToolCallID == toolCallID {
			return &steps[i]
		}
	}
	return nil
}

// TestReplayAllToolsVisible 是 G41 ①②③ 的主体。
func TestReplayAllToolsVisible(t *testing.T) {
	env := newTestEnv(t)
	convID := seedAllToolsConversation(t, env)
	steps, truncated, reason := fetchReplay(t, env, convID)
	if truncated || reason != "" {
		t.Fatalf("truncated=%v reason=%q, want no truncation", truncated, reason)
	}

	executionTools := map[string]bool{}
	toolCallCount := 0
	for _, s := range steps {
		if s.Kind == "tool_call" {
			toolCallCount++
			if s.ResultSummary == nil {
				t.Errorf("tool_call %v: result_summary is null", s.Name)
				continue
			}
			if n := len([]rune(*s.ResultSummary)); n == 0 || n > replaySummaryMaxRunes {
				t.Errorf("tool_call %v: result_summary length %d", s.Name, n)
			}
			// 契约：tool_call 步的 result_truncated / affected_servers 恒 null。
			if s.ResultTruncated != nil {
				t.Errorf("tool_call %v: result_truncated = %v, want null", s.Name, *s.ResultTruncated)
			}
			if s.AffectedServers != nil {
				t.Errorf("tool_call %v: affected_servers = %v, want null", s.Name, s.AffectedServers)
			}
			continue
		}
		if s.Kind != "execution" {
			continue
		}
		if s.Name == nil {
			t.Fatal("execution step without name")
		}
		executionTools[*s.Name] = true
		// execution 步：command/result_summary/result_truncated/affected_servers/
		// duration_ms 恒非 null；arguments 恒 null（契约 7.8 补充条款）。
		if s.Command == nil {
			t.Errorf("execution %s: command is null", *s.Name)
		}
		if s.ResultSummary == nil {
			t.Errorf("execution %s: result_summary is null", *s.Name)
		}
		if s.ResultTruncated == nil {
			t.Errorf("execution %s: result_truncated is null", *s.Name)
		}
		if s.DurationMS == nil {
			t.Errorf("execution %s: duration_ms is null (契约 7.8 恒非 null)", *s.Name)
		}
		// arguments 恒 null（JSON null 经 RawMessage 往返后是字面 "null"）。
		if len(s.Arguments) != 0 && string(s.Arguments) != "null" {
			t.Errorf("execution %s: arguments = %s, want null", *s.Name, s.Arguments)
		}
	}
	if toolCallCount != 18 {
		t.Fatalf("tool_call steps = %d, want 18", toolCallCount)
	}
	// ① 11 个动作类工具各一条 execution 步。
	for _, name := range agent.ReplayActionTools() {
		if !executionTools[name] {
			t.Errorf("action tool %s: missing execution step", name)
		}
	}
	// ② 7 个本地查询/交互工具不生成 execution 步。
	for _, name := range []string{
		"list_servers", "list_databases", "get_metrics", "list_alerts",
		"audit_search", "get_inspection_report", "ask_user",
	} {
		if executionTools[name] {
			t.Errorf("local tool %s must not produce an execution step", name)
		}
	}
	if len(executionTools) != 11 {
		t.Fatalf("execution tools = %d (%v), want exactly 11", len(executionTools), executionTools)
	}
}

// TestReplayExecutionOutputByTool 是 G41 ④：output 按 7.8 的固定键取值。
func TestReplayExecutionOutputByTool(t *testing.T) {
	env := newTestEnv(t)
	convID := seedAllToolsConversation(t, env)
	steps, _, _ := fetchReplay(t, env, convID)

	cases := []struct {
		tool   string
		output string
	}{
		{agent.ToolSSHExec, "up 3 days"},
		{agent.ToolServerFacts, "hostname=web-1\n"},
		{agent.ToolContainerAction, "9f2c"},
		{agent.ToolContainerLogs, "a\nb\nc\n"},
	}
	for _, tc := range cases {
		st := findReplayStep(steps, "execution", toolCallIDFor(t, env, convID, tc.tool))
		if st == nil {
			t.Fatalf("%s: execution step missing", tc.tool)
		}
		if st.Output == nil || *st.Output != tc.output {
			t.Errorf("%s: output = %v, want %q", tc.tool, st.Output, tc.output)
		}
	}
	// 结构化工具：output = JSON.stringify(result)（逐字等于落库 result）。
	for _, tool := range []string{
		agent.ToolContainerList, agent.ToolContainerInspect, agent.ToolImageList,
		agent.ToolDBQuery, agent.ToolBatchExecute, agent.ToolRunDiagnosis, agent.ToolRunInspection,
	} {
		st := findReplayStep(steps, "execution", toolCallIDFor(t, env, convID, tool))
		if st == nil {
			t.Fatalf("%s: execution step missing", tool)
		}
		want := string(replayVisibilityResult(tool))
		// batch_execute 的 result 里 batch_job_id 在 seed 时替换成真实 job ID。
		if tool == agent.ToolBatchExecute {
			env.st.mu.Lock()
			for _, tc := range env.st.toolCalls {
				if tc.Name == tool && tc.ConversationID == convID {
					want = string(tc.Result)
				}
			}
			env.st.mu.Unlock()
		}
		if st.Output == nil || *st.Output != want {
			t.Errorf("%s: output = %v, want raw JSON", tool, st.Output)
		}
	}
}

func toolCallIDFor(t *testing.T, env *testEnv, convID, tool string) string {
	t.Helper()
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	for _, tc := range env.st.toolCalls {
		if tc.ConversationID == convID && tc.Name == tool {
			return tc.ID
		}
	}
	t.Fatalf("tool call %s not found", tool)
	return ""
}

// TestReplayExecutionFieldsPerTool 是 G41 ④⑤⑥：exit_code 只有两个命令类、
// result_truncated 取自工具、affected_servers 与资源句柄。
func TestReplayExecutionFieldsPerTool(t *testing.T) {
	env := newTestEnv(t)
	convID := seedAllToolsConversation(t, env)
	steps, _, _ := fetchReplay(t, env, convID)

	get := func(tool string) *replayStep {
		st := findReplayStep(steps, "execution", toolCallIDFor(t, env, convID, tool))
		if st == nil {
			t.Fatalf("%s: execution step missing", tool)
		}
		return st
	}

	// exit_code：只有 ssh_exec/container_action 非 null。
	for _, name := range agent.ReplayActionTools() {
		st := get(name)
		hasExit := name == agent.ToolSSHExec || name == agent.ToolContainerAction
		if hasExit != (st.ExitCode != nil) {
			t.Errorf("%s: exit_code = %v, want non-null=%v", name, st.ExitCode, hasExit)
		}
	}

	// result_truncated 来源（契约 7.8）：ssh_exec 的 result.truncated、
	// container_logs 的 truncated、run_inspection 的任一 item truncated。
	if st := get(agent.ToolSSHExec); st.ResultTruncated == nil || !*st.ResultTruncated {
		t.Error("ssh_exec: result_truncated must be true (result.truncated=true)")
	}
	if st := get(agent.ToolContainerLogs); st.ResultTruncated == nil || !*st.ResultTruncated {
		t.Error("container_logs: result_truncated must be true")
	}
	if st := get(agent.ToolRunInspection); st.ResultTruncated == nil || !*st.ResultTruncated {
		t.Error("run_inspection: result_truncated must be true (an item is truncated)")
	}
	if st := get(agent.ToolContainerList); st.ResultTruncated == nil || *st.ResultTruncated {
		t.Error("container_list: result_truncated must be false")
	}

	// affected_servers：单目标 1 元素（名字）、db_query 为 null（且字段不出现
	// 在本地工具上，已在上一测试断言）、batch/inspection 多元素按名字升序。
	if st := get(agent.ToolSSHExec); len(st.AffectedServers) != 1 || st.AffectedServers[0] != "web-1" {
		t.Errorf("ssh_exec affected_servers = %v", st.AffectedServers)
	}
	if st := get(agent.ToolDBQuery); st.AffectedServers != nil {
		t.Errorf("db_query affected_servers = %v, want null", st.AffectedServers)
	}
	// batch_execute：真实执行面（batch_jobs.server_ids）优先——模型参数是
	// [srv-1,srv-2]，但 job 的真实目标只有 srv-2，故必须是 [web-2]。
	if st := get(agent.ToolBatchExecute); len(st.AffectedServers) != 1 || st.AffectedServers[0] != "web-2" {
		t.Errorf("batch_execute affected_servers = %v, want [web-2] (真实执行面)", st.AffectedServers)
	}
	// run_inspection：优先取报告 items 的 server_id（真实执行面）。
	if st := get(agent.ToolRunInspection); len(st.AffectedServers) != 2 {
		t.Errorf("run_inspection affected_servers = %v", st.AffectedServers)
	}
	// server_name：单目标动作与 affected_servers 同值；多目标/无目标为 null
	// （契约 7.8 的补充条款）。必须在删除 srv-2 之前断言（否则名字变占位）。
	if st := get(agent.ToolSSHExec); st.ServerName == nil || *st.ServerName != "web-1" {
		t.Errorf("ssh_exec server_name = %v, want web-1", st.ServerName)
	}
	if st := get(agent.ToolContainerLogs); st.ServerName == nil || *st.ServerName != "web-1" {
		t.Errorf("container_logs server_name = %v, want web-1", st.ServerName)
	}
	// 契约口径是「affected_servers 恰好 1 个时同值」：本 fixture 的批量真实
	// 目标只有 srv-2（1 元素），故 server_name=web-2；多目标见 run_inspection。
	if st := get(agent.ToolBatchExecute); st.ServerName == nil || *st.ServerName != "web-2" {
		t.Errorf("batch_execute server_name = %v, want web-2", st.ServerName)
	}
	if st := get(agent.ToolRunInspection); st.ServerName != nil {
		t.Errorf("run_inspection server_name = %v, want null (multi-target)", st.ServerName)
	}
	if st := get(agent.ToolDBQuery); st.ServerName != nil {
		t.Errorf("db_query server_name = %v, want null (no server)", st.ServerName)
	}

	// 已删除资产占位：srv-2 删除后 → <已删除>。
	env.st.mu.Lock()
	delete(env.st.servers, "srv-2")
	env.st.mu.Unlock()
	steps, _, _ = fetchReplay(t, env, convID)
	// 已删资产的占位（run_inspection 的两个目标里 srv-2 已删除）。
	if st := findReplayStep(steps, "execution", toolCallIDFor(t, env, convID, agent.ToolRunInspection)); st == nil ||
		len(st.AffectedServers) != 2 || st.AffectedServers[0] != "web-1" ||
		st.AffectedServers[1] != replayDeletedServerPlaceholder {
		t.Errorf("deleted server placeholder = %+v", st)
	}

	// 资源句柄只在对应工具出现。
	if st := get(agent.ToolRunDiagnosis); st.DiagnosisID == nil || *st.DiagnosisID != "diag-1" {
		t.Errorf("run_diagnosis diagnosis_id = %v", st.DiagnosisID)
	}
	if st := get(agent.ToolRunInspection); st.InspectionReportID == nil || *st.InspectionReportID != "rep-1" {
		t.Errorf("run_inspection inspection_report_id = %v", st.InspectionReportID)
	}
	if st := get(agent.ToolBatchExecute); st.BatchJobID == nil || *st.BatchJobID != "job-1" {
		t.Errorf("batch_execute batch_job_id = %v", st.BatchJobID)
	}
	for _, name := range []string{agent.ToolSSHExec, agent.ToolContainerList, agent.ToolDBQuery} {
		st := get(name)
		if st.DiagnosisID != nil || st.InspectionReportID != nil || st.BatchJobID != nil {
			t.Errorf("%s: resource handles must be null: %+v", name, st)
		}
	}

	// command 动作串（契约 2.12 约定 3）：命令类取 result.command；容器只读
	// 按 runtime 展开；db_query 取实际 SQL；编排取 template/id。
	commandWants := map[string]string{
		agent.ToolSSHExec:          "uptime",
		agent.ToolContainerAction:  "docker restart 9f2c",
		agent.ToolContainerList:    "podman ps -a",
		agent.ToolContainerInspect: "podman inspect 9f2c",
		agent.ToolContainerLogs:    "podman logs --tail 50 9f2c",
		agent.ToolImageList:        "docker image ls",
		agent.ToolDBQuery:          "select 1",
		agent.ToolBatchExecute:     "systemctl restart nginx",
		agent.ToolRunDiagnosis:     "diagnosis:diag-1",
		agent.ToolRunInspection:    "inspection:full",
	}
	for tool, want := range commandWants {
		st := get(tool)
		if st.Command == nil || *st.Command != want {
			t.Errorf("%s: command = %v, want %q", tool, st.Command, want)
		}
	}
	// server_facts 的动作串是模板标识（linux 资产 → facts:linux）。
	if st := get(agent.ToolServerFacts); st.Command == nil || *st.Command != "facts:linux" {
		t.Errorf("server_facts: command = %v, want facts:linux", st.Command)
	}

	// duration_ms 恒非 null：无 result.duration_ms 的工具走 finished-started。
	if st := get(agent.ToolContainerList); st.DurationMS == nil || *st.DurationMS != 1000 {
		t.Errorf("container_list: duration_ms = %v, want 1000 (finished-started)", st.DurationMS)
	}
	if st := get(agent.ToolSSHExec); st.DurationMS == nil || *st.DurationMS != 88 {
		t.Errorf("ssh_exec: duration_ms = %v, want 88 (result.duration_ms)", st.DurationMS)
	}
}

// TestReplayTruncatedWrapperDegrades 覆盖契约 7.8 的整包 64KB 截断包装。
func TestReplayTruncatedWrapperDegrades(t *testing.T) {
	env := newTestEnv(t)
	const convID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	fin := base.Add(2 * time.Second)
	env.st.mu.Lock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "截断包装"}
	env.st.servers["srv-1"] = store.Server{ID: "srv-1", Name: "web-1"}
	env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
		ID: "tc-wrap", Seq: 1, ConversationID: convID, TurnID: "t", ProviderCallID: "c",
		Name:      agent.ToolServerFacts,
		Arguments: json.RawMessage(`{"server_id":"srv-1"}`),
		Result:    json.RawMessage(`{"truncated":true,"original_bytes":131072,"preview":"hostname=web-1\n"}`),
		RiskLevel: "safe", Status: store.ToolSucceeded, StartedAt: base, FinishedAt: &fin,
	})
	env.st.mu.Unlock()

	steps, _, _ := fetchReplay(t, env, convID)
	st := findReplayStep(steps, "execution", "tc-wrap")
	if st == nil {
		t.Fatal("execution step missing for wrapped result")
	}
	if st.Output == nil || *st.Output != "hostname=web-1\n" {
		t.Errorf("output = %v, want preview", st.Output)
	}
	if st.ResultTruncated == nil || !*st.ResultTruncated {
		t.Error("result_truncated must be true")
	}
	if st.ResultSummary == nil || *st.ResultSummary != "输出超限（原始 131072B），仅预览" {
		t.Errorf("result_summary = %v", *st.ResultSummary)
	}
	if st.ExitCode != nil {
		t.Errorf("exit_code = %v, want null (wrapper)", st.ExitCode)
	}
	if st.DurationMS == nil || *st.DurationMS != 2000 {
		t.Errorf("duration_ms = %v, want 2000 (finished-started)", st.DurationMS)
	}
}

// TestReplayFailedAndDeniedActionTools 覆盖 G41 ⑦：failed 无 result 生成
// exit_code=-1 的 execution 步（仅命令类；其余工具 exit_code=null），
// denied/expired 不生成。
func TestReplayFailedAndDeniedActionTools(t *testing.T) {
	env := newTestEnv(t)
	const convID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	fin := base.Add(time.Second)
	env.st.mu.Lock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "失败"}
	env.st.servers["srv-1"] = store.Server{ID: "srv-1", Name: "web-1"}
	env.st.toolCalls = append(env.st.toolCalls,
		store.ToolCall{
			ID: "tc-fail-ssh", Seq: 1, ConversationID: convID, TurnID: "t", ProviderCallID: "c1",
			Name: agent.ToolSSHExec, Arguments: json.RawMessage(`{"server_id":"srv-1","command":"uptime"}`),
			RiskLevel: "needs_approval", Status: store.ToolFailed, ErrorCode: "SSH_EXEC_FAILED",
			StartedAt: base, FinishedAt: &fin,
		},
		store.ToolCall{
			ID: "tc-fail-list", Seq: 2, ConversationID: convID, TurnID: "t", ProviderCallID: "c2",
			Name: agent.ToolContainerList, Arguments: json.RawMessage(`{"server_id":"srv-1"}`),
			RiskLevel: "safe", Status: store.ToolFailed, ErrorCode: "CONTAINER_CLI_FAILED",
			StartedAt: base, FinishedAt: &fin,
		},
		store.ToolCall{
			ID: "tc-denied", Seq: 3, ConversationID: convID, TurnID: "t", ProviderCallID: "c3",
			Name: agent.ToolBatchExecute, Arguments: json.RawMessage(`{"server_ids":["srv-1"],"command":"reboot"}`),
			RiskLevel: "needs_approval", Status: store.ToolDenied, ErrorCode: "APPROVAL_DENIED",
			StartedAt: base,
		},
		store.ToolCall{
			ID: "tc-expired", Seq: 4, ConversationID: convID, TurnID: "t", ProviderCallID: "c4",
			Name: agent.ToolRunDiagnosis, Arguments: json.RawMessage(`{"server_id":"srv-1"}`),
			RiskLevel: "safe", Status: store.ToolExpired, ErrorCode: "APPROVAL_TIMEOUT",
			StartedAt: base,
		},
	)
	env.st.mu.Unlock()

	steps, _, _ := fetchReplay(t, env, convID)
	sshExec := findReplayStep(steps, "execution", "tc-fail-ssh")
	if sshExec == nil {
		t.Fatal("failed ssh_exec: execution step missing")
	}
	if sshExec.ExitCode == nil || *sshExec.ExitCode != -1 {
		t.Errorf("failed ssh_exec exit_code = %v, want -1", sshExec.ExitCode)
	}
	if sshExec.ResultSummary == nil || *sshExec.ResultSummary != "error=SSH_EXEC_FAILED" {
		t.Errorf("failed ssh_exec summary = %v", sshExec.ResultSummary)
	}
	if sshExec.Command == nil || *sshExec.Command != "uptime" {
		t.Errorf("failed ssh_exec command = %v, want arguments fallback", sshExec.Command)
	}
	if sshExec.Output == nil || *sshExec.Output != "" {
		t.Errorf("failed ssh_exec output = %v, want empty", sshExec.Output)
	}

	cli := findReplayStep(steps, "execution", "tc-fail-list")
	if cli == nil {
		t.Fatal("failed container_list: execution step missing")
	}
	// 契约 7.8 补充条款：failed 且无 result 的动作类工具一律 exit_code=-1
	// （六期裁决扩展到全部动作类）；正常路径才只给两个命令类工具。
	if cli.ExitCode == nil || *cli.ExitCode != -1 {
		t.Errorf("failed container_list exit_code = %v, want -1", cli.ExitCode)
	}

	if findReplayStep(steps, "execution", "tc-denied") != nil {
		t.Error("denied call must not produce an execution step")
	}
	if findReplayStep(steps, "execution", "tc-expired") != nil {
		t.Error("expired call must not produce an execution step")
	}
	// tool_call 步仍然带摘要（错误摘要）。
	for _, id := range []string{"tc-denied", "tc-expired"} {
		st := findReplayStep(steps, "tool_call", id)
		if st == nil || st.ResultSummary == nil || *st.ResultSummary == "" {
			t.Errorf("%s: tool_call result_summary missing", id)
		}
	}
}

// TestReplayPayloadBudgetArguments 覆盖 G41 ⑧ 第一道预算：arguments 合计
// 超 6 MiB 后更晚步 arguments=null 且 truncated_reason=payload。
func TestReplayPayloadBudgetArguments(t *testing.T) {
	env := newTestEnv(t)
	const convID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	base := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	env.st.mu.Lock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "大参数"}
	// 每条约 2.5 MiB 的参数：3 条合计 7.5 MiB > 6 MiB 预算。
	blob := strings.Repeat("x", 2_500_000)
	for i := 0; i < 3; i++ {
		args, err := json.Marshal(map[string]string{"server_id": "srv-1", "command": blob})
		if err != nil {
			t.Fatal(err)
		}
		env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
			ID: fmt.Sprintf("tc-big-%d", i), Seq: int64(i + 1), ConversationID: convID,
			TurnID: "t", ProviderCallID: fmt.Sprintf("c%d", i), Name: agent.ToolSSHExec,
			Arguments: args,
			Result:    json.RawMessage(`{"server_id":"srv-1","command":"x","exit_code":0,"output":"ok","truncated":false,"duration_ms":5}`),
			RiskLevel: "safe", Status: store.ToolSucceeded,
			StartedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	env.st.mu.Unlock()

	steps, truncated, reason := fetchReplay(t, env, convID)
	if !truncated || reason != replayTruncatedPayload {
		t.Fatalf("truncated=%v reason=%q, want payload", truncated, reason)
	}
	var kept, dropped int
	for _, s := range steps {
		if s.Kind != "tool_call" {
			continue
		}
		if replayArgumentsNull(s.Arguments) {
			dropped++
		} else {
			kept++
		}
	}
	if dropped != 1 || kept != 2 {
		t.Fatalf("arguments kept=%d dropped=%d, want 2/1 (按 seq 升序累加)", kept, dropped)
	}
	// 更晚的（seq 最大的）那条被置 null。
	last := findReplayStep(steps, "tool_call", "tc-big-2")
	if last == nil || !replayArgumentsNull(last.Arguments) {
		t.Fatal("latest tool_call arguments must be null after budget")
	}
	// execution 步的 output 不受 arguments 预算影响。
	if st := findReplayStep(steps, "execution", "tc-big-2"); st == nil || st.Output == nil || *st.Output != "ok" {
		t.Fatalf("execution output must survive arguments budget: %+v", st)
	}
}

// TestReplayPayloadBudgetSerialized 覆盖 G41 ⑧ 第二道预算：响应序列化超
// 8 MiB 时最旧步 output=” 且 result_truncated=true（摘要保留）。
func TestReplayPayloadBudgetSerialized(t *testing.T) {
	env := newTestEnv(t)
	const convID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	env.st.mu.Lock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "大输出"}
	// 每步 2 MiB 输出 × 5 步 = 10 MiB > 8 MiB。
	blob := strings.Repeat("y", 2<<20)
	for i := 0; i < 5; i++ {
		result, err := json.Marshal(map[string]any{
			"server_id": "srv-1", "command": "cat big", "exit_code": 0,
			"output": blob, "truncated": false, "duration_ms": 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
			ID: fmt.Sprintf("tc-out-%d", i), Seq: int64(i + 1), ConversationID: convID,
			TurnID: "t", ProviderCallID: fmt.Sprintf("c%d", i), Name: agent.ToolSSHExec,
			Arguments: json.RawMessage(`{"server_id":"srv-1","command":"cat big"}`),
			Result:    result, RiskLevel: "safe", Status: store.ToolSucceeded,
			StartedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	env.st.mu.Unlock()

	steps, truncated, reason := fetchReplay(t, env, convID)
	if !truncated || reason != replayTruncatedPayload {
		t.Fatalf("truncated=%v reason=%q, want payload", truncated, reason)
	}
	var cleared int
	for _, s := range steps {
		if s.Kind != "execution" {
			continue
		}
		if s.Output == nil {
			t.Fatal("execution output must stay non-null ('' = 已清空)")
		}
		if *s.Output == "" {
			cleared++
			if s.ResultTruncated == nil || !*s.ResultTruncated {
				t.Error("cleared output must set result_truncated=true")
			}
			if s.ResultSummary == nil || *s.ResultSummary == "" {
				t.Error("cleared output must keep result_summary")
			}
		}
	}
	if cleared == 0 {
		t.Fatal("serialized budget must clear at least one output")
	}
	// 清空从最旧步开始：第一条必须已被清空。
	first := findReplayStep(steps, "execution", "tc-out-0")
	if first == nil || first.Output == nil || *first.Output != "" {
		t.Fatalf("oldest step output must be cleared first: %+v", first)
	}
}

// TestReplayPayloadBudgetStepsPriority 覆盖 ⑧ 的优先级：两种预算同时命中且
// 需要丢步时，truncated_reason 报 steps（契约 2.12 约定 6）。
func TestReplayPayloadBudgetStepsPriority(t *testing.T) {
	// 20001 条普通步骤 + 一条超长 output：步数先命中 → reason=steps。
	steps := make([]replayStep, 0, replayMaxSteps+2)
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i <= replayMaxSteps+1; i++ {
		kind := replayKindUserMessage
		s := replayStep{
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Kind:      replayKindName(kind), kindWeight: kind,
			sourceKey: fmt.Sprintf("%020d", i),
		}
		content := "x"
		s.Content = &content
		steps = append(steps, s)
	}
	steps, truncated, reason := replayApplyTruncation(steps, true)
	if !truncated || reason != replayTruncatedSteps {
		t.Fatalf("truncated=%v reason=%q, want steps", truncated, reason)
	}
	// 再叠一道 arguments 预算命中：reason 必须保持 steps（优先级最高）。
	big := make([]byte, replayArgumentsBudgetBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	steps[0].Kind = replayKindName(replayKindToolCall)
	steps[0].Arguments = big
	if !replayApplyArgumentsBudget(steps, replayArgumentsBudgetBytes) {
		t.Fatal("arguments budget must trigger")
	}
	if reason != replayTruncatedSteps {
		t.Fatalf("reason = %q, want steps to stay dominant", reason)
	}
}

// TestReplayServerMapFailureDoesNotBlock 覆盖契约 5.19.2 第 4 条：servers.List
// 失败不阻断回放（全部输出 <已删除> 占位）。
func TestReplayServerMapFailureDoesNotBlock(t *testing.T) {
	env := newTestEnv(t)
	const convID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	base := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	env.st.mu.Lock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "资产失败"}
	env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
		ID: "tc-x", Seq: 1, ConversationID: convID, TurnID: "t", ProviderCallID: "c",
		Name: agent.ToolSSHExec, Arguments: json.RawMessage(`{"server_id":"srv-1","command":"uptime"}`),
		Result:    json.RawMessage(`{"server_id":"srv-1","command":"uptime","exit_code":0,"output":"ok","truncated":false,"duration_ms":1}`),
		RiskLevel: "safe", Status: store.ToolSucceeded, StartedAt: base,
	})
	env.st.errServersList = fmt.Errorf("boom")
	env.st.mu.Unlock()
	t.Cleanup(func() {
		env.st.mu.Lock()
		env.st.errServersList = nil
		env.st.mu.Unlock()
	})

	steps, _, _ := fetchReplay(t, env, convID)
	st := findReplayStep(steps, "execution", "tc-x")
	if st == nil {
		t.Fatal("replay must not fail when servers.List fails")
	}
	if len(st.AffectedServers) != 1 || st.AffectedServers[0] != replayDeletedServerPlaceholder {
		t.Fatalf("affected_servers = %v, want <已删除>", st.AffectedServers)
	}
}

// TestReplayArgumentsBudgetPure 是 replayApplyArgumentsBudget 的纯函数边界。
func TestReplayArgumentsBudgetPure(t *testing.T) {
	mk := func(seq int, size int) replayStep {
		return replayStep{
			Kind: replayKindName(replayKindToolCall), sourceKey: fmt.Sprintf("%020d", seq),
			Arguments: json.RawMessage(strings.Repeat("a", size)),
		}
	}
	// 未超预算：全部保留、不报告命中。
	steps := []replayStep{mk(1, 1000), mk(2, 1000)}
	if replayApplyArgumentsBudget(steps, replayArgumentsBudgetBytes) {
		t.Fatal("small arguments must not trigger the budget")
	}
	if replayArgumentsNull(steps[0].Arguments) || replayArgumentsNull(steps[1].Arguments) {
		t.Fatal("small arguments must be kept")
	}
	// 超预算：命中后所有更晚的 tool_call 一律置 null（含后续小参数）。
	steps = []replayStep{mk(1, replayArgumentsBudgetBytes-10), mk(2, 100), mk(3, 1)}
	if !replayApplyArgumentsBudget(steps, replayArgumentsBudgetBytes) {
		t.Fatal("budget must trigger")
	}
	if replayArgumentsNull(steps[0].Arguments) {
		t.Fatal("first step within budget must keep arguments")
	}
	if !replayArgumentsNull(steps[1].Arguments) || !replayArgumentsNull(steps[2].Arguments) {
		t.Fatal("later steps must drop arguments once the budget is hit")
	}
	// 非 tool_call 步不受影响。
	msg := replayStep{Kind: replayKindName(replayKindUserMessage)}
	content := "hello"
	msg.Content = &content
	steps = []replayStep{msg}
	if replayApplyArgumentsBudget(steps, replayArgumentsBudgetBytes) {
		t.Fatal("no tool_call steps → no budget hit")
	}
}

// TestReplayCatalogHasActionsCountsFailedCalls 断言「动作事实」口径：failed
// 的动作类调用同样计入 action_count（契约 ReplayCatalogItem.ActionCount 注释）。
func TestReplayCatalogHasActionsCountsFailedCalls(t *testing.T) {
	env := newTestEnv(t)
	const convID = "99999999-9999-4999-8999-999999999999"
	base := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	env.st.mu.Lock()
	env.st.conversations[convID] = store.Conversation{ID: convID, Title: "失败动作", LastMessageAt: base}
	env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
		ID: "tc-f", Seq: 1, ConversationID: convID, TurnID: "t", ProviderCallID: "c",
		Name: agent.ToolContainerList, Arguments: json.RawMessage(`{}`),
		RiskLevel: "safe", Status: store.ToolFailed, ErrorCode: "CONTAINER_CLI_FAILED",
		StartedAt: base,
	})
	env.st.mu.Unlock()

	rec := env.do(http.MethodGet, "/api/replays?has_actions=true", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var page struct {
		Items []store.ReplayCatalogItem `json:"items"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ActionCount != 1 || !page.Items[0].HasActions {
		t.Fatalf("failed action call must count: %+v", page.Items)
	}
}

func ptrTo(s string) *string { return &s }

// replayArgumentsNull 判定序列化往返后的 arguments 是否为 null（JSON null
// 经 json.RawMessage 往返为字面 "null"）。
func replayArgumentsNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// TestReplayPayloadBudgetUnified 覆盖契约 5.19.2 第 5 条的统一入口
// replayApplyPayloadBudget：两道预算组合时返回三个独立事实，且丢步会同时
// 报告 payload 与 steps。
func TestReplayPayloadBudgetUnified(t *testing.T) {
	// 小数据：两道都不命中。
	content := "x"
	small := []replayStep{
		{Kind: replayKindName(replayKindUserMessage), Content: &content},
		{Kind: replayKindName(replayKindToolCall), Arguments: json.RawMessage(`{"a":1}`)},
	}
	out, payload, dropped := replayApplyPayloadBudget(small, replayArgumentsBudgetBytes, replaySerializedBudgetBytes)
	if payload || dropped || len(out) != 2 {
		t.Fatalf("small: payload=%v dropped=%v len=%d", payload, dropped, len(out))
	}

	// 只有 arguments 超预算：payload=true、dropped=false。
	bigArgs := make([]byte, replayArgumentsBudgetBytes+1)
	for i := range bigArgs {
		bigArgs[i] = 'a'
	}
	steps := []replayStep{{Kind: replayKindName(replayKindToolCall), Arguments: bigArgs}}
	out, payload, dropped = replayApplyPayloadBudget(steps, replayArgumentsBudgetBytes, replaySerializedBudgetBytes)
	if !payload || dropped || len(out) != 1 || replayArgumentsNull(out[0].Arguments) != true {
		t.Fatalf("args-only: payload=%v dropped=%v %+v", payload, dropped, out[0])
	}

	// 只有 output 超预算：payload=true、dropped=false、输出被清空。
	huge := strings.Repeat("z", replaySerializedBudgetBytes+1)
	steps = []replayStep{{
		Kind: replayKindName(replayKindExecution), Output: &huge,
	}}
	out, payload, dropped = replayApplyPayloadBudget(steps, replayArgumentsBudgetBytes, replaySerializedBudgetBytes)
	if !payload || dropped || len(out) != 1 {
		t.Fatalf("output-only: payload=%v dropped=%v len=%d", payload, dropped, len(out))
	}
	if out[0].Output == nil || *out[0].Output != "" {
		t.Fatal("output must be cleared")
	}
	if out[0].ResultTruncated == nil || !*out[0].ResultTruncated {
		t.Fatal("cleared output must flag result_truncated")
	}
}
