// replay_summary.go 实现八期回放的结果摘要纯函数（契约 5.19.3 / 7.8）。
//
// 目的：六期回放只对 ssh_exec/container_action 暴露结果，其余 16 个工具在
// 时间轴上只有一条 tool_call 步、用户完全看不出「模型查了什么、拿到几条」。
// 八期给全部 18 个工具的 tool_call 步与 11 个动作类工具的 execution 步都装配
// 一条 ≤200 rune 的一句话摘要。
package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"aiops/internal/agent"
)

// replaySummaryMaxRunes 是 result_summary 的长度上限（契约 7.8：≤200 rune，
// 超出截断并补 "…"，同时 result_truncated=true）。
const replaySummaryMaxRunes = 200

// replayTruncatedWrapperSummary 是整包截断包装的固定摘要（契约 7.8 写死）。
const replayTruncatedWrapperSummary = "输出超限（原始 %dB），仅预览"

// replayNoResultSummary 是「没有可读结果」的兜底摘要。
const replayNoResultSummary = "无结果"

// replayAwaitingUserSummary 是 ask_user 的固定摘要（契约 7.8）。
const replayAwaitingUserSummary = "等待用户回答"

// isTruncatedResultWrapper 识别 agent.truncateResult 的整包截断包装
// （`internal/agent/tools.go` 的 truncateResult：键集恰为
// truncated + original_bytes + preview）。契约 7.8 写死：回放必须识别这个
// 包装并降级为 output=preview，不得因按工具取键落空而渲染成空输出。
func isTruncatedResultWrapper(result json.RawMessage) (originalBytes int, preview string, ok bool) {
	if len(result) == 0 {
		return 0, "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil || len(m) != 3 {
		return 0, "", false
	}
	for _, key := range []string{"truncated", "original_bytes", "preview"} {
		if _, exists := m[key]; !exists {
			return 0, "", false
		}
	}
	var truncated bool
	if err := json.Unmarshal(m["truncated"], &truncated); err != nil || !truncated {
		return 0, "", false
	}
	if err := json.Unmarshal(m["preview"], &preview); err != nil {
		return 0, "", false
	}
	if err := json.Unmarshal(m["original_bytes"], &originalBytes); err != nil {
		return 0, "", false
	}
	return originalBytes, preview, true
}

// replayResultSummary 把工具结果 JSON 压缩为一行可读摘要（契约 7.8）。
// 纯函数：不读库、不读时钟；对未知工具/未知形状返回 "无结果"，绝不 panic。
// 输出 ≤200 rune；超长截断并补 "…" 且 truncated=true。
//
// errorCode 非空时一律 `error=<code>`（失败调用的唯一可读原因）。
func replayResultSummary(name string, result json.RawMessage, errorCode string) (summary string, truncated bool) {
	if errorCode != "" {
		return "error=" + errorCode, false
	}
	if originalBytes, _, ok := isTruncatedResultWrapper(result); ok {
		return fmt.Sprintf(replayTruncatedWrapperSummary, originalBytes), true
	}
	if len(result) == 0 || string(result) == "null" {
		if name == agent.ToolAskUser {
			return replayAwaitingUserSummary, false
		}
		return replayNoResultSummary, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result, &fields); err != nil {
		// 未知形状（非对象 JSON）：不猜内容。
		return replayNoResultSummary, false
	}
	return replayClampSummary(replaySummaryForTool(name, fields))
}

