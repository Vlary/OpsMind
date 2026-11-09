// Package batchx 实现五期 M3 的批量操作编排（契约 5.18/2.10 约定 5）：
// 自然语言原文 + 统一命令 + 目标服务器集 → 创建 batch_job → per-server
// 独立 risk 分类 → safe 且 auto_exec 直执行（goroutine 池上限 8）、其余
// 逐台创建 source=batch 审批（复用 approvals.Service）→ 单台失败不中断
// 其余 → batch_results 逐台记录。
//
// 防内存上限（契约 11 节，写死不进配置）：池 8；单 job 目标 50；单台执行
// 60s/输出 256KB；过期扫描每 30s、单轮最多 200 job / 500 result。
package batchx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// 契约 5.18/11 节写死的固定上限（不进用户配置；改动先改契约）。
const (
	DefaultWorkerPool  = 8
	DefaultMaxTargets  = 50
	DefaultExecTimeout = 60 * time.Second
	DefaultMaxOutput   = 262144 // 256KB

	maxDescriptionRunes = 2000
	maxCommandBytes     = 16384

	// 过期扫描：每 30s 一轮（契约 5.18）；单轮检查的 job/result 上限
	// （防 OOM：审批 TTL 120s 内活跃 job 有限，200 job × ≤50 result 覆盖）。
	expireScanInterval  = 30 * time.Second
	expireScanMaxJobs   = 200
	expireScanMaxResult = 500
	// auditEnqueueTimeout 与 approvals 同款：请求取消后仍给审计入队机会。
	auditEnqueueTimeout = 2 * time.Second
)

// 审计动作名（契约 9.7：批量复用既有执行类动作，仅新增 batch.created/
// batch.cancelled）。
const (
	actionBatchCreated   = "batch.created"
	actionBatchCancelled = "batch.cancelled"
	actionCmdExecuted    = "command.executed"
	actionCmdDenied      = "command.denied"
	actionCmdForbidden   = "command.forbidden"
	// actionCmdApproved 是「命令批准」（契约 9.7）：批量审批 decide 批准
	// 时由 OnApprovalDecided 写，detail 带 batch_job_id 与命令全文。
	actionCmdApproved = "command.approved"
)

// 错误码（apierr 同名常量的执行层映射；不 import apierr——依赖表禁令）。
const (
	errCodeServerNotFound    = "SERVER_NOT_FOUND"
	errCodeCommandForbidden  = "COMMAND_FORBIDDEN"
	errCodeSSHTimeout        = "SSH_TIMEOUT"
	errCodeSSHExecFailed     = "SSH_EXEC_FAILED"
	errCodeSSHHostKeyChanged = "SSH_HOST_KEY_CHANGED"
	errCodeNetCLIExecFailed  = "NETCLI_EXEC_FAILED"
	errCodeExecInterrupted   = "EXEC_INTERRUPTED"
	// ErrTooManyTargets 是目标超上限的哨兵错误（handler 映射 400
	// BATCH_TOO_MANY_TARGETS）。
	ErrTooManyTargets = batchError("BATCH_TOO_MANY_TARGETS")
)

type batchError string

func (e batchError) Error() string { return string(e) }

// ServiceConfig 是批量编排配置（契约 5.18：字段固定值写死）。
type ServiceConfig struct {
	WorkerPool  int           // 固定 8：goroutine 池上限（契约 11 节）
	MaxTargets  int           // 固定 50：单 job 目标上限（与表 CHECK 一致）
	ExecTimeout time.Duration // 固定 60s：单台执行（safe 直执行与审批后执行共用）
	MaxOutput   int64         // 固定 262144：单台输出截断
}

