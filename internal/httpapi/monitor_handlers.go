// monitor_handlers.go 实现五期 M1 的监控 REST 路由（契约 2.5/2.10）：
//   - GET  /api/monitor/overview      每台最新指标 + firing 计数（按等级）
//   - GET  /api/monitor/collector     读采集配置（settings）
//   - PUT  /api/monitor/collector     写采集配置（下一轮生效，不重启）
//   - GET  /api/servers/:id/metrics   单机时间序列（minutes 15..1440 缺省
//     60，metric 可选过滤；契约五期行为
//     约定：不做降采样，60s×1440=24h 上限）
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/monitor"
	"aiops/internal/store"
)

// mustMarshalSetting 序列化采集配置（结构固定，失败是不可能的分支）。
func mustMarshalSetting(cs monitor.CollectorSetting) json.RawMessage {
	raw, _ := json.Marshal(cs)
	return raw
}

// RegisterMonitorRoutes 注册 M1 监控路由（契约 2.5）。
func (d *Deps) RegisterMonitorRoutes(api *gin.RouterGroup) {
	api.GET("/monitor/overview", d.handleMonitorOverview)
	api.GET("/monitor/collector", d.handleGetCollector)
	api.PUT("/monitor/collector", d.handleUpdateCollector)
	api.GET("/servers/:id/metrics", d.handleServerMetrics)
}

// firingCounts 是 overview 的 firing 计数（按等级）。
type firingCounts struct {
	Info     int64 `json:"info"`
	Warning  int64 `json:"warning"`
	Critical int64 `json:"critical"`
	Total    int64 `json:"total"`
}

// lastInspectionStat 是概要卡「最近巡检均分」的数据（契约
// MonitorOverview.last_inspection：report_id/avg_score/created_at；
// 无报告时整个对象为 null，前端回落占位符 —）。
type lastInspectionStat struct {
	ReportID  string    `json:"report_id"`
	AvgScore  int       `json:"avg_score"`
	CreatedAt time.Time `json:"created_at"`
}

// monitorOverviewResponse 是 GET /api/monitor/overview 响应（S22：servers
// 与 firing 计数字段必须存在；last_inspection/collector 按契约装配）。
type monitorOverviewResponse struct {
	Servers        []store.ServerMetricLatest `json:"servers"`
	Firing         firingCounts               `json:"firing"`
	LastInspection *lastInspectionStat        `json:"last_inspection"`
	Collector      monitor.CollectorSetting   `json:"collector"`
}

func (d *Deps) handleMonitorOverview(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	latest, err := d.Store.Metrics().Latest(c.Request.Context())
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if latest == nil {
		latest = []store.ServerMetricLatest{}
	}
	counts := firingCounts{}
	if n, err := d.Store.AlertEvents().CountFiring(c.Request.Context()); err == nil {
		counts.Total = n
	} else {
		d.logError(c, "count firing failed", err)
	}
	// 按等级拆分：走 List 分页（上限 200×5 页——firing 全量通常远小于此；
	// 超出部分只计入 total，不再翻页，防慢查询）。
	if counts.Total > 0 {
		filters := []store.AlertSeverity{store.AlertSeverityInfo, store.AlertSeverityWarning, store.AlertSeverityCritical}
		values := []*int64{&counts.Info, &counts.Warning, &counts.Critical}
		for i, sev := range filters {
			before := int64(0)
			for page := 0; page < 5; page++ {
				items, hasMore, err := d.Store.AlertEvents().List(c.Request.Context(), store.AlertEventFilter{
					Status: store.AlertEventFiring, Severity: sev, BeforeID: before, Limit: 200,
				})
				if err != nil {
					d.logError(c, "list firing events failed", err)
					break
				}
				*values[i] += int64(len(items))
				if !hasMore || len(items) == 0 {
					break
				}
				before = items[len(items)-1].ID
			}
		}
	}
	// 最近巡检均分（契约 8.7 概要卡）：取最近一条报告（List 固定
	// created_at DESC，LIMIT 1 有界）；查询失败不阻塞 overview（与上方
	// firing 计数同容错口径），无报告时保持 null。
	var lastInspection *lastInspectionStat
	if reports, err := d.Store.InspectionReports().List(c.Request.Context(),
		store.InspectionReportFilter{Limit: 1}); err == nil && len(reports) > 0 {
		lastInspection = &lastInspectionStat{
			ReportID: reports[0].ID, AvgScore: reports[0].AvgScore, CreatedAt: reports[0].CreatedAt,
		}
	} else if err != nil {
		d.logError(c, "list inspection reports failed", err)
	}
	c.JSON(http.StatusOK, monitorOverviewResponse{
		Servers: latest, Firing: counts, LastInspection: lastInspection,
		Collector: monitor.LoadCollectorSetting(c.Request.Context(), d.Store),
	})
}

