// manager.go 实现五期 M1 的 monitor.Manager（契约 5.15）：生命周期
// Start/Stop、采集循环（间隔由 settings["monitor.collector"] 控制，每轮
// 重新读取——变更下一轮生效）、评估循环（tick 默认 30s + 采集轮后追加）、
// 通知 worker、30 天保留每日清理。单实例（同 audit.Writer 约定）。
package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// 契约 11 节写死的固定上限（不进用户配置；改动先改契约）。
const (
	// MaxBufferedServers 固定 1024：环形缓冲 map 键上限。
	MaxBufferedServers = 1024
	// BufferPoints 固定 1440：每台环形缓冲点数（定长数组）。
	BufferPoints = 1440
	// BatchInsertMax 固定 500：单批写库行数上限。
	BatchInsertMax = 500
	// MaxTargets 固定 500：单轮采集目标上限。
	MaxTargets = 500
	// defaultExecTimeout 固定 30s：单台采集命令超时。
	defaultExecTimeout = 30 * time.Second
	// defaultEvalInterval 是评估 tick 缺省 30s（任务写死；采集轮后追加评估）。
	defaultEvalInterval = 30 * time.Second
	// retentionDays 是指标保留天数（契约 11 节 #13：30 天）。
	retentionDays = 30
	// stopBudget 是 Stop 同步收尾预算（契约：≤5s）。
	stopBudget = 5 * time.Second
)

// CollectorConfig 是采集器配置（契约 5.15）。
type CollectorConfig struct {
	Enabled            bool          // settings["monitor.collector"].enabled
	Interval           time.Duration // 30s..3600s，缺省 60s
	MaxBufferedServers int           // 固定 1024：环形缓冲 map 键上限（11 节）
	BufferPoints       int           // 固定 1440：每台环形缓冲点数（定长数组）
	BatchInsertMax     int           // 固定 500：单批写库行数上限
	ExecTimeout        time.Duration // 固定 30s：单台采集命令超时
	MaxTargets         int           // 固定 500：单轮采集目标上限
}

// NotifyConfig 是通知投递配置（契约 5.15）。
type NotifyConfig struct {
	QueueSize   int           // 固定 256：投递队列容量；满则丢弃并置 failed
	Workers     int           // 固定 2
	Retries     int           // 固定 3（退避 1s/5s/15s）
	HTTPTimeout time.Duration // 固定 10s
	SMTPTimeout time.Duration // 固定 15s
}

// NormalizeCollectorConfig 应用契约固定值与合法范围。
func NormalizeCollectorConfig(cfg CollectorConfig) CollectorConfig {
	if cfg.Interval < 30*time.Second {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Interval > 3600*time.Second {
		cfg.Interval = 3600 * time.Second
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = defaultExecTimeout
	}
	// 11 节固定值：任何输入都钳到写死常量。
	cfg.MaxBufferedServers = MaxBufferedServers
	cfg.BufferPoints = BufferPoints
	cfg.BatchInsertMax = BatchInsertMax
	cfg.MaxTargets = MaxTargets
	return cfg
}

// NotifyResult 是渠道测试/告警投递的统一结果（契约 5.15）。
type NotifyResult struct {
	Kind       string `json:"kind"`
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code"`
	DurationMS int64  `json:"duration_ms"`
}

// auditSink 抽象 audit.Writer（测试注入 fake；生产传 *audit.Writer）。
type auditSink interface {
	Enqueue(ctx context.Context, e audit.Event) error
}

// auditEvent 与 audit.Event 同形（collector.go 内部使用）。
type auditEvent = audit.Event

// Manager 是监控告警模块入口（单实例）。
type Manager struct {
	cfg    CollectorConfig
	ncfg   NotifyConfig
	st     store.Store
	ssh    sshx.Executor
	winrm  winrmx.Executor
	cipher *cryptox.Cipher
	log    *slog.Logger
	clock  func() time.Time
	audit  auditSink

	collector *collector
	evaluator *evaluator
	notifier  *notifier

	loopCtx    context.Context
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
	startOnce  sync.Once
	stopOnce   sync.Once
	stopped    chan struct{}
}

// NewManager 构造 Manager（契约 5.15 签名）。clock 为 nil 用 time.Now；
// log 为 nil 用 slog.Default()。
func NewManager(cfg CollectorConfig, nc NotifyConfig, st store.Store,
	ssh sshx.Executor, winrm winrmx.Executor, cipher *cryptox.Cipher,
	auditWriter *audit.Writer, clock func() time.Time, log *slog.Logger) *Manager {

	cfg = NormalizeCollectorConfig(cfg)
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	var sink auditSink
	if auditWriter != nil {
		sink = auditWriter
	} else {
		sink = noopAuditSink{}
	}

	buf := newMetricBuffer()
	notifier := newNotifier(nc, st, cipher, log, clock, sink)
	c := &collector{
		cfg: cfg, st: st, ssh: ssh, winrm: winrm, cipher: cipher,
		log: log, clock: clock, audit: sink,
		buf: buf, states: make(map[string]*collectorState, 16),
	}
	e := &evaluator{
		st: st, buf: buf, log: log, clock: clock, audit: sink,
		notify: notifier, interval: cfg.Interval,
	}

	return &Manager{
		cfg: cfg, ncfg: notifier.cfg, st: st, ssh: ssh, winrm: winrm,
		cipher: cipher, log: log, clock: clock, audit: sink,
		collector: c, evaluator: e, notifier: notifier,
		stopped: make(chan struct{}),
	}
}

// SetDiagnosisStarter 注入自动诊断回调（main 组装 M2 后调用；契约 5.15：
// monitor 不 import diagnosis，避免包依赖反转）。
func (m *Manager) SetDiagnosisStarter(fn func(ctx context.Context, ev store.AlertEvent)) {
	m.evaluator.startDiagnosis = fn
}

// Start 启动采集循环 + 评估循环 + 通知 worker + 保留清理（幂等）。
func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		m.loopCtx, m.loopCancel = context.WithCancel(ctx)
		m.notifier.start(m.loopCtx)

		m.wg.Add(3)
		go m.collectLoop()
		go m.evaluateLoop()
		go m.retentionLoop()
		m.log.Info("monitor_started",
			"interval", m.cfg.Interval.String(), "enabled", m.cfg.Enabled)
	})
}

