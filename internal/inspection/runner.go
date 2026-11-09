// runner.go 实现五期 M2 的巡检执行器（契约 5.17 / 11 节 #9-#10）：
// 模板命令（risk.Inspect* 常量，SAFE_INSPECT 整串豁免——执行前逐条
// Classify 断言 safe，失败关闭跳过）经 sshx/winrmx 直连执行（不经审批，
// 同 server_facts 模式），按 score.go 评分，报告与明细落库；不用 LLM。
// 并发约束：全局并发 job 上限 2（信号量）、同 job 互斥（上一轮未结束
// 跳过）、单次目标上限 200、单台执行预算 60s、单台原始输出截断 64KB。
package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// ManagerConfig 是巡检配置（契约 5.17；「固定」值写死于常量）。
type ManagerConfig struct {
	MaxConcurrentJobs int           // 固定 2：并行运行的巡检（cron+手动合计）上限
	MaxTargets        int           // 固定 200：单次巡检目标上限；超出 ErrTooManyTargets
	ExecTimeout       time.Duration // 固定 60s：单台模板命令（多条串行共享该预算）
	RawOutputMax      int           // 固定 65536：单台原始输出截断
	CronTick          time.Duration // 固定 1m
}

// 契约 11 节写死的固定值。
const (
	maxConcurrentJobs = 2
	maxTargets        = 200
	execTimeout       = 60 * time.Second
	rawOutputMax      = 65536
	cronTick          = time.Minute
	// runConcurrency 是单次巡检内逐台执行的并发上限（有界 goroutine 池；
	// 契约 5.17：受 SSH 池 MaxPerServer=4、全局 64 天然约束）。
	runConcurrency     = 8
	stopBudget         = 5 * time.Second
	notifyBodyMaxLines = 200
	// itemCommandMax 是单台命令清单落库上限（与迁移 00011 的 CHECK 一致）：
	// 模板固定条数（basic 约 4 条 / full 约 7 条）远小于此值，写死兜底防
	// 模板被扩到异常规模时撑大报告行。
	itemCommandMax = 4096
)

// DefaultManagerConfig 返回全部固定值的配置（11 节）。
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxConcurrentJobs: maxConcurrentJobs,
		MaxTargets:        maxTargets,
		ExecTimeout:       execTimeout,
		RawOutputMax:      rawOutputMax,
		CronTick:          cronTick,
	}
}

// ErrTooManyTargets 表示单次巡检目标超过上限（HTTP 400 VALIDATION_FAILED）。
var ErrTooManyTargets = errors.New("inspection: too many targets")

// NotifyResult 与 monitor.NotifyResult 同形（契约 5.17：Notifier 由
// monitor.Manager 适配注入——inspection 不 import monitor，main 装配时
// 转换）。
type NotifyResult struct {
	Kind       string `json:"kind"`
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code"`
	DurationMS int64  `json:"duration_ms"`
}

// Notifier 是巡检报告通知接口（契约 5.17 签名语义；返回同形结果）。
type Notifier interface {
	NotifyReport(ctx context.Context, channelID string, subject string, body string) (NotifyResult, error)
}

// auditSink 抽象 audit.Writer（测试注入 fake）。
type auditSink interface {
	Enqueue(ctx context.Context, e audit.Event) error
}

type noopAuditSink struct{}

func (noopAuditSink) Enqueue(context.Context, audit.Event) error { return nil }

// Manager 是巡检模块入口（单实例）。Run 由 cron tick、REST、AI 工具共用。
type Manager struct {
	cfg      ManagerConfig
	st       store.Store
	ssh      sshx.Executor
	winrm    winrmx.Executor
	cipher   *cryptox.Cipher
	notifier Notifier
	clock    func() time.Time
	log      *slog.Logger
	audit    auditSink

	sem chan struct{} // 全局并发 job 上限（MaxConcurrentJobs）

	runningMu sync.Mutex
	running   map[string]bool // 同 job 互斥（jobID→在跑）

	loopCtx    context.Context
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
	startOnce  sync.Once
	stopOnce   sync.Once
	stopped    chan struct{}
}

