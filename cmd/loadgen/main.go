// Command loadgen 是自包含的负载/浸泡工具。
//
// 它编排一个完整的被测环境：本机 PostgreSQL（127.0.0.1:65432，不可用时
// docker compose up -d --wait db）、每次运行新建的 aiops_load 库、go build
// 出的服务端二进制（/tmp，AIOPS_PPROF=1）、以及内嵌的假 OpenAI 兼容 SSE
// 服务；随后以 N 个并发会话循环执行 SSE 聊天（含 approval_required 自动
// 批准）、CRUD churn 与审计分页查询，期间每 5s 采样 RSS / goroutine /
// 各请求延迟分位数，并按固定阈值判定，产出 aiops/reports/<mode>.md/.json。
//
// 用法：
//
//	go run ./cmd/loadgen -mode selftest -duration=90s
//	go run ./cmd/loadgen -mode soak    -duration=30m -concurrency=8
//	go run ./cmd/loadgen -mode stress  -duration=10m -concurrency=32
//
// 判定阈值（任一触发即非零退出）：
//   - 出现 HTTP 5xx（或 SSE 流内 error 事件，视同服务器错误）；
//   - SSE 聊天 p95 > 1.5s；
//   - warmup（前 1/4 时长）峰值基线之后 goroutine 峰值增幅 > 25%（峰对峰
//     比较，滤除周期性负载的瞬态工作 goroutine 波动，见 verdict.go）；
//   - warmup 峰值基线之后 RSS 峰值增幅 > 30%；
//   - 服务端 RSS 绝对值 > 1.5GB；
//   - 运行中宿主机 MemAvailable < 2GiB（立即停止负载并判失败）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// 阈值与节奏常量；集中在此便于审查。
const (
	sseP95Budget   = 1500 * time.Millisecond // SSE p95 预算
	goroutineGrow  = 0.25                    // warmup 后 goroutine 增幅上限
	rssGrow        = 0.30                    // warmup 后 RSS 增幅上限
	sampleInterval = 5 * time.Second         // 采样周期
	warmupFraction = 4                       // warmup = duration/4
)

type modeConfig struct {
	defaultDuration time.Duration
	defaultConc     int
	maxConc         int // 并发上限保护：本机 30GB 内存，需给 ZCode 与桌面环境留足余量
	// tokenDelay 是假 LLM 每个 token 的间隔，tokenCount*tokenDelay 决定
	// 单次聊天流的近似时长。
	tokenDelay time.Duration
	tokenCount int
	// loopPause 是 worker 每轮循环之间的思考时间；stress 为 0 以压满。
	loopPause time.Duration
}

var modes = map[string]modeConfig{
	"selftest": {defaultDuration: 90 * time.Second, defaultConc: 4, maxConc: 12, tokenDelay: 5 * time.Millisecond, tokenCount: 40, loopPause: 100 * time.Millisecond},
	"soak":     {defaultDuration: 30 * time.Minute, defaultConc: 8, maxConc: 12, tokenDelay: 5 * time.Millisecond, tokenCount: 60, loopPause: 250 * time.Millisecond},
	"stress":   {defaultDuration: 10 * time.Minute, defaultConc: 32, maxConc: 40, tokenDelay: 2 * time.Millisecond, tokenCount: 60, loopPause: 0},
}

func main() {
	var (
		mode        = flag.String("mode", "selftest", "selftest | soak | stress")
		durationStr = flag.String("duration", "", "运行时长，如 90s / 30m；缺省取模式默认值")
		conc        = flag.Int("concurrency", 0, "并发会话数；缺省取模式默认值")
	)
	flag.Parse()

	mc, ok := modes[*mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "loadgen: unknown mode %q (selftest|soak|stress)\n", *mode)
		os.Exit(2)
	}
	duration := mc.defaultDuration
	if *durationStr != "" {
		d, err := time.ParseDuration(*durationStr)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "loadgen: invalid -duration %q\n", *durationStr)
			os.Exit(2)
		}
		duration = d
	}
	concurrency := mc.defaultConc
	if *conc > 0 {
		concurrency = *conc
	}
	if concurrency > mc.maxConc {
		// 上限保护：本机 30GB 内存且需给 ZCode 与桌面环境留足余量；
		// stress 也不得超过服务端 AIOPS_MAX_STREAMS 的合法上限（100）。
		fmt.Fprintf(os.Stderr, "loadgen: -concurrency %d exceeds mode %q cap %d (memory headroom protection)\n", concurrency, *mode, mc.maxConc)
		os.Exit(2)
	}

	if err := run(*mode, mc, duration, concurrency); err != nil {
		fmt.Fprintf(os.Stderr, "LOADGEN_FAILED: %v\n", err)
		os.Exit(1)
	}
}

