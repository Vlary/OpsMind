// Package approvals 实现命令审批的创建、等待、决策与超时。
//
// 状态机：pending → approved | denied | expired，只有 pending 可迁移。
// 等待者保存在进程内 map（审批 ID → 等待者 ticket → 带缓冲 channel）；
// 同一审批允许多个并发 Wait，Decide 先落库再广播通知全部等待者。
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aiops/internal/audit"
	"aiops/internal/risk"
	"aiops/internal/store"
)

// 审计动作名（CONTRACT.md 9.7）。
const (
	// revokeNote 是账号吊销收敛写入审批的 note（与 httpapi/rbacops 同值）。
	revokeNote            = "rbac:user_revoked"
	actionCreated         = "approval.created"
	actionApproved        = "approval.approved"
	actionDenied          = "approval.denied"
	actionExpired         = "approval.expired"
	actionGrantExpired    = "terminal.grant_expired"
	actionStreamCancelled = "approval.stream_cancelled"
	// actionCommandApproved 是「命令批准」动作（契约 9.7 command.approved）：
	// chat/console 审批 decide 批准时写入；source=batch 由 batchx 在
	// OnApprovalDecided 写（detail 才能带 batch_job_id）。
	actionCommandApproved  = "command.approved"
	cancelReasonStream     = "stream_cancelled"
	cancelReasonShutdown   = "server_shutting_down"
	defaultApprovalTTL     = 120 * time.Second
	cancelWriteTimeout     = 5 * time.Second
	auditEnqueueTimeout    = 2 * time.Second
	pendingExpireBatchSize = 200
	pendingExpireMaxRounds = 50
	// approvalPollInterval 是 Wait 轮询 DB 状态的间隔（走查第 3 轮 medium：
	// 主机侧 break-glass/另一实例的决定不经过本进程 notify，只能读库看到）。
	// 1s 远小于默认 120s TTL；每秒一次按主键 SELECT。
	approvalPollInterval = time.Second
)

// ErrStoreUnavailable 表示未注入 store（仅骨架启动路径会出现）。
var ErrStoreUnavailable = errors.New("approvals: store unavailable")

// ErrPolicyStale 表示策略变更审批的目标已不适用于当前全局门限（期间已被
// 他人收紧/调整）。httpapi 映射 409 POLICY_STALE（契约 12.3 第 10 条）。
var ErrPolicyStale = errors.New("approvals: policy change is stale")

// ErrPolicyNotWidening 表示策略审批的目标不比当前更宽——已经过期，批准
// 不得覆盖更严的现行值。
var ErrPolicyNotWidening = errors.New("approvals: policy change no longer widens current value")

// ErrSelfDecision 表示审批人正是发起人（职责分离，契约 12.3 第 6 条）。
// 审批状态保持 pending，绝不落 decided_by。
var ErrSelfDecision = errors.New("approvals: requester cannot decide own approval")

// maxTrackedRequesters 是进程内发起人表上限（防被刷时无界增长）。
const maxTrackedRequesters = 4096

// Decision 是审批等待结果。
type Decision struct {
	ApprovalID string
	Status     store.ApprovalStatus
	DecidedAt  *time.Time
	Note       string
	// EditedCommand 是 decide 批准时的用户改写命令（六期 BE-P，契约 2.11
	// 约定 3）；未改写为空串。chat 等待方（agent runner）据此执行改写后
	// 命令（逐字节，不再回读模型原始参数）。
	EditedCommand string
}

// CreateRequest 是创建审批请求。
type CreateRequest struct {
	// C1（四期）：Source 取 "chat"|"console"。chat=模型工具调用（原语义，
	// ConversationID/ToolCallID 恒非空）；console=控制台发起（容器页操作，
	// 契约 2.9 约定 3），ConversationID/ToolCallID 为 nil。
	// 五期 M3（契约 4.26）：追加 "batch"——批量操作逐台审批，语义同
	// console（无对话/工具调用上下文，多台审批无法共享唯一 tool_call_id）。
	ConversationID *string
	ToolCallID     *string
	Source         string
	ServerID       *string
	ServerName     string
	Command        string
	RiskLevel      string
	RuleID         string
	Reason         string
	// ApprovalMode 是创建时有效门限快照（六期 BE-P，契约 2.11 约定 1：
	// EffectiveMode 的结果原样传入）；空串按 standard 落库。
	ApprovalMode string
	RequestID    string
	// RequestedBy 是实际发起人（契约 12.3 第 5 条）：chat=会话 owner；
	// console/诊断 fix=HTTP 用户；batch=batch_jobs.created_by；terminal/policy
	// =请求用户。必填。
	RequestedBy string
	// 00012：source=terminal 的 grant 元数据（grant_kind + grant_expires_at）。
	GrantKind      *string
	GrantExpiresAt *time.Time
	// 00012：source=policy 的不可变变更目标（创建后不可改）。
	PolicyFrom *string
	PolicyTo   *string
	// ExpiresAt 可选覆盖默认审批 TTL（terminal grant 需要 30 分钟窗口）。
	// 零值使用服务默认 TTL。
	ExpiresAt time.Time
}

// DecideRequest 是 decide 请求（httpapi → Service，六期 BE-P 结构体化）。
// EditedCommand 仅历史兼容：HTTP handler 拒绝该字段（bindJSONStrict），
// Service 侧保留旧内部路径；执行层永不读取。重分类在本函数内完成，
// httpapi 只透传用户输入、不得自行分类。
type DecideRequest struct {
	ApprovalID string
	Approve    bool
	DecidedBy  string
	// RequestedBy 可显式传入（DB 行字段）；为空时用进程内创建时记录。
	RequestedBy   string
	Note          string
	EditedCommand string
	RequestID     string
}

// Service 是审批服务。
type Service struct {
	st    store.Store
	audit *audit.Writer
	ttl   time.Duration
	clock func() time.Time
	log   *slog.Logger

	mu      sync.Mutex
	waiters map[string]map[uint64]chan Decision
	// requesters 记录进程内创建的审批发起人（审批 TTL 到期/进程重启后
	// pending 行由 sweeper 置 expired，故无需持久化即可覆盖存活审批）。
	requesters map[string]string
	waitSeq    uint64
	started    bool

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// NewService 构造审批服务。
func NewService(st store.Store, auditWriter *audit.Writer, ttl time.Duration, clock func() time.Time, log *slog.Logger) *Service {
	if ttl <= 0 {
		ttl = defaultApprovalTTL
	}
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		st:         st,
		audit:      auditWriter,
		ttl:        ttl,
		clock:      clock,
		log:        log,
		waiters:    make(map[string]map[uint64]chan Decision),
		requesters: make(map[string]string, 64),
		stop:       make(chan struct{}),
	}
}