// normalize 应用契约固定值（零值/越界一律钳到写死常量）。
func (c ServiceConfig) normalize() ServiceConfig {
	if c.WorkerPool <= 0 {
		c.WorkerPool = DefaultWorkerPool
	}
	if c.WorkerPool > DefaultMaxTargets {
		c.WorkerPool = DefaultMaxTargets
	}
	if c.MaxTargets <= 0 {
		c.MaxTargets = DefaultMaxTargets
	}
	if c.MaxTargets > DefaultMaxTargets {
		c.MaxTargets = DefaultMaxTargets
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = DefaultExecTimeout
	}
	if c.MaxOutput <= 0 || c.MaxOutput > DefaultMaxOutput {
		c.MaxOutput = DefaultMaxOutput
	}
	return c
}

// ExecResult 是注入通道的执行结果（签名形状同 containerx.ExecFunc——
// batchx 不 import containerx/netdev，契约 2.3 依赖表禁令）。
type ExecResult struct {
	ExitCode   int
	Combined   string
	Truncated  bool
	DurationMS int64
}

// ExecFunc 是单台执行通道闭包（目标与凭据在闭包内绑定；契约 5.18：
// 网络设备执行经 main 组装时注入的适配，batchx 不 import netcli）。
type ExecFunc func(ctx context.Context, command string, timeout time.Duration) (ExecResult, error)

// NetExecFactory 按服务器构造网络设备执行通道（httpapi 同款辅助适配）。
type NetExecFactory func(srv store.Server) (ExecFunc, error)

// Service 是批量编排服务（单实例）。
type Service struct {
	cfg       ServiceConfig
	st        store.Store
	ap        *approvals.Service
	ssh       sshx.Executor
	winrm     winrmx.Executor
	netExec   NetExecFactory
	cipher    *cryptox.Cipher
	audit     *audit.Writer
	clock     func() time.Time
	log       *slog.Logger
	execSem   chan struct{} // 固定容量 WorkerPool：并发执行上限（含审批后执行）
	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	wg        sync.WaitGroup
}

// NewService 构造批量编排服务（契约 5.18 签名）。
func NewService(cfg ServiceConfig, st store.Store, approvalsSvc *approvals.Service,
	ssh sshx.Executor, winrm winrmx.Executor, cipher *cryptox.Cipher,
	auditWriter *audit.Writer, clock func() time.Time, log *slog.Logger) *Service {
	cfg = cfg.normalize()
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:     cfg,
		st:      st,
		ap:      approvalsSvc,
		ssh:     ssh,
		winrm:   winrm,
		cipher:  cipher,
		audit:   auditWriter,
		clock:   clock,
		log:     log,
		execSem: make(chan struct{}, cfg.WorkerPool),
		stop:    make(chan struct{}),
	}
}

// SetNetExecFactory 注入网络设备执行通道工厂（main 组装时适配 netdev；
// 契约 5.18：batchx 不 import netcli，网络设备执行经注入）。
func (s *Service) SetNetExecFactory(f NetExecFactory) { s.netExec = f }

// Start 启动审批过期扫描（每 30s；契约 5.18）。不订阅调用方 ctx——与
// approvals sweeper 同理由：SIGTERM 排空期内仍需收敛 awaiting_approval 台。
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.expireLoop()
	})
}

// Stop 停止后台扫描并等待退出（goroutine 池有界且有退出路径）。
func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *Service) now() time.Time { return s.clock().UTC() }

