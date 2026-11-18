package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/authz"
	"aiops/internal/store"
)

// approvalDecisionRequest 是 decide 请求体。00012 定稿（契约 12.3 第 8
// 条）：只允许 decision 与 note；任何 edited_command 或目标字段都是未知
// 字段，由 decodeJSON 的 DisallowUnknownFields 返回 400。执行层无条件使用
// approval.command。
type approvalDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

func (d *Deps) handleListApprovals(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := parseLimit(c, 50)
	if !ok {
		return
	}
	conversationID := strings.TrimSpace(c.Query("conversation_id"))
	status := store.ApprovalStatus(strings.TrimSpace(c.Query("status")))
	switch status {
	case "", store.ApprovalPending, store.ApprovalApproved, store.ApprovalDenied, store.ApprovalExpired:
	default:
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid status")
		return
	}

	items, err := d.Store.Approvals().List(c.Request.Context(), conversationID, status, limit)
	if err != nil {
		d.writeStoreError(c, err, apierr.ApprovalNotFound)
		return
	}
	out := make([]store.Approval, 0, len(items))
	out = append(out, items...)
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// handleGetApproval 返回审批详情。00012 允许发起人例外（路由表）：拥有
// approval:read 的用户可读全部；否则只有 requested_by=自己 可读；其余
// 一律 404——不泄露审批是否存在。
func (d *Deps) handleGetApproval(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	ap, err := d.Store.Approvals().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.ApprovalNotFound)
		return
	}
	if authz.Allowed(c, authz.PermApprovalRead) {
		c.JSON(http.StatusOK, ap)
		return
	}
	if ap.RequestedBy != "" && ap.RequestedBy == c.GetString("user_id") {
		c.JSON(http.StatusOK, ap)
		return
	}
	writeAPIError(c, http.StatusNotFound, apierr.ApprovalNotFound, "approval not found")
}

// 有界审批上下文（契约 12.3 第 23 条 / openapi ApprovalContext）：
// 审批 + 对应 tool_call + 资产元数据 + 审批前最多 20 条消息。审批后的
// 新消息绝不返回，也不授予 /conversations/{id}/replay。
const (
	approvalContextMessageLimit = 20
	approvalContextPageSize     = 100
	// approvalContextMaxPages 是回溯页数硬上限：至多扫描 2000 条消息，
	// 保证历史很长的会话也不会无界读取（防 OOM 铁律）。
	approvalContextMaxPages = 20
)

type approvalContextResponse struct {
	Approval store.Approval  `json:"approval"`
	ToolCall *store.ToolCall `json:"tool_call"`
	Server   *store.Server   `json:"server"`
	Messages []store.Message `json:"messages"`
}

func (d *Deps) handleGetApprovalContext(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	ctx := c.Request.Context()
	ap, err := d.Store.Approvals().Get(ctx, c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.ApprovalNotFound)
		return
	}

	var toolCall *store.ToolCall
	if ap.ToolCallID != nil && *ap.ToolCallID != "" {
		tc, err := d.Store.ToolCalls().ByID(ctx, *ap.ToolCallID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				d.handleStoreError(c, err, apierr.ApprovalNotFound)
				return
			}
		} else {
			toolCall = &tc
		}
	}

	var server *store.Server
	if ap.ServerID != nil && *ap.ServerID != "" {
		srv, err := d.Store.Servers().Get(ctx, *ap.ServerID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				d.handleStoreError(c, err, apierr.ApprovalNotFound)
				return
			}
		} else {
			server = &srv
		}
	}

	var messages []store.Message
	if ap.ConversationID != nil && *ap.ConversationID != "" {
		messages, err = d.approvalContextMessages(c, *ap.ConversationID, ap.RequestedAt)
		if err != nil {
			d.handleStoreError(c, err, apierr.ApprovalNotFound)
			return
		}
	}
	if messages == nil {
		messages = []store.Message{}
	}

	// 跨用户读取治理审计（契约 12.3 第 20/23 条）：审批由他人发起时写。
	me := c.GetString("user_id")
	if ap.RequestedBy != "" && ap.RequestedBy != me {
		detail := map[string]any{
			"approval_id":  ap.ID,
			"requested_by": ap.RequestedBy,
		}
		if ap.ConversationID != nil && *ap.ConversationID != "" {
			if conv, cerr := d.Store.Conversations().Get(ctx, *ap.ConversationID); cerr == nil {
				detail["owner_id"] = conv.UserID
				detail["conversation_id"] = conv.ID
			}
		}
		d.auditEvent(c, "approval.context_viewed", "approval", ap.ID, detail)
	}

	c.JSON(http.StatusOK, approvalContextResponse{
		Approval: ap,
		ToolCall: toolCall,
		Server:   server,
		Messages: messages,
	})
}