// Stop 同步收尾 ≤5s；进行中的采集轮不强杀（ctx 取消后自然返回）。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		if m.loopCancel != nil {
			m.loopCancel()
		}
		m.notifier.stop(stopBudget)
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(stopBudget):
			m.log.Warn("monitor_stop_timeout")
		}
		close(m.stopped)
		m.log.Info("monitor_stopped")
	})
}

// Stopped 返回停机完成信号（测试用）。
func (m *Manager) Stopped() <-chan struct{} { return m.stopped }

// EvaluateNow 跑一轮评估并返回触发数（契约 5.15：测试与即时场景）。
func (m *Manager) EvaluateNow(ctx context.Context) (int, error) {
	return m.evaluator.evaluateOnce(ctx)
}

// TestChannel 渠道测试投递（解密配置 → 发送测试消息 → 结果；不写
// alert_events，审计动作 notification.test 由 HTTP 层写）。
func (m *Manager) TestChannel(ctx context.Context, ch store.NotificationChannel) (NotifyResult, error) {
	started := m.clock()
	cfg, err := m.notifier.decodeConfig(ch)
	res := NotifyResult{Kind: string(ch.Kind)}
	if err != nil {
		res.ErrorCode = "NOTIFICATION_CONFIG_INVALID"
		res.DurationMS = m.clock().Sub(started).Milliseconds()
		return res, ErrConfigInvalid
	}
	task := notifyTask{
		Event: store.AlertEvent{
			Metric: store.AlertMetricCPU, Severity: store.AlertSeverityInfo,
			Value: 0, Message: "OpsMind 渠道测试消息（channel test）",
			FiredAt: m.clock(),
		},
		RuleName:   "test",
		ServerName: "opsmind",
	}
	if err := m.notifier.send(ctx, ch.Kind, cfg, task); err != nil {
		res.ErrorCode = "NOTIFICATION_TEST_FAILED"
		res.DurationMS = m.clock().Sub(started).Milliseconds()
		return res, err
	}
	res.OK = true
	res.DurationMS = m.clock().Sub(started).Milliseconds()
	return res, nil
}

// CollectorSetting 返回当前生效的采集配置（handler 组装响应用）。
func (m *Manager) CollectorSetting(ctx context.Context) CollectorSetting {
	return LoadCollectorSetting(ctx, m.st)
}