// Dispatch 创建 job + 逐台 results 并分派执行（契约 5.18）。
//
// 同步部分：逐台 Classify、建 results、创建审批、safe+auto_exec 台的执行
// （池内并行，Dispatch 返回前完成）；审批台在用户 decide 后由 httpapi
// decide handler 调用 OnApprovalDecided 完成执行与回填。
//
// 执行 goroutine 使用 WithoutCancel(ctx)：客户端断连不中断已分派的单台
// 执行（结果仍要落库），退出路径=执行返回或 ExecTimeout（60s 硬上限）。
func (s *Service) Dispatch(ctx context.Context, p store.CreateBatchJobParams) (store.BatchJob, error) {
	if s.st == nil {
		return store.BatchJob{}, errors.New("batchx: store unavailable")
	}
	targets := dedupeKeepOrder(p.ServerIDs)
	if len(targets) == 0 {
		return store.BatchJob{}, store.ErrInvalid
	}
	if len(targets) > s.cfg.MaxTargets {
		return store.BatchJob{}, ErrTooManyTargets
	}
	if p.Command == "" || len(p.Command) > maxCommandBytes {
		return store.BatchJob{}, store.ErrInvalid
	}
	if len([]rune(p.Description)) > maxDescriptionRunes {
		return store.BatchJob{}, store.ErrInvalid
	}
	source := p.Source
	if source == "" {
		source = store.BatchSourceConsole
	}

	job, err := s.st.BatchJobs().Create(ctx, store.CreateBatchJobParams{
		Description:    p.Description,
		Command:        p.Command,
		ServerIDs:      targets,
		Source:         source,
		ConversationID: p.ConversationID,
		CreatedBy:      p.CreatedBy,
	})
	if err != nil {
		return store.BatchJob{}, err
	}
	s.emitAudit(ctx, s.batchActor(source, p), audit.Event{
		Action:         actionBatchCreated,
		ResourceType:   "batch_job",
		ResourceID:     job.ID,
		ConversationID: p.ConversationID,
		Detail: map[string]any{
			"description": p.Description, "command": p.Command, "server_count": len(targets),
		},
	})

	// 六期 BE-P（契约 2.11 约定 2 路径③）：批量分派接入统一门限策略——
	// 先取全局门限（settings["approval.mode"]），source=chat 再叠加会话覆盖
	// （p.ConversationID），console 直接用全局。解析失败一律失败关闭为 all
	// （全量审批），不静默放行。
	//
	// 六期打磨 sprint 1：此前 chat 分支把兜底值 all 当成「全局」传给
	// ResolveMode，导致会话无覆盖时有效门限恒为 all——全局 standard/none
	// 下 chat 批量仍逐台造审批（与「免审批档全部自动执行」矛盾）。
	globalMode, err := approvals.GlobalMode(ctx, s.st)
	failClosed := err != nil
	if failClosed {
		globalMode = store.ApprovalModeAll
	}
	approvalMode := globalMode
	if !failClosed && source == store.BatchSourceChat && p.ConversationID != nil && *p.ConversationID != "" {
		conv, err := s.st.Conversations().Get(ctx, *p.ConversationID)
		if err != nil {
			approvalMode = store.ApprovalModeAll
		} else {
			approvalMode = approvals.ResolveMode(conv, globalMode)
		}
	}

	var wg sync.WaitGroup
	createFail := 0
	for _, sid := range targets {
		srv, err := s.st.Servers().Get(ctx, sid)
		if err != nil {
			if err := s.createResultTerminal(ctx, job.ID, sid, "", "", store.BatchResultSkipped, errCodeServerNotFound); err != nil {
				createFail++
			}
			continue
		}
		decision := classifyFor(srv, p.Command)
		needsApproval := approvals.NeedsApproval(approvals.PolicyInput{
			Mode:            approvalMode,
			AutoExecEnabled: srv.AutoExecEnabled,
			Level:           decision.Level,
			Severity:        risk.SeverityOf(decision.RuleID),
		})
		switch {
		case decision.Level == risk.LevelForbidden:
			if err := s.createResultTerminal(ctx, job.ID, sid, "forbidden", decision.RuleID, store.BatchResultSkipped, errCodeCommandForbidden); err != nil {
				createFail++
			}
			sidCopy := sid
			s.emitAudit(ctx, s.batchActor(source, p), audit.Event{
				Action: actionCmdForbidden, ResourceType: "server", ResourceID: sidCopy,
				ConversationID: p.ConversationID, ServerID: &sidCopy,
				Detail: map[string]any{"rule_id": decision.RuleID, "batch_job_id": job.ID},
			})
		case !needsApproval:
			// 六期打磨 sprint 3：risk_level 是逐台分类快照（params.go 的
			// CreateBatchResultParams 注释 / 契约 3.3）。none/high 档下
			// needs_approval 级命令同样走直执行——此前写死 "safe" 会把
			// rm -rf 这类落成「安全」。落库与审计同用 decision.Level，与下方
			// 审批台的 string(decision.Level) 同口径。
			result, err := s.st.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
				BatchJobID: job.ID, ServerID: sid,
				RiskLevel: string(decision.Level), RuleID: decision.RuleID, Status: store.BatchResultPending,
			})
			if err != nil {
				createFail++
				continue
			}
			wg.Add(1)
			go func(srv store.Server, result store.BatchResult, ruleID string, level risk.Level, mode store.ApprovalMode) {
				defer wg.Done()
				s.runDirect(source, p, job.ID, result, srv, ruleID, level, mode)
			}(srv, result, decision.RuleID, decision.Level, approvalMode)
		default:
			// 门限判定需审批（含 all 档全量、none 档下 needs_approval 且未开
			// auto_exec 等）：逐台创建 source=batch 审批，携带门限快照
			// ApprovalMode（契约 2.11 约定 1：审批落库时冻结有效门限）。
			if s.ap == nil {
				if err := s.createResultTerminal(ctx, job.ID, sid, string(decision.Level), decision.RuleID, store.BatchResultFailed, "APPROVAL_SERVICE_UNAVAILABLE"); err != nil {
					createFail++
				}
				continue
			}
			serverID := srv.ID
			ap, err := s.ap.Create(ctx, approvals.CreateRequest{
				Source:       "batch",
				ServerID:     &serverID,
				ServerName:   srv.Name,
				Command:      p.Command,
				RiskLevel:    string(decision.Level),
				RuleID:       decision.RuleID,
				Reason:       s.approvalReason(p.Description, decision.Reason),
				ApprovalMode: string(approvalMode),
			})
			if err != nil {
				createFail++
				continue
			}
			if _, err := s.st.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
				BatchJobID: job.ID, ServerID: sid, RiskLevel: string(decision.Level),
				RuleID: decision.RuleID, Status: store.BatchResultAwaitingApproval,
				ApprovalID: &ap.ID,
			}); err != nil {
				createFail++
			}
		}
	}
	// 不等待直执行台完成（此前 wg.Wait() 在 Dispatch 内同步等：50 台全
	// safe+auto_exec 场景最长约 375s，REST POST 与 chat 工具阻塞数分钟，
	// 与工具承诺「立即返回分派概要」相悖——五期评审发现）。改为后台
	// 等待：全部直执行台回填后收敛 job 终态，Dispatch 返回分派中间态。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		wg.Wait()
		finCtx, finCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer finCancel()
		if _, err := s.RecalcJobStatus(finCtx, job.ID); err != nil {
			s.log.Warn("batch_recalc_after_direct_failed", "job_id", job.ID, "error", err.Error())
		}
	}()

	// 编排错误（全部台创建失败）→ failed（契约 5.18 状态机）。
	if createFail == len(targets) {
		if err := s.st.BatchJobs().SetStatus(ctx, job.ID, store.BatchFailed, s.now()); err != nil {
			return store.BatchJob{}, err
		}
		return s.jobByID(ctx, job.ID)
	}
	return s.RecalcJobStatus(ctx, job.ID)
}

