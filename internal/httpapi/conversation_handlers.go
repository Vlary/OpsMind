package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/agent"
	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/store"
)

type conversationCreateRequest struct {
	Title string `json:"title"`
}

// conversationUpdateRequest 是 PATCH /api/conversations/:id 请求体（六期
// BE-P：title 与/或 approval_mode 至少一项；approval_mode 为 null 表示
// 清除会话覆盖、回到跟随全局门限——契约 2.11 约定 1）。
//
// ApprovalMode 用 RawMessage 精确区分三种态：缺失（len==0，未提供）、
// 显式 null（清除覆盖）、合法枚举字符串（设置覆盖）。
type conversationUpdateRequest struct {
	Title        *string         `json:"title"`
	ApprovalMode json.RawMessage `json:"approval_mode"`
}

type messagePageResponse struct {
	Items         []store.Message `json:"items"`
	HasMore       bool            `json:"has_more"`
	NextBeforeSeq *int64          `json:"next_before_seq"`
}

type toolCallPageResponse struct {
	Items         []store.ToolCall `json:"items"`
	HasMore       bool             `json:"has_more"`
	NextBeforeSeq *int64           `json:"next_before_seq"`
}

type conversationPageResponse struct {
	Items      []store.Conversation `json:"items"`
	HasMore    bool                 `json:"has_more"`
	NextCursor *conversationCursor  `json:"next_cursor"`
}

// conversationCursor 是会话列表游标（上一页末条的排序键），
// 前端原样回传给下一页请求。
type conversationCursor struct {
	LastMessageAt time.Time `json:"last_message_at"`
	ID            string    `json:"id"`
}

// fillConversationModes 装配 Conversation.EffectiveApprovalMode（六期
// BE-P，契约 3.4：store 读方法恒空串，handler 读取 settings 后统一填充；
// 一次请求至多一次 settings 读——批量列表共享同一个全局门限）。
func (d *Deps) fillConversationModes(c *gin.Context, items []store.Conversation) bool {
	if len(items) == 0 {
		return true
	}
	global, err := approvals.GlobalMode(c.Request.Context(), d.Store)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return false
	}
	for i := range items {
		items[i].EffectiveApprovalMode = approvals.ResolveMode(items[i], global)
	}
	return true
}

// fillConversationMode 是单会话包装：返回装配后的值。
func (d *Deps) fillConversationMode(c *gin.Context, conv store.Conversation) (store.Conversation, bool) {
	items := []store.Conversation{conv}
	if !d.fillConversationModes(c, items) {
		return store.Conversation{}, false
	}
	return items[0], true
}

func (d *Deps) handleListConversations(c *gin.Context) {
	limit, ok := queryConversationLimit(c)
	if !ok {
		return
	}
	cursor, ok := queryConversationCursor(c)
	if !ok {
		return
	}
	items, hasMore, err := d.Store.Conversations().ListPage(c.Request.Context(), cursor, limit)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.Conversation{}
	}
	if !d.fillConversationModes(c, items) {
		return
	}
	var next *conversationCursor
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = &conversationCursor{LastMessageAt: last.LastMessageAt, ID: last.ID}
	}
	c.JSON(http.StatusOK, conversationPageResponse{Items: items, HasMore: hasMore, NextCursor: next})
}

func (d *Deps) handleCreateConversation(c *gin.Context) {
	var req conversationCreateRequest
	hasBody := c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0
	if hasBody {
		if !bindJSONStrict(c, &req) {
			return
		}
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "新会话"
	}
	if n := utf8.RuneCountInString(title); n < 1 || n > 200 {
		writeError(c, apierr.ValidationFailed, "title must be between 1 and 200 characters")
		return
	}

	conv, err := d.Store.Conversations().Create(c.Request.Context(), store.CreateConversationParams{Title: title})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	user, _ := userFromContext(c)
	d.auditEnqueue(c, audit.Event{
		ActorType:    "user",
		ActorID:      user.ID,
		Action:       "conversation.create",
		ResourceType: "conversation",
		ResourceID:   conv.ID,
		Detail:       map[string]any{"title": conv.Title},
	})
	conv, ok := d.fillConversationMode(c, conv)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, conv)
}

func (d *Deps) handleGetConversation(c *gin.Context) {
	conv, err := d.Store.Conversations().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	conv, ok := d.fillConversationMode(c, conv)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, conv)
}

// handleUpdateConversation 处理 PATCH：title 重命名与/或 approval_mode
// 覆盖（六期 BE-P，契约 2.11 约定 1）。title 走 conversation.rename 审计；
// 门限覆盖属设置变更，走 settings.update（resource_id=
// 'conversation:<id>:approval_mode'，契约 9.7）。
func (d *Deps) handleUpdateConversation(c *gin.Context) {
	var req conversationUpdateRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	hasTitle := req.Title != nil
	hasMode := len(req.ApprovalMode) > 0
	if !hasTitle && !hasMode {
		writeError(c, apierr.ValidationFailed, "title or approval_mode is required")
		return
	}

	convID := c.Param("id")
	var conv store.Conversation
	if hasTitle {
		title := strings.TrimSpace(*req.Title)
		if n := utf8.RuneCountInString(title); n < 1 || n > 200 {
			writeError(c, apierr.ValidationFailed, "title must be between 1 and 200 characters")
			return
		}
		var err error
		conv, err = d.Store.Conversations().UpdateTitle(c.Request.Context(), convID, title, d.Clock())
		if err != nil {
			d.handleStoreError(c, err, apierr.NotFound)
			return
		}
		user, _ := userFromContext(c)
		d.auditEnqueue(c, audit.Event{
			ActorType:    "user",
			ActorID:      user.ID,
			Action:       "conversation.rename",
			ResourceType: "conversation",
			ResourceID:   conv.ID,
			Detail:       map[string]any{"title": conv.Title},
		})
	}
	if hasMode {
		var mode *store.ApprovalMode
		value := "null"
		if string(req.ApprovalMode) != "null" {
			var raw string
			if err := json.Unmarshal(req.ApprovalMode, &raw); err != nil {
				writeError(c, apierr.ValidationFailed, "approval_mode must be one of all, standard, high, none or null")
				return
			}
			m := store.ApprovalMode(raw)
			if !store.ValidApprovalMode(m) {
				writeError(c, apierr.ValidationFailed, "approval_mode must be one of all, standard, high, none or null")
				return
			}
			mode = &m
			value = raw
		}
		var err error
		conv, err = d.Store.Conversations().SetApprovalMode(c.Request.Context(), convID, mode, d.Clock())
		if err != nil {
			d.handleStoreError(c, err, apierr.NotFound)
			return
		}
		user, _ := userFromContext(c)
		d.auditEnqueue(c, audit.Event{
			ActorType:    "user",
			ActorID:      user.ID,
			Action:       "settings.update",
			ResourceType: "setting",
			ResourceID:   "conversation:" + convID + ":approval_mode",
			Detail:       map[string]any{"key": "approval_mode", "value": value, "conversation_id": convID},
		})
	}
	conv, ok := d.fillConversationMode(c, conv)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, conv)
}

