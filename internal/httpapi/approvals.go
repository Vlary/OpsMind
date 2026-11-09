package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/store"
)

type approvalDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// EditedCommand 是批准时的用户改写命令（六期 BE-P，契约 2.11 约定 3）：
	// 仅 decision=approve 时合法（deny 携带由 Service 返回 ErrInvalid →
	// 400）；trim 后非空 1..2000 字符且禁止 NUL（binding 与 DB CHECK 双
	// 重校验）；空串=未改写。
	EditedCommand string `json:"edited_command"`
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

func (d *Deps) handleGetApproval(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	ap, err := d.Store.Approvals().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.ApprovalNotFound)
		return
	}
	c.JSON(http.StatusOK, ap)
}

func (d *Deps) handleDecideApproval(c *gin.Context) {
	if d.Approvals == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "approval service unavailable")
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
	// 六期 BE-P：改写命令 trim 后空串视为未改写（等价不携带）；非空时
	// 由 Service 校验长度/NUL/仅 approve 并重新分类（契约 2.11 约定 3）。
	edited := strings.TrimSpace(req.EditedCommand)

	ap, err := d.Approvals.Decide(c.Request.Context(), approvals.DecideRequest{
		ApprovalID:    c.Param("id"),
		Approve:       approve,
		DecidedBy:     c.GetString("user_id"),
		Note:          req.Note,
		EditedCommand: edited,
		RequestID:     c.GetString("request_id"),
	})
	if err != nil {
		switch {
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
