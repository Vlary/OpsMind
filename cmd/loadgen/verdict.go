package main

import (
	"fmt"
	"time"
)

// verdict 是判定结论；任一 violation 触发即 Pass=false。
//
// 说明：SSE 流内的 error 事件发生在 HTTP 200 已写出之后（internal/httpapi/
// chat.go streamChat 先写 SSE 头再执行 agent 循环），HTTP 状态码捕捉不到，
// 因此视同服务器错误一并计入判定；这一点在报告中注明。
type verdict struct {
	Pass       bool     `json:"pass"`
	Violations []string `json:"violations,omitempty"`

	HTTP5xx   int64 `json:"http_5xx"`
	SSEErrors int64 `json:"sse_errors"`

	ChatCount int   `json:"chat_count"`
	SSEP95MS  int64 `json:"sse_p95_ms"`

	WarmupS            float64 `json:"warmup_s"`
	GoroutineBaseline  int64   `json:"goroutine_baseline"`
	GoroutineMax       int64   `json:"goroutine_max"`
	GoroutineGrowthPct float64 `json:"goroutine_growth_pct"`
	RSSBaselineKB      int64   `json:"rss_baseline_kb"`
	RSSMaxKB           int64   `json:"rss_max_kb"`
	RSSPeakAllKB       int64   `json:"rss_peak_all_kb"` // 全程（含 warmup）RSS 峰值，用于绝对值判定
	RSSGrowthPct       float64 `json:"rss_growth_pct"`
	HostMemAvailMinKB  int64   `json:"host_mem_avail_min_kb"` // 运行中宿主 MemAvailable 最小值
	HostMemLowAborted  bool    `json:"host_mem_low_aborted"`  // 是否因 MemAvailable<2GiB 立即停止
	SampleFailures     int     `json:"sample_failures"`
}

// rssAbsoluteCapKB：服务端 RSS 绝对值上限（1.5GB）。
const rssAbsoluteCapKB = 1536 * 1024 // 1.5GB，单位 kB

