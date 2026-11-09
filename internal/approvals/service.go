// Package approvals 实现命令审批的创建、等待、决策与超时。
//
// 状态机：pending → approved | denied | expired，只有 pending 可迁移。
// 等待者保存在进程内 map（审批 ID → 等待者 ticket → 带缓冲 channel）；
// 同一审批允许多个并发 Wait，Decide 先落库再广播通知全部等待者。
package approvals

import (
	"context"
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
	actionCreated         = "approval.created"
	actionApproved        = "approval.approved"
	actionDenied          = "approval.denied"
	actionExpired         = "approval.expired"
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
)

// ErrStoreUnavailable 表示未注入 store（仅骨架启动路径会出现）。
var ErrStoreUnavailable = errors.New("approvals: store unavailable")

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
}

// DecideRequest 是 decide 请求（httpapi → Service，六期 BE-P 结构体化）。
// EditedCommand 仅 approve 时允许非空（deny 携带返回 ErrInvalid）；
// 重分类在本函数内完成，httpapi 只透传用户输入、不得自行分类。
type DecideRequest struct {
	ApprovalID    string
	Approve       bool
	DecidedBy     string
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
	waitSeq uint64
	started bool

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
		st:      st,
		audit:   auditWriter,
		ttl:     ttl,
		clock:   clock,
		log:     log,
		waiters: make(map[string]map[uint64]chan Decision),
		stop:    make(chan struct{}),
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
	case "console", "batch":
		// 五期 M3（契约 4.26/2.10 约定 5）：batch 与 console 同语义——
		// conversation_id/tool_call_id 为 NULL（NULL 不参与 UNIQUE）。
		if req.ConversationID != nil || req.ToolCallID != nil {
			return store.Approval{}, store.ErrInvalid
		}
	default:
		return store.Approval{}, store.ErrInvalid
	}
	now := s.clock().UTC()
	ap, err := s.st.Approvals().Create(ctx, store.CreateApprovalParams{
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
		ExpiresAt:      now.Add(s.ttl),
		CreatedAt:      now,
	})
	if err != nil {
		return store.Approval{}, err
	}
	s.emitAudit(ctx, audit.Event{
		ActorType:      "model",
		ActorID:        conversationActorID(req.ConversationID),
		Action:         actionCreated,
		ResourceType:   "approval",
		ResourceID:     ap.ID,
		RequestID:      req.RequestID,
		ConversationID: req.ConversationID,
		ServerID:       req.ServerID,
		Detail: map[string]any{
			"approval_id":  ap.ID,
			"tool_call_id": derefOrEmpty(ap.ToolCallID),
			"source":       source,
			"risk_level":   req.RiskLevel,
			"expires_at":   ap.ExpiresAt.UTC().Format(time.RFC3339Nano),
		},
	})
	return ap, nil
}

// Wait 等待审批决策或超时。
//
// 返回 approved/denied/expired；ctx 取消返回 ctx.Err()，不改变数据库状态
// （调用方必须调用 Cancel 或由 sweeper 过期）。
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

	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		return Decision{}, ctx.Err()
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

// Decide 批准或拒绝审批（六期 BE-P 结构体化，契约 5.5/2.11 约定 3）。
// 先持久化，再通知等待者。改写流程：EditedCommand 非空时——
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
	now := s.clock().UTC()
	ap, err := s.st.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID:              req.ApprovalID,
		Status:          status,
		DecidedBy:       req.DecidedBy,
		Note:            req.Note,
		EditedCommand:   edited,
		EditedRiskLevel: editedRiskLevel,
		EditedRuleID:    editedRuleID,
		DecidedAt:       now,
	})
	if err != nil {
		return store.Approval{}, err
	}
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
	// 契约 9.7：批准另写 command.approved（命令生命周期动作，审计页
	//「命令批准」筛选的落地行）。source=batch 不在此写——批量审批的
	// command.approved 由 batchx.OnApprovalDecided 补齐 batch_job_id
	//（与 command.executed/command.denied 同口径），避免一次批准两行重复。
	if req.Approve && ap.Source != "batch" {
		s.emitCommandApproved(ctx, ap, req, edited, editedRiskLevel)
	}
	return ap, nil
}

// emitCommandApproved 写 command.approved 审计（契约 9.7）。资源锚定
// 服务器（与 command.executed 同口径）；审批无 server_id 时回落到审批行，
// 保证命令文本始终有出处可查。
func (s *Service) emitCommandApproved(ctx context.Context, ap store.Approval, req DecideRequest, edited, editedRiskLevel string) {
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
	s.emitAudit(ctx, audit.Event{
		ActorType:      "user",
		ActorID:        req.DecidedBy,
		Action:         actionCommandApproved,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		RequestID:      req.RequestID,
		ConversationID: ap.ConversationID,
		ServerID:       ap.ServerID,
		Detail:         commandDetail,
	})
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
	ap, err := s.st.Approvals().Decide(writeCtx, store.DecideApprovalParams{
		ID:        approvalID,
		Status:    store.ApprovalExpired,
		DecidedBy: "",
		Note:      reason,
		DecidedAt: now,
	})
	if err != nil {
		return store.Approval{}, err
	}
	s.notify(approvalID, Decision{
		ApprovalID: ap.ID,
		Status:     store.ApprovalExpired,
		DecidedAt:  ap.DecidedAt,
		Note:       reason,
	})

	action := actionExpired
	if reason == cancelReasonStream {
		action = actionStreamCancelled
	}
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
	// 候选快照失败不阻断过期本身（过期是安全方向的收敛）；只丢明细。
	candidates, snapErr := s.expiringCandidates(ctx, now)
	if snapErr != nil && s.log != nil {
		s.log.Warn("approval_expire_snapshot_failed", "error", snapErr.Error())
	}
	n, err := s.st.Approvals().Expire(ctx, now)
	if err != nil {
		return n, err
	}
	if n == 0 {
		return 0, nil
	}
	expiredIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		// 复核：并发 decide 抢先的行不是「过期」，不得落「已过期」事实。
		cur, gerr := s.st.Approvals().Get(ctx, candidate.ID)
		if gerr != nil || cur.Status != store.ApprovalExpired {
			continue
		}
		expiredIDs = append(expiredIDs, cur.ID)
		s.emitAudit(ctx, audit.Event{
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
	s.emitAudit(ctx, audit.Event{
		ActorType:    "system",
		ActorID:      "system",
		Action:       actionExpired,
		ResourceType: "approval",
		ResourceID:   "",
		Detail:       detail,
	})
	return n, nil
}

// expiringCandidates 读取 pending 审批中已到期的行（逐条审计的明细来源）。
// List 无游标（契约 3.3/4.9），因此只取一页 expireCandidateLimit 条——
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
			expired, err := s.st.Approvals().Decide(ctx, store.DecideApprovalParams{
				ID:        ap.ID,
				Status:    store.ApprovalExpired,
				Note:      cancelReasonShutdown,
				DecidedAt: now,
			})
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
