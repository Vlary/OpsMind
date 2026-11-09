// engine.go 实现五期 M2 的诊断引擎（契约 5.16 / 11 节 #6-#8）：
// worker 池（=MaxConcurrent=2）从队列取 pending → CAS 置 diagnosing →
// 装配上下文 → mini-loop（loop.go）→ 报告固化 → awaiting_fix（FixTTL 后
// 由过期扫描置 expired）。pending 队列上限 QueueSize=100，满时 Create
// 返回 ErrBusy（HTTP 429 DIAGNOSIS_BUSY）。停机时进行中的会话置
// failed(SERVER_SHUTTING_DOWN)。
package diagnosis

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/llm"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// EngineConfig 是引擎配置（契约 5.16；「固定」值写死于 NormalizeEngineConfig）。
type EngineConfig struct {
	MaxConcurrent  int           // 固定 2：全局并发诊断会话上限（11 节）
	QueueSize      int           // 固定 100：pending 等待队列上限；满时创建返回 ErrBusy
	SessionTimeout time.Duration // 固定 300s：单会话 diagnosing 硬超时
	MaxModelRounds int           // 固定 6：模型循环轮数上限
	CommandTimeout time.Duration // 固定 30s：单条采集命令
	MaxOutputBytes int64         // 固定 262144：单命令输出上限（通道层）
	ReportMaxBytes int64         // 固定 65536：report jsonb 序列化上限
	FixTTL         time.Duration // 固定 30m：awaiting_fix 过期窗口
}

// 契约 11 节写死的固定值。
const (
	maxConcurrent  = 2
	queueSize      = 100
	sessionTimeout = 300 * time.Second
	fixTTL         = 30 * time.Minute
	expireScanTick = 30 * time.Second
	stopBudget     = 5 * time.Second
)

// DefaultEngineConfig 返回全部固定值的配置（11 节）。
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		MaxConcurrent:  maxConcurrent,
		QueueSize:      queueSize,
		SessionTimeout: sessionTimeout,
		MaxModelRounds: MaxModelRounds,
		CommandTimeout: CommandTimeout,
		MaxOutputBytes: MaxOutputBytes,
		ReportMaxBytes: ReportMaxBytes,
		FixTTL:         fixTTL,
	}
}

// ErrBusy 表示 pending 队列已满（HTTP 429 DIAGNOSIS_BUSY）。
var ErrBusy = errors.New("diagnosis: queue limit reached")

// auditSink 抽象 audit.Writer（测试注入 fake；生产传 *audit.Writer）。
type auditSink interface {
	Enqueue(ctx context.Context, e audit.Event) error
}

type noopAuditSink struct{}

func (noopAuditSink) Enqueue(context.Context, audit.Event) error { return nil }

// Engine 是诊断引擎（单实例，同 audit.Writer 约定）。
type Engine struct {
	cfg      EngineConfig
	st       store.Store
	registry *llm.Registry
	ssh      sshx.Executor
	winrm    winrmx.Executor
	cipher   *cryptox.Cipher
	audit    auditSink
	clock    func() time.Time
	log      *slog.Logger

	queue chan string // pending 会话 id（容量 QueueSize；写满 Create 报 ErrBusy）

	loopCtx    context.Context
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
	startOnce  sync.Once
	stopOnce   sync.Once
	stopped    chan struct{}

	// client 非 nil 时绕过 registry，仅测试注入。
	client llm.Client
}

// NewEngine 构造引擎（契约 5.16 签名）。clock 为 nil 用 time.Now；
// auditWriter 为 nil 丢弃审计；log 为 nil 用 slog.Default()。
func NewEngine(cfg EngineConfig, st store.Store, registry *llm.Registry,
	ssh sshx.Executor, winrm winrmx.Executor, cipher *cryptox.Cipher,
	auditWriter *audit.Writer, clock func() time.Time, log *slog.Logger) *Engine {

	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = maxConcurrent
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = queueSize
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = sessionTimeout
	}
	if cfg.MaxModelRounds <= 0 {
		cfg.MaxModelRounds = MaxModelRounds
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = CommandTimeout
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = MaxOutputBytes
	}
	if cfg.ReportMaxBytes <= 0 {
		cfg.ReportMaxBytes = ReportMaxBytes
	}
	if cfg.FixTTL <= 0 {
		cfg.FixTTL = fixTTL
	}
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	var sink auditSink = noopAuditSink{}
	if auditWriter != nil {
		sink = auditWriter
	}
	return &Engine{
		cfg: cfg, st: st, registry: registry, ssh: ssh, winrm: winrm,
		cipher: cipher, audit: sink, clock: clock, log: log,
		queue:   make(chan string, cfg.QueueSize),
		stopped: make(chan struct{}),
	}
}