func (d *Deps) handleDeleteConversation(c *gin.Context) {
	id := c.Param("id")
	if d.Streams != nil && d.Streams.Active(id) {
		writeError(c, apierr.StreamActive, "conversation already has an active stream")
		return
	}
	if err := d.Store.Conversations().Delete(c.Request.Context(), id); err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	user, _ := userFromContext(c)
	d.auditEnqueue(c, audit.Event{
		ActorType:    "user",
		ActorID:      user.ID,
		Action:       "conversation.delete",
		ResourceType: "conversation",
		ResourceID:   id,
	})
	c.Status(http.StatusNoContent)
}

func (d *Deps) handleListMessages(c *gin.Context) {
	beforeSeq, ok := queryBeforeSeq(c)
	if !ok {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	items, hasMore, err := d.Store.Messages().ListPage(c.Request.Context(), c.Param("id"), beforeSeq, limit)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.Message{}
	}
	var next *int64
	if hasMore && len(items) > 0 {
		seq := items[0].Seq
		next = &seq
	}
	c.JSON(http.StatusOK, messagePageResponse{Items: items, HasMore: hasMore, NextBeforeSeq: next})
}

func (d *Deps) handleListToolCalls(c *gin.Context) {
	beforeSeq, ok := queryBeforeSeq(c)
	if !ok {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	items, hasMore, err := d.Store.ToolCalls().ListPage(c.Request.Context(), c.Param("id"), beforeSeq, limit)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.ToolCall{}
	}
	var next *int64
	if hasMore && len(items) > 0 {
		seq := items[0].Seq
		next = &seq
	}
	c.JSON(http.StatusOK, toolCallPageResponse{Items: items, HasMore: hasMore, NextBeforeSeq: next})
}

// ---------------------------------------------------------------------------
// 六期 BE-P：会话执行回放（契约 2.11 约定 4 / openapi ReplayStep）
// ---------------------------------------------------------------------------

// 回放上限（契约 11 节第 17/21 条，写死）：步数 20000，超出丢弃最旧并置
// truncated=true；分页页大小与单表迭代上限是防 OOM 的终止条件。
const (
	replayMaxSteps       = 20000
	replayPageSize       = 200
	replayMaxPagesPerRow = 101 // 200×101 > 20000，任何单表超过即由步数上限截断

	// 八期体积预算（契约 2.12 约定 6 / 11 节第 21 条，写死）：
	//   - arguments 装载累计 6 MiB（按 tool_calls.seq 升序累加原始 len）；
	//   - 响应序列化 8 MiB（超限从最旧步起清 output；仍超限丢弃最旧步）。
	replayArgumentsBudgetBytes   = 6 << 20
	replaySerializedBudgetBytes  = 8 << 20
	replaySerializedStepOverhead = 1024 // 单步非 output/arguments 部分的估算上界

	// replayBatchTargetPrefetchLimit 是单请求为 affected_servers 预取
	// batch_jobs.server_ids 的任务数上限（防 N+1 与无界查询；零值超限的
	// job 回退用模型参数——契约 7.8 允许的回退路径）。
	replayBatchTargetPrefetchLimit = 20
)

// truncated_reason 的取值（六期打磨 sprint 4；八期扩 payload）：前端据此选
// 文案。优先级 steps > payload > approvals（只报最高优先级的一个——前两者
// 确实丢了步骤内容）。
const (
	// replayTruncatedSteps：步数超 replayMaxSteps，丢弃最旧步骤。
	replayTruncatedSteps = "steps"
	// replayTruncatedApprovals：单状态审批满 replayPageSize 条（approvals
	// .List 无游标，4×200=800 上限），更早的审批未纳入回放。
	replayTruncatedApprovals = "approvals"
	// replayTruncatedPayload：八期体积预算命中（arguments 超 6 MiB 或响应
	// 序列化超 8 MiB），部分 arguments/output 被清空。
	replayTruncatedPayload = "payload"
)

// replayStepKind 是回放步类型；数值即同 timestamp 内的排序权重（契约
// 2.11 约定 4：user_message=0 < tool_call=1 < approval=2 < execution=3
// < reasoning=4 < assistant_text=5，近似真实因果序）。
const (
	replayKindUserMessage = iota
	replayKindToolCall
	replayKindApproval
	replayKindExecution
	replayKindReasoning
	replayKindAssistantText
)

func replayKindName(w int) string {
	switch w {
	case replayKindUserMessage:
		return "user_message"
	case replayKindToolCall:
		return "tool_call"
	case replayKindApproval:
		return "approval"
	case replayKindExecution:
		return "execution"
	case replayKindReasoning:
		return "reasoning"
	default:
		return "assistant_text"
	}
}

// replayStep 是重建后的回放步（openapi ReplayStep：字段全集写死，未用
// 字段为 null、不缺省键）。
type replayStep struct {
	Seq        int64     `json:"seq"`
	Timestamp  time.Time `json:"timestamp"`
	Kind       string    `json:"kind"`
	MessageID  *string   `json:"message_id"`
	ToolCallID *string   `json:"tool_call_id"`
	ApprovalID *string   `json:"approval_id"`
	Name       *string   `json:"name"`
	Source     *string   `json:"source"`
	ServerName *string   `json:"server_name"`
	Content    *string   `json:"content"`
	Command    *string   `json:"command"`
	// OriginalCommand 仅审批被改写时非 null（原命令历史事实）。
	OriginalCommand *string         `json:"original_command"`
	Arguments       json.RawMessage `json:"arguments"`
	RiskLevel       *string         `json:"risk_level"`
	EditedRiskLevel *string         `json:"edited_risk_level"`
	Status          *string         `json:"status"`
	ExitCode        *int            `json:"exit_code"`
	Output          *string         `json:"output"`
	ErrorCode       *string         `json:"error_code"`
	DurationMS      *int64          `json:"duration_ms"`
	// ApprovalMode 仅命令执行类工具的 tool_call 步非 null（六期打磨 sprint 2）：
	// 取自 tool_calls.approval_mode（判定时的有效门限快照）。回放据此把
	// none/high 档「确实没走审批」的 needs_approval 命令显示为「免审批执行」，
	// 与聊天工具卡（toolCallBadges.isExemptExecution）同一口径；空串=旧行
	// 未记录，如实返回 null（前端不渲染门限徽标）。
	ApprovalMode *string `json:"approval_mode"`
	// ReasoningDurationMS 仅 reasoning 步非 null（六期打磨 sprint 1）：
	// 取自 messages.reasoning_duration_ms，回放摘要「深度思考 · 耗时 Ns」
	// 与消息页一致；00009 之前的旧消息无计时数据为 null。
	ReasoningDurationMS *int64 `json:"reasoning_duration_ms"`

	// ---- 八期（BE-R）：AI 操作全量可见 ----

	// ResultSummary 结果一句话摘要：tool_call 步恒非 null（18 个工具；失败为
	// error=<code>），execution 步恒非 null（11 个动作类工具）。≤200 rune，
	// 超出截断补 "…"（此时 ResultTruncated=true）。
	ResultSummary *string `json:"result_summary"`
	// ResultTruncated 该步输出是否被截断（仅 execution 步非 null）：这是
	// 「输出是否完整」的唯一判据。来源：工具自身的 truncated 字段、整包 64KB
	// 截断包装、摘要自身截断、output 因 8 MiB 预算被清空。
	ResultTruncated *bool `json:"result_truncated"`
	// AffectedServers 该动作实际触达的服务器名清单（仅 execution 步非 null）：
	// 单目标 1 元素、批量/巡检多元素、db_query 为 null（外部数据库不是
	// 服务器资产）。已删除资产以 `<已删除>` 占位。
	AffectedServers []string `json:"affected_servers"`
	// DiagnosisID / InspectionReportID / BatchJobID 是资源句柄（八期，仅
	// run_diagnosis / run_inspection / batch_execute 的 execution 步非 null），
	// 供播放器给出深链。
	DiagnosisID        *string `json:"diagnosis_id"`
	InspectionReportID *string `json:"inspection_report_id"`
	BatchJobID         *string `json:"batch_job_id"`

	// 排序键（不序列化）：timestamp ASC → kind 权重 → 来源键（messages/
	// tool_calls=seq 补零；approvals=requested_at,id）。
	kindWeight int
	sourceKey  string
}