// Start 先把重启前遗留的 pending 审批标记为 expired，再启动每秒过期扫描。
//
// 进程重启后等待者已不存在，旧的 pending 审批不可能再被执行，因此必须作废。
//
// 后台扫描不订阅调用方 ctx：SIGTERM 后 HTTP 还有排空窗口，sweeper 提前
// 退出会让排空期内的 pending 审批失去超时；扫描仅由 Stop() 停止，Stop 在
// main 的关闭序列里位于 HTTP 排空之后。重启清理用带超时的 ctx 副本执行，
// 避免 store 故障时卡住启动。
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	bootCtx, cancelBoot := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	s.expirePendingAfterRestart(bootCtx)
	cancelBoot()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-ticker.C:
				if _, err := s.Expire(context.Background(), now); err != nil {
					s.log.Warn("approval_expire_failed", "error", err.Error())
				}
			}
		}
	}()
}

// Stop 停止后台扫描。
func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Create 创建 pending 审批。
func (s *Service) Create(ctx context.Context, req CreateRequest) (store.Approval, error) {
	if s.st == nil {
		return store.Approval{}, ErrStoreUnavailable
	}
	// 四期 C1（契约 5.5）：chat 审批要求对话/工具调用上下文齐全；
	// console 审批（容器页操作）两者为 nil，且不参与 ToolCallID 唯一性
	// 检查（NULL 不参与 UNIQUE）。
	source := req.Source
	if source == "" {
		source = "chat"
	}
	switch source {
	case "chat":
		if req.ConversationID == nil || req.ToolCallID == nil || *req.ConversationID == "" || *req.ToolCallID == "" {
			return store.Approval{}, store.ErrInvalid
		}
	case "console", "batch", "terminal", "policy":
		// 五期 M3（契约 4.26/2.10 约定 5）：batch 与 console 同语义——
		// conversation_id/tool_call_id 为 NULL（NULL 不参与 UNIQUE）。
		// 00012：terminal/policy 同样无对话上下文。
		if req.ConversationID != nil || req.ToolCallID != nil {
			return store.Approval{}, store.ErrInvalid
		}
		if source == "terminal" {
			if req.GrantKind == nil || (*req.GrantKind != "term" && *req.GrantKind != "rdp") ||
				req.GrantExpiresAt == nil {
				return store.Approval{}, store.ErrInvalid
			}
			if req.ServerID == nil || *req.ServerID == "" {
				return store.Approval{}, store.ErrInvalid
			}
		}
		if source == "policy" {
			if req.PolicyFrom == nil || req.PolicyTo == nil ||
				!store.ValidApprovalMode(store.ApprovalMode(*req.PolicyFrom)) ||
				!store.ValidApprovalMode(store.ApprovalMode(*req.PolicyTo)) {
				return store.Approval{}, store.ErrInvalid
			}
			// 策略请求只能放宽（rank 更低）；收紧直接由 PUT /settings 执行，
			// 不占用审批通道。
			if !IsWidening(store.ApprovalMode(*req.PolicyFrom), store.ApprovalMode(*req.PolicyTo)) {
				return store.Approval{}, ErrPolicyNotWidening
			}
		}
	default:
		return store.Approval{}, store.ErrInvalid
	}
	if strings.TrimSpace(req.RequestedBy) == "" {
		// 契约 12.3 第 5 条：新审批 requested_by 非空；缺失即拒绝。
		return store.Approval{}, store.ErrInvalid
	}
	now := s.clock().UTC()
	expiresAt := now.Add(s.ttl)
	if !req.ExpiresAt.IsZero() {
		expiresAt = req.ExpiresAt.UTC()
	}
	params := store.CreateApprovalParams{
		ConversationID: req.ConversationID,
		ToolCallID:     req.ToolCallID,
		Source:         source,
		ServerID:       req.ServerID,
		ServerName:     req.ServerName,
		Command:        req.Command,
		RiskLevel:      req.RiskLevel,
		RuleID:         req.RuleID,
		Reason:         req.Reason,
		ApprovalMode:   req.ApprovalMode,
		RequestID:      req.RequestID,
		ExpiresAt:      expiresAt,
		CreatedAt:      now,
		// 契约 12.3 第 5 条：发起人必须落库（chat=会话 owner、
		// console/诊断 fix=HTTP 用户、batch=batch_jobs.created_by），
		// 不能只依赖进程内 recorders 表——重启后审批仍可被复检/决策。
		RequestedBy:    req.RequestedBy,
		GrantKind:      req.GrantKind,
		GrantExpiresAt: req.GrantExpiresAt,
		PolicyFrom:     req.PolicyFrom,
		PolicyTo:       req.PolicyTo,
	}
	// 审批行与「approval.created / terminal.grant_requested /
	// policy.change_requested」审计在同一事务内提交（契约 12.3 第 19 条）：
	// 进程在提交与进程内入队之间崩溃不再丢这条审计。
	actorType, actorID := "model", conversationActorID(req.ConversationID)
	if source != "chat" {
		actorType, actorID = "user", req.RequestedBy
	}
	action := actionCreated
	detail := map[string]any{
		"tool_call_id": derefOrEmpty(req.ToolCallID),
		"source":       source,
		"requested_by": req.RequestedBy,
		"risk_level":   req.RiskLevel,
		"expires_at":   expiresAt.UTC().Format(time.RFC3339Nano),
	}
	switch source {
	case "terminal":
		action = "terminal.grant_requested"
		detail["grant_kind"] = derefOrEmpty(req.GrantKind)
		detail["server_id"] = derefOrEmpty(req.ServerID)
	case "policy":
		action = "policy.change_requested"
		detail["policy_from"] = derefOrEmpty(req.PolicyFrom)
		detail["policy_to"] = derefOrEmpty(req.PolicyTo)
	}

	ev := func(approvalID string) audit.Event {
		detail["approval_id"] = approvalID
		// 契约 9.7 / 6016：policy.change_requested 与 policy.change_applied
		// 同口径 resource_type=setting、resource_id=approval.mode（第 4 轮
		// 走查 low）。此前 policy 申请也硬编码 approval，审计页按
		// 「目标类型=设置」过滤时看不到「谁申请了放宽审批门限」。
		resourceType, resourceID := "approval", approvalID
		if source == "policy" {
			resourceType, resourceID = "setting", approvalModeSettingKey
		}
		return audit.Event{
			ActorType:      actorType,
			ActorID:        actorID,
			Action:         action,
			ResourceType:   resourceType,
			ResourceID:     resourceID,
			RequestID:      req.RequestID,
			ConversationID: req.ConversationID,
			ServerID:       req.ServerID,
			Detail:         detail,
		}
	}

	var ap store.Approval
	if store.OutboxOf(s.st) == nil {
		// 未实现 outbox 的存储（内存 fake/骨架）：保持旧路径（直接建行 +
		// 进程内异步审计），保证单测与骨架可运行。
		created, cerr := s.st.Approvals().Create(ctx, params)
		if cerr != nil {
			return store.Approval{}, cerr
		}
		ap = created
		s.emitAudit(ctx, ev(ap.ID))
		s.trackRequester(ap.ID, req.RequestedBy)
		return ap, nil
	}
	err := s.withOutboxTx(ctx, func(tx store.Store) error {
		created, cerr := tx.Approvals().Create(ctx, params)
		if cerr != nil {
			return cerr
		}
		ap = created
		_, aerr := s.emitAuditTx(ctx, tx, ev(ap.ID))
		return aerr
	})
	if err != nil {
		return store.Approval{}, err
	}
	s.trackRequester(ap.ID, req.RequestedBy)
	return ap, nil
}

