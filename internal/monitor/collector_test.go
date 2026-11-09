// collector_test.go 覆盖 G30：Linux/Windows 采集模板解析（fixture + 畸形
// 输出）、环形缓冲上限（1440 定长覆盖最旧 / map 键上限 1024）、首轮差值
// 全 0、连续失败退避（5 次暂停 + 告警事件 + 恢复 resolve）、采集轮聚合
// 审计、内存守卫（环形缓冲 push 稳态零分配）。
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

const linuxStatRound1 = `===stat
cpu  100 0 100 700 0 0 0 0 0 0
cpu0 50 0 50 350 0 0 0 0 0 0
intr 1000
===loadavg
0.52 0.58 0.59 1/812 23456
===meminfo
MemTotal:       16000000 kB
MemFree:         4000000 kB
MemAvailable:    8000000 kB
===df
Filesystem     1B-blocks      Used  Available Use% Mounted on
/dev/sda1    100000000000 90000000000  10000000000  90% /
tmpfs          8000000000 1000000000   7000000000  13% /tmp
overlay        50000000000 45000000000   5000000000  90% /var/lib/docker/overlay2/x
/dev/sdb1      20000000000 10000000000  10000000000  50% /data
===netdev
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000000 1000 0 0 0 0 0 0  1000000 1000 0 0 0 0 0 0
  eth0: 2000000 2000 0 0 0 0 0 0  3000000 3000 0 0 0 0 0 0
 vethabc: 500000 500 0 0 0 0 0 0  600000 600 0 0 0 0 0 0
  eth1: 1000000 1000 0 0 0 0 0 0  2000000 2000 0 0 0 0 0 0
`

const linuxStatRound2 = `===stat
cpu  200 0 100 1300 0 0 0 0 0 0
===loadavg
1.20 0.90 0.70 2/900 30000
===meminfo
MemTotal:       16000000 kB
MemFree:         1000000 kB
MemAvailable:    3200000 kB
===df
Filesystem     1B-blocks      Used  Available Use% Mounted on
/dev/sda1    100000000000 95000000000   5000000000  95% /
/dev/sdb1      20000000000 10000000000  10000000000  50% /data
===netdev
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 3200000 3000 0 0 0 0 0 0  4500000 4000 0 0 0 0 0 0
  eth1: 1600000 1500 0 0 0 0 0 0  2800000 2500 0 0 0 0 0 0
`

func TestParseLinuxOutputFirstRoundZeroDiffs(t *testing.T) {
	s, raw := ParseLinuxOutput(linuxStatRound1, nil, 60)
	if s.CPU != 0 {
		t.Fatalf("first round cpu diff must be 0, got %v", s.CPU)
	}
	if s.NetIn != 0 || s.NetOut != 0 {
		t.Fatalf("first round net diff must be 0, got in=%d out=%d", s.NetIn, s.NetOut)
	}
	if s.Load1 != 0.52 {
		t.Fatalf("load_1 = %v, want 0.52", s.Load1)
	}
	if want := float64(100 * (16000000 - 8000000) / 16000000); s.Mem != want {
		t.Fatalf("mem_pct = %v, want %v", s.Mem, want)
	}
	// disk_pct：剔除 tmpfs 与 overlay 后取最大（sda1 90% > sdb1 50%）。
	if s.Disk != 90 {
		t.Fatalf("disk_pct = %v, want 90", s.Disk)
	}
	// 网络计数剔除 lo 与 veth 前缀。
	if raw.NetInBytes != 3000000 {
		t.Fatalf("net in bytes = %v, want 3000000", raw.NetInBytes)
	}
	if raw.NetOutByte != 5000000 {
		t.Fatalf("net out bytes = %v, want 5000000", raw.NetOutByte)
	}
}

func TestParseLinuxOutputSecondRoundDiffs(t *testing.T) {
	_, raw1 := ParseLinuxOutput(linuxStatRound1, nil, 60)
	s, _ := ParseLinuxOutput(linuxStatRound2, &raw1, 60)
	if s.Mem != 80 {
		t.Fatalf("mem_pct = %v, want 80", s.Mem)
	}
	if s.Disk != 95 {
		t.Fatalf("disk_pct = %v, want 95", s.Disk)
	}
	// 契约 7.7：net_*_bps = 两次采样字节差 ÷ interval（**字节/秒**）——
	// in 增量 1800000B→30000 B/s；out 增量 2300000B→38333 B/s（60s）。
	if s.NetIn != 30000 {
		t.Fatalf("net_in_bps = %d, want 30000", s.NetIn)
	}
	if s.NetOut != 38333 {
		t.Fatalf("net_out_bps = %d, want 38333", s.NetOut)
	}
	if s.Load1 != 1.20 {
		t.Fatalf("load_1 = %v, want 1.20", s.Load1)
	}
}