// NewManager 构造 Manager（契约 5.17 签名；notifier 可为 nil=不通知）。
func NewManager(cfg ManagerConfig, st store.Store,
	ssh sshx.Executor, winrm winrmx.Executor, cipher *cryptox.Cipher,
	notifier Notifier, clock func() time.Time, log *slog.Logger) *Manager {

	if cfg.MaxConcurrentJobs <= 0 {
		cfg.MaxConcurrentJobs = maxConcurrentJobs
	}
	if cfg.MaxTargets <= 0 {
		cfg.MaxTargets = maxTargets
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = execTimeout
	}
	if cfg.RawOutputMax <= 0 {
		cfg.RawOutputMax = rawOutputMax
	}
	if cfg.CronTick <= 0 {
		cfg.CronTick = cronTick
	}
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		cfg: cfg, st: st, ssh: ssh, winrm: winrm, cipher: cipher,
		notifier: notifier, clock: clock, log: log, audit: noopAuditSink{},
		sem:     make(chan struct{}, cfg.MaxConcurrentJobs),
		running: make(map[string]bool, 8),
		stopped: make(chan struct{}),
	}
}

// SetAuditWriter 注入审计写入器（main 组装；契约 5.17 审计行）。
func (m *Manager) SetAuditWriter(w *audit.Writer) {
	if w != nil {
		m.audit = w
	}
}

// Start 启动 cron tick 循环（契约 5.17：ListDue → 逐 job，同 job 互斥）。
func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		m.loopCtx, m.loopCancel = context.WithCancel(ctx)
		m.wg.Add(1)
		go m.cronLoop()
		m.log.Info("inspection_manager_started", "tick", m.cfg.CronTick.String())
	})
}

// Stop 同步收尾 ≤5s。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		if m.loopCancel != nil {
			m.loopCancel()
		}
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(stopBudget):
			m.log.Warn("inspection_stop_timeout")
		}
		close(m.stopped)
		m.log.Info("inspection_manager_stopped")
	})
}

// Stopped 返回停机完成信号（测试用）。
func (m *Manager) Stopped() <-chan struct{} { return m.stopped }

// cronLoop 每分钟 tick：ListDue → 逐 job 触发（终止条件=ctx 取消）。
func (m *Manager) cronLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.CronTick)
	defer ticker.Stop()
	for {
		select {
		case <-m.loopCtx.Done():
			return
		case <-ticker.C:
			m.tickDue(m.loopCtx)
		}
	}
}

func (m *Manager) tickDue(ctx context.Context) {
	now := m.clock()
	jobs, err := m.st.InspectionJobs().ListDue(ctx, now)
	if err != nil {
		m.log.Warn("inspection_list_due_failed", "error", err.Error())
		return
	}
	for _, job := range jobs {
		job := job
		// 先顺延 next_run_at（无论本轮是否真的执行——同 job 互斥跳过时也
		// 顺延，避免每分钟重复命中同一 due 行）。
		next, nerr := NextRun(job.Cron, now)
		if nerr != nil {
			next = now.Add(time.Hour) // 非法 cron（建库后被人改坏）：顺延 1h 再试
		}
		if err := m.st.InspectionJobs().MarkRun(ctx, job.ID, now, next); err != nil {
			m.log.Warn("inspection_mark_run_failed", "job", job.ID, "error", err.Error())
			continue
		}
		if !m.DispatchJob(job, store.InspectionTriggerCron) {
			continue // 同 job 上一轮未结束：跳过（契约 5.17）
		}
	}
}

