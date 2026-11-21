// monitor_handlers.go 实现五期 M1 的监控 REST 路由（契约 2.5/2.10）：
//   - GET  /api/monitor/overview      每台最新指标 + firing 计数（按等级）
//   - GET  /api/monitor/trend         图表聚合（24h/7d 时间桶，单条 date_bin SQL）
//   - GET  /api/monitor/collector     读采集配置（settings）
//   - PUT  /api/monitor/collector     写采集配置（下一轮生效，不重启）
//   - GET  /api/servers/:id/metrics   单机时间序列（minutes 15..1440 缺省
//     60，metric 可选过滤；契约五期行为
//     约定：不做降采样，60s×1440=24h 上限）
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/authz"
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
	d.guard(api, http.MethodGet, "/monitor/overview", authz.PermMetricsRead, d.handleMonitorOverview)
	d.guard(api, http.MethodGet, "/monitor/trend", authz.PermMetricsRead, d.handleMonitorTrend)
	d.guard(api, http.MethodGet, "/monitor/collector", authz.PermMetricsRead, d.handleGetCollector)
	d.guard(api, http.MethodPut, "/monitor/collector", authz.PermAlertConfig, d.handleUpdateCollector)
	d.guard(api, http.MethodGet, "/servers/:id/metrics", authz.PermMetricsRead, d.handleServerMetrics)
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
	Servers []store.ServerMetricLatest `json:"servers"`
	Firing  firingCounts               `json:"firing"`
	// ServerTime 是本响应生成时的服务端时间：前端用它与浏览器时钟的偏移
	// 计算「N 前/过期」，避免值班机时钟偏差把未来样本显示成「1 秒前」
	// （走查第 14 轮 medium）。
	ServerTime     time.Time                `json:"server_time"`
	LastInspection *lastInspectionStat      `json:"last_inspection"`
	Collector      monitor.CollectorSetting `json:"collector"`
}

func (d *Deps) handleMonitorOverview(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	now := d.now()
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
	// 按等级拆分：一条 GROUP BY severity 索引聚合（不再走 3×最多 5 页×200
	// 的 List 分页——旧实现在单等级 firing >1000 时只统计前 1000 条，
	// 会把概览卡显示得比实际少）。失败只丢分级、total 仍保留。
	if counts.Total > 0 {
		sev, err := d.Store.AlertEvents().CountFiringBySeverity(c.Request.Context())
		if err != nil {
			d.logError(c, "count firing by severity failed", err)
		} else {
			counts.Info, counts.Warning, counts.Critical = sev.Info, sev.Warning, sev.Critical
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
		Servers: latest, Firing: counts, ServerTime: now, LastInspection: lastInspection,
		Collector: monitor.LoadCollectorSetting(c.Request.Context(), d.Store),
	})
}

// ---------------------------------------------------------------------------
// 图表聚合（GET /api/monitor/trend 与 GET /api/alert-events/stats 共用窗口）
// ---------------------------------------------------------------------------

// trendOrigin 是 date_bin 的对齐原点（SQL 内同值，UTC）；它是 3600/21600
// 的整数倍，因此 Go 侧按 Unix 秒下取整与 SQL 完全一致。
var trendOrigin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	trendWindow24h = "24h"
	trendWindow7d  = "7d"
)

// trendWindow 是图表窗口解析结果：桶宽与桶数由服务端写死（window 参数
// 只做枚举，不接受任意跨度），from 含 / to 不含。
type trendWindow struct {
	name          string
	bucketSeconds int
	buckets       int
	from          time.Time
	to            time.Time
}