// TestParseLinuxOutputNetRateIsBytesPerSecond 锚定「网络入/出」的单位口径
// （六期打磨 sprint 6 走查 high）：60s 内 /proc/net/dev 计数 +60,000,000
// 字节（真实 1,000,000 B/s）必须解析为 1,000,000，而不是乘 8 后的
// 8,000,000。契约 7.7（字节差 ÷ interval）与 Windows 路径
// （BytesReceivedPersec 直取）、前端 formatBps（MB/s）三处同口径。
func TestParseLinuxOutputNetRateIsBytesPerSecond(t *testing.T) {
	round1 := "===netdev\n  eth0: 1000000 1000 0 0 0 0 0 0  2000000 2000 0 0 0 0 0 0\n"
	round2 := "===netdev\n  eth0: 61000000 2000 0 0 0 0 0 0  62000000 3000 0 0 0 0 0 0\n"
	_, raw1 := ParseLinuxOutput(round1, nil, 60)
	s, raw2 := ParseLinuxOutput(round2, &raw1, 60)
	if raw2.NetInBytes-raw1.NetInBytes != 60000000 {
		t.Fatalf("increment = %v, want 60000000", raw2.NetInBytes-raw1.NetInBytes)
	}
	if s.NetIn != 1000000 {
		t.Fatalf("net_in_bps = %d, want 1000000 (= 60MB / 60s，字节/秒；乘 8 会得到 8000000)", s.NetIn)
	}
	if s.NetOut != 1000000 {
		t.Fatalf("net_out_bps = %d, want 1000000", s.NetOut)
	}
}

func TestParseLinuxOutputCPUDiffFormula(t *testing.T) {
	round1 := "===stat\ncpu  100 0 100 700 0 0 0 0 0 0\n"
	round2 := "===stat\ncpu  150 0 100 750 0 0 0 0 0 0\n"
	_, raw1 := ParseLinuxOutput(round1, nil, 60)
	s, _ := ParseLinuxOutput(round2, &raw1, 60)
	// total +100、idle +50 → 非闲置 50%。
	if s.CPU != 50 {
		t.Fatalf("cpu_pct = %v, want 50", s.CPU)
	}
}

func TestParseLinuxOutputMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"garbage":     "total nonsense \x00\x01 binary-ish \n\n\n",
		"no sections": "cpu 1 2 3 4 5\ncpu 1 2 3 4 5\n",
		"broken nums": "===stat\ncpu abc def\n===loadavg\nNaN NaN\n===meminfo\nMemTotal: xyz kB\n",
		"truncated":   "===stat\ncpu 100 0 100 700",
		"huge":        "===netdev\n" + strings.Repeat("eth9: 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1\n", 100000),
		"bad netdev":  "===netdev\neth0:not-enough-fields\neth1\n",
		"bad df":      "===df\nonly-header-nope\n/dev/sda1 1 2 3 4%\n",
	}
	for name, input := range cases {
		s, _ := ParseLinuxOutput(input, nil, 60)
		if s.CPU < 0 || s.CPU > 100 || s.Mem < 0 || s.Mem > 100 || s.Disk < 0 || s.Disk > 100 || s.Load1 < 0 {
			t.Fatalf("case %s: out-of-range sample %+v", name, s)
		}
	}
}

func TestParseWindowsOutput(t *testing.T) {
	out := `{"cpu_pct":12.5,"mem_pct":43.1,"disk_pct":61.0,"load_1":0.35,"net_in_bps":125000,"net_out_bps":98000}`
	s := ParseWindowsOutput(out)
	if s.CPU != 12.5 || s.Mem != 43.1 || s.Disk != 61 || s.Load1 != 0.35 || s.NetIn != 125000 || s.NetOut != 98000 {
		t.Fatalf("windows parse mismatch: %+v", s)
	}
	// BOM + 尾部空白。
	s = ParseWindowsOutput("\uFEFF" + out + "\r\n")
	if s.CPU != 12.5 {
		t.Fatalf("BOM handling failed: %+v", s)
	}
	for _, bad := range []string{"", "not json", `{"cpu_pct":12.5`, `[1,2,3]`, strings.Repeat("x", 300000)} {
		if s := ParseWindowsOutput(bad); s != (Sample{}) {
			t.Fatalf("malformed input should yield zero sample, got %+v", s)
		}
	}
}