// handleConversationReplay 从持久化表（messages/tool_calls/approvals）
// 重建会话的有序事件序列（契约 2.11 约定 4：不建回放表；仅 handler 内
// 合并三表既有分页查询，不新增 store 方法）。权限同 messages 端点
// （登录态；v1 单管理员，会话存在性即 ACL——不存在返回 404）。
func (d *Deps) handleConversationReplay(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	ctx := c.Request.Context()
	convID := c.Param("id")
	if _, err := d.Store.Conversations().Get(ctx, convID); err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}

	msgs, ok := d.replayAllMessages(c, convID)
	if !ok {
		return
	}
	calls, ok := d.replayAllToolCalls(c, convID)
	if !ok {
		return
	}
	aps, approvalsComplete := d.replayAllApprovals(c, convID)
	if !approvalsComplete && aps == nil {
		return // store 错误（非截断）。
	}

	// 八期（BE-R）契约 5.19.2 第 4 条：affected_servers 的装配在请求开始时
	// 一次 servers.List 建 id→资产 映射（失败不阻断回放：全部输出
	// `<已删除>` 占位并记日志），不得在循环里逐台 Get。
	servers := d.replayServerMap(c)
	batchTargets := d.replayBatchTargets(c, calls)

	steps := buildReplaySteps(msgs, calls, aps, servers, batchTargets)

	// 排序键写死：timestamp ASC，同 timestamp 按 kind 权重，同权重按来源
	// 键升序；排序后统一重编 seq=1..N。
	sort.SliceStable(steps, func(i, j int) bool {
		if !steps[i].Timestamp.Equal(steps[j].Timestamp) {
			return steps[i].Timestamp.Before(steps[j].Timestamp)
		}
		if steps[i].kindWeight != steps[j].kindWeight {
			return steps[i].kindWeight < steps[j].kindWeight
		}
		return steps[i].sourceKey < steps[j].sourceKey
	})
	// 截断原因随响应下发（六期打磨 sprint 4 走查 low）：此前两种截断共用
	// 一个 truncated 标志——单状态审批满页（replayAllApprovals 的
	// 4×200 上限）与步数超 20000 都置 true，前端文案硬编码「超出 20000 步
	// 上限」，用户看到 200 步却被告知 20000 上限，自相矛盾。
	// 取值：''（未截断）/ steps（步数上限）/ payload（八期体积预算）/
	// approvals（审批满页）。
	steps, truncated, truncatedReason := replayApplyTruncation(steps, approvalsComplete)
	// 八期（契约 2.12 约定 6）：先应用 arguments 装载预算（按 seq 升序，
	// 超 6 MiB 后更晚的 tool_call 步 arguments=null），再应用响应序列化
	// 预算（超 8 MiB 从最旧步起清 output；仍超限丢弃最旧步）。
	// 优先级 steps > payload > approvals，只报最高者。
	var payloadTruncated, stepsDropped bool
	steps, payloadTruncated, stepsDropped = replayApplyPayloadBudget(
		steps, replayArgumentsBudgetBytes, replaySerializedBudgetBytes)
	if stepsDropped {
		truncated = true
		truncatedReason = replayTruncatedSteps
	} else if payloadTruncated {
		truncated = true
		if truncatedReason != replayTruncatedSteps {
			truncatedReason = replayTruncatedPayload
		}
	}
	for i := range steps {
		steps[i].Seq = int64(i + 1)
	}
	if steps == nil {
		steps = []replayStep{}
	}
	c.JSON(http.StatusOK, gin.H{
		"conversation_id":  convID,
		"steps":            steps,
		"truncated":        truncated,
		"truncated_reason": truncatedReason,
	})
}

// replayApplyTruncation 应用回放的两道上限并返回截断原因（六期打磨
// sprint 4）。两道上限相互独立：
//   - approvalsComplete=false：单状态审批满 replayPageSize 条，更早审批
//     未纳入（approvals.List 无游标，契约 3.4 禁止为回放新增 store 方法）；
//   - 步数超 replayMaxSteps：丢弃最旧步骤（11 节防 OOM）。
//
// 两者同时命中时以步数为准（步骤确实被丢弃，用户更需知道 20000 口径）；
// 无论哪条，seq 的重编由调用方完成。
func replayApplyTruncation(steps []replayStep, approvalsComplete bool) ([]replayStep, bool, string) {
	truncated := false
	reason := ""
	if !approvalsComplete {
		truncated = true
		reason = replayTruncatedApprovals
	}
	if len(steps) > replayMaxSteps {
		steps = steps[len(steps)-replayMaxSteps:]
		truncated = true
		reason = replayTruncatedSteps
	}
	return steps, truncated, reason
}

// replayAllMessages 翻页拉取会话全部消息（ListPage beforeSeq 游标循环；
// 页数上限=终止条件）。
func (d *Deps) replayAllMessages(c *gin.Context, convID string) ([]store.Message, bool) {
	var out []store.Message
	before := int64(0)
	for page := 0; page < replayMaxPagesPerRow; page++ {
		items, hasMore, err := d.Store.Messages().ListPage(c.Request.Context(), convID, before, replayPageSize)
		if err != nil {
			d.handleStoreError(c, err, apierr.NotFound)
			return nil, false
		}
		out = append(out, items...)
		if !hasMore || len(items) == 0 {
			return out, true
		}
		before = items[len(items)-1].Seq
	}
	return out, true
}

