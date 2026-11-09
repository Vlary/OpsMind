// collector.go 实现五期 M1 的指标采集器（契约 2.10 约定 1/5.15/11 节）：
// 固定模板直连 sshx/winrmx（不经审批，同 server_facts 模式）、分段/JSON
// 解析 6 指标、进程内环形缓冲（定长数组 1440 点、map 键上限 1024）、批量
// 写库（≤500/批）+ UPSERT latest（同事务）、每轮聚合审计 metric.collect、
// 30 天保留每日清理、连续失败退避（5 次暂停 + 系统告警事件，恢复后自动
// 复采并 resolve）。
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// Sample 是一轮采集解析出的 6 指标（与 server_metrics 列一一对应）。
type Sample struct {
	CPU    float64
	Mem    float64
	Disk   float64
	Load1  float64
	NetIn  int64
	NetOut int64
}

// LinuxCollectTemplate / WindowsCollectTemplate 是采集固定模板（契约 7.7
// 写死；与 risk 的 LinuxCollectCommand/WinCollectCommand 同文——risk 不能
// import monitor，两边各持一份，测试断言字节一致）。
const (
	LinuxCollectTemplate = `echo ===stat; cat /proc/stat; echo ===loadavg; cat /proc/loadavg; echo ===meminfo; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; echo ===df; df -B1; echo ===netdev; cat /proc/net/dev`

	WindowsCollectTemplate = `$os=Get-CimInstance Win32_OperatingSystem; $cpu=[int](Get-CimInstance Win32_Processor|Measure-Object LoadPercentage -Average).Average; $mem=[math]::Round(100*($os.TotalVisibleMemorySize-$os.FreePhysicalMemory)/$os.TotalVisibleMemorySize,1); $d=Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3'|Sort-Object -Property @{Expression={$_.FreeSpace/($_.Size+1)}}|Select-Object -First 1; $disk=[math]::Round(100*(1-$d.FreeSpace/$d.Size),1); $p=Get-CimInstance Win32_PerfFormattedData_PerfOS_System; $n=Get-CimInstance Win32_PerfFormattedData_Tcpip_NetworkInterface|Measure-Object -Property BytesReceivedPersec,BytesSentPersec -Sum; [pscustomobject]@{cpu_pct=$cpu;mem_pct=$mem;disk_pct=$disk;load_1=[double]$p.ProcessorQueueLength;net_in_bps=[int64]$n.Sum[0];net_out_bps=[int64]$n.Sum[1]}|ConvertTo-Json -Compress`
)

const (
	// collectorFailureThreshold 是连续失败暂停阈值（任务写死：5 次）。
	collectorFailureThreshold = 5
	// collectorPauseAfterFailures 是暂停时长：暂停期该台不采集，期满自动
	// 重试（有界退避，不会无限静默死亡）。
	collectorPauseAfterFailures = 10 * time.Minute
	// collectConcurrency 是单轮采集的并发上限（goroutine 池有界——铁律）。
	collectConcurrency = 8
	// collectOutputLimit 是单台采集输出字节上限（通道层 256KB 之外进程内
	// 的第二道解析器输入限制）。
	collectOutputLimit = 262144
	// suspendRuleName 是「采集不可达」系统规则名（enabled=false，不参与
	// 评估；仅承载暂停/恢复告警事件——alert_events.rule_id 有 FK）。
	suspendRuleName = "monitor.collector.unreachable"
	// suspendRuleMetric / suspendRuleThreshold 是系统哨兵规则的指标与阈值。
	//
	// 六期打磨 sprint 2（走查 high）：此前用 metric=cpu_pct + threshold=101，
	// 被迁移 00007 的 CHECK（cpu/mem/disk 的 threshold<=100）必然拒绝，映射
	// 为 store.ErrInvalid——「采集不可达」告警永远创建失败，服务器连续采集
	// 失败对用户完全静默（无事件、无通知、无自动诊断）。
	//
	// 该规则 enabled=false、评估器只遍历 enabled 规则，指标/阈值不参与任何
	// 判断，只用于满足 alert_rules 的 NOT NULL 列。取 load_1（不受百分比
	// CHECK 约束）并设一个现实中不可达的阈值，语义即「永不命中」。
	suspendRuleMetric    = store.AlertMetricLoad1
	suspendRuleThreshold = 1e12
)

