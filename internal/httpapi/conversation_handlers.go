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

// 回放上限（契约 11 节第 17 条，写死）：步数 20000，超出丢弃最旧并置
// truncated=true；分页页大小与单表迭代上限是防 OOM 的终止条件。
const (
	replayMaxSteps       = 20000
	replayPageSize       = 200
	replayMaxPagesPerRow = 101 // 200×101 > 20000，任何单表超过即由步数上限截断
)

// truncated_reason 的取值（六期打磨 sprint 4）：两种截断来源语义不同，
// 前端据此选文案，不再把「审批记录满 200 条」说成「超出 20000 步上限」。
const (
	// replayTruncatedSteps：步数超 replayMaxSteps，丢弃最旧步骤。
	replayTruncatedSteps = "steps"
	// replayTruncatedApprovals：单状态审批满 replayPageSize 条（approvals
	// .List 无游标，4×200=800 上限），更早的审批未纳入回放。
	replayTruncatedApprovals = "approvals"
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

	steps := buildReplaySteps(msgs, calls, aps)

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
	// 取值：''（未截断）/ steps（步数上限）/ approvals（审批满页）。
	steps, truncated, truncatedReason := replayApplyTruncation(steps, approvalsComplete)
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

// buildReplaySteps 把三表行展开为回放步（顺序由调用方统一排序）。
func buildReplaySteps(msgs []store.Message, calls []store.ToolCall, aps []store.Approval) []replayStep {
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
		steps = append(steps, step)
		// execution 步：仅命令执行类工具 ssh_exec/container_action，从
		// result JSON 提取（result 已受 ToolResultBytes 64KB 截断，回放
		// 不再截断）；timestamp=finished_at ?? started_at。
		if tc.Name == "ssh_exec" || tc.Name == "container_action" {
			if ex, ok := replayExecutionStep(tc, key); ok {
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

// replayExecutionStep 从 tool_calls.result 提取执行步字段。
//
// 六期打磨 sprint 2：
//   - result 存在（正常执行完成）→ 按 result 的 command/exit_code/output/
//     duration_ms 重建；
//   - result 缺失但状态为 failed（通道在写入 result 之前就失败——SSH 拨号
//     失败、集群通道错误等，runner 早退不落 result）→ 仍然生成一个
//     status=failed 的 execution 步，带 error_code、exit_code=-1、空输出，
//     让回放能回答「为什么失败」；
//   - 其余缺失 result 的情形（denied/expired/awaiting_* —— 命令从未执行）
//     不产生 execution 步（契约：不存在执行事实，不伪造）。
func replayExecutionStep(tc store.ToolCall, key string) (replayStep, bool) {
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
		errorCode := ""
		if tc.ErrorCode != "" {
			errorCode = tc.ErrorCode
		}
		// 通道失败口径同 console/batch 路径：exit_code=-1、无输出（契约
		// openapi「退出码（execution；通道失败 -1）」）。
		exitCode := -1
		output := ""
		step := replayStep{
			Timestamp: ts, kindWeight: replayKindExecution, sourceKey: key,
			Kind: replayKindName(replayKindExecution), ToolCallID: &tc.ID, Name: &name,
			Status: &status, ExitCode: &exitCode, Output: &output,
		}
		if errorCode != "" {
			step.ErrorCode = &errorCode
		}
		return step, true
	}
	var result struct {
		Command    string  `json:"command"`
		ExitCode   *int    `json:"exit_code"`
		Output     *string `json:"output"`
		DurationMS *int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(tc.Result, &result); err != nil {
		return replayStep{}, false
	}
	status := "succeeded"
	if tc.Status == store.ToolFailed {
		status = "failed"
	}
	step := replayStep{
		Timestamp: ts, kindWeight: replayKindExecution, sourceKey: key,
		Kind: replayKindName(replayKindExecution), ToolCallID: &tc.ID, Name: &name,
		Status: &status, ExitCode: result.ExitCode, Output: result.Output,
		DurationMS: result.DurationMS,
	}
	if result.Command != "" {
		command := result.Command
		step.Command = &command
	}
	if tc.ErrorCode != "" {
		errorCode := tc.ErrorCode
		step.ErrorCode = &errorCode
	}
	return step, true
}