// runDirect 在池内执行免审批台并回填结果（goroutine 退出路径：执行返回或
// ExecTimeout）。riskLevel 是该台的分类快照：none/high 档下 needs_approval
// 级命令也会走到这里，落库与审计都必须用它，不得写死 safe（六期打磨
// sprint 3）。
func (s *Service) runDirect(source store.BatchSource, p store.CreateBatchJobParams, jobID string, result store.BatchResult, srv store.Server, ruleID string, riskLevel risk.Level, approvalMode store.ApprovalMode) {
	s.execSem <- struct{}{}
	defer func() { <-s.execSem }()

	out := s.executeCommand(srv, p.Command)
	status := store.BatchResultExecuted
	if out.ErrorCode != "" {
		status = store.BatchResultFailed
	}
	ctx := context.Background()
	if _, err := s.st.BatchJobs().FinishResult(ctx, store.FinishBatchResultParams{
		ID: result.ID, Status: status, ExitCode: out.ExitCode, Output: out.Output,
		Truncated: out.Truncated, ErrorCode: out.ErrorCode, DurationMS: out.DurationMS,
		FinishedAt: s.now(),
	}); err != nil {
		s.log.Warn("batch_finish_result_failed", "result_id", result.ID, "error", err.Error())
		return
	}
	if status == store.BatchResultExecuted {
		sid := srv.ID
		s.emitAudit(ctx, s.batchActor(source, p), audit.Event{
			Action: actionCmdExecuted, ResourceType: "server", ResourceID: srv.ID,
			ConversationID: p.ConversationID, ServerID: &sid,
			Detail: map[string]any{
				"batch_job_id": jobID, "rule_id": ruleID,
				"exit_code": derefInt(out.ExitCode), "risk_level": string(riskLevel),
				// 契约 9.7（六期打磨 sprint 3）：command.executed 必须携带
				// 实际执行命令文本——免审批档事后从审计复原「跑的是什么」
				// 的唯一途径；此前批量行使审计页「命令摘要」恒为「—」。
				"command": p.Command,
				// 契约 9.7：command.executed 携带门限快照（none 档直执行
				// 的审计证据，六期评审发现曾缺失）。
				"approval_mode": string(approvalMode),
			},
		})
	}
	_, _ = s.RecalcJobStatus(ctx, jobID)
}