// DispatchJob 异步触发一次 job 巡检（cron tick 与 POST run 共用）：
// 同 job 互斥（false=上一轮未结束）；全局并发受 sem（MaxConcurrentJobs）
// 约束——满则阻塞排队（调用方在 goroutine 外，POST 路径立即返回）。
func (m *Manager) DispatchJob(job store.InspectionJob, trigger store.InspectionTrigger) bool {
	if !m.tryLockJob(job.ID) {
		return false
	}
	// 纳入 wg：Stop 等待在途巡检收尾（此前未 Add，大量 due job 时
	// goroutine 无界排队、Stop 不等待即返回——五期评审发现）。
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.unlockJob(job.ID)
		m.acquireSem()
		defer m.releaseSem()
		runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		actorType := "system"
		if trigger != store.InspectionTriggerCron {
			actorType = "user"
		}
		targets, err := m.resolveTargets(runCtx, &job.ID, job.TargetTags, nil)
		if err != nil {
			m.log.Warn("inspection_resolve_targets_failed", "job", job.ID, "error", err.Error())
			return
		}
		if _, err := m.execute(runCtx, &job, trigger, targets, job.Template, actorType, ""); err != nil {
			m.log.Warn("inspection_dispatch_run_failed", "job", job.ID, "error", err.Error())
		}
	}()
	return true
}

func (m *Manager) tryLockJob(id string) bool {
	m.runningMu.Lock()
	defer m.runningMu.Unlock()
	if m.running[id] {
		return false
	}
	m.running[id] = true
	return true
}

func (m *Manager) unlockJob(id string) {
	m.runningMu.Lock()
	defer m.runningMu.Unlock()
	delete(m.running, id)
}

func (m *Manager) acquireSem() { m.sem <- struct{}{} }
func (m *Manager) releaseSem() { <-m.sem }

// Run 立即执行一次巡检（cron 到点、POST run、POST /api/inspection/run、
// run_inspection 工具共用；契约 5.17 签名）。jobID=nil 表示即时巡检。
// actorType/actorID 用于审计（cron=system、手动=user、对话=model）。
func (m *Manager) Run(ctx context.Context, jobID *string, trigger store.InspectionTrigger,
	serverIDs []string, template store.InspectionTemplate) (store.InspectionReport, error) {
	var job *store.InspectionJob
	var tags []string
	actorType, actorID := "user", ""
	if trigger == store.InspectionTriggerChat {
		actorType, actorID = "model", "" // conversation 归属由调用方（agent 工具）在 detail 补充
	}
	if jobID != nil {
		j, err := m.st.InspectionJobs().Get(ctx, *jobID)
		if err != nil {
			return store.InspectionReport{}, err
		}
		job = &j
		tags = j.TargetTags
		if trigger == store.InspectionTriggerCron {
			actorType = "system"
		}
	}
	// 全局并发约束（契约 5.17：并行巡检（cron+手动合计）上限
	// MaxConcurrentJobs）：即时巡检路径（REST run 与 AI 工具）与 job 型
	// 路径共用同一信号量——此前只有 DispatchJob 受约束，跨请求即时巡检
	// 并发无上限（五期评审发现）。
	m.acquireSem()
	defer m.releaseSem()

	targets, err := m.resolveTargets(ctx, jobID, tags, serverIDs)
	if err != nil {
		return store.InspectionReport{}, err
	}
	return m.execute(ctx, job, trigger, targets, template, actorType, actorID)
}

// resolveTargets 解析目标：serverIDs 显式给定（即时巡检）优先；否则按
// tags 匹配（tags 空=全部 device_kind=server）。上限 MaxTargets（超限
// ErrTooManyTargets）。显式列表中不存在/非 server 的资产跳过。
func (m *Manager) resolveTargets(ctx context.Context, jobID *string, tags []string, serverIDs []string) ([]store.Server, error) {
	if len(serverIDs) > 0 {
		if len(serverIDs) > m.cfg.MaxTargets {
			return nil, ErrTooManyTargets
		}
		var out []store.Server
		for _, id := range serverIDs {
			srv, err := m.st.Servers().Get(ctx, id)
			if err != nil {
				continue // 已删除目标跳过（部分成功不整单失败）
			}
			if srv.DeviceKind != store.DeviceKindServer {
				continue
			}
			out = append(out, srv)
		}
		return out, nil
	}
	servers, err := m.st.Servers().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.Server, 0, len(servers))
	for _, srv := range servers {
		if srv.DeviceKind != store.DeviceKindServer {
			continue // network_device 不巡检（契约 2.10 约定 4）
		}
		if len(tags) > 0 && !hasAnyTag(srv.Tags, tags) {
			continue
		}
		if len(out) >= m.cfg.MaxTargets {
			return nil, ErrTooManyTargets
		}
		out = append(out, srv)
	}
	return out, nil
}

