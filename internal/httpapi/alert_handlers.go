// alert_handlers.go 实现五期 M1 的告警 REST 路由（契约 2.5/4.19/4.20/4.18）：
// 规则 CRUD、事件列表（status/severity/server_id 过滤 + before_id 分页）、
// 通知渠道 CRUD + 测试发送（凭据 AES-GCM 加密落库，永不回显）。
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
	"aiops/internal/authz"
	"aiops/internal/monitor"
	"aiops/internal/store"
)

// RegisterAlertRoutes 注册 M1 告警路由（契约 2.5）。
func (d *Deps) RegisterAlertRoutes(api *gin.RouterGroup) {
	d.guard(api, http.MethodGet, "/alert-rules", authz.PermMetricsRead, d.handleListAlertRules)
	d.guard(api, http.MethodPost, "/alert-rules", authz.PermAlertConfig, d.handleCreateAlertRule)
	d.guard(api, http.MethodGet, "/alert-rules/:id", authz.PermMetricsRead, d.handleGetAlertRule)
	d.guard(api, http.MethodPatch, "/alert-rules/:id", authz.PermAlertConfig, d.handleUpdateAlertRule)
	d.guard(api, http.MethodDelete, "/alert-rules/:id", authz.PermAlertConfig, d.handleDeleteAlertRule)

	d.guard(api, http.MethodGet, "/alert-events", authz.PermMetricsRead, d.handleListAlertEvents)
	d.guard(api, http.MethodGet, "/alert-events/stats", authz.PermMetricsRead, d.handleAlertEventStats)

	d.guard(api, http.MethodGet, "/notification-channels", authz.PermMetricsRead, d.handleListChannels)
	d.guard(api, http.MethodPost, "/notification-channels", authz.PermAlertConfig, d.handleCreateChannel)
	d.guard(api, http.MethodPatch, "/notification-channels/:id", authz.PermAlertConfig, d.handleUpdateChannel)
	d.guard(api, http.MethodDelete, "/notification-channels/:id", authz.PermAlertConfig, d.handleDeleteChannel)
	d.guard(api, http.MethodPost, "/notification-channels/:id/test", authz.PermAlertConfig, d.handleTestChannel)
}

// ---------------------------------------------------------------------------
// 告警规则
// ---------------------------------------------------------------------------

// alertRuleRequest 是创建/更新规则的请求体（指针字段 nil=不改）。
type alertRuleRequest struct {
	Name               *string    `json:"name"`
	Metric             *string    `json:"metric"`
	Comparator         *string    `json:"comparator"`
	Threshold          *float64   `json:"threshold"`
	DurationWindows    *int       `json:"duration_windows"`
	CooldownSeconds    *int       `json:"cooldown_seconds"`
	Severity           *string    `json:"severity"`
	SilentUntil        *time.Time `json:"silent_until"`
	ClearSilentUntil   *bool      `json:"clear_silent_until"`
	NotifyChannelID    *string    `json:"notify_channel_id"`
	ClearNotifyChannel *bool      `json:"clear_notify_channel"`
	Enabled            *bool      `json:"enabled"`
}

func validMetric(m string) (store.AlertMetric, bool) {
	switch store.AlertMetric(m) {
	case store.AlertMetricCPU, store.AlertMetricMem, store.AlertMetricDisk,
		store.AlertMetricLoad1, store.AlertMetricNetIn, store.AlertMetricNetOut:
		return store.AlertMetric(m), true
	}
	return "", false
}

func validSeverity(s string) (store.AlertSeverity, bool) {
	switch store.AlertSeverity(s) {
	case store.AlertSeverityInfo, store.AlertSeverityWarning, store.AlertSeverityCritical:
		return store.AlertSeverity(s), true
	}
	return "", false
}

func isPctMetric(m store.AlertMetric) bool {
	return m == store.AlertMetricCPU || m == store.AlertMetricMem || m == store.AlertMetricDisk
}

func validName(name string) bool { return len(strings.TrimSpace(name)) >= 1 && len(name) <= 100 }

// handleListAlertRules 列出全部规则（排序 store 固定 created_at ASC）。
func (d *Deps) handleListAlertRules(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	rules, err := d.Store.AlertRules().List(c.Request.Context())
	if err != nil {
		d.handleStoreError(c, err, apierr.AlertRuleNotFound)
		return
	}
	if rules == nil {
		rules = []store.AlertRule{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rules})
}