// parseTrendWindow 解析 window=24h|7d（缺省 24h；其它值 400）。
// 24h→3600s×24 桶，7d→21600s×28 桶；to 对齐到当前桶结束（当前未走完的
// 桶作为最后一个点），from=to-桶数×桶宽，天然正好 24/28 个整桶。
func parseTrendWindow(c *gin.Context, now time.Time) (trendWindow, bool) {
	name := strings.TrimSpace(c.Query("window"))
	if name == "" {
		name = trendWindow24h
	}
	var bucketSeconds, buckets int
	switch name {
	case trendWindow24h:
		bucketSeconds, buckets = 3600, 24
	case trendWindow7d:
		bucketSeconds, buckets = 21600, 28
	default:
		writeError(c, apierr.ValidationFailed, "window must be 24h or 7d")
		return trendWindow{}, false
	}
	span := time.Duration(bucketSeconds) * time.Second
	to := alignTrendBucket(now, bucketSeconds).Add(span)
	from := to.Add(-time.Duration(buckets) * span)
	return trendWindow{name: name, bucketSeconds: bucketSeconds, buckets: buckets,
		from: from, to: to}, true
}

// alignTrendBucket 把时刻下取整到 UTC 桶边界（origin=2000-01-01T00:00:00Z）。
// 仅用于当前时间与 7d 前的窗口边界（恒在 origin 之后），整除无负数分支。
func alignTrendBucket(t time.Time, bucketSeconds int) time.Time {
	origin := trendOrigin.Unix()
	b := int64(bucketSeconds)
	return time.Unix(origin+((t.UTC().Unix()-origin)/b)*b, 0).UTC()
}

// roundMetric 把聚合值保留 2 位小数（百分比展示口径）；nil 表示 null。
func roundMetric(v float64) *float64 {
	r := math.Round(v*100) / 100
	return &r
}

// metricTrendPointResponse 是一个时间桶。无样本桶的 6 个指标字段为 null
// （前端断线不连点），servers/samples 为 0。
type metricTrendPointResponse struct {
	BucketStart time.Time `json:"bucket_start"`
	CPUP95      *float64  `json:"cpu_p95"`
	CPUAvg      *float64  `json:"cpu_avg"`
	CPUMax      *float64  `json:"cpu_max"`
	MemP95      *float64  `json:"mem_p95"`
	MemAvg      *float64  `json:"mem_avg"`
	MemMax      *float64  `json:"mem_max"`
	Servers     int       `json:"servers"`
	Samples     int64     `json:"samples"`
}

// monitorTrendResponse 是 GET /api/monitor/trend 响应（points ≤28）。
// FirstSample / LastSample 是窗口内实际参与聚合的最早/最晚采样时刻（已按
// now 钳制），无样本时为 null；前端用 last-first 算真实数据跨度（走查
// 第 17 轮 low：首末桶未满时桶跨度会高估采集时长）。
type monitorTrendResponse struct {
	Window        string                     `json:"window"`
	BucketSeconds int                        `json:"bucket_seconds"`
	From          time.Time                  `json:"from"`
	To            time.Time                  `json:"to"`
	FirstSample   *time.Time                 `json:"first_sample"`
	LastSample    *time.Time                 `json:"last_sample"`
	Points        []metricTrendPointResponse `json:"points"`
}