// Cancel 把 job 的 pending/awaiting_approval 台置 cancelled（已 executing
// 的台不中断——Dispatch 同步段结束后不存在 executing 台；运行中的直执行
// 台不受影响）；仍 pending 的批量审批随之作废（approval.expired）；job
// 状态重算（契约 5.18）。
func (s *Service) Cancel(ctx context.Context, jobID string) (store.BatchJob, error) {
	job, results, err := s.st.BatchJobs().Get(ctx, jobID)
	if err != nil {
		return store.BatchJob{}, err
	}
	cancelled := 0
	for _, r := range results {
		if r.Status != store.BatchResultPending && r.Status != store.BatchResultAwaitingApproval {
			continue
		}
		if r.Status == store.BatchResultAwaitingApproval && r.ApprovalID != nil && s.ap != nil {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, _ = s.ap.Cancel(cancelCtx, *r.ApprovalID, "batch_cancelled", "")
			cancel()
		}
		if _, err := s.st.BatchJobs().FinishResult(ctx, store.FinishBatchResultParams{
			ID: r.ID, Status: store.BatchResultCancelled, FinishedAt: s.now(),
		}); err == nil {
			cancelled++
		}
	}
	if cancelled > 0 {
		s.emitAudit(ctx, s.batchActor(job.Source, store.CreateBatchJobParams{
			ConversationID: job.ConversationID, CreatedBy: job.CreatedBy,
		}), audit.Event{
			Action: actionBatchCancelled, ResourceType: "batch_job", ResourceID: job.ID,
			ConversationID: job.ConversationID,
			Detail:         map[string]any{"cancelled_count": cancelled},
		})
	}
	return s.RecalcJobStatus(ctx, jobID)
}

// RecalcJobStatus 归并 job 状态（契约 5.18）：全部台终态 → done（无
// failed/denied/expired/skipped/cancelled）或 partial（有）；存在非终态台
// → running。finished_at 由 store SetStatus 在终态时写入。
func (s *Service) RecalcJobStatus(ctx context.Context, jobID string) (store.BatchJob, error) {
	job, results, err := s.st.BatchJobs().Get(ctx, jobID)
	if err != nil {
		return store.BatchJob{}, err
	}
	status := s.mergeStatus(results)
	if job.Status == status {
		return job, nil
	}
	if err := s.st.BatchJobs().SetStatus(ctx, jobID, status, s.now()); err != nil {
		return store.BatchJob{}, err
	}
	job.Status = status
	if isBatchTerminal(status) {
		fin := s.now()
		job.FinishedAt = &fin
	}
	job.UpdatedAt = s.now()
	return job, nil
}