// withOutboxTx 在事务内执行审批状态变更；不支持事务的存储（内存 fake）
// 直接以自身为事务执行（行为与旧路径一致）。
//
// 返回 store.ErrTxUnsupported 时回落到非事务路径，保证骨架/单测可运行。
func (s *Service) withOutboxTx(ctx context.Context, fn func(tx store.Store) error) error {
	err := s.st.WithTx(ctx, fn)
	if errors.Is(err, store.ErrTxUnsupported) {
		return fn(s.st)
	}
	return err
}

// emitAuditTx 在业务事务内写 audit_outbox（契约 12.3 第 19 条）。
// 返回 false 表示存储未实现 outbox，调用方应在提交后回落到进程内 Enqueue。
func (s *Service) emitAuditTx(ctx context.Context, tx store.Store, e audit.Event) (bool, error) {
	outbox := store.OutboxOf(tx)
	if outbox == nil {
		return false, nil
	}
	payload, err := audit.OutboxPayload(e, s.clock())
	if err != nil {
		return true, err
	}
	if err := outbox.EnqueueAuditOutbox(ctx, payload, s.clock()); err != nil {
		return true, err
	}
	return true, nil
}

// Wait 等待审批决策或超时。
//
// 返回 approved/denied/expired；ctx 取消返回 ctx.Err()，不改变数据库状态
// （调用方必须调用 Cancel 或由 sweeper 过期）。
//
// 走查第 3 轮 medium：跨进程决策只能从 DB 看到（主机侧
// `aiops break-glass` 直接 UPDATE approvals，进程内 channel 永不触发），
// 旧实现只等 channel 或 TTL——等待方会挂到审批到期才继续，期间库里已是
// approved，用户以为没生效。这里在 channel 之外按 approvalPollInterval
// 轮询一次 DB 状态；轮询间隔远小于 TTL，代价是一次按主键的 SELECT。
func (s *Service) Wait(ctx context.Context, approvalID string) (Decision, error) {
	if s.st == nil {
		return Decision{}, ErrStoreUnavailable
	}
	if approvalID == "" {
		return Decision{}, store.ErrNotFound
	}

	// 同一审批可能有多个并发 Wait（如重试的 SSE 流与旧流的排空期重叠）：
	// 每个等待者持独立 ticket 注册，避免后注册者覆盖先注册者的通知 channel。
	ch := make(chan Decision, 1)
	s.mu.Lock()
	ticket := s.waitSeq
	s.waitSeq++
	if s.waiters[approvalID] == nil {
		s.waiters[approvalID] = make(map[uint64]chan Decision)
	}
	s.waiters[approvalID][ticket] = ch
	s.mu.Unlock()
	defer s.removeWaiter(approvalID, ticket, ch)

	// 先注册再读库：即便 Decide 发生在读库之前，通知也不会丢。
	ap, err := s.st.Approvals().Get(ctx, approvalID)
	if err != nil {
		return Decision{}, err
	}
	if ap.Status != store.ApprovalPending {
		return decisionOf(ap), nil
	}

	timer := time.NewTimer(time.Until(ap.ExpiresAt))
	defer timer.Stop()
	poll := time.NewTicker(approvalPollInterval)
	defer poll.Stop()

	for {
		select {
		case d := <-ch:
			return d, nil
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-poll.C:
			// 跨进程决策（主机侧 break-glass / 另一实例的 HTTP decide）
			// 不经过本进程的 notify：直接读库确认终态。
			cur, gerr := s.st.Approvals().Get(ctx, approvalID)
			if gerr != nil {
				if ctx.Err() != nil {
					return Decision{}, ctx.Err()
				}
				// 瞬时读失败不结束等待：下一 tick 再试，TTL 兜底。
				continue
			}
			if cur.Status != store.ApprovalPending {
				return decisionOf(cur), nil
			}
		case <-timer.C:
			// 落库交给 sweeper；这里也尝试一次，确保状态及时可见。
			if _, err := s.Expire(context.WithoutCancel(ctx), s.clock()); err != nil {
				s.log.Warn("approval_expire_failed", "approval_id", approvalID, "error", err.Error())
			}
			d, err := s.Get(context.WithoutCancel(ctx), approvalID)
			if err == nil && d.Status != store.ApprovalPending {
				return decisionOf(d), nil
			}
			return Decision{ApprovalID: approvalID, Status: store.ApprovalExpired}, nil
		}
	}
}

