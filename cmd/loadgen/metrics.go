package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// opRecorder 按操作名收集延迟样本；结束时一次性计算分位数。
type opRecorder struct {
	mu   sync.Mutex
	ops  map[string][]time.Duration
	errs map[string]int
}

func newOpRecorder() *opRecorder {
	return &opRecorder{ops: map[string][]time.Duration{}, errs: map[string]int{}}
}

func (r *opRecorder) record(op string, d time.Duration) {
	r.mu.Lock()
	r.ops[op] = append(r.ops[op], d)
	r.mu.Unlock()
}

func (r *opRecorder) recordErr(op string) {
	r.mu.Lock()
	r.errs[op]++
	r.mu.Unlock()
}

func (r *opRecorder) snapshot() map[string]opStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]opStats, len(r.ops))
	for name, durs := range r.ops {
		out[name] = computeStats(durs)
	}
	return out
}

// opStats 是一个操作的延迟统计（毫秒）；次数为 0 时分位数为 0。
type opStats struct {
	Count int   `json:"count"`
	Errs  int   `json:"errs"`
	P50MS int64 `json:"p50_ms"`
	P95MS int64 `json:"p95_ms"`
	P99MS int64 `json:"p99_ms"`
	MinMS int64 `json:"min_ms"`
	MaxMS int64 `json:"max_ms"`
}

func computeStats(durs []time.Duration) opStats {
	if len(durs) == 0 {
		return opStats{}
	}
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(p float64) time.Duration {
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx]
	}
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	return opStats{
		Count: len(sorted),
		P50MS: ms(pick(0.50)),
		P95MS: ms(pick(0.95)),
		P99MS: ms(pick(0.99)),
		MinMS: ms(sorted[0]),
		MaxMS: ms(sorted[len(sorted)-1]),
	}
}

// sample 是一次资源采样；Err 非 NIL 字段记录采样失败原因。
type sample struct {
	T          float64 `json:"t_s"`          // 相对负载开始的秒数
	RSSKB      int64   `json:"rss_kb"`       // /proc/<pid>/status VmRSS，kB；失败为 -1
	Gorou      int64   `json:"goroutine"`    // /debug/pprof goroutine 总数；失败为 -1
	MemAvailKB int64   `json:"mem_avail_kb"` // /proc/meminfo MemAvailable，kB；失败为 -1
	ErrNote    string  `json:"err,omitempty"`
}

// readMeminfoKB 从 /proc/meminfo 读取指定字段（如 MemAvailable）的 kB 值。
func readMeminfoKB(field string) (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	prefix := field + ":"
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed meminfo line %q", line)
		}
		return strconv.ParseInt(fields[1], 10, 64)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s not found in /proc/meminfo", field)
}

// readMemAvailableKB 读取宿主机当前 MemAvailable（kB）。
func readMemAvailableKB() (int64, error) { return readMeminfoKB("MemAvailable") }

// hostMemFloorKB：运行中宿主机 MemAvailable 下限（2GiB）；低于此值
// 立即停止负载并判失败（本机还需给 ZCode 与桌面环境留足余量）。
const hostMemFloorKB = 2 << 30 / 1024 // 2GiB，单位 kB

// sampler 周期采样被测服务端的 RSS 与 goroutine 数，以及宿主机
// MemAvailable；后者跌破 hostMemFloorKB 时通过 onAbort 立即停止负载。
type sampler struct {
	serverPID int
	pprofURL  string
	hc        *http.Client
	pprofHC   *http.Client
	start     time.Time

	onAbort func() // MemAvailable < 下限时调用一次（立即停止负载）
	fired   atomic.Bool

	mu      sync.Mutex
	samples []sample
}

// newSampler 构造采样器；pprofHC 必须携带已登录会话（admin 客户端），
// 服务端 /debug/pprof 与 API 同等鉴权，匿名抓取会 401。
func newSampler(serverPID int, pprofURL string, pprofHC *http.Client, onAbort func()) *sampler {
	if pprofHC == nil {
		pprofHC = &http.Client{Timeout: 4 * time.Second}
	}
	return &sampler{
		serverPID: serverPID,
		pprofURL:  pprofURL,
		hc:        &http.Client{Timeout: 4 * time.Second},
		pprofHC:   pprofHC,
		start:     time.Now(),
		onAbort:   onAbort,
	}
}

func (s *sampler) loop(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.take()
		}
	}
}

func (s *sampler) take() {
	sm := sample{T: time.Since(s.start).Seconds()}

	rss, err := readRSSKB(s.serverPID)
	if err != nil {
		sm.RSSKB = -1
		sm.ErrNote = "rss: " + err.Error()
	} else {
		sm.RSSKB = rss
	}

	g, err := fetchGoroutineCount(s.pprofHC, s.pprofURL)
	if err != nil {
		sm.Gorou = -1
		if sm.ErrNote != "" {
			sm.ErrNote += "; "
		}
		sm.ErrNote += "goroutine: " + err.Error()
	} else {
		sm.Gorou = g
	}

	mem, err := readMemAvailableKB()
	if err != nil {
		sm.MemAvailKB = -1
		if sm.ErrNote != "" {
			sm.ErrNote += "; "
		}
		sm.ErrNote += "meminfo: " + err.Error()
	} else {
		sm.MemAvailKB = mem
	}

	s.mu.Lock()
	s.samples = append(s.samples, sm)
	s.mu.Unlock()

	// 宿主机可用内存跌破下限：立即触发停止（只触发一次）并记录原因。
	if sm.MemAvailKB >= 0 && sm.MemAvailKB < hostMemFloorKB && s.onAbort != nil && s.fired.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "loadgen: host MemAvailable %dKB < %dKB floor — aborting load NOW\n", sm.MemAvailKB, hostMemFloorKB)
		s.onAbort()
	}
}

func (s *sampler) snapshot() []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sample(nil), s.samples...)
}

// readRSSKB 读取 /proc/<pid>/status 的 VmRSS（kB）。
func readRSSKB(pid int) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("malformed VmRSS line %q", line)
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/%d/status", pid)
}

// fetchGoroutineCount 从 /debug/pprof/goroutine?debug=1 解析 total。
// 该端点仅在 AIOPS_PPROF=1 时存在（cmd/aiops/main.go withPprof）。
func fetchGoroutineCount(hc *http.Client, pprofBase string) (int64, error) {
	resp, err := hc.Get(pprofBase + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("pprof status %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		// 第一行形如 "goroutine profile: total 42"。
		if total, ok := parseGoroutineProfileTotal(scanner.Text()); ok {
			return total, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("goroutine profile total not found")
}

func parseGoroutineProfileTotal(line string) (int64, bool) {
	// 兼容 "goroutine profile: total N" 与旧格式 "goroutine    N [running]"。
	if strings.HasPrefix(line, "goroutine profile: total ") {
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "goroutine profile: total ")), 10, 64)
		if err == nil {
			return n, true
		}
	}
	return 0, false
}
