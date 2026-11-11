// replay_summary_test.go 覆盖八期 replayResultSummary（契约 5.19.3 / 7.8 /
// G41 ③）：18 个工具 × {有结果, 无结果, 错误} 三态表驱动，外加整包 64KB
// 截断包装的降级、≤200 rune 上限与「未知工具/未知形状不 panic」。
package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"aiops/internal/agent"
)

// replaySummaryFixtures 是 18 个工具的「有结果」样例（对应 agent 侧 result
// 逐字形状：runner.go / runner_container.go / runner_monitor.go /
// runner_diagnosis.go / runner_batch.go）。
var replaySummaryFixtures = map[string]json.RawMessage{
	agent.ToolSSHExec:          json.RawMessage(`{"server_id":"s1","command":"df -h","exit_code":0,"output":"Filesystem\n/dev/sda1","truncated":false,"duration_ms":120}`),
	agent.ToolServerFacts:      json.RawMessage(`{"server_id":"s1","facts":{"hostname":"web-1"},"raw_output":"hostname=web-1\n","duration_ms":300}`),
	agent.ToolContainerAction:  json.RawMessage(`{"server_id":"s1","container_id":"9f2c","action":"stop","command":"docker stop 9f2c","exit_code":0,"output":"9f2c","truncated":false,"duration_ms":400}`),
	agent.ToolContainerList:    json.RawMessage(`{"server_id":"s1","runtime":"docker","containers":[{"id":"a"},{"id":"b"}]}`),
	agent.ToolContainerInspect: json.RawMessage(`{"server_id":"s1","container_id":"9f2c","container":{"id":"9f2c","name":"pg","state":"running"}}`),
	agent.ToolContainerLogs:    json.RawMessage(`{"server_id":"s1","container_id":"9f2c","tail":200,"output":"line1\nline2\n","truncated":false}`),
	agent.ToolImageList:        json.RawMessage(`{"server_id":"s1","runtime":"docker","images":[{"repo":"pg"},{"repo":"nginx"},{"repo":"redis"}]}`),
	agent.ToolDBQuery:          json.RawMessage(`{"database_id":"db1","columns":[],"rows":[],"row_count":7,"truncated":false,"duration_ms":12}`),
	agent.ToolBatchExecute:     json.RawMessage(`{"batch_job_id":"b1","status":"running","total":3,"executed":1,"awaiting_approval":1,"skipped":1}`),
	agent.ToolRunDiagnosis:     json.RawMessage(`{"diagnosis_id":"d1","server_id":"s1","server_name":"web-1","status":"done","evidence_count":4,"suggestion_count":2,"severity":"warning","report":{}}`),
	agent.ToolRunInspection:    json.RawMessage(`{"report_id":"r1","trigger":"chat","template":"full","server_count":3,"avg_score":72,"anomaly_count":2,"items":[{"server_id":"s1","truncated":false}]}`),
	agent.ToolListServers:      json.RawMessage(`{"servers":[{"id":"s1"},{"id":"s2"}]}`),
	agent.ToolListDatabases:    json.RawMessage(`{"databases":[{"id":"db1"}]}`),
	agent.ToolGetMetrics:       json.RawMessage(`{"server_id":"s1","minutes":30,"points":[{"created_at":"x"},{"created_at":"y"}],"truncated":false}`),
	agent.ToolListAlerts:       json.RawMessage(`{"alerts":[{"id":1},{"id":2}],"has_more":false}`),
	agent.ToolAuditSearch:      json.RawMessage(`{"items":[{"id":1}],"has_more":true}`),
	agent.ToolGetInspectionReport: json.RawMessage(
		`{"report":{"report_id":"r9","server_count":2,"avg_score":88},"items":[]}`),
	agent.ToolAskUser: json.RawMessage(`{"question":"继续吗","status":"awaiting_user"}`),
}