// replayAllToolCalls 翻页拉取会话全部工具调用（同上）。
func (d *Deps) replayAllToolCalls(c *gin.Context, convID string) ([]store.ToolCall, bool) {
	var out []store.ToolCall
	before := int64(0)
	for page := 0; page < replayMaxPagesPerRow; page++ {
		items, hasMore, err := d.Store.ToolCalls().ListPage(c.Request.Context(), convID, before, replayPageSize)
		if err != nil {
			d.handleStoreError(c, err, apierr.NotFound)
			return nil, false
		}
		out = append(out, items...)
		if !hasMore || len(items) == 0 {
			return out, true
		}
		before = items[len(items)-1].Seq
	}
	return out, true
}

// replayAllApprovals 拉取会话全部审批。approvals.List 无游标（契约 3.4
// 禁止为回放新增 store 方法），按 status 四分片拉取（4×200=800 上限）；
// 单状态超 200 条的极端场景由 20000 步上限兜底（chat 审批 TTL 120s，
// 实际会话远达不到）。
func (d *Deps) replayAllApprovals(c *gin.Context, convID string) ([]store.Approval, bool) {
	var out []store.Approval
	truncated := false
	for _, status := range []store.ApprovalStatus{
		store.ApprovalPending, store.ApprovalApproved, store.ApprovalDenied, store.ApprovalExpired,
	} {
		items, err := d.Store.Approvals().List(c.Request.Context(), convID, status, replayPageSize)
		if err != nil {
			d.handleStoreError(c, err, apierr.NotFound)
			return nil, false
		}
		// List 无游标（固定 ORDER BY requested_at DESC LIMIT n）：返回满页
		// 即可能存在第 201 条起的更早审批——静默丢弃会破坏回放完整性，
		// 置 truncated=true 如实告知（六期评审发现；契约 2.11 约定 4
		// 「超出丢弃最旧并置 truncated」）。
		if len(items) >= replayPageSize {
			truncated = true
		}
		out = append(out, items...)
	}
	return out, !truncated
}

// replayContextPrefix 匹配 ChatPage 附加在 user 消息前的服务器/数据库上下文
// 标记（`<!--opsmind-context … -->\n`，前端 markdownBlocks.stripContextPrefix
// 同款口径）。回放只展示用户原文，内部上下文不呈现给用户。
var replayContextPrefix = regexp.MustCompile(`^<!--\s*opsmind-context(?s:.*?)-->\s*`)

// stripReplayContextPrefix 去掉 user 消息的内部上下文前缀（六期打磨 sprint 2
// 走查：回放此前原样展示 `<!--opsmind-context\nservers: …\n-->`，聊天气泡
// 则已剥离，两者不一致）。
func stripReplayContextPrefix(content string) string {
	if strings.HasPrefix(content, "<!--") {
		return replayContextPrefix.ReplaceAllString(content, "")
	}
	return content
}

// replayActionToolSet 是动作类工具判据（契约 5.19.2 第 1 条）：`execution` 步
// 只对 agent.ReplayActionTools() 的 11 个工具生成。名单来自 agent 包
// 具名导出，**禁止在此复制字面量**（7.8 / 11 节第 22 条）；包级初始化一次。
var replayActionToolSet = func() map[string]struct{} {
	names := agent.ReplayActionTools()
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}()

// replayIsActionTool 判断工具是否触达真实系统（动作类，会生成 execution 步）。
func replayIsActionTool(name string) bool {
	_, ok := replayActionToolSet[name]
	return ok
}

// replayDeletedServerPlaceholder 是服务器资产已被删除时的 affected_servers
// 占位（契约 7.8：不因删除资产而丢步骤）。
const replayDeletedServerPlaceholder = "<已删除>"

// replayServerRef 是回放装配所需的最小资产事实：名字（affected_servers）与
// OS/设备形态（server_facts 的模板标识 facts:linux|windows|brand）。
type replayServerRef struct {
	Name             string
	OS               store.ServerOS
	DeviceKind       store.DeviceKind
	NetdevBrand      store.NetdevBrand
	ContainerRuntime store.ContainerRuntime
}

// replayServerMap 一次 servers.List 建 id→资产 映射（契约 5.19.2 第 4 条：
// 每请求至多一次，禁止循环 Get）。失败不阻断回放——返回空映射，映射不到的
// id 一律以 `<已删除>` 占位并记日志。
func (d *Deps) replayServerMap(c *gin.Context) map[string]replayServerRef {
	servers, err := d.Store.Servers().List(c.Request.Context())
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("replay_servers_list_failed",
				"request_id", requestIDOf(c),
				"error", err.Error(),
			)
		}
		return map[string]replayServerRef{}
	}
	refs := make(map[string]replayServerRef, len(servers))
	for _, s := range servers {
		refs[s.ID] = replayServerRef{
			Name: s.Name, OS: s.OS, DeviceKind: s.DeviceKind,
			NetdevBrand: s.NetdevBrand, ContainerRuntime: s.ContainerRuntime,
		}
	}
	return refs
}