// SettingKeyCollector 是采集器配置的 settings 键（契约 4.11 五期追加键）。
const SettingKeyCollector = "monitor.collector"

// CollectorSetting 是 settings["monitor.collector"] 的 JSON 结构。
type CollectorSetting struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
}

// DefaultCollectorSetting 返回缺省 {enabled:true, interval_seconds:60}。
func DefaultCollectorSetting() CollectorSetting {
	return CollectorSetting{Enabled: true, IntervalSeconds: 60}
}

// LoadCollectorSetting 读取并校验 settings（interval 合法 30..3600；越界或
// 配置损坏回落默认值——采集器不能因坏配置停摆）。
func LoadCollectorSetting(ctx context.Context, st store.Store) CollectorSetting {
	def := DefaultCollectorSetting()
	setting, err := st.Settings().Get(ctx, SettingKeyCollector)
	if err != nil {
		return def
	}
	var cs CollectorSetting
	if err := json.Unmarshal(setting.Value, &cs); err != nil {
		return def
	}
	if cs.IntervalSeconds < 30 || cs.IntervalSeconds > 3600 {
		cs.IntervalSeconds = def.IntervalSeconds
	}
	return cs
}

// ---------------------------------------------------------------------------
// 环形缓冲（契约 11 节 #1：每台 1440 点定长数组，写满覆盖最旧；map 键
// 上限 1024，超出该台不缓存、评估时回退查库）
// ---------------------------------------------------------------------------

// ringBuffer 是单台定长环形缓冲。points 为定长数组（禁止 append 切片）。
type ringBuffer struct {
	points [BufferPoints]Sample
	stamps [BufferPoints]time.Time
	head   int // 下一个写入位置
	count  int // 已写入点数（≤BufferPoints）
}

// push 写入一个点，写满覆盖最旧。
func (r *ringBuffer) push(s Sample, at time.Time) {
	r.points[r.head] = s
	r.stamps[r.head] = at
	r.head = (r.head + 1) % BufferPoints
	if r.count < BufferPoints {
		r.count++
	}
}

// lastN 返回最近 n 个点（时间升序）；n 超出存量取全部。
func (r *ringBuffer) lastN(n int) ([]Sample, []time.Time) {
	if n > r.count {
		n = r.count
	}
	if n <= 0 {
		return nil, nil
	}
	samples := make([]Sample, n)
	stamps := make([]time.Time, n)
	start := (r.head - n + BufferPoints) % BufferPoints
	for i := 0; i < n; i++ {
		idx := (start + i) % BufferPoints
		samples[i] = r.points[idx]
		stamps[i] = r.stamps[idx]
	}
	return samples, stamps
}

// metricBuffer 是全部服务器的环形缓冲 map（键上限 MaxBufferedServers）。
type metricBuffer struct {
	mu    sync.Mutex
	rings map[string]*ringBuffer
}

func newMetricBuffer() *metricBuffer {
	return &metricBuffer{rings: make(map[string]*ringBuffer, 16)}
}

// push 写入一个点；map 超过 MaxBufferedServers 时丢弃该台（不缓存，
// 评估回退查库——契约 11 节 #1）。
func (b *metricBuffer) push(serverID string, s Sample, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ring, ok := b.rings[serverID]
	if !ok {
		if len(b.rings) >= MaxBufferedServers {
			return
		}
		ring = &ringBuffer{}
		b.rings[serverID] = ring
	}
	ring.push(s, at)
}

// lastN 读最近 n 点与对应时间戳（时间戳供评估侧新鲜度校验）；未命中
// 返回 ok=false（调用方回退查库）。
func (b *metricBuffer) lastN(serverID string, n int) ([]Sample, []time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ring, ok := b.rings[serverID]
	if !ok {
		return nil, nil, false
	}
	samples, stamps := ring.lastN(n)
	return samples, stamps, true
}

// size 返回当前缓存的服务器数。
func (b *metricBuffer) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rings)
}