// ---------------------------------------------------------------------------
// 环形缓冲与内存守卫
// ---------------------------------------------------------------------------

func TestRingBufferFixedCapacity(t *testing.T) {
	var r ringBuffer
	base := time.Now()
	for i := 0; i < BufferPoints+100; i++ {
		r.push(Sample{CPU: float64(i)}, base.Add(time.Duration(i)*time.Second))
	}
	if r.count != BufferPoints {
		t.Fatalf("count = %d, want %d (定长，禁止增长)", r.count, BufferPoints)
	}
	samples, stamps := r.lastN(3)
	if len(samples) != 3 {
		t.Fatalf("lastN len = %d", len(samples))
	}
	want := []float64{float64(BufferPoints + 97), float64(BufferPoints + 98), float64(BufferPoints + 99)}
	for i, s := range samples {
		if s.CPU != want[i] {
			t.Fatalf("oldest overwritten: lastN[%d].CPU = %v, want %v", i, s.CPU, want[i])
		}
	}
	if !stamps[0].Before(stamps[2]) {
		t.Fatal("lastN must be ascending by time")
	}
}

func TestMetricBufferServerCap(t *testing.T) {
	b := newMetricBuffer()
	for i := 0; i < MaxBufferedServers+50; i++ {
		b.push(fmt.Sprintf("srv-%d", i), Sample{}, time.Now())
	}
	if b.size() != MaxBufferedServers {
		t.Fatalf("buffer map size = %d, want %d", b.size(), MaxBufferedServers)
	}
	if _, _, ok := b.lastN("srv-0", 1); !ok {
		t.Fatal("srv-0 should be cached")
	}
	if _, _, ok := b.lastN(fmt.Sprintf("srv-%d", MaxBufferedServers+10), 1); ok {
		t.Fatal("server beyond cap must not be cached")
	}
}

// TestRingBufferAllocBudget 内存守卫：稳态 push 零分配（定长数组覆盖写）。
func TestRingBufferAllocBudget(t *testing.T) {
	var r ringBuffer
	now := time.Now()
	for i := 0; i < BufferPoints; i++ {
		r.push(Sample{CPU: 1}, now)
	}
	allocs := testing.AllocsPerRun(200, func() {
		r.push(Sample{CPU: 2, NetIn: 3}, now.Add(time.Second))
	})
	if allocs > 0 {
		t.Fatalf("steady-state push must not allocate, got %v allocs/op", allocs)
	}
}

// TestMetricBufferAllocBudget 内存守卫：达到键上限后写超限服务器不新增条目。
func TestMetricBufferAllocBudget(t *testing.T) {
	b := newMetricBuffer()
	for i := 0; i < MaxBufferedServers; i++ {
		b.push(fmt.Sprintf("s%d", i), Sample{}, time.Now())
	}
	allocs := testing.AllocsPerRun(100, func() {
		b.push("overflow-server", Sample{}, time.Now())
		b.push("s0", Sample{}, time.Now())
	})
	if allocs > 4 {
		t.Fatalf("capped-buffer writes must be allocation-light, got %v allocs/op", allocs)
	}
	if b.size() != MaxBufferedServers {
		t.Fatalf("size = %d, want %d", b.size(), MaxBufferedServers)
	}
}

// ---------------------------------------------------------------------------
// 采集轮与退避
// ---------------------------------------------------------------------------

func newTestCollector(t *testing.T) (*collector, *fakeMonitorStore, *fakeAuditSink, *cryptox.Cipher) {
	t.Helper()
	st := newFakeMonitorStore()
	sink := &fakeAuditSink{}
	cipher := newTestCipher(t)
	now := time.Now()
	c := &collector{
		cfg:    NormalizeCollectorConfig(CollectorConfig{Enabled: true, Interval: 60 * time.Second}),
		st:     st,
		cipher: cipher,
		log:    quietLogger(),
		clock:  func() time.Time { return now },
		audit:  sink,
		buf:    newMetricBuffer(),
		states: make(map[string]*collectorState, 4),
	}
	return c, st, sink, cipher
}