// approvalContextMessages 返回审批时间点之前最近 20 条消息（升序）。
// 从最新一页向旧回溯，单页 100 条、最多 20 页（2000 条）——有界读取。
func (d *Deps) approvalContextMessages(c *gin.Context, conversationID string, cutoff time.Time) ([]store.Message, error) {
	beforeSeq := int64(0)
	collected := make([]store.Message, 0, approvalContextMessageLimit)
	for page := 0; page < approvalContextMaxPages && len(collected) < approvalContextMessageLimit; page++ {
		items, hasMore, err := d.Store.Messages().ListPage(c.Request.Context(), conversationID, beforeSeq, approvalContextPageSize)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].CreatedAt.After(cutoff) {
				continue
			}
			collected = append(collected, items[i])
			if len(collected) >= approvalContextMessageLimit {
				break
			}
		}
		if !hasMore {
			break
		}
		beforeSeq = items[0].Seq
	}
	// 回溯得到的是「由新到旧」，翻转为契约要求的升序。
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}

func (d *Deps) handleDecideApproval(c *gin.Context) {
	if d.Approvals == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "approval service unavailable")
		return
	}
	// 第 8 轮走查 low：审批 id 必须是 uuid。此前非 UUID 直落 store 的
	// `$1::uuid` 转换错误——approve 经 recheckRequester 被映射为 503
	// 「approval store unavailable」、deny 走 Decide 的兜底 500 INTERNAL，
	// 调用方会误判服务故障并触发 5xx 告警；同一 id 的 GET 详情却是 400。
	// 在入口校验，approve/deny 统一 400 VALIDATION_FAILED。
	approvalID := c.Param("id")
	if _, err := uuid.Parse(approvalID); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "approval id must be a uuid")
		return
	}
	var req approvalDecisionRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	var approve bool
	switch req.Decision {
	case "approve":
		approve = true
	case "deny":
		approve = false
	default:
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "decision must be approve or deny")
		return
	}
	if utf8.RuneCountInString(req.Note) > 500 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "note exceeds 500 characters")
		return
	}
	// 契约 12.3 第 8 条：decide 只接受 decision/note，禁止命令与目标改写。
	// 这里显式不读取 edited_command（结构体无该字段，未知字段已在
	// decodeJSON 阶段 400）。

	// 契约 12.3 第 11 条：单管理员降级模式（启用管理员 <= 1）下不得批准
	// 任何审批——唯一管理员不能借「他人批准」扩大自己的执行面。
	// 契约 12.4 第 4 条：执行前从 store 复读发起人并复检权限。approve 会
	// 触发 console/batch 同步执行或唤醒 chat 等待方，故批准前必须复检；
	// deny 不执行命令，无需复检（职责分离仍在 Service.Decide 内强制）。
	if approve && !d.requireSecondApprover(c, authz.PermApprovalDecide, "approval.approve") {
		return
	}
	if approve && !d.recheckRequester(c, approvalID) {
		return
	}

	// 进程内发起人仅作为历史兼容兜底；Service.Decide 以持久化行为准。
	requestedBy, _ := d.Approvals.RequestedBy(approvalID)
	ap, err := d.Approvals.Decide(c.Request.Context(), approvals.DecideRequest{
		ApprovalID:  approvalID,
		Approve:     approve,
		DecidedBy:   c.GetString("user_id"),
		RequestedBy: requestedBy,
		Note:        req.Note,
		RequestID:   c.GetString("request_id"),
	})
	if err != nil {
		switch {
		case errors.Is(err, approvals.ErrSelfDecision):
			// 职责分离：发起人不能审批自己的请求（审批保持 pending）。
			writeErrorStatus(c, http.StatusForbidden, "APPROVAL_SELF_DECISION", "requester cannot decide own approval")
		case errors.Is(err, approvals.ErrLegacyEdit):
			// 00012 之前的历史 pending 改写行：只能重新发起。
			writeErrorStatus(c, http.StatusConflict, "APPROVAL_LEGACY_EDIT", "legacy edited approval cannot be decided")
		case errors.Is(err, approvals.ErrPolicyStale), errors.Is(err, approvals.ErrPolicyNotWidening):
			// 契约 12.3 第 10 条：策略目标已不适用当前门限，批准不生效。
			writeErrorStatus(c, http.StatusConflict, "POLICY_STALE", "policy change is stale; request a new one")
		case errors.Is(err, approvals.ErrInvalid):
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid decision request")
		case errors.Is(err, approvals.ErrForbidden):
			// 改写后命令 forbidden：403 且审批保持 pending（契约 2.11 约定 3）。
			writeAPIError(c, http.StatusForbidden, apierr.CommandForbidden, "edited command is forbidden by the risk policy")
		case errors.Is(err, store.ErrNotFound):
			writeAPIError(c, http.StatusNotFound, apierr.ApprovalNotFound, "approval not found")
		case errors.Is(err, store.ErrConflict):
			writeAPIError(c, http.StatusConflict, apierr.ApprovalNotPending, "approval is not pending")
		case errors.Is(err, approvals.ErrStoreUnavailable):
			writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "approval store unavailable")
		default:
			d.logError(c, "approval decide failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
		}
		return
	}

	// 五期 M3（契约 2.10 约定 5）：source=batch 审批 decide 后经 batchx
	// 回填该台 result（approve 同步逐字节执行；deny 置 denied）并重算 job
	// 状态。回填失败不回滚审批（已 approved 不可逆）——batchx 过期扫描兜
	// 底收敛；此时 execution 携带 error_code=BACKFILL_FAILED。
	if ap.Source == "batch" {
		if d.Batch == nil {
			writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "batch service unavailable")
			return
		}
		execution := map[string]any{}
		if err := d.Batch.OnApprovalDecided(c.Request.Context(), ap, approve); err != nil {
			d.logError(c, "batch approval backfill failed", err)
			execution = map[string]any{"error_code": "BACKFILL_FAILED"}
		} else if approve {
			execution = d.batchApprovalExecution(c, ap)
		}
		if !approve {
			// deny：不执行，execution=null（与 chat/console deny 一致）。
			execution = nil
		}
		d.writeApprovalWithExecution(c, ap, execution)
		return
	}

	// 四期 C1（契约 5.5/2.9 约定 3）：source=console 且批准时，decide
	// handler 同步逐字节执行 approval.command（不得改写），结果写入响应
	// execution 并落审计 container.action；执行失败不回滚审批状态
	//（已 approved 不可逆），失败信息只进 execution 与审计 detail。
	// deny 或 source=chat 时行为与既有完全一致（execution=null）。
	if approve && ap.Source == "console" {
		if !d.requireStore(c) {
			return
		}
		execution := d.executeConsoleApproval(c, ap)
		d.writeApprovalWithExecution(c, ap, execution)
		return
	}
	d.writeApprovalWithExecution(c, ap, nil)
}