// ---------------------------------------------------------------------------
// 采集器
// ---------------------------------------------------------------------------

// collectorState 是单台采集状态（连续失败计数、暂停截止、上轮原始采样）。
type collectorState struct {
	consecutiveFails int
	pausedUntil      time.Time
	lastLinux        *LinuxSample
	alertFired       bool // 暂停告警事件是否已写（未恢复）
}

// collector 是采集循环体。
type collector struct {
	cfg    CollectorConfig
	st     store.Store
	ssh    sshx.Executor
	winrm  winrmx.Executor
	cipher *cryptox.Cipher
	log    *slog.Logger
	clock  func() time.Time
	audit  auditSink

	buf *metricBuffer

	mu     sync.Mutex
	states map[string]*collectorState // 键=server id；上限 MaxTargets

	suspendRuleID string // 懒创建的系统规则 id 缓存
}

// ExecFunc 是单台执行抽象（测试注入 fake；生产按 2.7 路由选通道）。
type ExecFunc func(ctx context.Context, command string) (output string, err error)

// errPaused 标记暂停中的目标（不计入失败）。
var errPaused = errors.New("collector: target paused after consecutive failures")

// collectOne 对单台执行采集并解析。
func collectOne(ctx context.Context, srv store.Server, exec ExecFunc,
	prev *LinuxSample, interval float64) (Sample, *LinuxSample, error) {
	switch srv.OS {
	case store.ServerOSWindows:
		out, err := exec(ctx, WindowsCollectTemplate)
		if err != nil {
			return Sample{}, nil, err
		}
		if len(out) > collectOutputLimit {
			out = out[:collectOutputLimit]
		}
		return ParseWindowsOutput(out), nil, nil
	default: // linux
		out, err := exec(ctx, LinuxCollectTemplate)
		if err != nil {
			return Sample{}, nil, err
		}
		if len(out) > collectOutputLimit {
			out = out[:collectOutputLimit]
		}
		s, cur := ParseLinuxOutput(out, prev, interval)
		return s, &cur, nil
	}
}

// runRound 跑一轮采集：目标筛选（device_kind=server，上限 MaxTargets）→
// 有界并发执行 → 解析 → 环形缓冲 → 分批写库。返回 (成功数, 失败数)。
// 单台失败跳过不中断（契约 5.15）。
func (c *collector) runRound(ctx context.Context) (okCount, failCount int) {
	servers, err := c.st.Servers().List(ctx)
	if err != nil {
		c.log.Warn("monitor_collect_list_servers_failed", "error", err.Error())
		return 0, 0
	}
	targets := make([]store.Server, 0, len(servers))
	for _, s := range servers {
		if s.DeviceKind != store.DeviceKindServer {
			continue // network_device 不采集（契约 2.10 约定 1）
		}
		if len(targets) >= MaxTargets {
			c.log.Warn("monitor_collect_targets_capped", "max", MaxTargets)
			break // 终止条件（契约 11 节 #2）
		}
		targets = append(targets, s)
	}
	if len(targets) == 0 {
		return 0, 0
	}

	started := c.clock()
	type result struct {
		srv   store.Server
		s     Sample
		linux *LinuxSample
		err   error
	}
	results := make([]result, len(targets))

	sem := make(chan struct{}, collectConcurrency)
	var wg sync.WaitGroup
	for i := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, srv store.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			state := c.state(srv.ID)
			c.mu.Lock()
			paused := started.Before(state.pausedUntil)
			prev := state.lastLinux
			c.mu.Unlock()
			if paused {
				results[i] = result{srv: srv, err: errPaused}
				return
			}
			exec := c.execFor(srv)
			if exec == nil {
				results[i] = result{srv: srv, err: errors.New("monitor: no executor for server auth method")}
				return
			}
			execCtx, cancel := context.WithTimeout(ctx, c.cfg.ExecTimeout)
			defer cancel()
			s, linux, err := collectOne(execCtx, srv, exec, prev, c.cfg.Interval.Seconds())
			results[i] = result{srv: srv, s: s, linux: linux, err: err}
		}(i, targets[i])
	}
	wg.Wait()

	items := make([]store.InsertMetricParams, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			if !errors.Is(r.err, errPaused) {
				failCount++
				c.recordFailure(ctx, r.srv, r.err)
			}
			continue
		}
		okCount++
		at := c.clock()
		c.buf.push(r.srv.ID, r.s, at)
		c.recordSuccess(r.srv, r.linux, at)
		items = append(items, store.InsertMetricParams{
			ServerID: r.srv.ID, CreatedAt: at,
			CPU: r.s.CPU, Mem: r.s.Mem, Disk: r.s.Disk, Load1: r.s.Load1,
			NetIn: r.s.NetIn, NetOut: r.s.NetOut,
		})
	}

	// 分批写库（≤BatchInsertMax/批——契约 11 节 #3；UPSERT latest 在
	// BatchInsert 内同事务完成）。
	for start := 0; start < len(items); start += c.cfg.BatchInsertMax {
		end := start + c.cfg.BatchInsertMax
		if end > len(items) {
			end = len(items)
		}
		if err := c.st.Metrics().BatchInsert(ctx, items[start:end]); err != nil {
			c.log.Warn("monitor_collect_batch_insert_failed", "error", err.Error(), "rows", end-start)
		}
	}

	c.audit.Enqueue(ctx, auditEvent{
		ActorType: "system", Action: "metric.collect", ResourceType: "monitor",
		Detail: map[string]any{
			"servers": len(targets), "ok": okCount, "failed": failCount,
			"duration_ms": time.Since(started).Milliseconds(),
		},
	})
	return okCount, failCount
}