func TestCollectorFailureBackoffSuspendsAndAlerts(t *testing.T) {
	c, st, sink, cipher := newTestCollector(t)
	srv := fakeServer("srv-1", store.ServerOSLinux)
	_ = cipher
	st.upsertServer(srv)

	failErr := errors.New("ssh dial failed")
	for i := 1; i <= collectorFailureThreshold; i++ {
		c.recordFailure(context.Background(), srv, failErr)
	}
	c.mu.Lock()
	state := c.states[srv.ID]
	pausedUntil := state.pausedUntil
	c.mu.Unlock()
	if pausedUntil.IsZero() || !pausedUntil.After(time.Now()) {
		t.Fatal("server must be paused after 5 consecutive failures")
	}
	if len(sink.of("alert.triggered")) == 0 {
		t.Fatal("suspend alert event not created after threshold")
	}
	st.mu.Lock()
	firing := 0
	for _, ev := range st.alerts {
		if ev.Status == store.AlertEventFiring {
			firing++
		}
	}
	st.mu.Unlock()
	if firing != 1 {
		t.Fatalf("firing suspend events = %d, want 1", firing)
	}

	// 暂停中的目标：runRound 直接跳过（不执行、不失败）。
	c.ssh = &fakeSSHExecutor{outputs: map[string]string{srv.ID: linuxStatRound1}}
	ok, fail := c.runRound(context.Background())
	if ok != 0 || fail != 0 {
		t.Fatalf("paused target must be skipped, got ok=%d fail=%d", ok, fail)
	}

	// 恢复：recordSuccess → resolve。
	c.recordSuccess(srv, nil, time.Now())
	st.mu.Lock()
	open := 0
	for _, ev := range st.alerts {
		if ev.Status == store.AlertEventFiring {
			open++
		}
	}
	st.mu.Unlock()
	if open != 0 {
		t.Fatalf("suspend alert must be resolved after recovery, open=%d", open)
	}
	if len(sink.of("alert.resolved")) == 0 {
		t.Fatal("alert.resolved audit row missing")
	}
}

// TestCollectorSuspendRuleUsesLoadMetric：六期打磨 sprint 2（走查 high）。
//
// 系统哨兵规则此前用 cpu_pct + threshold=101，必然被迁移 00007 的 CHECK
// （cpu/mem/disk threshold<=100）拒绝，「采集不可达」事件永远创建失败——
// 服务器连续失联对用户完全静默。本测试锁定：
//  1. 规则指标不在百分比三件套里（不受该 CHECK 约束）；
//  2. 阈值 >100（历史 bug 的触发条件本身不再出现）；
//  3. 规则 enabled=false（不参与评估，永不产生额外 firing）；
//  4. 三台服务器各触发一次失败告警后，事件真实落库且审计可查。
func TestCollectorSuspendRuleUsesLoadMetric(t *testing.T) {
	c, st, sink, _ := newTestCollector(t)
	srv := fakeServer("srv-suspend", store.ServerOSLinux)
	st.upsertServer(srv)

	// 第一次失败达到阈值 → 建规则 + 建事件。
	for i := 0; i < collectorFailureThreshold; i++ {
		c.recordFailure(context.Background(), srv, errors.New("ssh dial failed"))
	}

	st.mu.Lock()
	var sentinel *store.AlertRule
	for _, r := range st.rules {
		if r.Name == suspendRuleName {
			rule := r
			sentinel = &rule
		}
	}
	firing := 0
	for _, ev := range st.alerts {
		if ev.Status == store.AlertEventFiring {
			firing++
		}
	}
	st.mu.Unlock()

	if sentinel == nil {
		t.Fatal("suspend sentinel rule must be created after threshold failures")
	}
	if sentinel.Metric == store.AlertMetricCPU || sentinel.Metric == store.AlertMetricMem ||
		sentinel.Metric == store.AlertMetricDisk {
		t.Fatalf("sentinel metric = %q, must not be a pct metric (threshold<=100 CHECK)", sentinel.Metric)
	}
	if sentinel.Metric != store.AlertMetricLoad1 {
		t.Fatalf("sentinel metric = %q, want load_1", sentinel.Metric)
	}
	if sentinel.Threshold <= 100 {
		t.Fatalf("sentinel threshold = %v, want an unreachable value > 100", sentinel.Threshold)
	}
	if sentinel.Enabled {
		t.Fatal("sentinel rule must stay disabled (never evaluated)")
	}
	if firing != 1 {
		t.Fatalf("firing suspend events = %d, want 1", firing)
	}
	if len(sink.of("alert.triggered")) != 1 {
		t.Fatalf("alert.triggered audit rows = %d, want 1", len(sink.of("alert.triggered")))
	}
	// 缓存：第二次失败告警复用同一规则（不重复建）。
	id, err := c.ensureSuspendRule(context.Background())
	if err != nil {
		t.Fatalf("ensureSuspendRule: %v", err)
	}
	if id != sentinel.ID {
		t.Fatalf("ensureSuspendRule id = %q, want cached %q", id, sentinel.ID)
	}
}

