// diagnosis_handlers.go 实现五期 M2 的诊断 REST 路由（契约 2.5/4.21/5.16）：
// 会话创建（trigger=manual）与列表/详情；fix（命令必须逐字节等于报告
// suggestions[].command 之一，创建 source=console 审批——复用四期 decide
// 同步执行路径）；complete（awaiting_fix→done CAS）。并发/队列上限由
// 引擎 Create 返回 ErrBusy（429 DIAGNOSIS_BUSY）。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/authz"
	"aiops/internal/diagnosis"
	"aiops/internal/risk"
	"aiops/internal/store"
)

// RegisterDiagnosisRoutes 注册 M2 诊断路由（契约 2.5）。
func (d *Deps) RegisterDiagnosisRoutes(api *gin.RouterGroup) {
	d.guard(api, http.MethodGet, "/diagnosis/sessions", authz.PermExecuteReadonly, d.handleListDiagnosis)
	d.guard(api, http.MethodPost, "/diagnosis/sessions", authz.PermExecuteReadonly, d.handleCreateDiagnosis)
	d.guard(api, http.MethodGet, "/diagnosis/sessions/:id", authz.PermExecuteReadonly, d.handleGetDiagnosis)
	d.guard(api, http.MethodPost, "/diagnosis/sessions/:id/fix", authz.PermExecuteReadonly, d.handleDiagnosisFix)
	d.guard(api, http.MethodPost, "/diagnosis/sessions/:id/complete", authz.PermExecuteReadonly, d.handleDiagnosisComplete)
}

// createDiagnosisRequest 是 POST /api/diagnosis/sessions 请求体。
type createDiagnosisRequest struct {
	ServerID     string `json:"server_id" binding:"required,uuid"`
	AlertEventID *int64 `json:"alert_event_id"`
}

func (d *Deps) handleCreateDiagnosis(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.Diagnosis == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "diagnosis module unavailable")
		return
	}
	var req createDiagnosisRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid request body")
		return
	}
	userID := c.GetString("user_id")
	sess, err := d.Diagnosis.Create(c.Request.Context(), store.CreateDiagnosisParams{
		ServerID:     req.ServerID,
		AlertEventID: req.AlertEventID,
		Trigger:      store.DiagnosisTriggerManual,
		ExpiresAt:    time.Now().UTC().Add(30 * time.Minute),
		// 00012：手工诊断归属当前用户（complete/fix 的 owner 规则依据）。
		CreatedBy: &userID,
	})
	if err != nil {
		switch {
		case errors.Is(err, diagnosis.ErrBusy):
			writeAPIError(c, http.StatusTooManyRequests, apierr.DiagnosisBusy, "diagnosis queue limit reached")
		case errors.Is(err, store.ErrNotFound):
			writeAPIError(c, http.StatusNotFound, apierr.ServerNotFound, "server not found")
		case errors.Is(err, store.ErrInvalid):
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid request")
		default:
			d.logHandlerError(c, "diagnosis create failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to create diagnosis session")
		}
		return
	}
	c.JSON(http.StatusCreated, sess)
}