// Decide 批准或拒绝审批（六期 BE-P 结构体化，契约 5.5/2.11 约定 3）。
// 00012 起先读持久化行做职责分离（requested_by != decided_by）与历史改写
// 拦截，再持久化并通知等待者。改写流程（仅历史兼容路径；HTTP 已拒绝
// edited_command）：EditedCommand 非空时——
// approve=false → ErrInvalid；长度/NUL 校验 → ErrInvalid；server_id 为空
// → ErrInvalid；按 server 的 device_kind/os 重新分类（network_device 走
// ClassifyNet）→ forbidden 返回 ErrForbidden（httpapi 映射 403
// COMMAND_FORBIDDEN，审批保持 pending）；否则 edited_* 与决策状态同一
// UPDATE 落库（原 command/risk_level/rule_id/reason 不动——创建时的历史
// 事实）。改写不改变「已批准」语义。
func (s *Service) Decide(ctx context.Context, req DecideRequest) (store.Approval, error) {
	if s.st == nil {
		return store.Approval{}, ErrStoreUnavailable
	}
	// 先读持久化行：requested_by 是职责分离的唯一事实源（契约 12.3 第 6
	// 条）。不能只依赖进程内 map——多实例/重启后 map 为空时也必须拒绝自审；
	// 调用方传入的 RequestedBy 只作为历史行的兜底，不覆盖 DB 值。
	stored, err := s.st.Approvals().Get(ctx, req.ApprovalID)
	if err != nil {
		return store.Approval{}, err
	}
	if stored.Status != store.ApprovalPending {
		return store.Approval{}, store.ErrConflict
	}
	// 第 11 轮走查 high：TTL 是审批有效性的一部分。sweeper 尚未把过期行
	// 置为 expired 的窗口内（DB/审计积压时可达数秒），任何有
	// approval:decide 的账号仍能批准并触发 console/batch 同步执行或
	// policy 放宽。这里按决策时刻先判 TTL：过期即收敛为 expired 并返回
	// 冲突，保证「倒计时归零后不能批准」；store 的 UPDATE 另有 expires_at
	// 兜底（decideExpiryClause），覆盖读到与写入之间时间推进的竞态。
	now := s.clock().UTC()
	if !stored.ExpiresAt.After(now) {
		// 收敛写库不跟随请求取消，但必须有上限：请求方断开不应让 decide
		// handler 挂在审计积压上（与 Cancel 同用 5s）。
		expireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelWriteTimeout)
		defer cancel()
		if _, err := s.Expire(expireCtx, now); err != nil {
			// 收敛失败也不放行：DB 的 expires_at 兜底同样会拒绝这次决策。
			if s.log != nil {
				s.log.Warn("approval_expire_on_decide_failed", "approval_id", req.ApprovalID, "error", err.Error())
			}
		}
		return store.Approval{}, store.ErrConflict
	}
	// 契约 12.3 第 7 条：00012 之前的历史 pending 改写行不得决策。
	if stored.Legacy && stored.EditedCommand != "" {
		return store.Approval{}, ErrLegacyEdit
	}
	requestedBy := stored.RequestedBy
	if requestedBy == "" {
		requestedBy = req.RequestedBy
	}
	if requestedBy == "" {
		requestedBy = s.requesterOf(req.ApprovalID)
	}
	if requestedBy == "" || req.DecidedBy == "" {
		// 无法证明「第二人」时失败关闭，绝不落 decided_by。
		return store.Approval{}, ErrInvalid
	}
	if requestedBy == req.DecidedBy {
		return store.Approval{}, ErrSelfDecision
	}
	edited := req.EditedCommand
	editedRiskLevel := ""
	editedRuleID := ""
	if edited != "" {
		if !req.Approve {
			return store.Approval{}, ErrInvalid
		}
		if strings.ContainsRune(edited, 0) || utf8.RuneCountInString(edited) < 1 || utf8.RuneCountInString(edited) > EditedCommandMaxRunes {
			return store.Approval{}, ErrInvalid
		}
		decision, err := s.reclassify(ctx, req.ApprovalID, edited)
		if err != nil {
			return store.Approval{}, err
		}
		if decision.Level == risk.LevelForbidden {
			return store.Approval{}, ErrForbidden
		}
		editedRiskLevel = string(decision.Level)
		editedRuleID = decision.RuleID
	}
	status := store.ApprovalApproved
	action := actionApproved
	if !req.Approve {
		status = store.ApprovalDenied
		action = actionDenied
	}
	now = s.clock().UTC()
	params := store.DecideApprovalParams{
		ID:              req.ApprovalID,
		Status:          status,
		DecidedBy:       req.DecidedBy,
		Note:            req.Note,
		EditedCommand:   edited,
		EditedRiskLevel: editedRiskLevel,
		EditedRuleID:    editedRuleID,
		DecidedAt:       now,
	}
	// 决策状态（policy 批准时还含 settings）与决策审计在**同一事务**内提交
	// （契约 12.3 第 19 条）：approval.approved/denied、command.approved、
	// terminal.grant_approved、policy.change_applied 全部走 audit_outbox，
	// 进程在提交与入队之间崩溃不再永久丢审计。
	var ap store.Approval
	outboxUsed := false
	apply := func(tx store.Store) error {
		if stored.Source == "policy" && req.Approve {
			// 契约 12.3 第 10/11 条：策略变更批准在**同一事务**内复验目标
			// （policy_from 必须仍是当前值、policy_to 仍更宽）并写 settings，
			// 与审批状态一起提交或一起回滚。
			current, err := GlobalMode(ctx, tx)
			if err != nil {
				return err
			}
			from := derefOrEmpty(stored.PolicyFrom)
			to := derefOrEmpty(stored.PolicyTo)
			if from != string(current) {
				return ErrPolicyStale
			}
			if !IsWidening(current, store.ApprovalMode(to)) {
				return ErrPolicyNotWidening
			}
			raw, err := json.Marshal(to)
			if err != nil {
				return err
			}
			if _, err := tx.Settings().Set(ctx, approvalModeSettingKey, raw, now); err != nil {
				return err
			}
		}
		decided, err := tx.Approvals().Decide(ctx, params)
		if err != nil {
			return err
		}
		ap = decided
		used, err := s.emitDecideAuditTx(ctx, tx, ap, req, action, edited, editedRiskLevel)
		outboxUsed = used
		return err
	}
	if store.OutboxOf(s.st) != nil {
		err = s.withOutboxTx(ctx, apply)
	} else {
		err = apply(s.st)
	}
	if err != nil {
		return store.Approval{}, err
	}
	s.untrackRequester(req.ApprovalID)
	s.notify(req.ApprovalID, decisionOf(ap))

	detail := map[string]any{
		"approval_id":  ap.ID,
		"tool_call_id": derefOrEmpty(ap.ToolCallID),
		"source":       ap.Source,
		"risk_level":   ap.RiskLevel,
		"note":         req.Note,
		// 契约 9.7（六期打磨 sprint 5）：审批决策行必须能在审计页复原
		//「批/拒的是哪条命令」——detail 无条件带原命令（改写时另带
		// edited_command）。此前只有 approval_id/tool_call_id/source/
		// risk_level/note，审计页「命令摘要」列恒「—」。
		"command": ap.Command,
	}
	if edited != "" {
		// 契约 9.7：改写时 approval.approved detail 增原/改写命令与两者
		// 风险级别（未改写不增）。
		detail["original_command"] = ap.Command
		detail["edited_command"] = edited
		detail["original_risk_level"] = ap.RiskLevel
		detail["edited_risk_level"] = editedRiskLevel
	}
	if !outboxUsed {
		// 未实现 outbox 的存储（内存 fake/骨架）：进程内异步审计，顺序与
		// 事务内路径一致（决策行 → command.approved → 生命周期行）。
		s.emitAudit(ctx, audit.Event{
			ActorType:      "user",
			ActorID:        req.DecidedBy,
			Action:         action,
			ResourceType:   "approval",
			ResourceID:     ap.ID,
			RequestID:      req.RequestID,
			ConversationID: ap.ConversationID,
			ServerID:       ap.ServerID,
			Detail:         detail,
		})
		// 与事务内路径同口径：仅 chat/console 批准写 command.approved
		//（batch 由 batchx 补齐 batch_job_id），terminal/policy 另写生命周期行。
		if req.Approve && (ap.Source == "chat" || ap.Source == "console") {
			s.emitCommandApproved(ctx, ap, req, edited, editedRiskLevel)
		}
		if ev, ok := lifecycleAudit(ap, req); ok {
			s.emitAudit(ctx, ev)
		}
	}
	return ap, nil
}