// SetLLMClient 注入假客户端，仅供单元测试使用。
func (e *Engine) SetLLMClient(client llm.Client) { e.client = client }

// Start 启动 worker 池（=MaxConcurrent）与过期扫描（每 30s ExpireStale；
// 契约 5.16）。启动时回填库内遗留的 pending 会话（重启恢复，上限队列容量）。
func (e *Engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		e.loopCtx, e.loopCancel = context.WithCancel(ctx)
		e.backfillPending()
		for i := 0; i < e.cfg.MaxConcurrent; i++ {
			e.wg.Add(1)
			go e.worker()
		}
		e.wg.Add(1)
		go e.expireLoop()
		e.log.Info("diagnosis_engine_started",
			"workers", e.cfg.MaxConcurrent, "queue", e.cfg.QueueSize)
	})
}

// Stop 同步收尾 ≤5s（契约停机语义）：取消循环、等待 worker，把仍在
// diagnosing 的会话置 failed(SERVER_SHUTTING_DOWN)。
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		if e.loopCancel != nil {
			e.loopCancel()
		}
		done := make(chan struct{})
		go func() {
			e.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(stopBudget):
			e.log.Warn("diagnosis_stop_timeout")
		}
		e.failDiagnosing("SERVER_SHUTTING_DOWN")
		close(e.stopped)
		e.log.Info("diagnosis_engine_stopped")
	})
}

// Stopped 返回停机完成信号（测试用）。
func (e *Engine) Stopped() <-chan struct{} { return e.stopped }

// Create 创建会话（pending 入队）。trigger=chat 由 agent 工具调用、manual
// 由 REST、alert 由 monitor 回调。返回创建的会话；队列满时 ErrBusy；
// 服务器不存在时 store.ErrNotFound。
func (e *Engine) Create(ctx context.Context, p store.CreateDiagnosisParams) (store.DiagnosisSession, error) {
	if p.ServerID == "" {
		return store.DiagnosisSession{}, store.ErrInvalid
	}
	if _, err := e.st.Servers().Get(ctx, p.ServerID); err != nil {
		return store.DiagnosisSession{}, err
	}
	// 先检查队列水位再建行（11 节 #6：pending 队列上限 QueueSize）。
	if len(e.queue) >= cap(e.queue) {
		return store.DiagnosisSession{}, ErrBusy
	}
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = e.clock().Add(e.cfg.FixTTL)
	}
	sess, err := e.st.DiagnosisSessions().Create(ctx, p)
	if err != nil {
		return store.DiagnosisSession{}, err
	}
	select {
	case e.queue <- sess.ID:
	default:
		// 竞态窗口内队列被填满：把刚建的会话置 failed 并返回 ErrBusy。
		_, _ = e.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
			ID: sess.ID, From: store.DiagnosisPending, To: store.DiagnosisFailed,
			ErrorCode: "DIAGNOSIS_BUSY", Timestamp: e.clock(),
		})
		return store.DiagnosisSession{}, ErrBusy
	}
	return sess, nil
}

// StartDiagnosis 适配 monitor 的自动诊断回调（契约 5.15：monitor 不 import
// diagnosis，main 组装时注入）。critical 告警触发 trigger=alert 会话。
func (e *Engine) StartDiagnosis() func(ctx context.Context, ev store.AlertEvent) {
	return func(ctx context.Context, ev store.AlertEvent) {
		eventID := ev.ID
		p := store.CreateDiagnosisParams{
			ServerID:     ev.ServerID,
			AlertEventID: &eventID,
			Trigger:      store.DiagnosisTriggerAlert,
		}
		if _, err := e.Create(ctx, p); err != nil && !errors.Is(err, ErrBusy) {
			e.log.Warn("diagnosis_auto_create_failed", "server_id", ev.ServerID, "error", err.Error())
		}
	}
}