func TestReplayResultSummaryAllTools(t *testing.T) {
	if len(replaySummaryFixtures) != 18 {
		t.Fatalf("fixtures = %d, want 18 (契约 7.8 的固定 18 个工具)", len(replaySummaryFixtures))
	}
	// 有结果：每个工具都必须给出非空、非「无结果」的摘要。
	for name, result := range replaySummaryFixtures {
		got, truncated := replayResultSummary(name, result, "")
		if got == "" || got == replayNoResultSummary {
			t.Errorf("%s: summary = %q, want a real summary", name, got)
		}
		if truncated {
			t.Errorf("%s: summary truncated unexpectedly (%q)", name, got)
		}
		if n := utf8.RuneCountInString(got); n > replaySummaryMaxRunes {
			t.Errorf("%s: summary %d runes > %d", name, n, replaySummaryMaxRunes)
		}
	}
	// 抽样断言逐字格式（契约 7.8 第三列）。
	cases := []struct{ name, want string }{
		{agent.ToolSSHExec, "exit=0 · 20B"},
		{agent.ToolServerFacts, "facts · 15B"},
		{agent.ToolContainerAction, "stop · exit=0 · 4B"},
		{agent.ToolContainerList, "2 个容器"},
		{agent.ToolContainerInspect, "pg · running"},
		{agent.ToolContainerLogs, "2 行日志"},
		{agent.ToolImageList, "3 个镜像"},
		{agent.ToolDBQuery, "7 行 · 12ms"},
		{agent.ToolBatchExecute, "3 台 · 已执行 1 / 待审批 1 / 跳过 1"},
		{agent.ToolRunDiagnosis, "done · warning · 4 证据"},
		{agent.ToolRunInspection, "3 台 · 均分 72 · 2 异常"},
		{agent.ToolListServers, "2 台资产"},
		{agent.ToolListDatabases, "1 个数据库"},
		{agent.ToolGetMetrics, "30 分钟 · 2 点"},
		{agent.ToolListAlerts, "2 条告警"},
		{agent.ToolAuditSearch, "1+ 条审计"},
		{agent.ToolGetInspectionReport, "report:r9 · 2 台 · 均分 88"},
		{agent.ToolAskUser, "等待用户回答"},
	}
	for _, tc := range cases {
		got, _ := replayResultSummary(tc.name, replaySummaryFixtures[tc.name], "")
		if got != tc.want {
			t.Errorf("%s: summary = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReplayResultSummaryNoResultAndError(t *testing.T) {
	for name := range replaySummaryFixtures {
		// 无结果（result 为 null）：ask_user 固定文案，其余「无结果」。
		got, truncated := replayResultSummary(name, json.RawMessage(`null`), "")
		want := replayNoResultSummary
		if name == agent.ToolAskUser {
			want = replayAwaitingUserSummary
		}
		if got != want || truncated {
			t.Errorf("%s: no-result summary = %q truncated=%v, want %q", name, got, truncated, want)
		}
		// 缺失 result（空切片）。
		if got, _ := replayResultSummary(name, nil, ""); got != want {
			t.Errorf("%s: empty-result summary = %q, want %q", name, got, want)
		}
		// 错误：恒 error=<code>（即使有结果也不读结果）。
		if got, _ := replayResultSummary(name, replaySummaryFixtures[name], "SSH_EXEC_FAILED"); got != "error=SSH_EXEC_FAILED" {
			t.Errorf("%s: error summary = %q", name, got)
		}
	}
}

// TestReplayResultSummaryTruncatedWrapper 覆盖契约 7.8 写死的整包 64KB 截断
// 退化路径：{truncated, original_bytes, preview} 必须被识别。
func TestReplayResultSummaryTruncatedWrapper(t *testing.T) {
	wrapper := json.RawMessage(`{"truncated":true,"original_bytes":131072,"preview":"前半段"}`)
	got, truncated := replayResultSummary(agent.ToolSSHExec, wrapper, "")
	if got != "输出超限（原始 131072B），仅预览" {
		t.Fatalf("wrapper summary = %q", got)
	}
	if !truncated {
		t.Fatal("wrapper must report truncated")
	}
	// isTruncatedResultWrapper 的识别边界：键集必须是恰好三个且 truncated=true。
	if n, preview, ok := isTruncatedResultWrapper(wrapper); !ok || n != 131072 || preview != "前半段" {
		t.Fatalf("isTruncatedResultWrapper = %d,%q,%v", n, preview, ok)
	}
	for _, bad := range []string{
		`{"truncated":false,"original_bytes":1,"preview":""}`,
		`{"truncated":true,"preview":"x"}`,
		`{"truncated":true,"original_bytes":1,"preview":"x","extra":1}`,
		`{"truncated":true,"original_bytes":1,"preview":1}`,
		`not-json`,
		`null`,
	} {
		if _, _, ok := isTruncatedResultWrapper(json.RawMessage(bad)); ok {
			t.Errorf("isTruncatedResultWrapper(%s) = true, want false", bad)
		}
	}
}

// TestReplayResultSummaryClamp 覆盖 ≤200 rune 的截断（补 "…"）与未知输入。
func TestReplayResultSummaryClamp(t *testing.T) {
	long := strings.Repeat("x", 5000)
	raw, err := json.Marshal(map[string]any{"server_id": "s1", "runtime": "docker", "containers": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	got, truncated := replayResultSummary(agent.ToolContainerList, raw, "")
	if truncated || got != "0 个容器" {
		t.Fatalf("empty containers summary = %q truncated=%v", got, truncated)
	}

	// run_diagnosis 的 status 超长：摘要被钳到 200 rune 且报告截断。
	huge := fmt.Sprintf(`{"diagnosis_id":"d1","status":%q,"evidence_count":1}`, long)
	got, truncated = replayResultSummary(agent.ToolRunDiagnosis, json.RawMessage(huge), "")
	if !truncated {
		t.Fatal("overlong summary must report truncated")
	}
	if n := utf8.RuneCountInString(got); n != replaySummaryMaxRunes {
		t.Fatalf("clamped summary = %d runes, want %d", n, replaySummaryMaxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("clamped summary must end with ellipsis: %q", got)
	}

	// 未知工具 / 未知形状 / 非对象 JSON：一律「无结果」，绝不 panic。
	for _, tc := range []struct {
		name   string
		result json.RawMessage
	}{
		{"no_such_tool", json.RawMessage(`{"anything":1}`)},
		{agent.ToolSSHExec, json.RawMessage(`[1,2,3]`)},
		{agent.ToolSSHExec, json.RawMessage(`"plain-string"`)},
		{agent.ToolDBQuery, json.RawMessage(`{"row_count":"not-a-number"}`)},
		{agent.ToolRunInspection, json.RawMessage(`{"items":"bad"}`)},
	} {
		if got, _ := replayResultSummary(tc.name, tc.result, ""); got != replayNoResultSummary {
			t.Errorf("%s/%s: summary = %q, want %q", tc.name, tc.result, got, replayNoResultSummary)
		}
	}
}

// TestReplayActionToolsListMatchesContract 是 G41 ① 的双向断言：agent 导出
// 的动作类名单必须与契约 7.8 的 11 个工具**逐项一致**（多一个/少一个即失败）。
func TestReplayActionToolsListMatchesContract(t *testing.T) {
	want := []string{
		"ssh_exec", "server_facts", "container_action", "container_list",
		"container_inspect", "container_logs", "image_list", "db_query",
		"batch_execute", "run_diagnosis", "run_inspection",
	}
	got := agent.ReplayActionTools()
	if len(got) != len(want) {
		t.Fatalf("ReplayActionTools = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, name := range got {
		seen[name] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("missing action tool %s", name)
		}
	}
	// 反向：本地类工具不得出现在动作名单。
	for _, local := range []string{
		"list_servers", "list_databases", "get_metrics", "list_alerts",
		"audit_search", "get_inspection_report", "ask_user",
	} {
		if seen[local] {
			t.Errorf("local tool %s must not be an action tool", local)
		}
	}
	// 返回副本：调用方修改不影响包内名单。
	names := agent.ReplayActionTools()
	names[0] = "mutated"
	if agent.ReplayActionTools()[0] != want[0] {
		t.Fatal("ReplayActionTools must return a copy")
	}
}