func (c *collector) state(serverID string) *collectorState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.states[serverID]
	if !ok {
		s = &collectorState{}
		if len(c.states) < MaxTargets { // 状态 map 有界
			c.states[serverID] = s
		}
	}
	return s
}

// recordSuccess 清失败计数、缓存原始采样，并 resolve 暂停告警事件。
func (c *collector) recordSuccess(srv store.Server, linux *LinuxSample, at time.Time) {
	c.mu.Lock()
	state, ok := c.states[srv.ID]
	var fired bool
	if ok {
		state.consecutiveFails = 0
		state.lastLinux = linux
		fired = state.alertFired
		state.alertFired = false
	}
	c.mu.Unlock()
	if fired {
		c.resolveSuspendAlert(context.Background(), srv.ID)
	}
}

// recordFailure 累计失败；达到阈值暂停该台并写系统告警事件。
func (c *collector) recordFailure(ctx context.Context, srv store.Server, err error) {
	c.mu.Lock()
	state, ok := c.states[srv.ID]
	if !ok && len(c.states) < MaxTargets {
		state = &collectorState{}
		c.states[srv.ID] = state
		ok = true
	}
	var fails int
	var needAlert bool
	if ok {
		state.consecutiveFails++
		fails = state.consecutiveFails
		if fails >= collectorFailureThreshold {
			state.pausedUntil = c.clock().Add(collectorPauseAfterFailures)
			state.consecutiveFails = 0
			if !state.alertFired {
				state.alertFired = true
				needAlert = true
			}
		}
	}
	c.mu.Unlock()

	if !ok {
		return
	}
	c.log.Warn("monitor_collect_failed", "server_id", srv.ID, "server", srv.Name,
		"consecutive", fails, "error", err.Error())
	if needAlert {
		c.fireSuspendAlert(ctx, srv)
	}
}

// fireSuspendAlert 写采集不可达的系统告警事件（自动创建并复用唯一一条
// enabled=false 的系统规则承载事件——alert_events.rule_id 有 FK）。
func (c *collector) fireSuspendAlert(ctx context.Context, srv store.Server) {
	rule, err := c.suspendRule(ctx)
	if err != nil {
		c.log.Warn("monitor_suspend_rule_failed", "error", err.Error())
		return
	}
	// 消息用中文（六期打磨 sprint 4 走查 low）：这是全站唯一一条直出英文
	// 业务文案的告警——告警页事件行「消息」列原样显示英文，而 UI 其余部分
	// 全中文。规则名 suspendRuleName 是数据库键（告警规则唯一名 + 前端按名
	// 识别），保持英文原样，中文展示名由前端映射。
	msg := fmt.Sprintf("连续 %d 次指标采集失败，采集已暂停（%d 分钟后自动重试）",
		collectorFailureThreshold, int(collectorPauseAfterFailures/time.Minute))
	ev, err := c.st.AlertEvents().Create(ctx, rule, srv.ID, 0, msg, c.clock())
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			c.log.Warn("monitor_suspend_event_failed", "error", err.Error())
		}
		return
	}
	c.writeAlertAudit(ctx, "alert.triggered", ev)
}