// handleGetCollector 返回当前采集配置（settings["monitor.collector"]）。
func (d *Deps) handleGetCollector(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	c.JSON(http.StatusOK, monitor.LoadCollectorSetting(c.Request.Context(), d.Store))
}

// collectorRequest 是 PUT /api/monitor/collector 请求体。
type collectorRequest struct {
	Enabled         *bool `json:"enabled"`
	IntervalSeconds *int  `json:"interval_seconds"`
}

// handleUpdateCollector 更新采集配置（enabled/interval_seconds 30..3600；
// 变更下一轮采集生效，不重启进程——契约 4.11）。
func (d *Deps) handleUpdateCollector(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req collectorRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	cur := monitor.LoadCollectorSetting(c.Request.Context(), d.Store)
	next := cur
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.IntervalSeconds != nil {
		if *req.IntervalSeconds < 30 || *req.IntervalSeconds > 3600 {
			writeError(c, apierr.ValidationFailed, "interval_seconds must be between 30 and 3600")
			return
		}
		next.IntervalSeconds = *req.IntervalSeconds
	}
	if _, err := d.Store.Settings().Set(c.Request.Context(), monitor.SettingKeyCollector,
		mustMarshalSetting(next), d.now()); err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	c.JSON(http.StatusOK, next)
}

// metricPointResponse 是时间序列元素（全部 6 指标；metric 过滤时只含
// 请求的键——由 buildMetricPoints 装配）。
type metricPointResponse map[string]any

// metricsResponse 是 GET /api/servers/:id/metrics 与 get_metrics 工具共用
// 的响应结构（契约 7.1 示例同形）。
type metricsResponse struct {
	ServerID  string                `json:"server_id"`
	Minutes   int                   `json:"minutes"`
	Points    []metricPointResponse `json:"points"`
	Truncated bool                  `json:"truncated"`
}

// validMetrics 是 metric 过滤白名单（与 server_metrics 列一一对应）。
var validMetrics = map[string]store.AlertMetric{
	"cpu_pct":     store.AlertMetricCPU,
	"mem_pct":     store.AlertMetricMem,
	"disk_pct":    store.AlertMetricDisk,
	"load_1":      store.AlertMetricLoad1,
	"net_in_bps":  store.AlertMetricNetIn,
	"net_out_bps": store.AlertMetricNetOut,
}

// BuildMetricsResponse 装配时间序列响应（handler 与 agent get_metrics 工具
// 共用）。minutes 15..1440；metric 空串=全部 6 指标。
func BuildMetricsResponse(serverID string, minutes int, metric string, points []store.ServerMetric) metricsResponse {
	resp := metricsResponse{ServerID: serverID, Minutes: minutes, Points: make([]metricPointResponse, 0, len(points))}
	for _, m := range points {
		all := map[string]any{
			"created_at":  m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"cpu_pct":     m.CPU,
			"mem_pct":     m.Mem,
			"disk_pct":    m.Disk,
			"load_1":      m.Load1,
			"net_in_bps":  m.NetIn,
			"net_out_bps": m.NetOut,
		}
		if metric != "" {
			v, ok := all[metric]
			if !ok {
				continue
			}
			resp.Points = append(resp.Points, metricPointResponse{
				"created_at": all["created_at"], metric: v,
			})
			continue
		}
		resp.Points = append(resp.Points, all)
	}
	resp.Truncated = len(points) >= monitor.BufferPoints
	return resp
}

// handleServerMetrics 返回单机时间序列。
func (d *Deps) handleServerMetrics(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	serverID := c.Param("id")
	if _, err := uuid.Parse(serverID); err != nil {
		writeError(c, apierr.ValidationFailed, "server id must be a valid uuid")
		return
	}
	minutes := 60
	if raw := c.Query("minutes"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 15 || n > 1440 {
			writeError(c, apierr.ValidationFailed, "minutes must be between 15 and 1440")
			return
		}
		minutes = n
	}
	metric := ""
	if raw := strings.TrimSpace(c.Query("metric")); raw != "" {
		if _, ok := validMetrics[raw]; !ok {
			writeError(c, apierr.ValidationFailed, "metric must be one of cpu_pct,mem_pct,disk_pct,load_1,net_in_bps,net_out_bps")
			return
		}
		metric = raw
	}
	if _, err := d.Store.Servers().Get(c.Request.Context(), serverID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(c, apierr.ServerNotFound, "server not found")
			return
		}
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	to := d.now()
	points, err := d.Store.Metrics().Window(c.Request.Context(), store.MetricWindow{
		ServerID: serverID,
		From:     to.Add(-time.Duration(minutes) * time.Minute),
		To:       to.Add(time.Minute), Limit: monitor.BufferPoints,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	c.JSON(http.StatusOK, BuildMetricsResponse(serverID, minutes, metric, points))
}