func hasAnyTag(serverTags, wanted []string) bool {
	for _, t := range serverTags {
		for _, w := range wanted {
			if t == w {
				return true
			}
		}
	}
	return false
}

// execute 执行一次巡检主体：并发（≤runConcurrency）逐台执行模板 → 评分
// → 报告+明细落库 → 可选通知 → 审计。
func (m *Manager) execute(ctx context.Context, job *store.InspectionJob,
	trigger store.InspectionTrigger, targets []store.Server, template store.InspectionTemplate,
	actorType, actorID string) (store.InspectionReport, error) {

	if template == "" {
		template = store.InspectionBasic
	}
	started := m.clock()
	jobID := (*string)(nil)
	var jobName string
	notifyEnabled := false
	var notifyChannelID *string
	if job != nil {
		jobID = &job.ID
		jobName = job.Name
		notifyEnabled = job.NotifyEnabled
		notifyChannelID = job.NotifyChannelID
	}

	m.audit.Enqueue(ctx, audit.Event{
		ActorType: actorType, ActorID: actorID, Action: "inspection.started",
		ResourceType: "inspection_report", ServerID: nil,
		Detail: map[string]any{
			"job_id": deref(jobID), "trigger": string(trigger), "server_count": len(targets),
		},
	})

	type itemResult struct {
		srv    store.Server
		result scoreResult
		err    error
		dur    int64
	}
	results := make([]itemResult, len(targets))
	sem := make(chan struct{}, runConcurrency)
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
			res, dur, err := m.inspectOne(ctx, srv, template)
			results[i] = itemResult{srv: srv, result: res, err: err, dur: dur}
		}(i, targets[i])
	}
	wg.Wait()

	// 归一化：采集失败该台 score=0、anomalies=[collect critical]（契约
	// 5.17——部分成功不整单失败）。
	items := make([]scoredItem, 0, len(results))
	scores := make([]int, 0, len(results))
	anomalyTotal := 0
	for _, r := range results {
		item := scoredItem{srv: r.srv, score: r.result.Score, anomalyCount: len(r.result.Anomalies),
			rawOutput: r.result.RawOutput, command: r.result.Command,
			truncated: r.result.Truncated, durationMS: r.dur}
		if r.err != nil {
			item.score = 0
			item.anomalies = []Anomaly{{
				Item: "collect", Detail: truncateStr(r.err.Error(), 512), Severity: "critical",
			}}
			item.anomalyCount = 1
			item.errorCode = truncateStr(r.err.Error(), 128)
		} else {
			item.anomalies = r.result.Anomalies
		}
		items = append(items, item)
		scores = append(scores, item.score)
		anomalyTotal += item.anomalyCount
	}

	// 报告终值一次 Create（items FK 依赖报告行先建；执行期中间态在内存，
	// 有界：≤200 台 × 64KB raw）。
	finished := m.clock()
	report, err := m.st.InspectionReports().Create(ctx, store.CreateInspectionReportParams{
		JobID: jobID, Trigger: trigger, ServerCount: len(items),
		AvgScore: aggregateReport(scores), AnomalyCount: anomalyTotal,
		StartedAt: started, FinishedAt: &finished,
	})
	if err != nil {
		return store.InspectionReport{}, err
	}

	for _, it := range items {
		anomalies := it.anomalies
		if anomalies == nil {
			anomalies = []Anomaly{}
		}
		raw, _ := json.Marshal(anomalies)
		if _, err := m.st.InspectionReports().CreateItem(ctx, store.CreateInspectionItemParams{
			ReportID: report.ID, ServerID: it.srv.ID, Score: it.score,
			AnomalyCount: len(anomalies), Anomalies: raw,
			RawOutput: it.rawOutput, Command: it.command, Truncated: it.truncated,
			DurationMS: it.durationMS, ErrorCode: it.errorCode,
		}); err != nil {
			m.log.Warn("inspection_create_item_failed", "report", report.ID, "error", err.Error())
		}
	}

	m.audit.Enqueue(ctx, audit.Event{
		ActorType: actorType, ActorID: actorID, Action: "inspection.completed",
		ResourceType: "inspection_report", ResourceID: report.ID,
		Detail: map[string]any{
			"job_id": deref(jobID), "trigger": string(trigger),
			"server_count": len(results), "avg_score": report.AvgScore,
			"anomaly_count": report.AnomalyCount,
		},
	})

	if notifyEnabled && notifyChannelID != nil && m.notifier != nil {
		summary := make([]scoredItem, 0, len(items))
		for _, it := range items {
			summary = append(summary, scoredItem{
				srv: it.srv, score: it.score, anomalyCount: it.anomalyCount,
			})
		}
		m.notify(ctx, *notifyChannelID, jobName, report, summary)
	}
	report.JobName = jobName
	return report, nil
}

