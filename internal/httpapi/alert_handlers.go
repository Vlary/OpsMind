// alert_handlers.go 实现五期 M1 的告警 REST 路由（契约 2.5/4.19/4.20/4.18）：
// 规则 CRUD、事件列表（status/severity/server_id 过滤 + before_id 分页）、
// 通知渠道 CRUD + 测试发送（凭据 AES-GCM 加密落库，永不回显）。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/monitor"
	"aiops/internal/store"
)

// RegisterAlertRoutes 注册 M1 告警路由（契约 2.5）。
func (d *Deps) RegisterAlertRoutes(api *gin.RouterGroup) {
	api.GET("/alert-rules", d.handleListAlertRules)
	api.POST("/alert-rules", d.handleCreateAlertRule)
	api.GET("/alert-rules/:id", d.handleGetAlertRule)
	api.PATCH("/alert-rules/:id", d.handleUpdateAlertRule)
	api.DELETE("/alert-rules/:id", d.handleDeleteAlertRule)

	api.GET("/alert-events", d.handleListAlertEvents)

	api.GET("/notification-channels", d.handleListChannels)
	api.POST("/notification-channels", d.handleCreateChannel)
	api.PATCH("/notification-channels/:id", d.handleUpdateChannel)
	api.DELETE("/notification-channels/:id", d.handleDeleteChannel)
	api.POST("/notification-channels/:id/test", d.handleTestChannel)
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