// mergeStatus 由逐台状态归并 job 状态；空 results 视为 running（分派未落
// 库的瞬态，保持非终态防提前收敛）。
func (s *Service) mergeStatus(results []store.BatchResult) store.BatchStatus {
	nonTerminal := map[store.BatchResultStatus]bool{
		store.BatchResultPending: true, store.BatchResultAwaitingApproval: true,
	}
	bad := map[store.BatchResultStatus]bool{
		store.BatchResultFailed: true, store.BatchResultDenied: true,
		store.BatchResultExpired: true, store.BatchResultSkipped: true,
		store.BatchResultCancelled: true,
	}
	if len(results) == 0 {
		return store.BatchRunning
	}
	hasNonTerminal, hasBad := false, false
	for _, r := range results {
		if nonTerminal[r.Status] {
			hasNonTerminal = true
		}
		if bad[r.Status] {
			hasBad = true
		}
	}
	switch {
	case hasNonTerminal:
		return store.BatchRunning
	case hasBad:
		return store.BatchPartial
	default:
		return store.BatchDone
	}
}

func isBatchTerminal(st store.BatchStatus) bool {
	switch st {
	case store.BatchDone, store.BatchPartial, store.BatchFailed, store.BatchCancelled:
		return true
	}
	return false
}

// createResultTerminal 建一条终态 result（skipped/failed 等不需要执行；
// error_code 创建时即落列）。
func (s *Service) createResultTerminal(ctx context.Context, jobID, serverID, riskLevel, ruleID string, status store.BatchResultStatus, errorCode string) error {
	_, err := s.st.BatchJobs().CreateResult(ctx, store.CreateBatchResultParams{
		BatchJobID: jobID, ServerID: serverID, RiskLevel: riskLevel,
		RuleID: ruleID, Status: status, ErrorCode: errorCode,
	})
	return err
}

func (s *Service) jobByID(ctx context.Context, jobID string) (store.BatchJob, error) {
	job, _, err := s.st.BatchJobs().Get(ctx, jobID)
	return job, err
}

// batchActor 由 job 来源推导审计 actor（契约 9.7：批量审批链路复用既有
// 动作；console 创建 actor=user，chat 创建 actor=model）。
func (s *Service) batchActor(source store.BatchSource, p store.CreateBatchJobParams) audit.Event {
	if source == store.BatchSourceChat {
		actorID := ""
		if p.ConversationID != nil {
			actorID = "conversation:" + *p.ConversationID
		}
		return audit.Event{ActorType: "model", ActorID: actorID}
	}
	actorID := ""
	if p.CreatedBy != nil {
		actorID = *p.CreatedBy
	}
	return audit.Event{ActorType: "user", ActorID: actorID}
}

// approvalReason 附批量上下文与自然语言原文（契约 5.18）。
func (s *Service) approvalReason(description, decisionReason string) string {
	if description == "" {
		return fmt.Sprintf("批量操作：%s", decisionReason)
	}
	return fmt.Sprintf("批量操作「%s」：%s", description, decisionReason)
}

// emitAudit 记录审计；审计失败只记日志，不影响批量流程。
func (s *Service) emitAudit(ctx context.Context, actor audit.Event, e audit.Event) {
	if s.audit == nil {
		return
	}
	e.ActorType = actor.ActorType
	e.ActorID = actor.ActorID
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEnqueueTimeout)
	defer cancel()
	if err := s.audit.Enqueue(cctx, e); err != nil {
		s.log.Warn("batch_audit_enqueue_failed", "action", e.Action, "error", err.Error())
	}
}

func dedupeKeepOrder(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