func (d *Deps) handleCreateAlertRule(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req alertRuleRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if req.Name == nil || !validName(*req.Name) {
		writeError(c, apierr.ValidationFailed, "name must be 1..100 characters")
		return
	}
	if req.Metric == nil || req.Comparator == nil || req.Threshold == nil {
		writeError(c, apierr.ValidationFailed, "metric, comparator and threshold are required")
		return
	}
	metric, ok := validMetric(*req.Metric)
	if !ok {
		writeError(c, apierr.ValidationFailed, "invalid metric")
		return
	}
	var comparator store.AlertComparator
	switch store.AlertComparator(*req.Comparator) {
	case store.AlertComparatorGT, store.AlertComparatorLT:
		comparator = store.AlertComparator(*req.Comparator)
	default:
		writeError(c, apierr.ValidationFailed, "comparator must be gt or lt")
		return
	}
	if *req.Threshold < 0 || (isPctMetric(metric) && *req.Threshold > 100) {
		writeError(c, apierr.ValidationFailed, "threshold out of range for metric")
		return
	}
	windows := 3
	if req.DurationWindows != nil {
		if *req.DurationWindows < 1 || *req.DurationWindows > 30 {
			writeError(c, apierr.ValidationFailed, "duration_windows must be between 1 and 30")
			return
		}
		windows = *req.DurationWindows
	}
	cooldown := 300
	if req.CooldownSeconds != nil {
		if *req.CooldownSeconds < 60 || *req.CooldownSeconds > 86400 {
			writeError(c, apierr.ValidationFailed, "cooldown_seconds must be between 60 and 86400")
			return
		}
		cooldown = *req.CooldownSeconds
	}
	severity := store.AlertSeverityWarning
	if req.Severity != nil {
		s, ok := validSeverity(*req.Severity)
		if !ok {
			writeError(c, apierr.ValidationFailed, "invalid severity")
			return
		}
		severity = s
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var channelID *string
	if req.NotifyChannelID != nil && *req.NotifyChannelID != "" {
		if _, err := uuid.Parse(*req.NotifyChannelID); err != nil {
			writeError(c, apierr.ValidationFailed, "notify_channel_id must be a valid uuid")
			return
		}
		if _, err := d.Store.NotificationChannels().Get(c.Request.Context(), *req.NotifyChannelID); err != nil {
			writeError(c, apierr.NotificationChannelNotFound, "notify channel not found")
			return
		}
		id := *req.NotifyChannelID
		channelID = &id
	}

	rule, err := d.Store.AlertRules().Create(c.Request.Context(), store.CreateAlertRuleParams{
		Name: *req.Name, Metric: metric, Comparator: comparator, Threshold: *req.Threshold,
		DurationWindows: windows, CooldownSeconds: cooldown, Severity: severity,
		SilentUntil: req.SilentUntil, NotifyChannelID: channelID, Enabled: enabled,
	})
	if err != nil {
		// alert_rules 唯一约束只有 name（migrations 00007）：创建命中 409
		// 即重名——p5 sprint 6 走查：通用 "resource conflict" 让用户无从
		// 知道改哪里；message 场景化为中文「规则名称已存在」（对齐 servers
		// p3 sprint 1 的「服务器名称已存在」口径，前端 CONFLICT 映射把
		// 括号内中文原因透出）。
		if errors.Is(err, store.ErrConflict) {
			writeAPIError(c, http.StatusConflict, apierr.Conflict, "规则名称已存在")
			return
		}
		d.handleStoreError(c, err, apierr.AlertRuleNotFound)
		return
	}
	d.auditEvent(c, "alert_rule.create", "alert_rule", rule.ID, map[string]any{
		"name": rule.Name, "metric": string(rule.Metric), "threshold": rule.Threshold,
		"severity": string(rule.Severity), "enabled": rule.Enabled,
	})
	c.JSON(http.StatusCreated, rule)
}

func (d *Deps) handleGetAlertRule(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	rule, err := d.Store.AlertRules().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.handleStoreError(c, err, apierr.AlertRuleNotFound)
		return
	}
	c.JSON(http.StatusOK, rule)
}