// emitDecideAuditTx 在决策事务内写全部决策审计（决策行 + command.approved
// + terminal/policy 生命周期行）。
func (s *Service) emitDecideAuditTx(ctx context.Context, tx store.Store, ap store.Approval, req DecideRequest, action, edited, editedRiskLevel string) (bool, error) {
	detail := map[string]any{
		"approval_id":  ap.ID,
		"tool_call_id": derefOrEmpty(ap.ToolCallID),
		"source":       ap.Source,
		"risk_level":   ap.RiskLevel,
		"note":         req.Note,
		"command":      ap.Command,
	}
	if edited != "" {
		detail["original_command"] = ap.Command
		detail["edited_command"] = edited
		detail["original_risk_level"] = ap.RiskLevel
		detail["edited_risk_level"] = editedRiskLevel
	}
	events := []audit.Event{{
		ActorType:      "user",
		ActorID:        req.DecidedBy,
		Action:         action,
		ResourceType:   "approval",
		ResourceID:     ap.ID,
		RequestID:      req.RequestID,
		ConversationID: ap.ConversationID,
		ServerID:       ap.ServerID,
		Detail:         detail,
	}}
	// 契约 9.7：批准另写 command.approved。source=batch 由 batchx 补齐
	// batch_job_id，避免一次批准两行重复。
	if req.Approve && (ap.Source == "chat" || ap.Source == "console") {
		events = append(events, commandApprovedEvent(ap, req, edited, editedRiskLevel))
	}
	if ev, ok := lifecycleAudit(ap, req); ok {
		events = append(events, ev)
	}
	used := true
	for _, e := range events {
		u, err := s.emitAuditTx(ctx, tx, e)
		if err != nil {
			return used, err
		}
		used = used && u
	}
	return used, nil
}

// lifecycleAudit 返回 terminal/policy 的专用生命周期审计（契约 9.7）。
func lifecycleAudit(ap store.Approval, req DecideRequest) (audit.Event, bool) {
	if !req.Approve {
		return audit.Event{}, false
	}
	switch ap.Source {
	case "terminal":
		return audit.Event{
			ActorType:    "user",
			ActorID:      req.DecidedBy,
			Action:       "terminal.grant_approved",
			ResourceType: "approval",
			ResourceID:   ap.ID,
			RequestID:    req.RequestID,
			ServerID:     ap.ServerID,
			Detail: map[string]any{
				"approval_id":  ap.ID,
				"grant_kind":   derefOrEmpty(ap.GrantKind),
				"server_id":    derefOrEmpty(ap.ServerID),
				"requested_by": ap.RequestedBy,
				"decided_by":   req.DecidedBy,
			},
		}, true
	case "policy":
		return audit.Event{
			ActorType:    "user",
			ActorID:      req.DecidedBy,
			Action:       "policy.change_applied",
			ResourceType: "setting",
			ResourceID:   approvalModeSettingKey,
			RequestID:    req.RequestID,
			Detail: map[string]any{
				"policy_from":  derefOrEmpty(ap.PolicyFrom),
				"policy_to":    derefOrEmpty(ap.PolicyTo),
				"requested_by": ap.RequestedBy,
				"decided_by":   req.DecidedBy,
			},
		}, true
	default:
		return audit.Event{}, false
	}
}

// emitCommandApproved 写 command.approved 审计（契约 9.7）。资源锚定
// 服务器（与 command.executed 同口径）；审批无 server_id 时回落到审批行，
// 保证命令文本始终有出处可查。
func (s *Service) emitCommandApproved(ctx context.Context, ap store.Approval, req DecideRequest, edited, editedRiskLevel string) {
	s.emitAudit(ctx, commandApprovedEvent(ap, req, edited, editedRiskLevel))
}

// commandApprovedEvent 构造 command.approved 审计事件（契约 9.7）。
func commandApprovedEvent(ap store.Approval, req DecideRequest, edited, editedRiskLevel string) audit.Event {
	commandDetail := map[string]any{
		"approval_id":   ap.ID,
		"tool_call_id":  derefOrEmpty(ap.ToolCallID),
		"source":        ap.Source,
		"risk_level":    ap.RiskLevel,
		"approval_mode": string(ap.ApprovalMode),
		"command":       ap.Command,
	}
	if edited != "" {
		commandDetail["edited_command"] = edited
		commandDetail["original_command"] = ap.Command
		commandDetail["original_risk_level"] = ap.RiskLevel
		commandDetail["edited_risk_level"] = editedRiskLevel
	}
	resourceType := "approval"
	resourceID := ap.ID
	if ap.ServerID != nil && *ap.ServerID != "" {
		resourceType = "server"
		resourceID = *ap.ServerID
	}
	return audit.Event{
		ActorType:      "user",
		ActorID:        req.DecidedBy,
		Action:         actionCommandApproved,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		RequestID:      req.RequestID,
		ConversationID: ap.ConversationID,
		ServerID:       ap.ServerID,
		Detail:         commandDetail,
	}
}

// reclassify 按审批目标服务器对改写命令重新分类（契约 2.11 约定 3）：
// server_id 为空 → ErrInvalid；device_kind=network_device 走
// risk.ClassifyNet(command, brand)，否则 risk.Classify(command, os)。
// 重分类为单次调用（契约 11 节第 19 条），不做批量/循环。
func (s *Service) reclassify(ctx context.Context, approvalID, command string) (risk.Decision, error) {
	ap, err := s.st.Approvals().Get(ctx, approvalID)
	if err != nil {
		return risk.Decision{}, err
	}
	if ap.ServerID == nil || *ap.ServerID == "" {
		return risk.Decision{}, ErrInvalid
	}
	srv, err := s.st.Servers().Get(ctx, *ap.ServerID)
	if err != nil {
		// servers 外键 ON DELETE SET NULL 后 ServerID 为 nil 已在上面拦截；
		// 这里防御其余不存在场景，按请求非法处理（无目标无法分类）。
		if errors.Is(err, store.ErrNotFound) {
			return risk.Decision{}, ErrInvalid
		}
		return risk.Decision{}, err
	}
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		return risk.ClassifyNet(command, netBrandOf(srv.NetdevBrand)), nil
	}
	os := risk.OSLinux
	if srv.OS == store.ServerOSWindows {
		os = risk.OSWindows
	}
	return risk.Classify(command, os), nil
}