// inspectOne 对单台执行模板命令并评分。单台总预算 ExecTimeout；命令输出
// 拼接为 raw（===命令=== 分段），截断 RawOutputMax。执行前逐条
// risk.Classify 断言 safe（SAFE_INSPECT 豁免），非 safe 的条目跳过并记
// 日志（失败关闭——模板被改动时不执行）。
func (m *Manager) inspectOne(ctx context.Context, srv store.Server, template store.InspectionTemplate) (scoreResult, int64, error) {
	started := m.clock()
	exec, err := m.execFor(srv)
	if err != nil {
		return scoreResult{}, 0, err
	}
	oneCtx, cancel := context.WithTimeout(ctx, m.cfg.ExecTimeout)
	defer cancel()

	commands := templateCommands(srv, template)
	outputs := make(map[string]string, len(commands))
	var raw strings.Builder
	truncated := false
	var execErr error

	for _, cmd := range commands {
		if oneCtx.Err() != nil {
			execErr = oneCtx.Err()
			break
		}
		out, cerr := exec(oneCtx, cmd)
		if cerr != nil {
			execErr = cerr
			break
		}
		outputs[cmd] = out
		fmt.Fprintf(&raw, "===%s===\n%s\n", cmd, out)
		if raw.Len() > m.cfg.RawOutputMax {
			truncated = true
			break // 终止条件：超出单台输出预算即停
		}
	}
	if raw.Len() > m.cfg.RawOutputMax {
		truncated = true
	}

	var alerts []alertInput
	if execErr == nil {
		alerts = m.loadFiringAlerts(oneCtx, srv.ID)
	}
	result := scoreServer(scoreInput{
		IsWindows: srv.OS == store.ServerOSWindows,
		Outputs:   outputs, Alerts: alerts, ExecErr: execErr,
	})
	result.RawOutput = truncateBytes(raw.String(), m.cfg.RawOutputMax)
	result.Truncated = result.Truncated || truncated
	// 六期打磨 sprint 1（契约 8.7 ⑧）：命令清单随明细落库，报告抽屉的
	// 「命令」列与复制按钮据此提供模板展开后的真实命令。单台清单长度受
	// 模板固定条数约束（最多十余条），再按 itemCommandMax 兜底截断。
	result.Command = truncateBytes(strings.Join(commands, "\n"), itemCommandMax)
	return result, m.clock().Sub(started).Milliseconds(), execErr
}

// templateCommands 返回目标按 os/模板/容器运行时应执行的命令清单
// （契约 7.7；执行前在 inspectOne 逐条 Classify——这里只做清单装配）。
func templateCommands(srv store.Server, template store.InspectionTemplate) []string {
	if srv.OS == store.ServerOSWindows {
		cmds := make([]string, 0, 3)
		// 采集模板（WIN_SAFE_COLLECT，风险侧已登记）。
		cmds = append(cmds, winCollectCommand)
		cmds = append(cmds, risk.WinInspectBasicCommands...)
		if template == store.InspectionFull {
			cmds = append(cmds, risk.WinInspectFullCommands...)
		}
		return cmds
	}
	cmds := make([]string, 0, len(risk.InspectBasicCommands)+3)
	cmds = append(cmds, risk.InspectBasicCommands...)
	if template == store.InspectionFull {
		cmds = append(cmds, "journalctl -p err -n 50 --no-pager")
		if srv.ContainerRuntime == store.ContainerRuntimeDocker ||
			srv.ContainerRuntime == store.ContainerRuntimePodman {
			cmds = append(cmds, "docker ps --format json")
			cmds = append(cmds, "docker stats --no-stream --format json")
		}
	}
	return cmds
}