func computeVerdict(samples []sample, ops map[string]opStats, stats *runStats, duration time.Duration, hostMemLowAborted bool) verdict {
	v := verdict{
		Pass:              true,
		HTTP5xx:           stats.HTTP5xx.Load(),
		SSEErrors:         stats.SSEErrors.Load(),
		HostMemLowAborted: hostMemLowAborted,
	}

	if chat, ok := ops["chat_sse"]; ok {
		v.ChatCount = chat.Count
		v.SSEP95MS = chat.P95MS
	}

	// warmup 基线：取 warmup 窗口内各指标的最大值（满载峰值）。
	//
	// 泄漏的形态是「稳态峰值持续抬高」，必须满载峰对满载峰比较。负载是周期
	// 性突发（chat SSE 突发 → CRUD/审计/停顿），goroutine 在空闲底座与满载
	// 峰之间振荡（实测 8 并发 soak：43 ↔ 61±6，周期约 11s；每条在途 SSE 流
	// 携带 ~3 个瞬态工作 goroutine：sseWriter 心跳、LLM 上游读循环、请求
	// handler），它们随流结束全部退出，底座 45 分钟零抬升。若基线取窗口末
	// 单点，5s 采样与负载周期混叠，基线有约一半概率落在空闲低谷，把正常
	// 波动误判为 +37%~56% 的「增长」。改为 warmup 窗口峰值基线后，真实泄漏
	// （后续峰值持续超过 warmup 峰值）仍触发同样的 25% 阈值，判定强度不变。
	warmup := duration.Seconds() / float64(warmupFraction)
	v.WarmupS = warmup
	valid := func(s sample) bool { return s.RSSKB > 0 && s.Gorou > 0 }
	haveBaseline := false
	for i := range samples {
		s := samples[i]
		if !valid(s) {
			v.SampleFailures++
			continue
		}
		if s.T <= warmup {
			haveBaseline = true
			if s.Gorou > v.GoroutineBaseline {
				v.GoroutineBaseline = s.Gorou
			}
			if s.RSSKB > v.RSSBaselineKB {
				v.RSSBaselineKB = s.RSSKB
			}
		} else {
			if s.Gorou > v.GoroutineMax {
				v.GoroutineMax = s.Gorou
			}
			if s.RSSKB > v.RSSMaxKB {
				v.RSSMaxKB = s.RSSKB
			}
		}
	}

	add := func(format string, args ...any) {
		v.Pass = false
		v.Violations = append(v.Violations, fmt.Sprintf(format, args...))
	}

	if v.HTTP5xx > 0 {
		add("HTTP 5xx responses: %d", v.HTTP5xx)
	}
	if v.SSEErrors > 0 {
		add("SSE error events (treated as server errors): %d", v.SSEErrors)
	}
	if v.ChatCount == 0 {
		add("no completed SSE chat rounds; latency verdict unavailable")
	} else if time.Duration(v.SSEP95MS)*time.Millisecond > sseP95Budget {
		add("SSE chat p95 %dms exceeds budget %s", v.SSEP95MS, sseP95Budget)
	}

	// 服务端 RSS 绝对值（全程峰值，含 warmup 内的样本）。
	hostMemMinSeen := false
	for _, s := range samples {
		if s.RSSKB > v.RSSPeakAllKB {
			v.RSSPeakAllKB = s.RSSKB
		}
		if s.MemAvailKB >= 0 && (!hostMemMinSeen || s.MemAvailKB < v.HostMemAvailMinKB) {
			v.HostMemAvailMinKB = s.MemAvailKB
			hostMemMinSeen = true
		}
	}
	if v.RSSPeakAllKB > rssAbsoluteCapKB {
		add("server RSS peak %dKB exceeds absolute cap %dKB (1.5GB)", v.RSSPeakAllKB, rssAbsoluteCapKB)
	}
	if hostMemMinSeen && v.HostMemAvailMinKB < hostMemFloorKB {
		add("host MemAvailable dropped to %dKB below floor %dKB (2GiB)", v.HostMemAvailMinKB, hostMemFloorKB)
	}
	if hostMemLowAborted {
		add("load aborted mid-run: host MemAvailable below 2GiB floor")
	}

	if !haveBaseline {
		add("no valid resource sample within warmup %.0fs; growth verdict unavailable", warmup)
		return v
	}
	if v.GoroutineMax == 0 && v.RSSMaxKB == 0 {
		add("no valid resource sample after warmup %.0fs; growth verdict unavailable", warmup)
		return v
	}
	if v.GoroutineMax == 0 {
		v.GoroutineMax = v.GoroutineBaseline
	}
	if v.RSSMaxKB == 0 {
		v.RSSMaxKB = v.RSSBaselineKB
	}
	if v.GoroutineBaseline > 0 {
		v.GoroutineGrowthPct = float64(v.GoroutineMax-v.GoroutineBaseline) / float64(v.GoroutineBaseline) * 100
		if v.GoroutineGrowthPct > goroutineGrow*100 {
			add("goroutine growth after warmup %.1f%% exceeds %.0f%% (baseline %d -> max %d)",
				v.GoroutineGrowthPct, goroutineGrow*100, v.GoroutineBaseline, v.GoroutineMax)
		}
	}
	if v.RSSBaselineKB > 0 {
		v.RSSGrowthPct = float64(v.RSSMaxKB-v.RSSBaselineKB) / float64(v.RSSBaselineKB) * 100
		if v.RSSGrowthPct > rssGrow*100 {
			add("RSS growth after warmup %.1f%% exceeds %.0f%% (baseline %dKB -> max %dKB)",
				v.RSSGrowthPct, rssGrow*100, v.RSSBaselineKB, v.RSSMaxKB)
		}
	}
	return v
}
