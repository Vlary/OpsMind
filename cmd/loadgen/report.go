package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runResult 是写入 <mode>.json 的完整原始数据。
type runResult struct {
	Mode        string             `json:"mode"`
	Command     string             `json:"command"`
	StartedAt   time.Time          `json:"started_at"`
	DurationS   float64            `json:"duration_s"`
	ElapsedS    float64            `json:"elapsed_s"`
	Concurrency int                `json:"concurrency"`
	ServerPID   int                `json:"server_pid"`
	ServerAddr  string             `json:"server_addr"`
	MockLLMURL  string             `json:"mock_llm_url"`
	Thresholds  thresholdsJSON     `json:"thresholds"`
	Samples     []sample           `json:"samples"`
	Ops         map[string]opStats `json:"ops"`
	Counts      countsJSON         `json:"counts"`
	Verdict     verdict            `json:"verdict"`
}

type thresholdsJSON struct {
	SSEP95MS            int64   `json:"sse_p95_ms"`
	GoroutineGrowPct    float64 `json:"goroutine_growth_pct_max"`
	RSSGrowPct          float64 `json:"rss_growth_pct_max"`
	RSSAbsoluteCapKB    int64   `json:"rss_absolute_cap_kb"`
	HostMemAvailFloorKB int64   `json:"host_mem_avail_floor_kb"`
	HostMemPrecheckKB   int64   `json:"host_mem_precheck_kb"`
	WarmupFraction      int     `json:"warmup_fraction"`
	SampleIntervalS     float64 `json:"sample_interval_s"`
}

type countsJSON struct {
	Chats      int64 `json:"chats_done"`
	Approvals  int64 `json:"approvals_auto_approved"`
	Tokens     int64 `json:"token_events"`
	CRUDGroups int64 `json:"crud_groups"`
	AuditPages int64 `json:"audit_pages"`
	HTTP5xx    int64 `json:"http_5xx"`
	SSEErrors  int64 `json:"sse_error_events"`
	MockLLMReq int64 `json:"mock_llm_chat_requests"`
}

func writeReports(repoRoot, mode string, res runResult) (string, string, error) {
	dir := filepath.Join(repoRoot, "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	jsonPath := filepath.Join(dir, mode+".json")
	mdPath := filepath.Join(dir, mode+".md")

	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mdPath, []byte(renderMarkdown(res)), 0o644); err != nil {
		return "", "", err
	}
	return mdPath, jsonPath, nil
}

