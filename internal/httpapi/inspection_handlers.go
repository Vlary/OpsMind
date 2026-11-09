// inspection_handlers.go 实现五期 M2 的巡检 REST 路由（契约 2.5/4.22-4.24/
// 5.17）：任务 CRUD（cron 校验→CRON_INVALID，next_run_at 由本层按 cron
// 计算）、手动触发（202，同 job 互斥）、报告列表/详情（含 previous_scores
// 趋势对比）、即时巡检（不落 job）。
package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/inspection"
	"aiops/internal/store"
)

// RegisterInspectionRoutes 注册 M2 巡检路由（契约 2.5）。
func (d *Deps) RegisterInspectionRoutes(api *gin.RouterGroup) {
	api.GET("/inspection/jobs", d.handleListInspectionJobs)
	api.POST("/inspection/jobs", d.handleCreateInspectionJob)
	api.GET("/inspection/jobs/:id", d.handleGetInspectionJob)
	api.PATCH("/inspection/jobs/:id", d.handleUpdateInspectionJob)
	api.DELETE("/inspection/jobs/:id", d.handleDeleteInspectionJob)
	api.POST("/inspection/jobs/:id/run", d.handleRunInspection)

	api.GET("/inspection/reports", d.handleListInspectionReports)
	api.GET("/inspection/reports/:id", d.handleGetInspectionReport)

	api.POST("/inspection/run", d.handleRunInspectionNow)
}

// ---------------------------------------------------------------------------
// 巡检任务 CRUD
// ---------------------------------------------------------------------------

// inspectionJobRequest 是创建/更新巡检任务的请求体（指针字段 nil=不改）。
type inspectionJobRequest struct {
	Name               *string   `json:"name"`
	Cron               *string   `json:"cron"`
	TargetTags         *[]string `json:"target_tags"`
	Template           *string   `json:"template"`
	NotifyEnabled      *bool     `json:"notify_enabled"`
	NotifyChannelID    *string   `json:"notify_channel_id"`
	ClearNotifyChannel *bool     `json:"clear_notify_channel"`
	Enabled            *bool     `json:"enabled"`
}

func validInspectionTemplate(t string) (store.InspectionTemplate, bool) {
	switch store.InspectionTemplate(t) {
	case store.InspectionBasic, store.InspectionFull:
		return store.InspectionTemplate(t), true
	}
	return "", false
}

func (d *Deps) handleCreateInspectionJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req inspectionJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid request body")
		return
	}
	if req.Name == nil || len(*req.Name) < 1 || len(*req.Name) > 100 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "name must be 1..100 characters")
		return
	}
	if req.Cron == nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "cron is required")
		return
	}
	next, err := inspection.NextRun(*req.Cron, time.Now().UTC())
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.CronInvalid, "invalid cron expression")
		return
	}
	template := store.InspectionBasic
	if req.Template != nil && *req.Template != "" {
		t, ok := validInspectionTemplate(*req.Template)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "template must be basic or full")
			return
		}
		template = t
	}
	job, err := d.Store.InspectionJobs().Create(c.Request.Context(), store.CreateInspectionJobParams{
		Name:            *req.Name,
		Cron:            *req.Cron,
		TargetTags:      derefStrings(req.TargetTags),
		Template:        template,
		NotifyEnabled:   boolOf(req.NotifyEnabled),
		NotifyChannelID: req.NotifyChannelID,
		Enabled:         boolOr(req.Enabled, true),
		NextRunAt:       &next,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionJobNotFound)
		return
	}
	c.JSON(http.StatusCreated, job)
}

func (d *Deps) handleListInspectionJobs(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	jobs, err := d.Store.InspectionJobs().List(c.Request.Context())
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionJobNotFound)
		return
	}
	if jobs == nil {
		jobs = []store.InspectionJob{}
	}
	c.JSON(http.StatusOK, gin.H{"items": jobs})
}

func (d *Deps) handleGetInspectionJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	job, err := d.Store.InspectionJobs().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionJobNotFound)
		return
	}
	c.JSON(http.StatusOK, job)
}

func (d *Deps) handleUpdateInspectionJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req inspectionJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid request body")
		return
	}
	if req.Cron != nil {
		if _, err := inspection.NextRun(*req.Cron, time.Now().UTC()); err != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.CronInvalid, "invalid cron expression")
			return
		}
	}
	if req.Template != nil && *req.Template != "" {
		if _, ok := validInspectionTemplate(*req.Template); !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "template must be basic or full")
			return
		}
	}
	if req.Name != nil && (len(*req.Name) < 1 || len(*req.Name) > 100) {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "name must be 1..100 characters")
		return
	}
	job, err := d.Store.InspectionJobs().Update(c.Request.Context(), c.Param("id"), store.UpdateInspectionJobParams{
		Name: req.Name, Cron: req.Cron, TargetTags: req.TargetTags,
		Template: templatePtr(req.Template), NotifyEnabled: req.NotifyEnabled,
		NotifyChannelID: req.NotifyChannelID, ClearNotifyChannel: boolOf(req.ClearNotifyChannel),
		Enabled: req.Enabled,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionJobNotFound)
		return
	}
	// cron/enabled 变更后重算 next_run_at（契约 3.3 UpdateInspectionJobParams 注释）。
	if req.Cron != nil || req.Enabled != nil {
		if job.Enabled {
			if next, nerr := inspection.NextRun(job.Cron, time.Now().UTC()); nerr == nil {
				_ = d.Store.InspectionJobs().MarkRun(c.Request.Context(), job.ID, time.Now().UTC(), next)
				job.NextRunAt = &next
			}
		} else {
			job.NextRunAt = nil
		}
	}
	c.JSON(http.StatusOK, job)
}