// netBrandOf 把 store.NetdevBrand 映射为风险分册键（与 risk 包枚举一致；
// 未知值落 generic 保守并集）。
func netBrandOf(b store.NetdevBrand) risk.NetBrand {
	switch b {
	case store.NetdevBrandCisco:
		return risk.NetBrandCisco
	case store.NetdevBrandHuawei:
		return risk.NetBrandHuawei
	case store.NetdevBrandH3C:
		return risk.NetBrandH3C
	case store.NetdevBrandJunos:
		return risk.NetBrandJunos
	default:
		return risk.NetBrandGeneric
	}
}

// Cancel 把 pending 审批置为 expired（流断开/服务关闭）。
//
// 使用 context.WithoutCancel + 5s 超时落库，避免请求 context 取消导致状态丢失。
func (s *Service) Cancel(ctx context.Context, approvalID, reason, requestID string) (store.Approval, error) {
	if s.st == nil {
		return store.Approval{}, ErrStoreUnavailable
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelWriteTimeout)
	defer cancel()

	now := s.clock().UTC()
	action := actionExpired
	if reason == cancelReasonStream {
		action = actionStreamCancelled
	}
	var ap store.Approval
	outboxUsed := false
	apply := func(tx store.Store) error {
		decided, err := tx.Approvals().Decide(writeCtx, store.DecideApprovalParams{
			ID:        approvalID,
			Status:    store.ApprovalExpired,
			DecidedBy: "",
			Note:      reason,
			DecidedAt: now,
		})
		if err != nil {
			return err
		}
		ap = decided
		used, err := s.emitAuditTx(ctx, tx, audit.Event{
			ActorType:      "system",
			ActorID:        "system",
			Action:         action,
			ResourceType:   "approval",
			ResourceID:     ap.ID,
			RequestID:      requestID,
			ConversationID: ap.ConversationID,
			ServerID:       ap.ServerID,
			Detail: map[string]any{
				"approval_id": ap.ID,
				"reason":      reason,
			},
		})
		outboxUsed = used
		return err
	}
	var err error
	if store.OutboxOf(s.st) != nil {
		err = s.withOutboxTx(writeCtx, apply)
	} else {
		err = apply(s.st)
	}
	if err != nil {
		return store.Approval{}, err
	}
	s.notify(approvalID, Decision{
		ApprovalID: ap.ID,
		Status:     store.ApprovalExpired,
		DecidedAt:  ap.DecidedAt,
		Note:       reason,
	})
	if !outboxUsed {
		s.emitAudit(ctx, audit.Event{
			ActorType:      "system",
			ActorID:        "system",
			Action:         action,
			ResourceType:   "approval",
			ResourceID:     ap.ID,
			RequestID:      requestID,
			ConversationID: ap.ConversationID,
			ServerID:       ap.ServerID,
			Detail: map[string]any{
				"approval_id": ap.ID,
				"reason":      reason,
			},
		})
	}
	return ap, nil
}

// expireCandidateLimit 是逐条过期审计的明细上限（防 OOM 铁律：写死数值）。
// 与 pendingExpireBatchSize 一致——一次扫描至多展开 200 条明细，超出部分
// 只体现在聚合行的 count 里。
const expireCandidateLimit = 200

// expireCandidateReason 是过期审计的 detail.reason（与 Cancel 的
// stream_cancelled / server_shutting_down 同字段口径）。
const expireCandidateReason = "ttl_expired"

// Expire 批量过期审批，返回条数。
//
// 六期打磨 sprint 6（走查 low）：此前只写一行聚合审计
// （resource_id 空、detail={count}），审计页「审批过期」筛选下目标与命令
// 摘要恒「—」，无法定位具体哪条审批、哪条命令过期。现在在批量 UPDATE
// **之前**抓一份到期候选快照（List pending，至多 expireCandidateLimit 条），
// UPDATE 成功后逐条 emit（带 approval_id/command/source/risk_level/
// approval_mode 门限快照/服务器），并保留聚合行（count 仍是真实过期条数）。
//
// 并发正确性：快照行可能在 UPDATE 前被 decide 抢先（approval 变
// approved/denied）。逐条 emit 前用 Get 复核 status==expired，只对确实
// 由本次扫描过期的行落明细，不误报。
func (s *Service) Expire(ctx context.Context, now time.Time) (int64, error) {
	if s.st == nil {
		return 0, ErrStoreUnavailable
	}
	now = now.UTC()
	// 走查第 3 轮 medium：已批准但已过 grant_expires_at 的终端/RDP 授权
	// 也必须收敛为 expired 并写终端授权过期审计。此前库里状态永远是
	// approved，只有前端按 grant_expires_at 临时推算「已过期」，审计页
	// 「终端授权过期」筛选恒空。放在 pending 过期之前：两者互不相关。
	if _, err := s.expireStaleGrants(ctx, now); err != nil {
		s.log.Warn("terminal_grant_expire_failed", "error", err.Error())
	}
	// 候选快照失败不阻断过期本身（过期是安全方向的收敛）；只丢明细。
	candidates, snapErr := s.expiringCandidates(ctx, now)
	if snapErr != nil && s.log != nil {
		s.log.Warn("approval_expire_snapshot_failed", "error", snapErr.Error())
	}

	// 第 4 轮走查 medium：pending 过期此前是「裸 UPDATE + 进程内审计队列」，
	// 进程在两步之间崩溃会留下「已过期但无审计」的审批，与契约 12.3 第 19
	// 条「审批过期与 audit_outbox 同事务」冲突。这里与 expireStaleGrants
	// 同口径：UPDATE 与逐条/聚合 audit_outbox 在同一 WithTx 内提交；
	// 存储未实现 outbox（内存 fake/骨架）时回落到进程内审计。
	var n int64
	var detailEvents []audit.Event
	var aggregate audit.Event
	apply := func(tx store.Store) error {
		var err error
		n, err = tx.Approvals().Expire(ctx, now)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		detailEvents = detailEvents[:0]
		expiredIDs := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			// 复核：并发 decide 抢先的行不是「过期」，不得落「已过期」事实。
			cur, gerr := tx.Approvals().Get(ctx, candidate.ID)
			if gerr != nil || cur.Status != store.ApprovalExpired {
				continue
			}
			expiredIDs = append(expiredIDs, cur.ID)
			detailEvents = append(detailEvents, audit.Event{
				ActorType:      "system",
				ActorID:        "system",
				Action:         actionExpired,
				ResourceType:   "approval",
				ResourceID:     cur.ID,
				ConversationID: cur.ConversationID,
				ServerID:       cur.ServerID,
				Detail: map[string]any{
					"approval_id":   cur.ID,
					"reason":        expireCandidateReason,
					"source":        cur.Source,
					"command":       cur.Command,
					"risk_level":    cur.RiskLevel,
					"approval_mode": string(cur.ApprovalMode),
					"server_name":   cur.ServerName,
				},
			})
		}
		// 聚合行保留（一次扫描一行的旧口径不变；detail 另带本次落过明细的
		// 审批 ID 列表，便于按 ID 反查——上限 expireCandidateLimit，超出时
		// truncated=true 表示明细未覆盖全部）。
		detail := map[string]any{"count": n}
		if len(expiredIDs) > 0 {
			detail["approval_ids"] = expiredIDs
		}
		if int64(len(candidates)) < n {
			detail["detail_truncated"] = true
		}
		aggregate = audit.Event{
			ActorType:    "system",
			ActorID:      "system",
			Action:       actionExpired,
			ResourceType: "approval",
			ResourceID:   "",
			Detail:       detail,
		}
		return nil
	}

	outbox := store.OutboxOf(s.st) != nil
	if outbox {
		err := s.withOutboxTx(ctx, func(tx store.Store) error {
			if err := apply(tx); err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			for _, ev := range detailEvents {
				if _, err := s.emitAuditTx(ctx, tx, ev); err != nil {
					return err
				}
			}
			_, err := s.emitAuditTx(ctx, tx, aggregate)
			return err
		})
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	if err := apply(s.st); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	for _, ev := range detailEvents {
		s.emitAudit(ctx, ev)
	}
	s.emitAudit(ctx, aggregate)
	return n, nil
}