func renderMarkdown(res runResult) string {
	v := res.Verdict
	passMark := "PASS ✅"
	if !v.Pass {
		passMark = "FAIL ❌"
	}

	var b []byte
	w := func(format string, args ...any) {
		b = append(b, fmt.Sprintf(format, args...)...)
	}

	w("# aiops 负载报告 — %s（%s）\n\n", res.Mode, passMark)
	w("- 命令：`%s`\n", res.Command)
	w("- 开始时间：%s（计划 %s，实际 %s，并发 %d）\n",
		res.StartedAt.Format(time.RFC3339), fmtDur(res.DurationS), fmtDur(res.ElapsedS), res.Concurrency)
	w("- 被测服务端：pid=%d %s（AIOPS_PPROF=1，GOMEMLIMIT=768MiB，库 aiops_load 每次重建）\n", res.ServerPID, res.ServerAddr)
	w("- 假 OpenAI 兼容 SSE 服务：%s（随 loadgen 进程销毁）\n\n", res.MockLLMURL)

	w("## 判定结论：%s\n\n", passMark)
	w("| 检查项 | 阈值 | 实测 | 结果 |\n|---|---|---|---|\n")
	row := func(name, budget string, actual string, ok bool) {
		mark := "OK"
		if !ok {
			mark = "FAIL"
		}
		w("| %s | %s | %s | %s |\n", name, budget, actual, mark)
	}
	row("HTTP 5xx 响应", "0", fmt.Sprintf("%d", v.HTTP5xx), v.HTTP5xx == 0)
	row("SSE error 事件（视同服务器错误，见注 1）", "0", fmt.Sprintf("%d", v.SSEErrors), v.SSEErrors == 0)
	if v.ChatCount == 0 {
		row("SSE 聊天 p95", "<= 1500ms", "无样本", false)
	} else {
		row("SSE 聊天 p95", "<= 1500ms", fmt.Sprintf("%dms", v.SSEP95MS), v.SSEP95MS <= res.Thresholds.SSEP95MS)
	}
	row("warmup 后 goroutine 增幅", "<= 25%",
		fmt.Sprintf("%.1f%%（基线 %d → 峰值 %d）", v.GoroutineGrowthPct, v.GoroutineBaseline, v.GoroutineMax),
		v.GoroutineGrowthPct <= res.Thresholds.GoroutineGrowPct)
	row("warmup 后 RSS 增幅", "<= 30%",
		fmt.Sprintf("%.1f%%（基线 %dKB → 峰值 %dKB）", v.RSSGrowthPct, v.RSSBaselineKB, v.RSSMaxKB),
		v.RSSGrowthPct <= res.Thresholds.RSSGrowPct)
	row("服务端 RSS 绝对峰值", "<= 1.5GB",
		fmt.Sprintf("%.0fMB（%dKB）", float64(v.RSSPeakAllKB)/1024, v.RSSPeakAllKB),
		v.RSSPeakAllKB <= res.Thresholds.RSSAbsoluteCapKB)
	hostMemActual := "无有效采样"
	hostMemOK := false
	if v.HostMemAvailMinKB > 0 {
		hostMemActual = fmt.Sprintf("最低 %.2fGiB（%dKB）%s", float64(v.HostMemAvailMinKB)/(1<<20), v.HostMemAvailMinKB, abortMark(v.HostMemLowAborted))
		hostMemOK = v.HostMemAvailMinKB >= res.Thresholds.HostMemAvailFloorKB && !v.HostMemLowAborted
	}
	row("运行中宿主 MemAvailable", ">= 2GiB（跌破立即停止负载）", hostMemActual, hostMemOK)
	w("\n")

	if len(v.Violations) > 0 {
		w("违规明细：\n")
		for _, viol := range v.Violations {
			w("- %s\n", viol)
		}
		w("\n")
	}

	w("## 请求延迟分位数\n\n")
	w("| 操作 | 次数 | 错误 | p50 | p95 | p99 | max |\n|---|---|---|---|---|---|---|\n")
	for _, name := range sortedOpNames(res.Ops) {
		op := res.Ops[name]
		w("| %s | %d | %d | %dms | %dms | %dms | %dms |\n", name, op.Count, op.Errs, op.P50MS, op.P95MS, op.P99MS, op.MaxMS)
	}
	w("\n")

	w("## 资源曲线摘要（每 %.0fs 采样）\n\n", res.Thresholds.SampleIntervalS)
	w("- warmup 窗口：前 %.0fs（时长 1/%d）；采样点 %d 个（失败 %d 个）\n",
		v.WarmupS, res.Thresholds.WarmupFraction, len(res.Samples), v.SampleFailures)
	if v.RSSBaselineKB > 0 {
		w("- RSS：warmup 峰值基线 %dKB → 其后峰值 %dKB（增幅 %.1f%%）\n", v.RSSBaselineKB, v.RSSMaxKB, v.RSSGrowthPct)
	}
	if v.GoroutineBaseline > 0 {
		w("- goroutine：warmup 峰值基线 %d → 其后峰值 %d（增幅 %.1f%%）\n", v.GoroutineBaseline, v.GoroutineMax, v.GoroutineGrowthPct)
	}
	w("- 服务端 RSS 全程绝对峰值：%.0fMB（上限 1536MB）\n", float64(v.RSSPeakAllKB)/1024)
	if v.HostMemAvailMinKB > 0 {
		w("- 宿主机 MemAvailable 最低：%.2fGiB（下限 2GiB）\n", float64(v.HostMemAvailMinKB)/(1<<20))
	}
	w("\n降采样曲线（最多 40 点；完整数据见同名 .json）：\n\n")
	w("| t(s) | RSS(KB) | goroutine | 宿主MemAvail(GiB) |\n|---|---|---|---|\n")
	step := len(res.Samples)/40 + 1
	for i := 0; i < len(res.Samples); i += step {
		s := res.Samples[i]
		mem := "-"
		if s.MemAvailKB > 0 {
			mem = fmt.Sprintf("%.2f", float64(s.MemAvailKB)/(1<<20))
		}
		w("| %.0f | %d | %d | %s |\n", s.T, s.RSSKB, s.Gorou, mem)
	}
	w("\n")

	w("## 负载量\n\n")
	w("| 指标 | 值 |\n|---|---|\n")
	w("| 完成聊天轮（读到 done） | %d |\n", res.Counts.Chats)
	w("| 自动批准审批 | %d |\n", res.Counts.Approvals)
	w("| token 事件 | %d |\n", res.Counts.Tokens)
	w("| CRUD churn 组 | %d |\n", res.Counts.CRUDGroups)
	w("| 审计分页页数 | %d |\n", res.Counts.AuditPages)
	w("| 假 LLM chat 请求数 | %d |\n", res.Counts.MockLLMReq)
	w("\n")

	w("> 注 1：SSE 流内 error 事件发生在 200 头写出之后，HTTP 状态码捕捉不到，判定时视同服务器错误。\n")
	w("> 注 2：SSH 命令 `uptime` 指向不可达端口（127.0.0.1:1），批准后快速失败是预期负载形态，工具错误以 tool_result 事件返回，不计入判定。\n")
	return string(b)
}

func sortedOpNames(ops map[string]opStats) []string {
	order := []string{"login_worker", "chat_sse", "approval_decide", "conversation_create", "conversation_delete",
		"server_create", "server_patch", "server_delete", "provider_create", "provider_delete", "audit_page"}
	seen := map[string]bool{}
	var names []string
	for _, n := range order {
		if _, ok := ops[n]; ok {
			names = append(names, n)
			seen[n] = true
		}
	}
	for n := range ops {
		if !seen[n] {
			names = append(names, n)
		}
	}
	return names
}

func fmtDur(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	return d.String()
}

// abortMark 在宿主内存检查项里标注是否触发了立即停止。
func abortMark(aborted bool) string {
	if aborted {
		return "；已触发立即停止"
	}
	return ""
}