func (d *Deps) handleUpdateAlertRule(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req alertRuleRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	p := store.UpdateAlertRuleParams{
		Name: req.Name, Threshold: req.Threshold, DurationWindows: req.DurationWindows,
		CooldownSeconds: req.CooldownSeconds, Severity: nil,
		SilentUntil: req.SilentUntil, ClearSilentUntil: req.ClearSilentUntil != nil && *req.ClearSilentUntil,
		NotifyChannelID: req.NotifyChannelID, ClearNotifyChannel: req.ClearNotifyChannel != nil && *req.ClearNotifyChannel,
		Enabled: req.Enabled,
	}
	if req.Name != nil && !validName(*req.Name) {
		writeError(c, apierr.ValidationFailed, "name must be 1..100 characters")
		return
	}
	if req.Metric != nil {
		m, ok := validMetric(*req.Metric)
		if !ok {
			writeError(c, apierr.ValidationFailed, "invalid metric")
			return
		}
		p.Metric = &m
	}
	if req.Comparator != nil {
		switch store.AlertComparator(*req.Comparator) {
		case store.AlertComparatorGT, store.AlertComparatorLT:
			cmp := store.AlertComparator(*req.Comparator)
			p.Comparator = &cmp
		default:
			writeError(c, apierr.ValidationFailed, "comparator must be gt or lt")
			return
		}
	}
	if p.Threshold != nil && *p.Threshold < 0 {
		writeError(c, apierr.ValidationFailed, "threshold must be >= 0")
		return
	}
	if req.DurationWindows != nil && (*req.DurationWindows < 1 || *req.DurationWindows > 30) {
		writeError(c, apierr.ValidationFailed, "duration_windows must be between 1 and 30")
		return
	}
	if req.CooldownSeconds != nil && (*req.CooldownSeconds < 60 || *req.CooldownSeconds > 86400) {
		writeError(c, apierr.ValidationFailed, "cooldown_seconds must be between 60 and 86400")
		return
	}
	if req.Severity != nil {
		s, ok := validSeverity(*req.Severity)
		if !ok {
			writeError(c, apierr.ValidationFailed, "invalid severity")
			return
		}
		p.Severity = &s
	}
	if p.ClearSilentUntil && p.SilentUntil != nil {
		writeError(c, apierr.ValidationFailed, "silent_until and clear_silent_until are mutually exclusive")
		return
	}
	if p.ClearNotifyChannel && p.NotifyChannelID != nil {
		writeError(c, apierr.ValidationFailed, "notify_channel_id and clear_notify_channel are mutually exclusive")
		return
	}
	if req.NotifyChannelID != nil && *req.NotifyChannelID != "" {
		if _, err := d.Store.NotificationChannels().Get(c.Request.Context(), *req.NotifyChannelID); err != nil {
			writeError(c, apierr.NotificationChannelNotFound, "notify channel not found")
			return
		}
	}

	rule, err := d.Store.AlertRules().Update(c.Request.Context(), c.Param("id"), p)
	if err != nil {
		// 编辑改名撞其他规则名（同一条唯一约束）：同创建口径返回具体
		// 中文 message，避免通用 "resource conflict"。
		if errors.Is(err, store.ErrConflict) {
			writeAPIError(c, http.StatusConflict, apierr.Conflict, "规则名称已存在")
			return
		}
		d.handleStoreError(c, err, apierr.AlertRuleNotFound)
		return
	}
	d.auditEvent(c, "alert_rule.update", "alert_rule", rule.ID, map[string]any{
		"name": rule.Name, "enabled": rule.Enabled,
	})
	c.JSON(http.StatusOK, rule)
}