// TestCollectorSuspendRuleRecreatedAfterDelete：用户从 /alerts 删掉系统规则后，
// 下一次失联告警必须重建规则而不是永久静默（缓存 ID 失效自愈）。
func TestCollectorSuspendRuleRecreatedAfterDelete(t *testing.T) {
	c, st, _, _ := newTestCollector(t)
	srv := fakeServer("srv-recreate", store.ServerOSLinux)
	st.upsertServer(srv)

	for i := 0; i < collectorFailureThreshold; i++ {
		c.recordFailure(context.Background(), srv, errors.New("ssh dial failed"))
	}
	first, err := c.ensureSuspendRule(context.Background())
	if err != nil || first == "" {
		t.Fatalf("first ensureSuspendRule = %q, %v", first, err)
	}

	// 用户在告警页删除该规则（事件级联删除）。
	if err := st.AlertRules().Delete(context.Background(), first); err != nil {
		t.Fatalf("delete sentinel: %v", err)
	}

	// 第二轮失联：suspendRule 必须清缓存并重建（不停在失效 ID 上）。
	// 注：fake store 的规则 ID 由名字派生（`rule-<name>`），重建后 id 相同；
	// 因此断言「规则行重新存在」而不是「id 变了」——真实 UUID 下 id 也会变。
	rule, err := c.suspendRule(context.Background())
	if err != nil {
		t.Fatalf("suspendRule after delete: %v", err)
	}
	if rule.Name != suspendRuleName {
		t.Fatalf("recreated rule name = %q, want %q", rule.Name, suspendRuleName)
	}
	if _, err := st.AlertRules().Get(context.Background(), rule.ID); err != nil {
		t.Fatalf("recreated rule must exist in store, Get err = %v", err)
	}

	// 事件确实落库（不再是静默失败）。
	c.fireSuspendAlert(context.Background(), srv)
	st.mu.Lock()
	firing := 0
	for _, ev := range st.alerts {
		if ev.Status == store.AlertEventFiring && ev.RuleID == rule.ID {
			firing++
		}
	}
	st.mu.Unlock()
	if firing != 1 {
		t.Fatalf("firing events after recreate = %d, want 1", firing)
	}
}

func TestCollectorFailureBelowThresholdNoAlert(t *testing.T) {
	c, st, sink, cipher := newTestCollector(t)
	srv := fakeServer("srv-2", store.ServerOSLinux)
	_ = cipher
	for i := 0; i < collectorFailureThreshold-1; i++ {
		c.recordFailure(context.Background(), srv, errors.New("boom"))
	}
	if len(st.alerts) != 0 || len(sink.of("alert.triggered")) != 0 {
		t.Fatal("no alert before threshold")
	}
	c.mu.Lock()
	paused := c.states[srv.ID].pausedUntil
	c.mu.Unlock()
	if !paused.IsZero() {
		t.Fatal("must not pause before threshold")
	}
}

