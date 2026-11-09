package main

import (
	"testing"
	"time"
)

func mkSamples(vals []int64, interval float64) []sample {
	out := make([]sample, 0, len(vals))
	var t float64
	for _, g := range vals {
		t += interval
		out = append(out, sample{T: t, RSSKB: 50000 + g, Gorou: g, MemAvailKB: 16 << 20})
	}
	return out
}

// 周期性负载（chat 突发 → CRUD/停顿）下 goroutine 在空闲底座与满载峰之间
// 振荡，所有瞬态工作 goroutine 随流退出，底座不抬升：这不是泄漏，必须 PASS。
//
// 回归背景：曾用 warmup 末单点做基线，5s 采样与 ~11s 负载周期混叠使基线
// 落在空闲低谷（43），与满载峰（61）比较被误判 +41% 增长而 FAIL。
func TestVerdictOscillatingLoadIsNotALeak(t *testing.T) {
	const base = 43
	const peak = 61
	// 20 个采样点（每 5s）：周期 4 个采样（低低高低），warmup=120s → 前 24
	// 个点。构造 40 个点：warmup 内含峰与谷，末点恰好是低谷。
	var vals []int64
	for i := 0; i < 40; i++ {
		switch i % 4 {
		case 0, 1:
			vals = append(vals, base)
		case 2:
			vals = append(vals, peak)
		default:
			vals = append(vals, base+2)
		}
	}
	// warmup（前 24 点）末点为低谷；其后峰值 61。
	ops := map[string]opStats{"chat_sse": {Count: 100, P95MS: 300}}
	v := computeVerdict(mkSamples(vals, 5), ops, &runStats{}, 8*time.Minute, false)
	if !v.Pass {
		t.Fatalf("oscillating load flagged as leak: %+v (%v)", v, v.Violations)
	}
	if v.GoroutineBaseline != peak {
		t.Fatalf("baseline = %d, want warmup peak %d", v.GoroutineBaseline, peak)
	}
	if v.GoroutineMax != peak {
		t.Fatalf("max = %d, want %d", v.GoroutineMax, peak)
	}
}

// 真实泄漏：warmup 后稳态峰值持续抬高（每窗口峰 +3），必须仍触发 FAIL，
// 且增幅按峰对峰计算。
func TestVerdictStillDetectsGrowth(t *testing.T) {
	var vals []int64
	for i := 0; i < 40; i++ {
		g := int64(60)
		if i >= 24 { // warmup 之后（8min/4=2min=24 个采样点）
			g += int64((i - 24) / 4 * 6) // 每 4 个采样抬 6，模拟泄漏爬升
		}
		vals = append(vals, g)
	}
	v := computeVerdict(mkSamples(vals, 5), map[string]opStats{"chat_sse": {Count: 10, P95MS: 100}}, &runStats{}, 8*time.Minute, false)
	if v.Pass {
		t.Fatal("monotonic growth not detected")
	}
	found := false
	for _, s := range v.Violations {
		if len(s) > 10 && s[:10] == "goroutine " {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected goroutine growth violation, got %v", v.Violations)
	}
	if v.GoroutineBaseline != 60 {
		t.Fatalf("baseline = %d, want warmup peak 60", v.GoroutineBaseline)
	}
	// i=36..39（最后 4 点）：60 + (39-24)/4*6 = 60+18 = 78，增幅 30% > 25%。
	if v.GoroutineMax != 78 {
		t.Fatalf("max = %d, want %d", v.GoroutineMax, 78)
	}
	if want := 30.0; v.GoroutineGrowthPct != want {
		t.Fatalf("growth pct = %.1f, want %.1f", v.GoroutineGrowthPct, want)
	}
}

// 5xx / SSE error / p95 超预算与资源口径无关，判定保持。
func TestVerdictIndependentChecksUnchanged(t *testing.T) {
	vals := []int64{50, 52, 51, 50, 53, 52, 51, 52}
	var stats runStats
	stats.HTTP5xx.Add(3)
	stats.SSEErrors.Add(1)
	v := computeVerdict(mkSamples(vals, 5),
		map[string]opStats{"chat_sse": {Count: 100, P95MS: 2000}},
		&stats, 40*time.Second, false)
	if v.Pass {
		t.Fatal("expected FAIL from 5xx/sse errors/p95")
	}
	if v.HTTP5xx != 3 || v.SSEErrors != 1 {
		t.Fatalf("counts = %d/%d", v.HTTP5xx, v.SSEErrors)
	}
}

// 无有效样本：growth 判定不可用，仍判失败（信息性 violation）。
func TestVerdictNoValidSamples(t *testing.T) {
	samples := []sample{{T: 1, RSSKB: -1, Gorou: -1}}
	v := computeVerdict(samples, map[string]opStats{}, &runStats{}, time.Minute, false)
	if v.Pass {
		t.Fatal("expected FAIL when no valid samples")
	}
	if v.SampleFailures != 1 {
		t.Fatalf("sample failures = %d, want 1", v.SampleFailures)
	}
}