// handleMonitorTrend 返回 24h/7d 的 CPU/Mem 聚合折线（单条 date_bin SQL，
// 无样本桶补 null；空数据时 points 仍是完整 24/28 个 null 桶，不报错）。
// to 对齐到当前桶结束（未来时刻），但聚合上界单独用 now 钳制：时钟超前
// 机器写入的未来样本不得在当前桶制造假峰值（走查第 16 轮 medium）。
func (d *Deps) handleMonitorTrend(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	now := d.now()
	win, ok := parseTrendWindow(c, now)
	if !ok {
		return
	}
	rows, err := d.Store.Metrics().Trend(c.Request.Context(), store.MetricTrendQuery{
		From: win.from, To: win.to, BucketSeconds: win.bucketSeconds, Now: now,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.Internal)
		return
	}
	// 存储层最多返回 win.buckets 行（窗口内整桶数），map 容量有界。
	byBucket := make(map[int64]store.MetricTrendPoint, len(rows))
	var firstSample, lastSample *time.Time
	for _, p := range rows {
		byBucket[p.BucketStart.Unix()] = p
		if !p.FirstSample.IsZero() && (firstSample == nil || p.FirstSample.Before(*firstSample)) {
			at := p.FirstSample
			firstSample = &at
		}
		if !p.LastSample.IsZero() && (lastSample == nil || p.LastSample.After(*lastSample)) {
			at := p.LastSample
			lastSample = &at
		}
	}
	span := time.Duration(win.bucketSeconds) * time.Second
	points := make([]metricTrendPointResponse, 0, win.buckets)
	for i := 0; i < win.buckets; i++ {
		start := win.from.Add(time.Duration(i) * span)
		p, ok := byBucket[start.Unix()]
		if !ok {
			points = append(points, metricTrendPointResponse{BucketStart: start})
			continue
		}
		points = append(points, metricTrendPointResponse{
			BucketStart: start,
			CPUP95:      roundMetric(p.CPUP95),
			CPUAvg:      roundMetric(p.CPUAvg),
			CPUMax:      roundMetric(p.CPUMax),
			MemP95:      roundMetric(p.MemP95),
			MemAvg:      roundMetric(p.MemAvg),
			MemMax:      roundMetric(p.MemMax),
			Servers:     p.Servers,
			Samples:     p.Samples,
		})
	}
	c.JSON(http.StatusOK, monitorTrendResponse{
		Window: win.name, BucketSeconds: win.bucketSeconds,
		From: win.from, To: win.to,
		FirstSample: firstSample, LastSample: lastSample, Points: points,
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

// BuildMetricsResponse 装配时间序列响应。minutes 15..1440；metric 空串=
// 全部 6 指标。truncated 由调用方判定「窗口内确有更早的点被丢弃」，不能
// 按 len(points)>=limit 推断——60s 采集的 24h 窗口正好 1440 点（30s 则
// 2880 点），一个都没丢却会被误报为截断。
func BuildMetricsResponse(serverID string, minutes int, metric string, points []store.ServerMetric, truncated bool) metricsResponse {
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
	resp.Truncated = truncated
	return resp
}

// metricsWindowTruncated 判断单机窗口查询是否真的丢了更早的点。
//
// 调用前提：首次查询正好取满 limit（唯一有歧义的情形）。此时用同一下界
// + oldest 之前 1µs 再查 1 个点：存在就是「还有更早的点没返回」。时间戳
// 来自 Postgres（微秒精度），-1µs 只排除与 oldest 完全同刻的行；采集器
// 每台每轮只落一行，不存在同刻多行。
func (d *Deps) metricsWindowTruncated(ctx context.Context, serverID string, from, oldest time.Time) (bool, error) {
	older, err := d.Store.Metrics().Window(ctx, store.MetricWindow{
		ServerID: serverID,
		From:     from,
		To:       oldest.Add(-time.Microsecond),
		Limit:    1,
	})
	if err != nil {
		return false, err
	}
	return len(older) > 0, nil
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
	from := to.Add(-time.Duration(minutes) * time.Minute)
	// 点数上限必须随窗口放大：采集间隔允许 30s，24h 窗口就有 2880 点。
	// 旧实现固定 1440（60s 口径），30s 采集的 24h 只返回最近 12h，页面
	// 把其余 12h 标成「无采样」（第 10 轮 high）。上限取 minutes×2（每分钟
	// 最多 2 点），实际仍受 store.MetricWindowLimitMax 硬上限约束。
	limit := store.MetricWindowPointLimit(minutes)
	points, err := d.Store.Metrics().Window(c.Request.Context(), store.MetricWindow{
		ServerID: serverID,
		From:     from,
		To:       to.Add(time.Minute), Limit: limit,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	truncated := false
	if limit > 0 && len(points) == limit {
		oldest := points[0].CreatedAt
		truncated, err = d.metricsWindowTruncated(c.Request.Context(), serverID, from, oldest)
		if err != nil {
			d.handleStoreError(c, err, apierr.NotFound)
			return
		}
	}
	c.JSON(http.StatusOK, BuildMetricsResponse(serverID, minutes, metric, points, truncated))
}
