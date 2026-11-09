// batch_handlers.go 实现五期 M3 的批量任务 REST（契约 2.5/5.18）：
// 创建（≤50 台）、列表、详情（含全部逐台 results，前端 running 时 2s 轮询
// ——进度走轮询，不新增 SSE 事件）、取消（pending/awaiting_approval 台置
// cancelled）。执行编排全部委托 batchx.Service；decide handler 的 batch
// 审批执行回填见 approvals.go handleDecideApproval。
package httpapi

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/batchx"
	"aiops/internal/risk"
	"aiops/internal/store"
)

// batchMaxTargets 是单 job 目标上限（契约 11 节写死 50，与表 CHECK 一致）。
const batchMaxTargets = 50

type createBatchJobRequest struct {
	Description string   `json:"description"`
	Command     string   `json:"command"`
	ServerIDs   []string `json:"server_ids"`
}

// RegisterBatchRoutes 注册 M3 批量任务路由（契约 2.5）。
func (d *Deps) RegisterBatchRoutes(api *gin.RouterGroup) {
	api.POST("/batch/jobs", d.handleCreateBatchJob)
	api.GET("/batch/jobs", d.handleListBatchJobs)
	api.GET("/batch/jobs/:id", d.handleGetBatchJob)
	api.POST("/batch/jobs/:id/cancel", d.handleCancelBatchJob)
}

func (d *Deps) handleCreateBatchJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.Batch == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "batch service unavailable")
		return
	}
	var req createBatchJobRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if len(req.ServerIDs) == 0 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "server_ids is required")
		return
	}
	if len(req.ServerIDs) > batchMaxTargets {
		writeAPIError(c, http.StatusBadRequest, apierr.BatchTooManyTargets, "batch targets exceed 50 servers")
		return
	}
	if req.Command == "" {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "command is required")
		return
	}
	if len(req.Command) > risk.MaxCommandBytes {
		writeAPIError(c, http.StatusBadRequest, apierr.CommandTooLarge, "command exceeds 16384 bytes")
		return
	}
	if utf8.RuneCountInString(req.Description) > 2000 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "description exceeds 2000 characters")
		return
	}

	userID := c.GetString("user_id")
	job, err := d.Batch.Dispatch(c.Request.Context(), store.CreateBatchJobParams{
		Description: req.Description,
		Command:     req.Command,
		ServerIDs:   req.ServerIDs,
		Source:      store.BatchSourceConsole,
		CreatedBy:   &userID,
	})
	if err != nil {
		switch {
		case errors.Is(err, batchx.ErrTooManyTargets):
			writeAPIError(c, http.StatusBadRequest, apierr.BatchTooManyTargets, "batch targets exceed 50 servers")
		case errors.Is(err, store.ErrInvalid):
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid batch job")
		default:
			d.logError(c, "batch dispatch failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
		}
		return
	}
	c.JSON(http.StatusCreated, job)
}

func (d *Deps) handleListBatchJobs(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := parseLimit(c, 50)
	if !ok {
		return
	}
	jobs, err := d.Store.BatchJobs().List(c.Request.Context(), limit)
	if err != nil {
		d.writeStoreError(c, err, apierr.BatchJobNotFound)
		return
	}
	if jobs == nil {
		jobs = []store.BatchJob{}
	}
	c.JSON(http.StatusOK, gin.H{"items": jobs})
}

func (d *Deps) handleGetBatchJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	job, results, err := d.Store.BatchJobs().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.BatchJobNotFound)
		return
	}
	if results == nil {
		results = []store.BatchResult{}
	}
	c.JSON(http.StatusOK, gin.H{"job": job, "results": results})
}

func (d *Deps) handleCancelBatchJob(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.Batch == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "batch service unavailable")
		return
	}
	job, err := d.Batch.Cancel(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(c, http.StatusNotFound, apierr.BatchJobNotFound, "batch job not found")
			return
		}
		d.logError(c, "batch cancel failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
		return
	}
	c.JSON(http.StatusOK, job)
}