// batchApprovalExecution 读取 batch 审批 approve 后回填的该台结果摘要
// （exit_code/output/error_code/duration + batch 定位），供 decide 响应
// execution 字段返回（回填已完成，这里只读）。
func (d *Deps) batchApprovalExecution(c *gin.Context, ap store.Approval) map[string]any {
	result, err := d.Store.BatchJobs().ByApproval(c.Request.Context(), ap.ID)
	if err != nil {
		return map[string]any{"error_code": "BACKFILL_FAILED"}
	}
	execution := map[string]any{
		"batch_job_id":    result.BatchJobID,
		"batch_result_id": result.ID,
		"status":          string(result.Status),
		"error_code":      result.ErrorCode,
		"duration_ms":     result.DurationMS,
		"truncated":       result.Truncated,
	}
	if result.ExitCode != nil {
		execution["exit_code"] = *result.ExitCode
	} else {
		// 通道执行失败（命令未到达/连接错误）ExitCode=nil——契约 8.9
		// ApprovalExecution.exit_code 必填且约定此时为 -1（与 console 路径
		// containers_handlers.go 的 -1 口径一致）；缺省会令前端插值出
		// 「退出码 undefined」（五期 Sprint5 走查）。
		execution["exit_code"] = -1
	}
	return execution
}

// writeApprovalWithExecution 输出 ApprovalDecisionResult 包装结构
// {approval, execution}（契约 8.9/5.5 与 openapi ApprovalDecisionResult：
// console 容器审批的执行结果在 execution；chat 审批恒 null——四期拼装
// 曾误用扁平平铺 + 顶层 execution 键，与契约/openapi/前端三方不一致）。
func (d *Deps) writeApprovalWithExecution(c *gin.Context, ap store.Approval, execution map[string]any) {
	c.JSON(http.StatusOK, gin.H{"approval": ap, "execution": execution})
}