// winCollectCommand 与 risk.WinCollectCommand 同文（inspection 可 import
// risk——直接引用常量）。
const winCollectCommand = risk.WinCollectCommand

// loadFiringAlerts 装配评分输入的活跃告警（≤maxAlertAnomalyItems 条——
// 扣分上限 -20 已由 2 条封顶，取 10 条足够且防无界查询）。
func (m *Manager) loadFiringAlerts(ctx context.Context, serverID string) []alertInput {
	items, _, err := m.st.AlertEvents().List(ctx, store.AlertEventFilter{
		Status: store.AlertEventFiring, ServerID: serverID, Limit: maxAlertAnomalyItems,
	})
	if err != nil {
		return nil
	}
	out := make([]alertInput, 0, len(items))
	for _, ev := range items {
		out = append(out, alertInput{ID: ev.ID, Severity: string(ev.Severity), Message: ev.Message})
	}
	return out
}

// notify 投递报告摘要（尽力而为：失败只记日志——契约 5.17）。
func (m *Manager) notify(ctx context.Context, channelID, jobName string,
	report store.InspectionReport, results []scoredItem) {
	subject := fmt.Sprintf("[OpsMind] 巡检 %s avg=%d/100 异常 %d 项",
		jobName, report.AvgScore, report.AnomalyCount)
	var b strings.Builder
	for i, r := range results {
		if i >= notifyBodyMaxLines {
			fmt.Fprintf(&b, "... (%d more servers omitted)\n", len(results)-i)
			break
		}
		fmt.Fprintf(&b, "%s: score=%d anomalies=%d\n", r.srv.Name, r.score, r.anomalyCount)
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := m.notifier.NotifyReport(nctx, channelID, subject, b.String()); err != nil {
		m.log.Warn("inspection_notify_failed", "channel", channelID, "error", err.Error())
	}
}

// scoredItem 是单台归一化结果（落库与通知共用视图）。
type scoredItem struct {
	srv          store.Server
	score        int
	anomalyCount int
	anomalies    []Anomaly
	rawOutput    string
	// command 是模板展开后的命令清单（换行分隔，按执行顺序）。六期打磨
	// sprint 1（契约 8.7 ⑧）：报告抽屉据此提供「复制命令」入口；此前报告
	// 明细只有 raw_output，无命令列可复制。
	command    string
	truncated  bool
	durationMS int64
	errorCode  string
}

// execFor 按契约 2.7 路由构造单台执行函数；执行前对模板命令逐条
// risk.Classify（SAFE_INSPECT/WIN_SAFE_COLLECT 整串豁免）断言 safe——
// 失败关闭（模板被改动则不执行该条）。
func (m *Manager) execFor(srv store.Server) (func(context.Context, string) (string, error), error) {
	inner, err := m.channelFor(srv)
	if err != nil {
		return nil, err
	}
	os := risk.OSLinux
	if srv.OS == store.ServerOSWindows {
		os = risk.OSWindows
	}
	return func(ctx context.Context, command string) (string, error) {
		if d := risk.Classify(command, os); d.Level != risk.LevelSafe {
			return "", fmt.Errorf("inspection: template command rejected by risk classifier (%s)", d.Level)
		}
		return inner(ctx, command)
	}, nil
}

// channelFor 构造原始通道执行函数（sshx/winrmx 直连；Windows 经 SSH 通道
// 时由本层包装 PowerShell 外壳——同 server_facts）。
func (m *Manager) channelFor(srv store.Server) (func(context.Context, string) (string, error), error) {
	if srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic {
		if m.winrm == nil {
			return nil, errors.New("inspection: winrm executor not configured")
		}
		target, err := m.winrmTarget(srv)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, command string) (string, error) {
			out, err := m.winrm.Execute(ctx, target, winrmx.RunRequest{
				Command: command, Timeout: m.cfg.ExecTimeout, MaxOutputBytes: int64(m.cfg.RawOutputMax),
			})
			if err != nil {
				return "", err
			}
			if out.ExitCode != 0 {
				return "", fmt.Errorf("inspect command exit %d", out.ExitCode)
			}
			return out.Combined, nil
		}, nil
	}
	if m.ssh == nil {
		return nil, errors.New("inspection: ssh executor not configured")
	}
	target, err := m.sshTarget(srv)
	if err != nil {
		return nil, err
	}
	isWinSSH := srv.OS == store.ServerOSWindows
	return func(ctx context.Context, command string) (string, error) {
		execCommand := command
		if isWinSSH {
			execCommand = "powershell -NoProfile -NonInteractive -Command " + command
		}
		out, err := m.ssh.Execute(ctx, target, sshx.RunRequest{
			Command: execCommand, Timeout: m.cfg.ExecTimeout, MaxOutputBytes: int64(m.cfg.RawOutputMax),
		})
		if err != nil {
			return "", err
		}
		if out.ExitCode != 0 {
			return "", fmt.Errorf("inspect command exit %d", out.ExitCode)
		}
		return out.Combined, nil
	}, nil
}