func (d *Deps) handleListDiagnosis(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var status store.DiagnosisStatus
	if s := c.Query("status"); s != "" {
		switch store.DiagnosisStatus(s) {
		case store.DiagnosisPending, store.DiagnosisDiagnosing, store.DiagnosisReport,
			store.DiagnosisAwaitingFix, store.DiagnosisDone, store.DiagnosisExpired, store.DiagnosisFailed:
			status = store.DiagnosisStatus(s)
		default:
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid status filter")
			return
		}
	}
	serverID := c.Query("server_id")
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	// 契约 12.3 第 16 条 / 12.4 第 2 条（第 11 轮走查 medium）：会话列表与
	// GET /diagnosis/sessions/:id 必须同源——admin 看全量；其他账号在 SQL
	// 层只取 created_by=自己（created_by IS NULL 的 system 行仅 admin 可见）。
	// 先取全量再在 Go 侧过滤会让他人较新的会话占满 limit，把自己的会话挤出
	// 列表，前端还会出现「可见摘要但点开 404」的错位。
	createdBy := ""
	if !isAdmin(c) {
		createdBy = c.GetString("user_id")
		if createdBy == "" {
			// 失败关闭：拿不到当前用户时不得退化为「不过滤」。
			writeAPIError(c, http.StatusUnauthorized, apierr.AuthRequired, "authentication required")
			return
		}
	}
	items, err := d.Store.DiagnosisSessions().List(c.Request.Context(), status, serverID, createdBy, limit)
	if err != nil {
		d.handleStoreError(c, err, apierr.DiagnosisSessionNotFound)
		return
	}
	if items == nil {
		items = []store.DiagnosisSession{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (d *Deps) handleGetDiagnosis(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	sess, err := d.Store.DiagnosisSessions().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.DiagnosisSessionNotFound)
		return
	}
	// 契约 12.4 第 2 条：非 owner 且非 admin 按不存在处理。
	if !ownerOrAdmin(c, sess.CreatedBy) {
		writeAPIError(c, http.StatusNotFound, apierr.DiagnosisSessionNotFound, "diagnosis session not found")
		return
	}
	c.JSON(http.StatusOK, sess)
}

// fixRequest 是 POST /api/diagnosis/sessions/:id/fix 请求体。
type fixRequest struct {
	Command string `json:"command" binding:"required"`
}

// handleDiagnosisFix 把建议命令转为 console 审批（契约 2.10 约定 3）：
// command 逐字节等于 report.suggestions[].command 之一，否则 400；创建
// source=console 审批（decide 批准后由四期路径同步逐字节执行）。
func (d *Deps) handleDiagnosisFix(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req fixRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "command is required")
		return
	}
	ctx := c.Request.Context()
	sess, err := d.Store.DiagnosisSessions().Get(ctx, c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.DiagnosisSessionNotFound)
		return
	}
	// 契约 12.4 第 2 条：fix 仅 created_by=user 或 admin。
	if !ownerOrAdmin(c, sess.CreatedBy) {
		writeAPIError(c, http.StatusNotFound, apierr.DiagnosisSessionNotFound, "diagnosis session not found")
		return
	}
	if sess.Status != store.DiagnosisAwaitingFix {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "diagnosis session is not awaiting fix")
		return
	}
	if len(sess.Report) == 0 {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "diagnosis session has no report")
		return
	}
	var report diagnosis.DiagnosisReport
	if err := json.Unmarshal(sess.Report, &report); err != nil {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "diagnosis report is unreadable")
		return
	}
	matched := false
	for _, s := range report.Suggestions {
		if s.Command == req.Command { // 逐字节相等（契约）
			matched = true
			break
		}
	}
	if !matched {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "command does not match any suggestion")
		return
	}
	srv, err := d.Store.Servers().Get(ctx, sess.ServerID)
	if err != nil {
		d.handleStoreError(c, err, apierr.ServerNotFound)
		return
	}
	// 审批风险等级按目标分册实时分类（模型预估值仅提示）：网络设备走
	// ClassifyNet（此前误用 Linux/Windows 分册，审批展示的风险等级失真
	// ——五期评审发现；对齐诊断引擎内部分类 classifyForServer）。
	var level risk.Decision
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		level = risk.ClassifyNet(req.Command, riskNetBrand(srv.NetdevBrand))
	} else {
		level = risk.Classify(req.Command, riskOSFor(srv))
	}
	// 六期 BE-P（契约 2.11 约定 2 第 ⑤ 条）：fix 按统一门限分流——
	// NeedsApproval=false（none/high+auto_exec 等）直接同步执行返回
	// DiagnosisFixResult{approval:null, execution}；五期「修复命令没有
	// 免审批路径」的裁决放宽为直执行（command 逐字节等于建议之一的校验
	// 不变，门限裁决由用户全局选择承担）。console 路径用全局门限。
	if d.Approvals == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "approval service unavailable")
		return
	}
	mode, err := approvals.GlobalMode(ctx, d.Store)
	if err != nil {
		d.handleStoreError(c, err, apierr.Internal)
		return
	}
	if !approvals.NeedsApproval(approvals.PolicyInput{
		Mode:            mode,
		AutoExecEnabled: srv.AutoExecEnabled,
		Level:           level.Level,
		Severity:        risk.SeverityOf(level.RuleID),
	}) {
		execution := d.executeConsoleCommand(c, srv, req.Command)
		// 契约 2.11 约定 1：none 档「免审批、全部自动执行，仍必须落审计」
		//——诊断 fix 直执行分支此前无任何审计（六期评审发现）。
		d.auditEvent(c, "command.executed", "server", srv.ID, map[string]any{
			"command":       req.Command,
			"exit_code":     execution["exit_code"],
			"approval_mode": string(mode),
			"diagnosis_id":  sess.ID,
			"error_code":    execution["error_code"],
		})
		c.JSON(http.StatusOK, gin.H{"approval": nil, "execution": execution})
		return
	}
	ap, err := d.Approvals.Create(ctx, approvals.CreateRequest{
		Source:       "console",
		ServerID:     &srv.ID,
		ServerName:   srv.Name,
		Command:      req.Command,
		RiskLevel:    string(level.Level),
		RuleID:       level.RuleID,
		Reason:       "diagnosis fix suggestion (session " + sess.ID + ")",
		ApprovalMode: string(mode),
		RequestID:    c.GetString("request_id"),
		RequestedBy:  c.GetString("user_id"),
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.Conflict)
		return
	}
	// 六期 BE-P（openapi DiagnosisFixResult wrapper）：需审批时
	// {approval, execution:null}。五期直返裸 Approval 的形态废弃（决定
	// 批准后仍由 decide handler 的 console 路径同步执行——approvals.go）。
	c.JSON(http.StatusOK, gin.H{"approval": ap, "execution": nil})
}

// riskOSFor 把服务器 OS 映射到风险分册。
func riskOSFor(srv store.Server) risk.OS {
	if srv.OS == store.ServerOSWindows {
		return risk.OSWindows
	}
	return risk.OSLinux
}

// handleDiagnosisComplete 完成 awaiting_fix→done（CAS；契约 5.16）。
func (d *Deps) handleDiagnosisComplete(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	existing, err := d.Store.DiagnosisSessions().Get(ctx, id)
	if err != nil {
		d.handleStoreError(c, err, apierr.DiagnosisSessionNotFound)
		return
	}
	if !ownerOrAdmin(c, existing.CreatedBy) {
		writeAPIError(c, http.StatusNotFound, apierr.DiagnosisSessionNotFound, "diagnosis session not found")
		return
	}
	sess, err := d.Store.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: id, From: store.DiagnosisAwaitingFix, To: store.DiagnosisDone,
		Timestamp: time.Now().UTC(),
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.Conflict)
		return
	}
	c.JSON(http.StatusOK, sess)
}