// replayAffectedServers 把一组服务器 id 装配成名字清单（契约 7.8）：
// 映射不到（资产已删）输出 `<已删除>` 占位，**按 id 升序**稳定——同一动作的
// 多次回放在服务器删除前后、以及参数乱序时都保持同样的顺序；结果恒非 nil
// 切片（无有效 id 时为空切片，由调用方决定是否置 null）。
func replayAffectedServers(ids []string, servers map[string]replayServerRef) []string {
	pairs := make([]struct{ id, name string }, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		name, ok := servers[id]
		pairs = append(pairs, struct{ id, name string }{id: id, name: name.Name})
		if !ok {
			pairs[len(pairs)-1].name = replayDeletedServerPlaceholder
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	if len(pairs) == 0 {
		// 无有效目标 → null 而不是 []（契约 7.8：不得用 [] 冒充「触达了
		// 零台」；目标不可解析时如实表达「没有目标事实」）。
		return nil
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.name)
	}
	return out
}

// replayFactsTemplate 返回 server_facts 的模板标识（契约 2.12 约定 3：
// facts:linux / facts:windows / facts:<brand>）；资产已删除时回退 facts。
func replayFactsTemplate(serverID string, servers map[string]replayServerRef) string {
	ref, ok := servers[serverID]
	if !ok {
		return "facts"
	}
	if ref.DeviceKind == store.DeviceKindNetworkDevice {
		if ref.NetdevBrand != "" {
			return "facts:" + string(ref.NetdevBrand)
		}
		return "facts"
	}
	switch ref.OS {
	case store.ServerOSWindows:
		return "facts:windows"
	case store.ServerOSLinux, "":
		return "facts:linux"
	}
	return "facts"
}

// replayArgumentString / replayArgumentStrings 从 tool_calls.arguments 取键
// （模型参数事实，用于 command 动作串与 affected_servers 的回退路径）。
func replayArgumentString(args json.RawMessage, key string) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func replayArgumentStrings(args json.RawMessage, key string) []string {
	if len(args) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// buildReplaySteps 把三表行展开为回放步（顺序由调用方统一排序）。
//
// 八期（BE-R，契约 2.12 约定 3 / 5.19.2）：
//   - execution 步的生成范围从 2 个命令工具扩到 replayIsActionTool 的 11 个
//     动作类工具（判据 = agent.ReplayActionTools 共享清单）；
//   - 全部 18 个工具的 tool_call 步都带 result_summary；
//   - servers 是请求开始时一次 servers.List 的 id→资产 映射（可为空）；
//   - batchTargets 是 batch_jobs.id → 真实目标 server_ids 的预取结果
//     （契约 7.8：批量动作的 affected_servers 不得只信模型参数）。
func buildReplaySteps(msgs []store.Message, calls []store.ToolCall, aps []store.Approval, servers map[string]replayServerRef, batchTargets map[string][]string) []replayStep {
	steps := make([]replayStep, 0, len(msgs)+len(calls)+len(aps))

	for i := range msgs {
		m := msgs[i]
		key := fmt.Sprintf("%020d", m.Seq)
		switch m.Role {
		case store.RoleUser:
			content := stripReplayContextPrefix(m.Content)
			steps = append(steps, replayStep{
				Timestamp: m.CreatedAt, kindWeight: replayKindUserMessage, sourceKey: key,
				Kind: replayKindName(replayKindUserMessage), MessageID: &m.ID, Content: &content,
			})
		default:
			// reasoning 一期建列、runner 落库；历史行为空串即无 reasoning 步。
			if m.Reasoning != "" {
				reasoning := m.Reasoning
				steps = append(steps, replayStep{
					Timestamp: m.CreatedAt, kindWeight: replayKindReasoning, sourceKey: key,
					Kind: replayKindName(replayKindReasoning), MessageID: &m.ID, Content: &reasoning,
					// 六期打磨 sprint 1：回放 reasoning 步带服务端落库的思考
					// 耗时（此前恒 null，回放摘要永远显示「耗时 —」）。
					ReasoningDurationMS: m.ReasoningDurationMS,
				})
			}
			content := m.Content
			steps = append(steps, replayStep{
				Timestamp: m.CreatedAt, kindWeight: replayKindAssistantText, sourceKey: key,
				Kind: replayKindName(replayKindAssistantText), MessageID: &m.ID, Content: &content,
			})
		}
	}

	for i := range calls {
		tc := calls[i]
		key := fmt.Sprintf("%020d", tc.Seq)
		name := tc.Name
		riskLevel := tc.RiskLevel
		status := string(tc.Status)
		step := replayStep{
			Timestamp: tc.StartedAt, kindWeight: replayKindToolCall, sourceKey: key,
			Kind: replayKindName(replayKindToolCall), ToolCallID: &tc.ID, Name: &name,
			Arguments: tc.Arguments, RiskLevel: &riskLevel, Status: &status,
		}
		// 六期打磨 sprint 2：tool_call 步补齐 error_code（openapi 早已声明，
		// 此前恒 null——失败的调用在回放里看不出任何原因）；approval_mode 供
		// 前端复用免审批判定（none/high 档直执行不再显示「需审批」）。
		if tc.ErrorCode != "" {
			errorCode := tc.ErrorCode
			step.ErrorCode = &errorCode
		}
		if tc.ApprovalMode != "" {
			approvalMode := tc.ApprovalMode
			step.ApprovalMode = &approvalMode
		}
		// 八期（契约 2.12 约定 3）：全部 18 个工具的 tool_call 步恒非 null 的
		// result_summary——解决「除两个工具外全都看不到结果」的根因。摘要自身
		// 超长时由 replayResultSummary 截断补 "…"；tool_call 步的
		// result_truncated 恒 null（契约：该字段只属于 execution 步）。
		summary, _ := replayResultSummary(tc.Name, tc.Result, tc.ErrorCode)
		step.ResultSummary = &summary
		steps = append(steps, step)
		// execution 步：仅动作类工具（11 个），从 result JSON 按工具取键
		// （result 已受 ToolResultBytes 64KB 截断，回放不再截断）；timestamp=
		// finished_at ?? started_at。
		if replayIsActionTool(tc.Name) {
			if ex, ok := replayExecutionStep(tc, key, servers, batchTargets); ok {
				steps = append(steps, ex)
			}
		}
	}

	for i := range aps {
		ap := aps[i]
		key := ap.RequestedAt.Format(time.RFC3339Nano) + ":" + ap.ID
		command := ap.Command
		if ap.EditedCommand != "" {
			// 实际执行命令=edited ?? original（契约 3.4 六期约束，回放/
			// 审计展示/UI 同源）。
			command = ap.EditedCommand
		}
		source := ap.Source
		serverName := ap.ServerName
		riskLevel := ap.RiskLevel
		status := string(ap.Status)
		step := replayStep{
			Timestamp: ap.RequestedAt, kindWeight: replayKindApproval, sourceKey: key,
			Kind: replayKindName(replayKindApproval), ApprovalID: &ap.ID, ToolCallID: ap.ToolCallID,
			Source: &source, ServerName: &serverName, Command: &command,
			RiskLevel: &riskLevel, Status: &status,
		}
		if ap.EditedCommand != "" {
			original := ap.Command
			editedLevel := ap.EditedRiskLevel
			step.OriginalCommand = &original
			step.EditedRiskLevel = &editedLevel
		}
		steps = append(steps, step)
	}
	return steps
}

// replayResultFields 是 tool_calls.result 的八期读取视图（按工具取键，契约
// 7.8 的表）。同一 JSON 只解析一次；字段按存在性判定，缺失即为 nil。
type replayResultFields struct {
	raw map[string]json.RawMessage
}

func parseReplayResult(result json.RawMessage) (replayResultFields, bool) {
	if len(result) == 0 || string(result) == "null" {
		return replayResultFields{}, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		return replayResultFields{}, false
	}
	return replayResultFields{raw: m}, true
}

// str 取字符串键（缺失/类型不符返回 ("", false)）。
func (f replayResultFields) str(key string) (string, bool) {
	raw, ok := f.raw[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// intPtr 取整数键为 *int（仅用于 exit_code 这类本就有「无值」语义的字段）。
func (f replayResultFields) intPtr(key string) *int {
	v, ok := f.int64(key)
	if !ok {
		return nil
	}
	n := int(v)
	return &n
}

// int64 取整数键。
func (f replayResultFields) int64(key string) (int64, bool) {
	raw, ok := f.raw[key]
	if !ok {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}

// boolean 取布尔键（缺失即 false）。
func (f replayResultFields) boolean(key string) bool {
	raw, ok := f.raw[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}

// replayExecutionStep 从 tool_calls.result 提取执行步字段（八期按工具分派）。
//
// 六期裁决原样保留：
//   - result 存在（正常执行完成）→ 按工具固定键装配；
//   - result 缺失但状态为 failed（通道在写入 result 之前就失败——SSH 拨号
//     失败、集群通道错误等，runner 早退不落 result）→ 仍然生成一个
//     status=failed 的 execution 步，带 error_code、exit_code=-1、空输出，
//     让回放能回答「为什么失败」；
//   - 其余缺失 result 的情形（denied/expired/awaiting_* —— 命令从未执行）
//     不产生 execution 步（契约：不存在执行事实，不伪造）。
func replayExecutionStep(tc store.ToolCall, key string, servers map[string]replayServerRef, batchTargets map[string][]string) (replayStep, bool) {
	name := tc.Name
	ts := tc.StartedAt
	if tc.FinishedAt != nil {
		ts = *tc.FinishedAt
	}
	if len(tc.Result) == 0 {
		if tc.Status != store.ToolFailed {
			return replayStep{}, false
		}
		status := "failed"
		errorCode := tc.ErrorCode
		// 通道失败口径同 console/batch 路径：exit_code=-1、无输出（契约
		// openapi「退出码（execution；通道失败 -1）」）。八期：-1 只对
		// ssh_exec/container_action 有意义，其余工具没有「退出码」语义。
		output := ""
		summary := replayNoResultSummary
		if errorCode != "" {
			summary = "error=" + errorCode
		}
		affected := replayExecutionAffectedServers(tc, replayResultFields{}, false, servers, batchTargets)
		step := replayStep{
			Timestamp: ts, kindWeight: replayKindExecution, sourceKey: key,
			Kind: replayKindName(replayKindExecution), ToolCallID: &tc.ID, Name: &name,
			Status: &status, Output: &output, ResultSummary: &summary,
			ResultTruncated: replayBoolPtr(false), AffectedServers: affected,
			DurationMS: replayStepDuration(tc, replayResultFields{}, false),
			Command:    replayExecutionCommand(tc, name, "", nil, servers),
			ServerName: replaySingleTargetServerName(affected),
		}
		// 契约 7.8 补充条款写死：failed 且无 result 的**动作类**工具一律
		// exit_code=-1（六期裁决扩展到全部动作类；正常路径才限定两个命令类
		// 工具，见 replayHasExitCode）。
		exitCode := -1
		step.ExitCode = &exitCode
		if errorCode != "" {
			step.ErrorCode = &errorCode
		}
		return step, true
	}

	// 整包 64KB 截断包装（契约 7.8 写死的退化路径）：output=preview、
	// result_truncated=true、摘要固定文案。不得因按工具取键落空渲染成空输出。
	originalBytes, preview, isWrapper := isTruncatedResultWrapper(tc.Result)
	fields, parsed := parseReplayResult(tc.Result)
	if !parsed && !isWrapper {
		return replayStep{}, false
	}

	status := "succeeded"
	if tc.Status == store.ToolFailed {
		status = "failed"
	}
	var (
		output          string
		outputPtr       *string
		summary         string
		resultTruncated bool
	)
	if isWrapper {
		output = preview
		summary = fmt.Sprintf(replayTruncatedWrapperSummary, originalBytes)
		resultTruncated = true
	} else {
		out, _ := replayExecutionOutput(name, fields, tc.Result)
		output = out
		var summaryTruncated bool
		summary, summaryTruncated = replayResultSummary(name, tc.Result, tc.ErrorCode)
		resultTruncated = replayExecutionResultTruncated(name, fields) || summaryTruncated
	}
	outputPtr = &output

	affected := replayExecutionAffectedServers(tc, fields, isWrapper, servers, batchTargets)
	step := replayStep{
		Timestamp: ts, kindWeight: replayKindExecution, sourceKey: key,
		Kind: replayKindName(replayKindExecution), ToolCallID: &tc.ID, Name: &name,
		Status: &status, Output: outputPtr, ResultSummary: &summary,
		ResultTruncated: replayBoolPtr(resultTruncated), AffectedServers: affected,
		DurationMS: replayStepDuration(tc, fields, isWrapper),
		Command:    replayExecutionCommand(tc, name, resultCommand(fields, isWrapper), &fields, servers),
		ServerName: replaySingleTargetServerName(affected),
	}
	// exit_code 仅 ssh_exec/container_action（契约 7.8：其余工具为 null，
	// 不得填 0 伪装成功）。
	if replayHasExitCode(name) && !isWrapper {
		step.ExitCode = fields.intPtr("exit_code")
	}
	// 资源句柄（契约 2.12 约定 3）：只在对应工具的 execution 步出现。
	if !isWrapper {
		switch name {
		case agent.ToolRunDiagnosis:
			if id, ok := fields.str("diagnosis_id"); ok && id != "" {
				step.DiagnosisID = &id
			}
		case agent.ToolRunInspection:
			if id, ok := fields.str("report_id"); ok && id != "" {
				step.InspectionReportID = &id
			}
		case agent.ToolBatchExecute:
			if id, ok := fields.str("batch_job_id"); ok && id != "" {
				step.BatchJobID = &id
			}
		}
	}
	if tc.ErrorCode != "" {
		errorCode := tc.ErrorCode
		step.ErrorCode = &errorCode
	}
	return step, true
}

// replayHasExitCode 判断工具是否有「退出码」语义（契约 7.8：只有命令类的
// ssh_exec / container_action）。
func replayHasExitCode(name string) bool {
	return name == agent.ToolSSHExec || name == agent.ToolContainerAction
}

func replayBoolPtr(v bool) *bool { return &v }

// replaySingleTargetServerName 返回 execution 步的 server_name（契约 7.8）：
// 只有**单目标**动作（affected_servers 恰好 1 个）才与 affected_servers 同值，
// 多目标/无目标为 null；tool_call 步恒 null。
func replaySingleTargetServerName(affected []string) *string {
	if len(affected) != 1 {
		return nil
	}
	return &affected[0]
}

// resultCommand 取 result.command（命令类工具的实际执行命令）。
func resultCommand(fields replayResultFields, isWrapper bool) string {
	if isWrapper {
		return ""
	}
	command, _ := fields.str("command")
	return command
}

// replayExecutionOutput 按工具固定键取 output 原文（契约 7.8 的表）：
//   - ssh_exec / container_action / container_logs → result.output；
//   - server_facts → result.raw_output；
//   - 其余结构化工具 → JSON.stringify(result)（与聊天工具卡同一 payload）。
func replayExecutionOutput(name string, fields replayResultFields, result json.RawMessage) (string, bool) {
	switch name {
	case agent.ToolSSHExec, agent.ToolContainerAction, agent.ToolContainerLogs:
		if out, ok := fields.str("output"); ok {
			return out, true
		}
	case agent.ToolServerFacts:
		if out, ok := fields.str("raw_output"); ok {
			return out, true
		}
	}
	// 结构化结果原样下发；序列化失败（理论上不会，result 已是合法 JSON）
	// 退化为空串并标记截断，绝不 panic。
	if len(result) == 0 {
		return "", false
	}
	return string(result), true
}

// replayExecutionResultTruncated 判定该步输出是否被截断（契约 7.8）：
// 命令类 / container_logs / db_query 取 result.truncated；run_inspection 取
// 任一 item 的 truncated；其余工具的自身 truncated 字段同样采纳。
func replayExecutionResultTruncated(name string, fields replayResultFields) bool {
	if fields.boolean("truncated") {
		return true
	}
	if name == agent.ToolRunInspection || name == agent.ToolGetInspectionReport {
		raw, ok := fields.raw["items"]
		if !ok {
			return false
		}
		var items []struct {
			Truncated bool `json:"truncated"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return false
		}
		for _, it := range items {
			if it.Truncated {
				return true
			}
		}
	}
	return false
}

// replayStepDuration 取 execution 步的耗时（契约 7.8：不得为 null）：
// result.duration_ms 优先；否则 finished_at - started_at。两者都没有（旧行
// 缺 finished_at 且工具结果无耗时）时返回 0（执行步恒有耗时事实）。
func replayStepDuration(tc store.ToolCall, fields replayResultFields, isWrapper bool) *int64 {
	if !isWrapper {
		if ms, ok := fields.int64("duration_ms"); ok {
			v := ms
			return &v
		}
	}
	var ms int64
	if tc.FinishedAt != nil {
		ms = tc.FinishedAt.Sub(tc.StartedAt).Milliseconds()
		if ms < 0 {
			ms = 0
		}
	}
	return &ms
}

// replayExecutionCommand 取 execution 步的 command（契约 2.12 约定 3：
// 「动作摘要」兼容字段）。命令类沿用 result.command（edited ?? original），
// 非命令类按工具填代表性动作串；结果缺失（failed 早退）时回退 arguments。
func replayExecutionCommand(tc store.ToolCall, name, resultCommandValue string, fields *replayResultFields, servers map[string]replayServerRef) *string {
	if name == agent.ToolSSHExec || name == agent.ToolContainerAction {
		command := resultCommandValue
		if command == "" {
			command = replayArgumentString(tc.Arguments, "command")
		}
		if command == "" {
			return nil
		}
		return &command
	}
	var command string
	switch name {
	case agent.ToolServerFacts:
		command = replayFactsTemplate(replayArgumentString(tc.Arguments, "server_id"), servers)
	case agent.ToolContainerList:
		command = replayContainerRuntimeCommand(tc, fields, servers) + " ps -a"
	case agent.ToolContainerInspect:
		command = replayContainerRuntimeCommand(tc, fields, servers) + " inspect " + replayArgumentString(tc.Arguments, "container_id")
	case agent.ToolContainerLogs:
		tail := replayContainerLogsTail(tc, fields)
		command = fmt.Sprintf("%s logs --tail %d %s",
			replayContainerRuntimeCommand(tc, fields, servers), tail, replayArgumentString(tc.Arguments, "container_id"))
	case agent.ToolImageList:
		command = replayContainerRuntimeCommand(tc, fields, servers) + " image ls"
	case agent.ToolDBQuery:
		command = replayArgumentString(tc.Arguments, "sql")
	case agent.ToolBatchExecute:
		command = replayArgumentString(tc.Arguments, "command")
	case agent.ToolRunDiagnosis:
		id := ""
		if fields != nil {
			id, _ = fields.str("diagnosis_id")
		}
		if id != "" {
			command = "diagnosis:" + id
		} else {
			// failed 早退无 result：不伪造 ID，只标动作族。
			command = "diagnosis"
		}
	case agent.ToolRunInspection:
		template := replayArgumentString(tc.Arguments, "template")
		if template == "" {
			if fields != nil {
				template, _ = fields.str("template")
			}
		}
		if template == "" {
			template = "basic"
		}
		command = "inspection:" + template
	}
	if command == "" {
		return nil
	}
	return &command
}

// replayContainerRuntimeCommand 取容器 CLI 前缀（docker/podman）。优先级：
// 结果里的 runtime（工具实际使用的运行时）→ 资产探测缓存
// servers.container_runtime → docker（契约 2.12 约定 3 的示例口径）。
func replayContainerRuntimeCommand(tc store.ToolCall, fields *replayResultFields, servers map[string]replayServerRef) string {
	if fields != nil {
		if rt, ok := fields.str("runtime"); ok && rt != "" {
			return rt
		}
	}
	if ref, ok := servers[replayArgumentString(tc.Arguments, "server_id")]; ok {
		switch ref.ContainerRuntime {
		case store.ContainerRuntimeDocker, store.ContainerRuntimePodman:
			return string(ref.ContainerRuntime)
		}
	}
	return "docker"
}

// replayContainerLogsTail 取日志 tail（result.tail 优先，缺省 200——与
// agent.ContainerLogsDefaultTail 同口径）。
func replayContainerLogsTail(tc store.ToolCall, fields *replayResultFields) int64 {
	if fields != nil {
		if tail, ok := fields.int64("tail"); ok && tail > 0 {
			return tail
		}
	}
	return 200
}

// replayExecutionAffectedServers 装配 affected_servers（契约 7.8）：
//   - db_query 为 null（外部数据库不是服务器资产，方法返回 nil）；
//   - batch_execute 优先取真实执行面（结果/参数的 server_ids），
//   - run_inspection 取报告 items 的 server_id，
//   - 其余单目标动作取 arguments.server_id；
//   - 映射不到的 id 输出 `<已删除>`，按 id 升序。
func replayExecutionAffectedServers(tc store.ToolCall, fields replayResultFields, isWrapper bool, servers map[string]replayServerRef, batchTargets map[string][]string) []string {
	switch tc.Name {
	case agent.ToolDBQuery:
		return nil
	case agent.ToolBatchExecute:
		// 真实执行面优先（batch_jobs.server_ids，契约 7.8）；读不到时回退
		// 模型参数 server_ids（回退不额外置截断标志——参数是事实而非结果）。
		if !isWrapper {
			if jobID, ok := fields.str("batch_job_id"); ok {
				if ids, ok := batchTargets[jobID]; ok && len(ids) > 0 {
					return replayAffectedServers(ids, servers)
				}
			}
		}
		ids := replayArgumentStrings(tc.Arguments, "server_ids")
		if len(ids) > 0 {
			return replayAffectedServers(ids, servers)
		}
		return nil
	case agent.ToolRunInspection:
		if !isWrapper {
			if raw, ok := fields.raw["items"]; ok {
				var items []struct {
					ServerID string `json:"server_id"`
				}
				if err := json.Unmarshal(raw, &items); err == nil && len(items) > 0 {
					ids := make([]string, 0, len(items))
					for _, it := range items {
						ids = append(ids, it.ServerID)
					}
					return replayAffectedServers(ids, servers)
				}
			}
		}
		return replayAffectedServers(replayArgumentStrings(tc.Arguments, "server_ids"), servers)
	}
	single := replayArgumentString(tc.Arguments, "server_id")
	if single == "" {
		return nil
	}
	return replayAffectedServers([]string{single}, servers)
}

// replayApplyArgumentsBudget 应用 arguments 装载预算（契约 2.12 约定 6 ① /
// 5.19.2 第 6 条）：按 tool_calls.seq 升序累加**原始 len(tc.Arguments)**，
// 超 6 MiB 后的更晚工具调用 arguments 置 null 并报告命中。纯函数（就地修改
// 切片元素）。steps 已按时间序排列，这里按 tool_call 步的 seq 顺序处理。
func replayApplyArgumentsBudget(steps []replayStep, budget int) bool {
	// 契约写死「按 tool_calls.seq 升序累加」：步骤已按时间排序，但同会话内
	// started_at 与 seq 不保证严格同序，故先按 sourceKey（补零 seq）取 tool_call
	// 步的下标序，再按该序累加。
	order := make([]int, 0, len(steps))
	for i := range steps {
		if steps[i].Kind == replayKindName(replayKindToolCall) {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return steps[order[a]].sourceKey < steps[order[b]].sourceKey })

	var used int
	hit := false
	for _, i := range order {
		if steps[i].Arguments == nil {
			continue
		}
		size := len(steps[i].Arguments)
		if hit || used+size > budget {
			steps[i].Arguments = nil
			hit = true
			continue
		}
		used += size
	}
	return hit
}

// replayApplySerializedBudget 应用响应序列化体积预算（契约 2.12 约定 6 ②）：
// 逐步真实 Marshal 求体积（含 JSON 转义，比长度求和更贴近 gin 的实际输出），
// 超 8 MiB 时**从最旧步开始**把 execution.output 置空串并置
// result_truncated=true（保留 result_summary）；仍超限则丢弃最旧步骤并报告
// （调用方据 stepsDropped 报 truncated_reason=steps，与 11 节第 21 条一致）。
func replayApplySerializedBudget(steps []replayStep, budget int) (out []replayStep, payloadTruncated, stepsDropped bool) {
	sizes := make([]int, len(steps))
	total := 2 // 数组方括号
	for i := range steps {
		sizes[i] = replayStepJSONSize(steps[i])
		total += sizes[i]
	}
	if total <= budget {
		return steps, false, false
	}
	// 从最旧步起清 output（保留 result_summary 与 result_truncated=true）。
	for i := range steps {
		if total <= budget {
			return steps, payloadTruncated, false
		}
		if steps[i].Kind != replayKindName(replayKindExecution) || steps[i].Output == nil || *steps[i].Output == "" {
			continue
		}
		empty := ""
		steps[i].Output = &empty
		steps[i].ResultTruncated = replayBoolPtr(true)
		newSize := replayStepJSONSize(steps[i])
		total -= sizes[i] - newSize
		sizes[i] = newSize
		payloadTruncated = true
	}
	if total <= budget {
		return steps, payloadTruncated, false
	}
	// 仍超限：丢弃最旧步骤（调用方据 stepsDropped 报 truncated_reason=steps，
	// 保持 steps > payload > approvals 的优先级）。
	drop := 0
	for drop < len(steps) && total > budget {
		total -= sizes[drop]
		drop++
	}
	return steps[drop:], payloadTruncated, true
}

// replayApplyPayloadBudget 是八期两道体积预算的统一入口（契约 5.19.2 第 5
// 条：`replayApplyPayloadBudget(steps, argsBytes, serializedBytes)`，纯函数、
// 可单测）：先按 seq 升序裁 arguments（6 MiB），再按序列化体积（8 MiB）从
// 最旧步清 output、仍超限丢最旧步。返回是否命中 payload 预算、是否发生了
// 丢步（后者让调用方报 truncated_reason=steps）。
func replayApplyPayloadBudget(steps []replayStep, argsBytes, serializedBytes int) (out []replayStep, payloadHit, stepsDropped bool) {
	// 非正值按契约常量兜底（避免误传 0 时清空全部内容）。
	if argsBytes <= 0 {
		argsBytes = replayArgumentsBudgetBytes
	}
	if serializedBytes <= 0 {
		serializedBytes = replaySerializedBudgetBytes
	}
	payloadHit = replayApplyArgumentsBudget(steps, argsBytes)
	out, serializedHit, stepsDropped := replayApplySerializedBudget(steps, serializedBytes)
	return out, payloadHit || serializedHit, stepsDropped
}

// replayStepJSONSize 返回单步序列化字节数（含分隔逗号；Marshal 失败时按固定
// 开销上界兜底——result 来自 jsonb，理论上不会失败）。
func replayStepJSONSize(step replayStep) int {
	raw, err := json.Marshal(step)
	if err != nil {
		return replaySerializedStepOverhead
	}
	return len(raw) + 1
}

// replayBatchTargets 预取 batch_execute 的真实执行面（契约 7.8：批量动作的
// affected_servers 优先取 batch_jobs.server_ids，不只信模型参数）。
//
// 有界性（11 节）：只处理本次回放里出现的 batch_execute 调用，最多
// replayBatchTargetPrefetchLimit 个任务；单个 job 的 server_ids 由表 CHECK
// 约束 ≤50。任一任务读取失败即跳过（回退模型参数，不影响其它步骤）。
func (d *Deps) replayBatchTargets(c *gin.Context, calls []store.ToolCall) map[string][]string {
	out := make(map[string][]string)
	// 未装配批量仓储（早期 fake / 精简依赖）时不预取——affected_servers
	// 回退模型参数，回放不因此失败。
	jobs := d.Store.BatchJobs()
	if jobs == nil {
		return out
	}
	ctx := c.Request.Context()
	for i := range calls {
		if len(out) >= replayBatchTargetPrefetchLimit {
			break
		}
		tc := calls[i]
		if tc.Name != agent.ToolBatchExecute || len(tc.Result) == 0 {
			continue
		}
		jobID := replayResultString(tc.Result, "batch_job_id")
		if jobID == "" {
			continue
		}
		if _, done := out[jobID]; done {
			continue
		}
		job, _, err := jobs.Get(ctx, jobID)
		if err != nil {
			out[jobID] = nil // 读取失败：显式标记已尝试，回退参数。
			continue
		}
		out[jobID] = job.ServerIDs
	}
	return out
}

// replayResultString 取 tool_calls.result 的字符串键（供预取定位资源句柄）。
func replayResultString(result json.RawMessage, key string) string {
	fields, ok := parseReplayResult(result)
	if !ok {
		return ""
	}
	v, _ := fields.str(key)
	return v
}