// grantExpireBatchSize 是单轮收敛的过期授权上限（防 OOM 铁律：写死数值；
// 与 expireCandidateLimit 同量级，sweeper 每秒复扫，积压会连续清空）。
const grantExpireBatchSize = 200

// expireStaleGrants 把「已批准、未消费、grant_expires_at 已过」的终端/RDP
// 授权收敛为 expired，并在同一事务内逐条写 terminal.grant_expired 审计
// （走查第 3 轮 medium：此前只有前端按时间戳临时推算「已过期」，库里的
// 状态永远是 approved，审计页「终端授权过期」筛选恒空）。
//
// 存储未实现 GrantExpiryStore（内存 fake/骨架）时无操作。
func (s *Service) expireStaleGrants(ctx context.Context, now time.Time) (int64, error) {
	if store.GrantExpiryStoreOf(s.st) == nil {
		return 0, nil
	}
	var expired []store.Approval
	apply := func(tx store.Store) error {
		expiry := store.GrantExpiryStoreOf(tx)
		if expiry == nil {
			return nil
		}
		rows, err := expiry.ExpireStaleGrants(ctx, now, grantExpireBatchSize)
		if err != nil {
			return err
		}
		expired = rows
		for _, ap := range rows {
			if _, err := s.emitAuditTx(ctx, tx, grantExpiredEvent(ap, "grant_expired")); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if store.OutboxOf(s.st) != nil {
		err = s.withOutboxTx(ctx, apply)
	} else {
		err = apply(s.st)
	}
	if err != nil {
		return 0, err
	}
	if store.OutboxOf(s.st) == nil {
		for _, ap := range expired {
			s.emitAudit(ctx, grantExpiredEvent(ap, "grant_expired"))
		}
	}
	return int64(len(expired)), nil
}

// grantExpiredEvent 构造终端/RDP 授权过期的审计事件（契约 9.7 动作名
// terminal.grant_expired，resource_type=approval）。
func grantExpiredEvent(ap store.Approval, reason string) audit.Event {
	detail := map[string]any{
		"approval_id":  ap.ID,
		"reason":       reason,
		"grant_kind":   derefOrEmpty(ap.GrantKind),
		"server_id":    derefOrEmpty(ap.ServerID),
		"requested_by": ap.RequestedBy,
	}
	if ap.GrantExpiresAt != nil {
		detail["grant_expires_at"] = ap.GrantExpiresAt.UTC().Format(time.RFC3339)
	}
	if ap.ConsumedAt != nil {
		detail["consumed_at"] = ap.ConsumedAt.UTC().Format(time.RFC3339)
	}
	return audit.Event{
		ActorType:    "system",
		ActorID:      "system",
		Action:       actionGrantExpired,
		ResourceType: "approval",
		ResourceID:   ap.ID,
		ServerID:     ap.ServerID,
		Detail:       detail,
	}
}

// expiringCandidates 读取 pending 审批中已到期的行（逐条审计的明细来源）。// List 无游标（契约 3.3/4.9），因此只取一页 expireCandidateLimit 条——
// 有界读取，超出部分由聚合行的 count + detail_truncated 表达。
func (s *Service) expiringCandidates(ctx context.Context, now time.Time) ([]store.Approval, error) {
	items, err := s.st.Approvals().List(ctx, "", store.ApprovalPending, expireCandidateLimit)
	if err != nil {
		return nil, err
	}
	out := make([]store.Approval, 0, len(items))
	for _, ap := range items {
		if ap.Status == store.ApprovalPending && !ap.ExpiresAt.After(now) {
			out = append(out, ap)
		}
	}
	return out, nil
}

// Get 读取审批。
func (s *Service) Get(ctx context.Context, approvalID string) (store.Approval, error) {
	if s.st == nil {
		return store.Approval{}, ErrStoreUnavailable
	}
	return s.st.Approvals().Get(ctx, approvalID)
}

// expirePendingAfterRestart 把所有 pending 审批标记为 expired。
//
// 重启后没有等待者能接收决策，继续保留 pending 只会让前端误以为可以批准。
//
// 每条收敛都必须落 approval.expired 审计（走查第 3 轮 high：此前审计页
// 只有 approval.created，重启把 pending 置 expired 后审计员无法证明该
// 审批是被系统作废还是丢失）。审计与状态变更在同一事务内写 outbox；
// 存储不支持 outbox 时在循环内回落到进程内 Enqueue。
func (s *Service) expirePendingAfterRestart(ctx context.Context) {
	now := s.clock().UTC()
	for round := 0; round < pendingExpireMaxRounds; round++ {
		items, err := s.st.Approvals().List(ctx, "", store.ApprovalPending, pendingExpireBatchSize)
		if err != nil {
			s.log.Warn("approval_restart_expire_failed", "error", err.Error())
			return
		}
		if len(items) == 0 {
			return
		}
		for _, ap := range items {
			expired, err := s.expireOneAfterRestart(ctx, ap, now)
			if err != nil {
				if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
					continue
				}
				s.log.Warn("approval_restart_expire_failed", "approval_id", ap.ID, "error", err.Error())
				continue
			}
			s.notify(ap.ID, Decision{ApprovalID: expired.ID, Status: store.ApprovalExpired, DecidedAt: expired.DecidedAt, Note: cancelReasonShutdown})
		}
		if len(items) < pendingExpireBatchSize {
			return
		}
	}
}

// expireOneAfterRestart 收敛单条 pending 审批并写审计（同一事务）。
func (s *Service) expireOneAfterRestart(ctx context.Context, ap store.Approval, now time.Time) (store.Approval, error) {
	var expired store.Approval
	outboxUsed := false
	apply := func(tx store.Store) error {
		row, err := tx.Approvals().Decide(ctx, store.DecideApprovalParams{
			ID:        ap.ID,
			Status:    store.ApprovalExpired,
			Note:      cancelReasonShutdown,
			DecidedAt: now,
		})
		if err != nil {
			return err
		}
		expired = row
		used, err := s.emitAuditTx(ctx, tx, approvalExpiredEvent(row, cancelReasonShutdown))
		outboxUsed = used
		return err
	}
	var err error
	if store.OutboxOf(s.st) != nil {
		err = s.withOutboxTx(ctx, apply)
	} else {
		err = apply(s.st)
	}
	if err != nil {
		return store.Approval{}, err
	}
	if !outboxUsed {
		s.emitAudit(ctx, approvalExpiredEvent(expired, cancelReasonShutdown))
	}
	return expired, nil
}

// approvalExpiredEvent 构造审批作废的审计事件（reason 区分
// server_shutting_down / rbac:user_revoked / ttl_expired / grant_expired）。
func approvalExpiredEvent(ap store.Approval, reason string) audit.Event {
	detail := map[string]any{
		"approval_id":   ap.ID,
		"reason":        reason,
		"source":        ap.Source,
		"command":       ap.Command,
		"risk_level":    ap.RiskLevel,
		"requested_by":  ap.RequestedBy,
		"approval_mode": string(ap.ApprovalMode),
		"server_name":   ap.ServerName,
	}
	return audit.Event{
		ActorType:      "system",
		ActorID:        "system",
		Action:         actionExpired,
		ResourceType:   "approval",
		ResourceID:     ap.ID,
		ConversationID: ap.ConversationID,
		ServerID:       ap.ServerID,
		Detail:         detail,
	}
}

func (s *Service) notify(approvalID string, d Decision) {
	s.mu.Lock()
	set := s.waiters[approvalID]
	delete(s.waiters, approvalID)
	s.mu.Unlock()
	for _, ch := range set {
		select {
		case ch <- d:
		default:
		}
	}
}

func (s *Service) removeWaiter(approvalID string, ticket uint64, ch chan Decision) {
	s.mu.Lock()
	if set, ok := s.waiters[approvalID]; ok {
		if cur, ok := set[ticket]; ok && cur == ch {
			delete(set, ticket)
		}
		if len(set) == 0 {
			delete(s.waiters, approvalID)
		}
	}
	s.mu.Unlock()
}

// emitAudit 记录审计；审计失败只记日志，不回滚审批状态。
func (s *Service) emitAudit(ctx context.Context, e audit.Event) {
	if s.audit == nil {
		return
	}
	// audit writer 有自己的背压；这里给一个独立短超时，避免请求取消后丢事件。
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEnqueueTimeout)
	defer cancel()
	if err := s.audit.Enqueue(cctx, e); err != nil {
		s.log.Warn("approval_audit_enqueue_failed", "action", e.Action, "error", err.Error())
	}
}