// collectLoop 采集循环：每轮重读 settings（变更下一轮生效，不重启）。
func (m *Manager) collectLoop() {
	defer m.wg.Done()
	for {
		setting := LoadCollectorSetting(m.loopCtx, m.st)
		if !setting.Enabled || !m.cfg.Enabled {
			// 采集停用：休眠一个缺省间隔再查（终止条件=ctx 取消）。
			select {
			case <-m.loopCtx.Done():
				return
			case <-time.After(m.cfg.Interval):
			}
			continue
		}
		m.collector.cfg.Interval = time.Duration(setting.IntervalSeconds) * time.Second
		// e.interval 由评估 goroutine 在 e.mu 下读取（collectInterval），
		// 写侧同样持锁，避免与 evaluateLoop tick 的数据竞争。
		m.evaluator.mu.Lock()
		m.evaluator.interval = m.collector.cfg.Interval
		m.evaluator.mu.Unlock()
		m.collector.runRound(m.loopCtx)
		// 紧随每轮采集后跑一轮评估（契约 5.15：先采集后评估）。
		if _, err := m.evaluator.evaluateOnce(m.loopCtx); err != nil {
			m.log.Warn("monitor_evaluate_failed", "error", err.Error())
		}
		select {
		case <-m.loopCtx.Done():
			return
		case <-time.After(m.collector.cfg.Interval):
		}
	}
}

// evaluateLoop 评估 tick（默认 30s；采集间隔更短时与采集轮后评估互补，
// evaluateOnce 有互斥锁不会并发重入）。
func (m *Manager) evaluateLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(defaultEvalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.loopCtx.Done():
			return
		case <-ticker.C:
			if _, err := m.evaluator.evaluateOnce(m.loopCtx); err != nil {
				m.log.Warn("monitor_evaluate_failed", "error", err.Error())
			}
		}
	}
}

// retentionLoop 30 天保留清理：每日一次 DeleteBefore（契约 11 节 #13）。
func (m *Manager) retentionLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	// 启动后先等一个周期再清理（避免启动风暴与迁移竞争）。
	for {
		select {
		case <-m.loopCtx.Done():
			return
		case <-ticker.C:
			cutoff := m.clock().Add(-retentionDays * 24 * time.Hour)
			n, err := m.st.Metrics().DeleteBefore(m.loopCtx, cutoff)
			if err != nil {
				m.log.Warn("monitor_retention_failed", "error", err.Error())
				continue
			}
			if n > 0 {
				m.log.Info("monitor_retention_deleted", "rows", n)
			}
		}
	}
}

// writeAlertAudit 写告警触发/恢复审计行（契约 9.7）。
func (e *evaluator) writeAlertAudit(ctx context.Context, action string, ev store.AlertEvent) {
	e.audit.Enqueue(ctx, audit.Event{
		ActorType: "system", Action: action, ResourceType: "alert_event",
		ResourceID: func() string {
			if ev.RuleID != "" {
				return ev.RuleID
			}
			return ""
		}(),
		ServerID: &ev.ServerID,
		Detail: map[string]any{
			"rule_id": ev.RuleID, "rule_name": ev.RuleName, "server_id": ev.ServerID,
			"metric": string(ev.Metric), "value": ev.Value, "message": ev.Message,
			"event_id": ev.ID,
		},
	})
}

// writeAlertAudit 是 collector 侧同款（system actor）。
func (c *collector) writeAlertAudit(ctx context.Context, action string, ev store.AlertEvent) {
	c.audit.Enqueue(ctx, audit.Event{
		ActorType: "system", Action: action, ResourceType: "alert_event",
		ServerID: &ev.ServerID,
		Detail: map[string]any{
			"rule_id": ev.RuleID, "rule_name": ev.RuleName, "server_id": ev.ServerID,
			"metric": string(ev.Metric), "value": ev.Value, "message": ev.Message,
			"event_id": ev.ID,
		},
	})
}

// alertNotifiedAudit 构造 alert.notified 审计（契约 9.7：detail 不含明文配置）。
func alertNotifiedAudit(eventID int64, ch store.NotificationChannel, ok bool, errorCode string) audit.Event {
	detail := map[string]any{
		"event_id": eventID, "channel_id": ch.ID, "kind": string(ch.Kind), "ok": ok,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	return audit.Event{
		ActorType: "system", Action: "alert.notified", ResourceType: "alert_event",
		Detail: detail,
	}
}

// noopAuditSink 在未装配审计写入器时丢弃事件（测试/最小部署）。
type noopAuditSink struct{}

func (noopAuditSink) Enqueue(context.Context, audit.Event) error { return nil }