func (d *Deps) handleDeleteAlertRule(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	if err := d.Store.AlertRules().Delete(c.Request.Context(), id); err != nil {
		d.handleStoreError(c, err, apierr.AlertRuleNotFound)
		return
	}
	d.auditEvent(c, "alert_rule.delete", "alert_rule", id, nil)
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 告警事件
// ---------------------------------------------------------------------------

// alertEventsResponse 是 GET /api/alert-events 响应。
type alertEventsResponse struct {
	Items        []store.AlertEvent `json:"items"`
	HasMore      bool               `json:"has_more"`
	NextBeforeID *int64             `json:"next_before_id"`
}

// alertEventRangeMax 是 fired_after..fired_before 的最大跨度（下钻窗口）。
const alertEventRangeMax = 7 * 24 * time.Hour

func (d *Deps) handleListAlertEvents(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	beforeID, ok := queryBeforeID(c)
	if !ok {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	filter := store.AlertEventFilter{BeforeID: beforeID, Limit: limit}
	if raw := c.Query("status"); raw != "" {
		switch store.AlertEventStatus(raw) {
		case store.AlertEventFiring, store.AlertEventResolved:
			filter.Status = store.AlertEventStatus(raw)
		default:
			writeError(c, apierr.ValidationFailed, "status must be firing or resolved")
			return
		}
	}
	if raw := c.Query("severity"); raw != "" {
		s, ok := validSeverity(raw)
		if !ok {
			writeError(c, apierr.ValidationFailed, "invalid severity")
			return
		}
		filter.Severity = s
	}
	if raw := c.Query("server_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			writeError(c, apierr.ValidationFailed, "server_id must be a valid uuid")
			return
		}
		filter.ServerID = raw
	}
	// 图表下钻过滤：rule_id + fired_after/fired_before（RFC3339，成对、
	// after<before、跨度 ≤7d）；与 before_id/limit 可叠加，游标语义不变。
	// 区间为半开 [after, before)，与 /api/alert-events/stats 的桶同口径；
	// RFC3339 解析保留小数秒（…59.9995Z 不会被截到毫秒）。
	if raw := c.Query("rule_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			writeError(c, apierr.ValidationFailed, "rule_id must be a valid uuid")
			return
		}
		filter.RuleID = raw
	}
	rawAfter, rawBefore := strings.TrimSpace(c.Query("fired_after")), strings.TrimSpace(c.Query("fired_before"))
	if (rawAfter == "") != (rawBefore == "") {
		writeError(c, apierr.ValidationFailed, "fired_after and fired_before must be provided together")
		return
	}
	if rawAfter != "" {
		after, errA := time.Parse(time.RFC3339, rawAfter)
		before, errB := time.Parse(time.RFC3339, rawBefore)
		if errA != nil || errB != nil {
			writeError(c, apierr.ValidationFailed, "fired_after and fired_before must be RFC3339 timestamps")
			return
		}
		after, before = after.UTC(), before.UTC()
		if !after.Before(before) {
			writeError(c, apierr.ValidationFailed, "fired_after must be before fired_before")
			return
		}
		if before.Sub(after) > alertEventRangeMax {
			writeError(c, apierr.ValidationFailed, "fired_after..fired_before must not exceed 7 days")
			return
		}
		filter.FiredAfter, filter.FiredBefore = &after, &before
	}
	items, hasMore, err := d.Store.AlertEvents().List(c.Request.Context(), filter)
	if err != nil {
		d.handleStoreError(c, err, apierr.AlertEventNotFound)
		return
	}
	if items == nil {
		items = []store.AlertEvent{}
	}
	resp := alertEventsResponse{Items: items, HasMore: hasMore}
	if hasMore && len(items) > 0 {
		id := items[len(items)-1].ID
		resp.NextBeforeID = &id
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// 告警事件统计（GET /api/alert-events/stats）
// ---------------------------------------------------------------------------

// alertSeverityTimelinePoint 是一个时间桶的分级计数；无事件桶补零。
type alertSeverityTimelinePoint struct {
	BucketStart time.Time `json:"bucket_start"`
	Info        int64     `json:"info"`
	Warning     int64     `json:"warning"`
	Critical    int64     `json:"critical"`
}

// alertRuleStatResponse 是 top_rules 元素。
type alertRuleStatResponse struct {
	RuleID      string              `json:"rule_id"`
	RuleName    string              `json:"rule_name"`
	Severity    store.AlertSeverity `json:"severity"`
	Count       int64               `json:"count"`
	ServerCount int64               `json:"server_count"`
	LastFiredAt time.Time           `json:"last_fired_at"`
}

// alertEventStatsResponse 是 GET /api/alert-events/stats 响应。
type alertEventStatsResponse struct {
	Window        string    `json:"window"`
	BucketSeconds int       `json:"bucket_seconds"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	// ServerTime 是本响应生成时的服务端时间。to 对齐到当前桶结束，
	// 前端据 to > server_time 判断最后一桶未走完（走查第 14 轮 low）。
	ServerTime       time.Time                    `json:"server_time"`
	Total            int64                        `json:"total"`
	DistinctRules    int64                        `json:"distinct_rules"`
	SeverityTimeline []alertSeverityTimelinePoint `json:"severity_timeline"`
	TopRules         []alertRuleStatResponse      `json:"top_rules"`
}

// handleAlertEventStats 是图表页的告警统计：两条独立 SQL（时间桶分级 +
// top rules），不翻页、不逐等级循环；空窗口返回 total=0、全零时间轴与
// 空 top_rules（不是错误）。
func (d *Deps) handleAlertEventStats(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	now := d.now()
	win, ok := parseTrendWindow(c, now)
	if !ok {
		return
	}
	topRules := 8
	if raw := strings.TrimSpace(c.Query("top_rules")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 20 {
			writeError(c, apierr.ValidationFailed, "top_rules must be between 1 and 20")
			return
		}
		topRules = n
	}
	stats, err := d.Store.AlertEvents().Stats(c.Request.Context(), store.AlertEventStatsQuery{
		From: win.from, To: win.to, BucketSeconds: win.bucketSeconds, TopRules: topRules,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.Internal)
		return
	}
	// 时间轴补零到窗口的完整桶数（≤28），保证前端 X 轴连续。
	byBucket := make(map[int64]store.AlertEventSeverityBucket, len(stats.Timeline))
	for _, b := range stats.Timeline {
		byBucket[b.BucketStart.Unix()] = b
	}
	span := time.Duration(win.bucketSeconds) * time.Second
	timeline := make([]alertSeverityTimelinePoint, 0, win.buckets)
	for i := 0; i < win.buckets; i++ {
		start := win.from.Add(time.Duration(i) * span)
		b, ok := byBucket[start.Unix()]
		if !ok {
			timeline = append(timeline, alertSeverityTimelinePoint{BucketStart: start})
			continue
		}
		timeline = append(timeline, alertSeverityTimelinePoint{
			BucketStart: start, Info: b.Info, Warning: b.Warning, Critical: b.Critical,
		})
	}
	rules := make([]alertRuleStatResponse, 0, topRules)
	for _, r := range stats.TopRules {
		rules = append(rules, alertRuleStatResponse{
			RuleID: r.RuleID, RuleName: r.RuleName, Severity: r.Severity,
			Count: r.Count, ServerCount: r.ServerCount, LastFiredAt: r.LastFiredAt,
		})
	}
	c.JSON(http.StatusOK, alertEventStatsResponse{
		Window: win.name, BucketSeconds: win.bucketSeconds, From: win.from, To: win.to,
		ServerTime: now,
		Total:      stats.Total, DistinctRules: stats.DistinctRules,
		SeverityTimeline: timeline, TopRules: rules,
	})
}

// ---------------------------------------------------------------------------
// 通知渠道
// ---------------------------------------------------------------------------

// channelRequest 是创建/更新渠道请求体。Config 为明文配置 JSON 对象
// （结构按 kind，见 monitor.NotificationConfig；S22 示例 {"kind":"webhook",
// "config":{...}}），也接受字符串形态；更新时非 nil=整体替换。
type channelRequest struct {
	Name       *string          `json:"name"`
	Kind       *string          `json:"kind"`
	Config     *json.RawMessage `json:"config"`
	ConfigHint *string          `json:"config_hint"`
}

// configString 把 Config 统一为字符串形态 JSON。
func (r *channelRequest) configString() (string, bool) {
	if r.Config == nil {
		return "", false
	}
	raw := *r.Config
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", true
	}
	if trimmed[0] == '"' {
		// 字符串形态：解出内层 JSON 文本。
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true
		}
		return s, true
	}
	return trimmed, true
}

// channelResponse 构造不含密文的响应（store 模型 ConfigEncrypted 已 json:"-"）。
func channelResponse(ch store.NotificationChannel) store.NotificationChannel { return ch }

func (d *Deps) handleListChannels(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	channels, err := d.Store.NotificationChannels().List(c.Request.Context())
	if err != nil {
		d.handleStoreError(c, err, apierr.NotificationChannelNotFound)
		return
	}
	if channels == nil {
		channels = []store.NotificationChannel{}
	}
	c.JSON(http.StatusOK, gin.H{"items": channels})
}

func (d *Deps) handleCreateChannel(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req channelRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if req.Name == nil || !validName(*req.Name) {
		writeError(c, apierr.ValidationFailed, "name must be 1..100 characters")
		return
	}
	if req.Kind == nil || req.Config == nil {
		writeError(c, apierr.ValidationFailed, "kind and config are required")
		return
	}
	configStr, hasConfig := req.configString()
	if !hasConfig || configStr == "" {
		writeError(c, apierr.ValidationFailed, "config must not be empty")
		return
	}
	var kind store.NotificationKind
	switch store.NotificationKind(*req.Kind) {
	case store.NotificationWebhook, store.NotificationFeishu, store.NotificationDingtalk, store.NotificationEmail:
		kind = store.NotificationKind(*req.Kind)
	default:
		writeError(c, apierr.ValidationFailed, "invalid channel kind")
		return
	}
	cfg, err := monitor.ParseNotificationConfig(kind, configStr)
	if err != nil {
		writeError(c, apierr.NotificationConfigInvalid, "channel config schema invalid")
		return
	}
	if d.Cipher == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "cipher unavailable")
		return
	}
	enc, err := d.Cipher.Encrypt(configStr)
	if err != nil {
		d.logError(c, "encrypt channel config failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt config")
		return
	}
	hint := monitor.ConfigHint(kind, cfg)
	if req.ConfigHint != nil && *req.ConfigHint != "" {
		hint = *req.ConfigHint
	}
	ch, err := d.Store.NotificationChannels().Create(c.Request.Context(), store.CreateNotificationChannelParams{
		Name: *req.Name, Kind: kind, ConfigJSON: enc, ConfigHint: hint,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotificationChannelNotFound)
		return
	}
	d.auditEvent(c, "notification.create", "notification_channel", ch.ID, map[string]any{
		"name": ch.Name, "kind": string(ch.Kind), "config_hint": ch.ConfigHint,
	})
	c.JSON(http.StatusCreated, channelResponse(ch))
}

func (d *Deps) handleUpdateChannel(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req channelRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	id := c.Param("id")
	p := store.UpdateNotificationChannelParams{Name: req.Name, ConfigHint: req.ConfigHint}
	if req.Name != nil && !validName(*req.Name) {
		writeError(c, apierr.ValidationFailed, "name must be 1..100 characters")
		return
	}
	var kind store.NotificationKind
	if req.Kind != nil {
		switch store.NotificationKind(*req.Kind) {
		case store.NotificationWebhook, store.NotificationFeishu, store.NotificationDingtalk, store.NotificationEmail:
			kind = store.NotificationKind(*req.Kind)
		default:
			writeError(c, apierr.ValidationFailed, "invalid channel kind")
			return
		}
		p.Kind = &kind
	}
	if req.Config != nil {
		configStr, hasConfig := req.configString()
		if !hasConfig || configStr == "" {
			writeError(c, apierr.ValidationFailed, "config must not be empty")
			return
		}
		effectiveKind := kind
		if effectiveKind == "" {
			cur, err := d.Store.NotificationChannels().Get(c.Request.Context(), id)
			if err != nil {
				d.handleStoreError(c, err, apierr.NotificationChannelNotFound)
				return
			}
			effectiveKind = cur.Kind
		}
		if _, err := monitor.ParseNotificationConfig(effectiveKind, configStr); err != nil {
			writeError(c, apierr.NotificationConfigInvalid, "channel config schema invalid")
			return
		}
		if d.Cipher == nil {
			writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "cipher unavailable")
			return
		}
		enc, err := d.Cipher.Encrypt(configStr)
		if err != nil {
			d.logError(c, "encrypt channel config failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt config")
			return
		}
		p.ConfigJSON = &enc
	}
	ch, err := d.Store.NotificationChannels().Update(c.Request.Context(), id, p)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotificationChannelNotFound)
		return
	}
	d.auditEvent(c, "notification.update", "notification_channel", ch.ID, map[string]any{
		"name": ch.Name, "kind": string(ch.Kind),
	})
	c.JSON(http.StatusOK, channelResponse(ch))
}

func (d *Deps) handleDeleteChannel(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	if err := d.Store.NotificationChannels().Delete(c.Request.Context(), id); err != nil {
		d.handleStoreError(c, err, apierr.NotificationChannelNotFound)
		return
	}
	d.auditEvent(c, "notification.delete", "notification_channel", id, nil)
	c.Status(http.StatusNoContent)
}

// handleTestChannel 投递一条测试消息（解密配置 → 真实发送 → NotifyResult）。
// 失败返回 502 NOTIFICATION_TEST_FAILED（含结果体）。
func (d *Deps) handleTestChannel(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.Monitor == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "monitor module unavailable")
		return
	}
	ch, err := d.Store.NotificationChannels().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(c, apierr.NotificationChannelNotFound, "channel not found")
			return
		}
		d.handleStoreError(c, err, apierr.NotificationChannelNotFound)
		return
	}
	res, err := d.Monitor.TestChannel(c.Request.Context(), ch)
	d.auditEvent(c, "notification.test", "notification_channel", ch.ID, map[string]any{
		"kind": string(ch.Kind), "ok": res.OK, "error_code": res.ErrorCode,
	})
	if err != nil {
		writeAPIErrorDetail(c, http.StatusBadGateway, apierr.NotificationTestFailed,
			"channel test delivery failed", map[string]any{"result": res})
		return
	}
	c.JSON(http.StatusOK, res)
}