// suspendRule 取哨兵规则行；缓存 ID 失效（规则被用户从 /alerts 删除）时
// 清缓存重建一次——否则失联告警会永久静默（六期打磨 sprint 2：这条路径
// 此前根本走不到，因为规则本身建不出来）。重试上限固定 1 次。
func (c *collector) suspendRule(ctx context.Context) (store.AlertRule, error) {
	ruleID, err := c.ensureSuspendRule(ctx)
	if err != nil {
		return store.AlertRule{}, err
	}
	rule, err := c.st.AlertRules().Get(ctx, ruleID)
	if err == nil {
		return rule, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.AlertRule{}, err
	}
	c.mu.Lock()
	c.suspendRuleID = ""
	c.mu.Unlock()
	ruleID, err = c.ensureSuspendRule(ctx)
	if err != nil {
		return store.AlertRule{}, err
	}
	return c.st.AlertRules().Get(ctx, ruleID)
}

// resolveSuspendAlert 恢复采集后 resolve 未决的暂停告警事件。
func (c *collector) resolveSuspendAlert(ctx context.Context, serverID string) {
	ruleID, err := c.ensureSuspendRule(ctx)
	if err != nil {
		return
	}
	items, _, err := c.st.AlertEvents().List(ctx, store.AlertEventFilter{
		Status: store.AlertEventFiring, ServerID: serverID, Limit: 50,
	})
	if err != nil {
		return
	}
	for _, ev := range items {
		if ev.RuleID != ruleID {
			continue
		}
		if resolved, rerr := c.st.AlertEvents().Resolve(ctx, ev.ID, c.clock()); rerr == nil {
			c.writeAlertAudit(ctx, "alert.resolved", resolved)
		}
	}
}

func (c *collector) ensureSuspendRule(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.suspendRuleID
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	byName := func() (string, bool) {
		rules, err := c.st.AlertRules().List(ctx)
		if err != nil {
			return "", false
		}
		for _, r := range rules {
			if r.Name == suspendRuleName {
				return r.ID, true
			}
		}
		return "", false
	}
	if id, ok := byName(); ok {
		c.mu.Lock()
		c.suspendRuleID = id
		c.mu.Unlock()
		return id, nil
	}
	rule, err := c.st.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: suspendRuleName, Metric: suspendRuleMetric,
		Comparator: store.AlertComparatorGT, Threshold: suspendRuleThreshold, // 现实中不可达：评估永不命中
		DurationWindows: 1, CooldownSeconds: 60, Severity: store.AlertSeverityCritical,
		Enabled: false,
	})
	if err == nil {
		c.mu.Lock()
		c.suspendRuleID = rule.ID
		c.mu.Unlock()
		return rule.ID, nil
	}
	if !errors.Is(err, store.ErrConflict) {
		return "", err
	}
	// 并发创建撞唯一名：重读一次。
	if id, ok := byName(); ok {
		c.mu.Lock()
		c.suspendRuleID = id
		c.mu.Unlock()
		return id, nil
	}
	return "", store.ErrNotFound
}