// backfillPending 把重启前遗留的 pending 会话回填进队列（≤QueueSize——
// 终止条件；更早的由过期语义兜底）。
func (e *Engine) backfillPending() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := e.st.DiagnosisSessions().List(ctx, store.DiagnosisPending, "", e.cfg.QueueSize)
	if err != nil {
		e.log.Warn("diagnosis_backfill_failed", "error", err.Error())
		return
	}
	for _, sess := range sessions {
		select {
		case e.queue <- sess.ID:
		default:
			return
		}
	}
	if len(sessions) > 0 {
		e.log.Info("diagnosis_backfilled", "sessions", len(sessions))
	}
}

// worker 是诊断 worker：从队列取会话处理，ctx 取消即退出（退出路径）。
func (e *Engine) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.loopCtx.Done():
			return
		case id := <-e.queue:
			e.process(id)
		}
	}
}

// process 驱动单个会话的状态机：pending→diagnosing（CAS）→ mini-loop →
// report→awaiting_fix 或 failed。
func (e *Engine) process(id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.loopCtx), e.cfg.SessionTimeout)
	defer cancel()

	sess, err := e.st.DiagnosisSessions().Get(ctx, id)
	if err != nil {
		return
	}
	if sess.Status != store.DiagnosisPending {
		return // 已被并发处理/取消
	}
	sess, err = e.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: id, From: store.DiagnosisPending, To: store.DiagnosisDiagnosing,
		Timestamp: e.clock(),
	})
	if err != nil {
		return // CAS 冲突：另一 worker 已取走
	}
	srv, err := e.st.Servers().Get(ctx, sess.ServerID)
	if err != nil {
		e.finishFailed(ctx, sess, "SERVER_NOT_FOUND")
		return
	}

	actorType, actorID := e.actor(sess)
	e.auditSession(ctx, "diagnosis.started", actorType, actorID, sess,
		map[string]any{"server_id": sess.ServerID, "trigger": string(sess.Trigger)})

	provider, client, err := e.resolveProvider(ctx)
	if err != nil {
		e.finishFailed(ctx, sess, "NO_PROVIDER_CONFIGURED")
		return
	}
	if e.client != nil { // 测试注入
		client = e.client
	}

	report, loopErr := e.runLoop(ctx, client, provider, sess, srv)
	if loopErr != nil {
		code := "LLM_UPSTREAM_ERROR"
		var le *loopError
		if errors.As(loopErr, &le) {
			code = le.code
		}
		e.finishFailed(ctx, sess, code)
		return
	}
	raw, err := report.Serialize()
	if err != nil {
		e.finishFailed(ctx, sess, "DIAGNOSIS_REPORT_INVALID")
		return
	}
	// report 是可见瞬态（供轮询方观察），随即进入 awaiting_fix（契约 5.16）。
	_, err = e.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: id, From: store.DiagnosisDiagnosing, To: store.DiagnosisReport,
		Report: raw, Timestamp: e.clock(),
	})
	if err != nil {
		return
	}
	sess, err = e.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: id, From: store.DiagnosisReport, To: store.DiagnosisAwaitingFix,
		Timestamp: e.clock(),
	})
	if err != nil {
		return
	}

	e.auditSession(ctx, "diagnosis.completed", actorType, actorID, sess, map[string]any{
		"status": string(store.DiagnosisAwaitingFix), "severity": report.Severity,
		"evidence_count": len(report.Evidence), "suggestion_count": len(report.Suggestions),
	})
}

// resolveProvider 用「created_at 最早的供应商 + 其 default_model」（同 chat
// 缺省规则，契约 5.16）。
func (e *Engine) resolveProvider(ctx context.Context) (store.Provider, llm.Client, error) {
	if e.client != nil {
		return store.Provider{}, e.client, nil
	}
	if e.registry == nil {
		return store.Provider{}, nil, errors.New("diagnosis: llm registry not configured")
	}
	providers, err := e.st.Providers().List(ctx)
	if err != nil {
		return store.Provider{}, nil, err
	}
	if len(providers) == 0 {
		return store.Provider{}, nil, errors.New("diagnosis: no provider configured")
	}
	earliest := providers[0]
	for _, p := range providers[1:] {
		if p.CreatedAt.Before(earliest.CreatedAt) {
			earliest = p
		}
	}
	if earliest.DefaultModel == "" {
		return store.Provider{}, nil, errors.New("diagnosis: provider has no default model")
	}
	client, _, err := e.registry.Client(ctx, earliest.ID)
	if err != nil {
		return store.Provider{}, nil, err
	}
	return earliest, client, nil
}