// replaySummaryForTool 按工具分派摘要（契约 7.8 的第三列逐条写死）。
// 必需键缺失/类型不符一律 "无结果"，不返回半截数字。
func replaySummaryForTool(name string, f map[string]json.RawMessage) string {
	switch name {
	case agent.ToolSSHExec:
		exit, ok := replaySummaryInt(f, "exit_code")
		if !ok {
			return replayNoResultSummary
		}
		out, _ := replaySummaryString(f, "output")
		return fmt.Sprintf("exit=%d · %dB", exit, len(out))

	case agent.ToolServerFacts:
		raw, ok := replaySummaryString(f, "raw_output")
		if !ok {
			return replayNoResultSummary
		}
		// 网络设备的 facts 带 vendor（brand，见 netdev.ParseFacts 调用处），
		// 摘要按 7.8 用 facts:<brand>；主机为 facts。
		brand := ""
		if facts, ok := f["facts"]; ok {
			var v struct {
				Vendor string `json:"vendor"`
			}
			_ = json.Unmarshal(facts, &v)
			brand = v.Vendor
		}
		if brand != "" {
			return fmt.Sprintf("facts:%s · %dB", brand, len(raw))
		}
		return fmt.Sprintf("facts · %dB", len(raw))

	case agent.ToolContainerAction:
		action, ok := replaySummaryString(f, "action")
		exit, exitOK := replaySummaryInt(f, "exit_code")
		if !ok || action == "" || !exitOK {
			return replayNoResultSummary
		}
		out, _ := replaySummaryString(f, "output")
		return fmt.Sprintf("%s · exit=%d · %dB", action, exit, len(out))

	case agent.ToolContainerList:
		n, ok := replaySummaryArrayLen(f, "containers")
		if !ok {
			return replayNoResultSummary
		}
		return fmt.Sprintf("%d 个容器", n)

	case agent.ToolContainerInspect:
		raw, ok := f["container"]
		if !ok {
			return replayNoResultSummary
		}
		var c struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return replayNoResultSummary
		}
		label := c.Name
		if label == "" {
			label = c.ID
		}
		if label == "" {
			return replayNoResultSummary
		}
		return fmt.Sprintf("%s · %s", label, c.State)

	case agent.ToolContainerLogs:
		out, ok := replaySummaryString(f, "output")
		if !ok {
			return replayNoResultSummary
		}
		n := replayCountLines(out)
		if truncated, _ := replaySummaryBool(f, "truncated"); truncated {
			return fmt.Sprintf("%d 行日志（已截断）", n)
		}
		return fmt.Sprintf("%d 行日志", n)

	case agent.ToolImageList:
		n, ok := replaySummaryArrayLen(f, "images")
		if !ok {
			return replayNoResultSummary
		}
		return fmt.Sprintf("%d 个镜像", n)

	case agent.ToolDBQuery:
		rows, ok := replaySummaryInt(f, "row_count")
		if !ok {
			return replayNoResultSummary
		}
		duration, _ := replaySummaryInt(f, "duration_ms")
		return fmt.Sprintf("%d 行 · %dms", rows, duration)

	case agent.ToolBatchExecute:
		total, ok := replaySummaryInt(f, "total")
		if !ok {
			return replayNoResultSummary
		}
		executed, _ := replaySummaryInt(f, "executed")
		awaiting, _ := replaySummaryInt(f, "awaiting_approval")
		skipped, _ := replaySummaryInt(f, "skipped")
		return fmt.Sprintf("%d 台 · 已执行 %d / 待审批 %d / 跳过 %d", total, executed, awaiting, skipped)

	case agent.ToolRunDiagnosis:
		status, ok := replaySummaryString(f, "status")
		if !ok || status == "" {
			return replayNoResultSummary
		}
		severity, _ := replaySummaryString(f, "severity")
		evidence, _ := replaySummaryInt(f, "evidence_count")
		if severity != "" {
			return fmt.Sprintf("%s · %s · %d 证据", status, severity, evidence)
		}
		return fmt.Sprintf("%s · %d 证据", status, evidence)

	case agent.ToolRunInspection:
		servers, ok := replaySummaryInt(f, "server_count")
		if !ok {
			return replayNoResultSummary
		}
		avg, _ := replaySummaryInt(f, "avg_score")
		anomalies, _ := replaySummaryInt(f, "anomaly_count")
		return fmt.Sprintf("%d 台 · 均分 %d · %d 异常", servers, avg, anomalies)

	case agent.ToolListServers:
		n, ok := replaySummaryArrayLen(f, "servers")
		if !ok {
			return replayNoResultSummary
		}
		return fmt.Sprintf("%d 台资产", n)

	case agent.ToolListDatabases:
		n, ok := replaySummaryArrayLen(f, "databases")
		if !ok {
			return replayNoResultSummary
		}
		return fmt.Sprintf("%d 个数据库", n)

	case agent.ToolGetMetrics:
		minutes, ok := replaySummaryInt(f, "minutes")
		if !ok {
			return replayNoResultSummary
		}
		n, _ := replaySummaryArrayLen(f, "points")
		return fmt.Sprintf("%d 分钟 · %d 点", minutes, n)

	case agent.ToolListAlerts:
		n, ok := replaySummaryArrayLen(f, "alerts")
		if !ok {
			return replayNoResultSummary
		}
		if more, _ := replaySummaryBool(f, "has_more"); more {
			return fmt.Sprintf("%d+ 条告警", n)
		}
		return fmt.Sprintf("%d 条告警", n)

	case agent.ToolAuditSearch:
		n, ok := replaySummaryArrayLen(f, "items")
		if !ok {
			return replayNoResultSummary
		}
		if more, _ := replaySummaryBool(f, "has_more"); more {
			return fmt.Sprintf("%d+ 条审计", n)
		}
		return fmt.Sprintf("%d 条审计", n)

	case agent.ToolGetInspectionReport:
		if raw, ok := f["report"]; ok {
			var r struct {
				ReportID    string `json:"report_id"`
				ServerCount int64  `json:"server_count"`
				AvgScore    int64  `json:"avg_score"`
			}
			if err := json.Unmarshal(raw, &r); err == nil && r.ReportID != "" {
				return fmt.Sprintf("report:%s · %d 台 · 均分 %d", r.ReportID, r.ServerCount, r.AvgScore)
			}
		}
		if n, ok := replaySummaryArrayLen(f, "reports"); ok {
			return fmt.Sprintf("最近 %d 份报告", n)
		}
		return replayNoResultSummary

	case agent.ToolAskUser:
		return replayAwaitingUserSummary
	}
	return replayNoResultSummary
}

// replayClampSummary 把摘要限制在 ≤200 rune（超出截断并补 "…"）。
func replayClampSummary(summary string) (string, bool) {
	if utf8.RuneCountInString(summary) <= replaySummaryMaxRunes {
		return summary, false
	}
	runes := []rune(summary)
	return string(runes[:replaySummaryMaxRunes-1]) + "…", true
}

// replayCountLines 数日志行数：空串为 0，末尾换行不多算一行。
func replayCountLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// replaySummaryInt 取整数键（JSON number 可能是小数或字符串，均视为缺失）。
func replaySummaryInt(f map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := f[key]
	if !ok {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}

// replaySummaryString 取字符串键。
func replaySummaryString(f map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := f[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// replaySummaryBool 取布尔键。
func replaySummaryBool(f map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := f[key]
	if !ok {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

// replaySummaryArrayLen 取数组长度（键存在但为 null 视为缺失）。
func replaySummaryArrayLen(f map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := f[key]
	if !ok {
		return 0, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0, false
	}
	return len(arr), true
}