func decisionOf(ap store.Approval) Decision {
	return Decision{
		ApprovalID:    ap.ID,
		Status:        ap.Status,
		DecidedAt:     ap.DecidedAt,
		Note:          ap.Note,
		EditedCommand: ap.EditedCommand,
	}
}

// conversationActorID 是审批创建审计的 actor：chat 审批取对话上下文，
// console 审批（容器页操作）无对话上下文，actor 为空（用户在 decide 时出现）。
func conversationActorID(conversationID *string) string {
	if conversationID == nil || *conversationID == "" {
		return ""
	}
	return "conversation:" + *conversationID
}

func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// trackRequester 记录进程内审批发起人；表有硬上限，满时清空重建
// （pending 审批受 TTL 约束，清空只会退化为「本进程内不再校验」，不会
// 无界增长；DB 的 requested_by 列由 store 层持久化）。
func (s *Service) trackRequester(approvalID, requestedBy string) {
	if approvalID == "" || requestedBy == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requesters == nil {
		s.requesters = make(map[string]string, 64)
	}
	if len(s.requesters) >= maxTrackedRequesters {
		s.requesters = make(map[string]string, 64)
	}
	s.requesters[approvalID] = requestedBy
}

// requesterOf 返回进程内记录的发起人；未记录时返回空串。
func (s *Service) requesterOf(approvalID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requesters[approvalID]
}

// NotifyRevoked 唤醒该用户所有仍在等待的审批（账号被停用/降级/吊销会话时
// 审批已由事务收敛为 expired，等待方必须立即结束，不能继续等到 TTL）。
//
// 只广播进程内已知道发起人的审批；不在 waiters 表中的审批无需通知。
func (s *Service) NotifyRevoked(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	s.mu.Lock()
	ids := make([]string, 0, 8)
	for id := range s.waiters {
		if s.requesters[id] == userID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		ap, err := s.Get(ctx, id)
		status := store.ApprovalExpired
		note := revokeNote
		if err == nil {
			status = ap.Status
			note = ap.Note
		}
		s.notify(id, Decision{ApprovalID: id, Status: status, Note: note})
	}
}

// RequestedBy 返回审批发起人（供 decide 执行前复检权限）。
func (s *Service) RequestedBy(approvalID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.requesters[approvalID]
	return v, ok && v != ""
}

func (s *Service) untrackRequester(approvalID string) {
	s.mu.Lock()
	delete(s.requesters, approvalID)
	s.mu.Unlock()
}