// finishFailed 置 failed 终态并写完成审计。
func (e *Engine) finishFailed(ctx context.Context, sess store.DiagnosisSession, code string) {
	updated, err := e.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: sess.ID, From: store.DiagnosisDiagnosing, To: store.DiagnosisFailed,
		ErrorCode: code, Timestamp: e.clock(),
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("diagnosis_fail_transition_failed", "id", sess.ID, "error", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		return
	}
	actorType, actorID := e.actor(sess)
	e.auditSession(ctx, "diagnosis.completed", actorType, actorID, updated, map[string]any{
		"status": string(store.DiagnosisFailed), "error_code": code,
		"evidence_count": 0, "suggestion_count": 0,
	})
}

// failDiagnosing 把所有 diagnosing 会话置 failed（停机路径）。
func (e *Engine) failDiagnosing(code string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
	defer cancel()
	sessions, err := e.st.DiagnosisSessions().List(ctx, store.DiagnosisDiagnosing, "", 200)
	if err != nil {
		return
	}
	for _, sess := range sessions {
		_, _ = e.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
			ID: sess.ID, From: store.DiagnosisDiagnosing, To: store.DiagnosisFailed,
			ErrorCode: code, Timestamp: e.clock(),
		})
	}
}

// expireLoop 每 30s 扫描过期 awaiting_fix 会话（终止条件=ctx 取消）。
func (e *Engine) expireLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(expireScanTick)
	defer ticker.Stop()
	for {
		select {
		case <-e.loopCtx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.WithoutCancel(e.loopCtx), 10*time.Second)
			n, err := e.st.DiagnosisSessions().ExpireStale(ctx, e.clock())
			cancel()
			if err != nil {
				e.log.Warn("diagnosis_expire_scan_failed", "error", err.Error())
				continue
			}
			if n > 0 {
				e.log.Info("diagnosis_sessions_expired", "count", n)
			}
		}
	}
}

// actor 返回审计 actor：chat 触发为 model/conversation:<id>，alert/manual
// 为 system（契约 5.16）。
func (e *Engine) actor(sess store.DiagnosisSession) (string, string) {
	if sess.Trigger == store.DiagnosisTriggerChat && sess.ConversationID != nil {
		return "model", "conversation:" + *sess.ConversationID
	}
	return "system", ""
}

// auditSession 写诊断审计行（resource_type=diagnosis_session，契约 9.7）。
func (e *Engine) auditSession(ctx context.Context, action, actorType, actorID string,
	sess store.DiagnosisSession, detail map[string]any) {
	if sess.AlertEventID != nil {
		detail["alert_event_id"] = *sess.AlertEventID
	}
	_ = e.audit.Enqueue(ctx, audit.Event{
		ActorType: actorType, ActorID: actorID, Action: action,
		ResourceType: "diagnosis_session", ResourceID: sess.ID,
		ServerID: &sess.ServerID, Detail: detail,
	})
}

// auditCommand 写诊断命令执行审计（复用 command.executed，detail 加
// diagnosis_session_id——契约 9.7）。
func (e *Engine) auditCommand(sess store.DiagnosisSession, srv store.Server,
	command string, exitCode int, durationMS int64, execErr error) {
	actorType, actorID := e.actor(sess)
	detail := map[string]any{
		"command": command, "duration_ms": durationMS,
		"diagnosis_session_id": sess.ID, "trigger": string(sess.Trigger),
	}
	if execErr != nil {
		detail["error"] = execErr.Error()
	} else {
		detail["exit_code"] = exitCode
	}
	_ = e.audit.Enqueue(context.Background(), audit.Event{
		ActorType: actorType, ActorID: actorID, Action: "command.executed",
		ResourceType: "server", ResourceID: srv.ID,
		ServerID: &srv.ID, Detail: detail,
	})
}