// run 完整执行一次负载运行并产出报告；判定失败时返回错误（非零退出）。
func run(mode string, mc modeConfig, duration time.Duration, concurrency int) error {
	ctx := context.Background()
	env, err := setupEnv(ctx, mc)
	if err != nil {
		return err
	}
	defer env.Close()

	rec := newOpRecorder()
	stats := &runStats{}
	admin, err := bootstrap(ctx, env, rec, stats)
	if err != nil {
		return err
	}

	lc := &loadController{env: env, mc: mc, rec: rec, stats: stats}

	// 宿主机内存下限触发的立即停止：sampler 检测 MemAvailable < 2GiB 时
	// 取消 abortCtx，所有 worker（含进行中的 SSE 流）立即退出，本次运行
	// 判失败并记入 violation。
	var hostMemLow atomic.Bool
	abortCtx, abortLoad := context.WithCancel(ctx)
	defer abortLoad()
	// sampler 复用 admin 的已登录会话抓 pprof（端点与 API 同等鉴权）。
	sampler := newSampler(env.serverPID, env.serverAddr, admin.hc, func() {
		hostMemLow.Store(true)
		abortLoad()
	})
	sampCtx, stopSampler := context.WithCancel(ctx)
	defer stopSampler()
	go sampler.loop(sampCtx, sampleInterval)

	fmt.Printf("loadgen: mode=%s duration=%s concurrency=%d — load start\n", mode, duration, concurrency)
	started := time.Now()
	err = lc.runWorkers(abortCtx, concurrency, duration)
	elapsed := time.Since(started)
	stopSampler()
	if err != nil {
		return fmt.Errorf("workers: %w", err)
	}
	if hostMemLow.Load() {
		fmt.Fprintf(os.Stderr, "loadgen: load aborted early after %.0fs: host MemAvailable below 2GiB\n", elapsed.Seconds())
	}
	fmt.Printf("loadgen: load finished in %s\n", elapsed.Round(time.Second))

	res := runResult{
		Mode:        mode,
		Command:     fmt.Sprintf("loadgen -mode=%s -duration=%s -concurrency=%d", mode, duration, concurrency),
		StartedAt:   started,
		DurationS:   duration.Seconds(),
		ElapsedS:    elapsed.Seconds(),
		Concurrency: concurrency,
		ServerPID:   env.serverPID,
		ServerAddr:  env.serverAddr,
		MockLLMURL:  env.mock.URL(),
		Thresholds: thresholdsJSON{
			SSEP95MS:            sseP95Budget.Milliseconds(),
			GoroutineGrowPct:    goroutineGrow * 100,
			RSSGrowPct:          rssGrow * 100,
			RSSAbsoluteCapKB:    rssAbsoluteCapKB,
			HostMemAvailFloorKB: hostMemFloorKB,
			HostMemPrecheckKB:   hostMemPrecheckKB,
			WarmupFraction:      warmupFraction,
			SampleIntervalS:     sampleInterval.Seconds(),
		},
		Samples: sampler.snapshot(),
		Ops:     rec.snapshot(),
		Counts: countsJSON{
			Chats:      stats.Chats.Load(),
			Approvals:  stats.Approvals.Load(),
			Tokens:     stats.Tokens.Load(),
			CRUDGroups: stats.CRUDOps.Load(),
			AuditPages: stats.AuditPages.Load(),
			HTTP5xx:    stats.HTTP5xx.Load(),
			SSEErrors:  stats.SSEErrors.Load(),
			MockLLMReq: env.mock.Requests(),
		},
		Verdict: computeVerdict(sampler.snapshot(), rec.snapshot(), stats, duration, hostMemLow.Load()),
	}

	mdPath, jsonPath, err := writeReports(env.repoRoot, mode, res)
	if err != nil {
		return fmt.Errorf("write reports: %w", err)
	}
	fmt.Printf("loadgen: reports written:\n  %s\n  %s\n", mdPath, jsonPath)

	v := res.Verdict
	fmt.Printf("loadgen: chats=%d approvals=%d tokens=%d 5xx=%d sse_errors=%d sse_p95=%dms\n",
		res.Counts.Chats, res.Counts.Approvals, res.Counts.Tokens, v.HTTP5xx, v.SSEErrors, v.SSEP95MS)
	fmt.Printf("loadgen: rss %dKB→%dKB (%.1f%%)  goroutine %d→%d (%.1f%%)\n",
		v.RSSBaselineKB, v.RSSMaxKB, v.RSSGrowthPct, v.GoroutineBaseline, v.GoroutineMax, v.GoroutineGrowthPct)

	if !v.Pass {
		return fmt.Errorf("verdict FAILED: %s", strings.Join(v.Violations, "; "))
	}
	fmt.Printf("loadgen: verdict PASS\n")
	return nil
}