// execFor 按契约 2.7 路由矩阵构造单台执行函数；凭据解密失败返回 nil。
func (c *collector) execFor(srv store.Server) ExecFunc {
	if srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic {
		if c.winrm == nil {
			return nil
		}
		target, err := c.winrmTarget(srv)
		if err != nil {
			return nil
		}
		return func(ctx context.Context, command string) (string, error) {
			out, err := c.winrm.Execute(ctx, target, winrmx.RunRequest{
				Command: command, Timeout: c.cfg.ExecTimeout, MaxOutputBytes: collectOutputLimit,
			})
			if err != nil {
				return "", err
			}
			if out.ExitCode != 0 {
				return "", fmt.Errorf("collect command exit %d", out.ExitCode)
			}
			return out.Combined, nil
		}
	}
	if c.ssh == nil {
		return nil
	}
	target, err := c.sshTarget(srv)
	if err != nil {
		return nil
	}
	isWinSSH := srv.OS == store.ServerOSWindows
	return func(ctx context.Context, command string) (string, error) {
		execCommand := command
		if isWinSSH {
			execCommand = "powershell -NoProfile -NonInteractive -Command " + command
		}
		out, err := c.ssh.Execute(ctx, target, sshx.RunRequest{
			Command: execCommand, Timeout: c.cfg.ExecTimeout, MaxOutputBytes: collectOutputLimit,
		})
		if err != nil {
			return "", err
		}
		if out.ExitCode != 0 {
			return "", fmt.Errorf("collect command exit %d", out.ExitCode)
		}
		return out.Combined, nil
	}
}

// winrmTarget / sshTarget 解密凭据构造连接目标（错误信息不含凭据）。
func (c *collector) winrmTarget(srv store.Server) (winrmx.Target, error) {
	if srv.AuthMethod != store.AuthMethodPasswordNTLM && srv.AuthMethod != store.AuthMethodPasswordBasic {
		return winrmx.Target{}, errors.New("monitor: not a winrm auth method")
	}
	pw, err := decrypt(c.cipher, srv.PasswordEncrypted)
	if err != nil {
		return winrmx.Target{}, errors.New("monitor: decrypt password failed")
	}
	return winrmx.Target{
		ServerID: srv.ID, Host: srv.Host, Port: srv.WinRMPort, Username: srv.Username,
		Password: pw, UseHTTPS: srv.AuthMethod == store.AuthMethodPasswordBasic && srv.WinRMPort == 5986,
		AuthType: srv.AuthMethod,
	}, nil
}

func (c *collector) sshTarget(srv store.Server) (sshx.Target, error) {
	target := sshx.Target{
		ServerID: srv.ID, Host: srv.Host, Port: srv.Port, Username: srv.Username,
		AuthMethod: srv.AuthMethod, KnownHostKey: srv.HostKeyPublic,
	}
	switch srv.AuthMethod {
	case store.AuthMethodPassword:
		pw, err := decrypt(c.cipher, srv.PasswordEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("monitor: decrypt password failed")
		}
		target.Password = pw
	case store.AuthMethodPrivateKey:
		key, err := decrypt(c.cipher, srv.PrivateKeyEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("monitor: decrypt private key failed")
		}
		passphrase, err := decrypt(c.cipher, srv.PassphraseEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("monitor: decrypt passphrase failed")
		}
		target.PrivateKey = key
		target.Passphrase = passphrase
	default:
		return sshx.Target{}, errors.New("monitor: unsupported auth method")
	}
	return target, nil
}

// decrypt 带 nil 防护的解密：cipher 未注入时返回错误而非 panic
// （五期评审：agent/batchx 的同名辅助有防护，monitor/diagnosis/inspection
// 曾直接调用，nil 接收者会在采集/诊断/巡检 goroutine 内 panic 杀死进程）。
func decrypt(cipher *cryptox.Cipher, ciphertext string) (string, error) {
	// 空串短路（六期打磨 sprint 4，走查 high）：passphrase 是私钥认证的
	// 可选字段（openapi 4.3：私钥认证必须提供 private_key，passphrase
	// 可选），未填时库里存空串。cryptox.Decrypt("") 返回
	// ErrInvalidCiphertext，此前把「没有口令」误判成解密失败，导致
	// 私钥认证服务器的采集/诊断/巡检三路全灭。agent/batchx 的同名辅助
	// 一直有该短路，此处对齐。
	if ciphertext == "" {
		return "", nil
	}
	if cipher == nil {
		return "", errors.New("monitor: cipher unavailable")
	}
	return cipher.Decrypt(ciphertext)
}