func (d *Deps) handleDeleteInspectionJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if err := d.Store.InspectionJobs().Delete(c.Request.Context(), c.Param("id")); err != nil {
		d.handleStoreError(c, err, apierr.InspectionJobNotFound)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleRunInspection 手动触发任务巡检（202；同 job 互斥——上一轮未结束
// 返回 409）。
func (d *Deps) handleRunInspection(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.Inspect == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "inspection module unavailable")
		return
	}
	job, err := d.Store.InspectionJobs().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionJobNotFound)
		return
	}
	if !d.Inspect.DispatchJob(job, store.InspectionTriggerManual) {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "previous run of this job is still in progress")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted", "job_id": job.ID})
}

// ---------------------------------------------------------------------------
// 巡检报告
// ---------------------------------------------------------------------------

func (d *Deps) handleListInspectionReports(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	reports, err := d.Store.InspectionReports().List(c.Request.Context(), store.InspectionReportFilter{
		JobID: c.Query("job_id"), Limit: limit,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionReportNotFound)
		return
	}
	if reports == nil {
		reports = []store.InspectionReport{}
	}
	c.JSON(http.StatusOK, gin.H{"items": reports})
}

// inspectionReportDetail 是报告详情响应（契约 InspectionReportDetail：
// 嵌套 {report, items, previous_scores}——openapi.yaml required
// [report, items, previous_scores]，前端 ReportDrawer/RunNowModal 按
// detail.report.* 解析。此前匿名嵌入 store.InspectionReport 导致 JSON 平铺，
// 前端读 detail?.report.avg_score 抛 TypeError（五期走查发现）。
type inspectionReportDetail struct {
	Report         store.InspectionReport       `json:"report"`
	Items          []store.InspectionReportItem `json:"items"`
	PreviousScores map[string]int               `json:"previous_scores"`
}

func (d *Deps) handleGetInspectionReport(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	ctx := c.Request.Context()
	report, items, err := d.Store.InspectionReports().Get(ctx, c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.InspectionReportNotFound)
		return
	}
	if items == nil {
		items = []store.InspectionReportItem{}
	}
	// 即时巡检报告（job_id=null）无「同 job 上一份」可比，previous_scores
	// 为 null（契约 InspectionReportDetail.previous_scores：即时巡检为 null）。
	var prev map[string]int
	if report.JobID != nil {
		prev = map[string]int{}
		if m, err := d.Store.InspectionReports().PreviousScores(ctx, *report.JobID, report.StartedAt); err == nil {
			prev = m
		}
	}
	c.JSON(http.StatusOK, inspectionReportDetail{
		Report: report, Items: items, PreviousScores: prev,
	})
}

// runInspectionNowRequest 是即时巡检请求体。
type runInspectionNowRequest struct {
	ServerIDs []string `json:"server_ids" binding:"required"`
	Template  string   `json:"template"`
}

// handleRunInspectionNow 即时巡检（不落 job；同步返回报告——目标上限 200）。
func (d *Deps) handleRunInspectionNow(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.Inspect == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "inspection module unavailable")
		return
	}
	var req runInspectionNowRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.ServerIDs) == 0 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "server_ids is required")
		return
	}
	if len(req.ServerIDs) > 200 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "at most 200 servers per inspection")
		return
	}
	template := store.InspectionBasic
	if req.Template != "" {
		t, ok := validInspectionTemplate(req.Template)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "template must be basic or full")
			return
		}
		template = t
	}
	report, err := d.Inspect.Run(c.Request.Context(), nil, store.InspectionTriggerManual, req.ServerIDs, template)
	if err != nil {
		if errors.Is(err, inspection.ErrTooManyTargets) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "at most 200 servers per inspection")
			return
		}
		d.handleStoreError(c, err, apierr.Internal)
		return
	}
	// Run 只返回报告头（逐台明细已落库）；按契约 InspectionReportDetail
	// 组装嵌套响应：读回 items（失败回退空数组，不放大为 500——报告本体
	// 已成功），即时巡检 previous_scores=null。
	items := []store.InspectionReportItem{}
	if _, saved, err := d.Store.InspectionReports().Get(c.Request.Context(), report.ID); err == nil && saved != nil {
		items = saved
	}
	c.JSON(http.StatusOK, inspectionReportDetail{
		Report: report, Items: items, PreviousScores: nil,
	})
}

// ---------------------------------------------------------------------------
// 请求体小工具
// ---------------------------------------------------------------------------

func derefStrings(p *[]string) []string {
	if p == nil {
		return []string{}
	}
	return *p
}

func boolOf(p *bool) bool {
	return p != nil && *p
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func templatePtr(p *string) *store.InspectionTemplate {
	if p == nil || *p == "" {
		return nil
	}
	t := store.InspectionTemplate(*p)
	return &t
}