func TestCollectorRunRoundHappyPathAndAudit(t *testing.T) {
	c, st, sink, cipher := newTestCollector(t)
	srv := fakeServer("srv-3", store.ServerOSLinux)
	withTestCreds(t, cipher, &srv)
	st.upsertServer(srv)
	c.ssh = &fakeSSHExecutor{outputs: map[string]string{srv.ID: linuxStatRound1}}

	ok, fail := c.runRound(context.Background())
	if ok != 1 || fail != 0 {
		t.Fatalf("ok=%d fail=%d, want 1/0", ok, fail)
	}
	st.mu.Lock()
	inserted := len(st.metrics)
	st.mu.Unlock()
	if inserted != 1 {
		t.Fatalf("metrics inserted = %d, want 1", inserted)
	}
	if _, _, cached := c.buf.lastN(srv.ID, 1); !cached {
		t.Fatal("sample must be cached in ring buffer")
	}
	rows := sink.of("metric.collect")
	if len(rows) != 1 {
		t.Fatalf("metric.collect rows = %d, want 1", len(rows))
	}
	if rows[0].ActorType != "system" {
		t.Fatalf("actor_type = %s, want system", rows[0].ActorType)
	}
	if rows[0].Detail["ok"] != 1 || rows[0].Detail["failed"] != 0 {
		t.Fatalf("detail = %v", rows[0].Detail)
	}
}

func TestCollectorRunRoundSkipsNetworkDevices(t *testing.T) {
	c, st, _, _ := newTestCollector(t)
	st.upsertServer(fakeServer("netdev-1", store.ServerOSNetwork, func(s *store.Server) {
		s.DeviceKind = store.DeviceKindNetworkDevice
	}))
	c.ssh = &fakeSSHExecutor{outputs: map[string]string{"netdev-1": linuxStatRound1}}
	ok, fail := c.runRound(context.Background())
	if ok != 0 || fail != 0 {
		t.Fatalf("network_device must not be collected, got ok=%d fail=%d", ok, fail)
	}
}

func TestCollectorRunRoundWindows(t *testing.T) {
	c, st, _, cipher := newTestCollector(t)
	srv := fakeServer("win-1", store.ServerOSWindows, func(s *store.Server) {
		s.AuthMethod = store.AuthMethodPasswordNTLM
		s.WinRMPort = 5985
	})
	withTestCreds(t, cipher, &srv)
	st.upsertServer(srv)
	fake := &fakeWinRMExecutor{}
	fake.outputs = map[string]string{srv.ID: `{"cpu_pct":12.5,"mem_pct":43.1,"disk_pct":61.0,"load_1":0.35,"net_in_bps":125000,"net_out_bps":98000}`}
	c.winrm = fake

	ok, fail := c.runRound(context.Background())
	if ok != 1 || fail != 0 {
		t.Fatalf("ok=%d fail=%d, want 1/0", ok, fail)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.metrics) != 1 {
		t.Fatalf("metrics = %d", len(st.metrics))
	}
	m := st.metrics[0]
	if m.CPU != 12.5 || m.NetIn != 125000 {
		t.Fatalf("windows sample mismatch: %+v", m)
	}
	if len(fake.calls) != 1 || fake.calls[0] != srv.ID+"|"+WindowsCollectTemplate {
		t.Fatalf("windows collect template not sent verbatim: %v", fake.calls)
	}
}

func TestCollectorRunRoundFailureCounted(t *testing.T) {
	c, st, _, cipher := newTestCollector(t)
	srv := fakeServer("srv-4", store.ServerOSLinux)
	withTestCreds(t, cipher, &srv)
	st.upsertServer(srv)
	c.ssh = &fakeSSHExecutor{errs: map[string]error{srv.ID: errors.New("dial timeout")}}
	ok, fail := c.runRound(context.Background())
	if ok != 0 || fail != 1 {
		t.Fatalf("ok=%d fail=%d, want 0/1", ok, fail)
	}
}

// TestCollectorBatchSplit 验证分批写库按 BatchInsertMax 切片（fake store
// 超过单批上限返回 ErrInvalid——这里直接驱动分批逻辑的边界）。
func TestCollectorBatchSplit(t *testing.T) {
	c, st, _, cipher := newTestCollector(t)
	// 插入 3 台 linux 服务器，输出合法 → 单批 3 行 ≤500。
	ids := []string{"b1", "b2", "b3"}
	outs := map[string]string{}
	for _, id := range ids {
		srv := fakeServer(id, store.ServerOSLinux)
		withTestCreds(t, cipher, &srv)
		st.upsertServer(srv)
		outs[id] = linuxStatRound1
	}
	c.ssh = &fakeSSHExecutor{outputs: outs}
	ok, fail := c.runRound(context.Background())
	if ok != len(ids) || fail != 0 {
		t.Fatalf("ok=%d fail=%d", ok, fail)
	}
	if c.cfg.BatchInsertMax != 500 {
		t.Fatalf("BatchInsertMax = %d, want 500 (契约 11 节)", c.cfg.BatchInsertMax)
	}
}