func (m *Manager) winrmTarget(srv store.Server) (winrmx.Target, error) {
	if srv.AuthMethod != store.AuthMethodPasswordNTLM && srv.AuthMethod != store.AuthMethodPasswordBasic {
		return winrmx.Target{}, errors.New("inspection: not a winrm auth method")
	}
	pw, err := decrypt(m.cipher, srv.PasswordEncrypted)
	if err != nil {
		return winrmx.Target{}, errors.New("inspection: decrypt password failed")
	}
	return winrmx.Target{
		ServerID: srv.ID, Host: srv.Host, Port: srv.WinRMPort, Username: srv.Username,
		Password: pw, UseHTTPS: srv.AuthMethod == store.AuthMethodPasswordBasic && srv.WinRMPort == 5986,
		AuthType: srv.AuthMethod,
	}, nil
}

func (m *Manager) sshTarget(srv store.Server) (sshx.Target, error) {
	target := sshx.Target{
		ServerID: srv.ID, Host: srv.Host, Port: srv.Port, Username: srv.Username,
		AuthMethod: srv.AuthMethod, KnownHostKey: srv.HostKeyPublic,
	}
	switch srv.AuthMethod {
	case store.AuthMethodPassword:
		pw, err := decrypt(m.cipher, srv.PasswordEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("inspection: decrypt password failed")
		}
		target.Password = pw
	case store.AuthMethodPrivateKey:
		key, err := decrypt(m.cipher, srv.PrivateKeyEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("inspection: decrypt private key failed")
		}
		passphrase, err := decrypt(m.cipher, srv.PassphraseEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("inspection: decrypt passphrase failed")
		}
		target.PrivateKey = key
		target.Passphrase = passphrase
	default:
		return sshx.Target{}, errors.New("inspection: unsupported auth method")
	}
	return target, nil
}

// truncateBytes 按字节截断并向后退到 UTF-8 边界。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// decrypt 带 nil 防护的解密：cipher 未注入时返回错误而非 panic
// （五期评审：agent/batchx 的同名辅助有防护，monitor/diagnosis/inspection
// 曾直接调用，nil 接收者会在采集/诊断/巡检 goroutine 内 panic 杀死进程）。
func decrypt(cipher *cryptox.Cipher, ciphertext string) (string, error) {
	// 空串短路（六期打磨 sprint 4，走查 high）：passphrase 是私钥认证的
	// 可选字段（openapi 4.3：私钥认证必须提供 private_key，passphrase
	// 可选），未填时库里存空串。cryptox.Decrypt("") 返回
	// ErrInvalidCiphertext，此前把「没有口令」误判成解密失败——私钥认证
	// 服务器的巡检整轮 0 分，报告错误列直显英文原文。agent/batchx 的
	// 同名辅助一直有该短路，此处对齐。
	if ciphertext == "" {
		return "", nil
	}
	if cipher == nil {
		return "", errors.New("inspection: cipher unavailable")
	}
	return cipher.Decrypt(ciphertext)
}