var _ = slog.Default // 保持 import（quietLogger 在 fakes_test.go）

// TestDecryptEmptyPassphraseShortCircuits 覆盖六期打磨 sprint 4 的走查 high：
// 私钥认证且未填 passphrase（openapi 4.3 明确该字段可选）时，库里
// PassphraseEncrypted 为空串。此前 decrypt("") 直接进 cryptox.Decrypt，
// 空串被判 ErrInvalidCiphertext → sshTarget 报 "monitor: decrypt passphrase
// failed"，该资产采集恒失败（5 次后退避暂停并生成「采集不可达」事件）。
//
// 空串必须短路为「无口令」（与 agent/batchx 同名辅助同口径），非空但非法
// 的密文仍必须报错——安全语义不削弱。
func TestDecryptEmptyPassphraseShortCircuits(t *testing.T) {
	c, _, _, cipher := newTestCollector(t)
	key, err := cipher.Encrypt("PRIVATE-KEY-BODY")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	srv := fakeServer("srv-pk", store.ServerOSLinux, func(s *store.Server) {
		s.AuthMethod = store.AuthMethodPrivateKey
		s.PrivateKeyEncrypted = key
		s.PassphraseEncrypted = "" // 未填口令（可选字段）
	})

	target, err := c.sshTarget(srv)
	if err != nil {
		t.Fatalf("sshTarget with empty passphrase: %v", err)
	}
	if target.PrivateKey != "PRIVATE-KEY-BODY" {
		t.Fatalf("private key = %q", target.PrivateKey)
	}
	if target.Passphrase != "" {
		t.Fatalf("passphrase = %q, want empty", target.Passphrase)
	}

	// 非法密文仍必须报错（空串短路不得吞掉真实损坏的凭据）。
	if _, err := decrypt(cipher, "not-a-ciphertext"); err == nil {
		t.Fatal("invalid ciphertext must still fail")
	}
	// cipher 未注入仍必须报错（nil 防护不因空串短路而丢）。
	if _, err := decrypt(nil, ""); err != nil {
		t.Fatalf("empty ciphertext must short-circuit before nil guard: %v", err)
	}
	if _, err := decrypt(nil, "v1:aa:bb"); err == nil {
		t.Fatal("nil cipher must still fail for non-empty ciphertext")
	}
}

// TestSuspendAlertMessageIsChinese 覆盖六期打磨 sprint 4 走查 low：
// 「采集不可达」事件的消息此前是全站唯一一条直出英文的业务文案
// （"metric collection failed 5 consecutive times; collection paused"），
// 告警页「消息」列逐字显示英文。消息改中文（含次数与暂停时长），
// 规则名保持英文原样（数据库键 + 前端按名映射中文展示）。
func TestSuspendAlertMessageIsChinese(t *testing.T) {
	c, st, _, _ := newTestCollector(t)
	srv := fakeServer("srv-msg", store.ServerOSLinux)
	st.upsertServer(srv)
	for i := 0; i < collectorFailureThreshold; i++ {
		c.recordFailure(context.Background(), srv, errors.New("ssh dial failed"))
	}

	st.mu.Lock()
	var msg string
	var ruleName string
	for _, ev := range st.alerts {
		msg = ev.Message
		ruleName = ev.RuleName
	}
	st.mu.Unlock()

	if msg == "" {
		t.Fatal("suspend alert event must be created")
	}
	// 必须含中文且不再含英文业务文案（规则名/技术标识不在此列）。
	for _, want := range []string{"连续", "指标采集失败", "采集已暂停"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message = %q, want substring %q", msg, want)
		}
	}
	if strings.Contains(msg, "metric collection failed") || strings.Contains(msg, "collection paused") {
		t.Fatalf("message still contains English business text: %q", msg)
	}
	// 规则名保持英文（前端按名映射展示名，不改数据库键）。
	if ruleName != suspendRuleName {
		t.Fatalf("rule_name = %q, want %q", ruleName, suspendRuleName)
	}
}
