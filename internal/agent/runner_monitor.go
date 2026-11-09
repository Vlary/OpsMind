// runner_monitor.go 实现五期 M1 的三个只读 AI 工具（契约 7.1/7.2）：
//   - get_metrics  指标时间序列（minutes 15..1440，缺省 60；点数上限 1440
//     ——契约五期行为约定：不做降采样）
//   - list_alerts  告警事件列表（status/severity 过滤，limit ≤200）
//   - audit_search 审计检索（server/action/resource_type/from/to 时间过滤，
//     limit ≤200；不返回 hash/prev_hash 内部字段）
//
// 三者恒 safe、无审批，纯 store 查询。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"aiops/internal/store"
)

// getMetricsMinutesLimits 是 minutes 参数边界（契约 7.1）。
const (
	getMetricsMinutesMin = 15
	getMetricsMinutesMax = 1440
	getMetricsPointLimit = 1440
)

// runGetMetrics 实现 get_metrics 工具。
func (r *Runner) runGetMetrics(ctx context.Context, raw json.RawMessage) toolOutcome {
	var args getMetricsArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	if args.ServerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id is required"}, Status: store.ToolFailed}
	}
	minutes := 60
	if args.Minutes != nil {
		if *args.Minutes < getMetricsMinutesMin || *args.Minutes > getMetricsMinutesMax {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "minutes must be between 15 and 1440"}, Status: store.ToolFailed}
		}
		minutes = *args.Minutes
	}
	metric := ""
	if args.Metric != nil && *args.Metric != "" {
		switch store.AlertMetric(*args.Metric) {
		case store.AlertMetricCPU, store.AlertMetricMem, store.AlertMetricDisk,
			store.AlertMetricLoad1, store.AlertMetricNetIn, store.AlertMetricNetOut:
			metric = *args.Metric
		default:
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid metric"}, Status: store.ToolFailed}
		}
	}
	if _, err := r.st.Servers().Get(ctx, args.ServerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
		}
		return toolOutcome{Fatal: err}
	}
	now := r.now()
	points, err := r.st.Metrics().Window(ctx, store.MetricWindow{
		ServerID: args.ServerID,
		From:     now.Add(-time.Duration(minutes) * time.Minute),
		To:       now.Add(time.Minute), Limit: getMetricsPointLimit,
	})
	if err != nil {
		return toolOutcome{Fatal: err}
	}
	out := make([]map[string]any, 0, len(points))
	for _, m := range points {
		all := map[string]any{
			"created_at": m.CreatedAt.UTC().Format(time.RFC3339),
			"cpu_pct":    m.CPU, "mem_pct": m.Mem, "disk_pct": m.Disk,
			"load_1": m.Load1, "net_in_bps": m.NetIn, "net_out_bps": m.NetOut,
		}
		if metric != "" {
			out = append(out, map[string]any{"created_at": all["created_at"], metric: all[metric]})
			continue
		}
		out = append(out, all)
	}
	res, _ := json.Marshal(map[string]any{
		"server_id": args.ServerID, "minutes": minutes,
		"points": out, "truncated": len(points) >= getMetricsPointLimit,
	})
	return toolOutcome{Result: res}
}

// runListAlerts 实现 list_alerts 工具。
func (r *Runner) runListAlerts(ctx context.Context, raw json.RawMessage) toolOutcome {
	var args listAlertsArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	filter := store.AlertEventFilter{Limit: 50}
	if args.Status != nil && *args.Status != "" {
		switch store.AlertEventStatus(*args.Status) {
		case store.AlertEventFiring, store.AlertEventResolved:
			filter.Status = store.AlertEventStatus(*args.Status)
		default:
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "status must be firing or resolved"}, Status: store.ToolFailed}
		}
	}
	if args.Severity != nil && *args.Severity != "" {
		switch store.AlertSeverity(*args.Severity) {
		case store.AlertSeverityInfo, store.AlertSeverityWarning, store.AlertSeverityCritical:
			filter.Severity = store.AlertSeverity(*args.Severity)
		default:
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid severity"}, Status: store.ToolFailed}
		}
	}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > 200 {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "limit must be between 1 and 200"}, Status: store.ToolFailed}
		}
		filter.Limit = *args.Limit
	}
	items, hasMore, err := r.st.AlertEvents().List(ctx, filter)
	if err != nil {
		return toolOutcome{Fatal: err}
	}
	if items == nil {
		items = []store.AlertEvent{}
	}
	res, _ := json.Marshal(map[string]any{"alerts": items, "has_more": hasMore})
	return toolOutcome{Result: res}
}

// auditSearchItem 是 audit_search 的输出元素：不含 hash/prev_hash（契约
// 7.2），server_name 由内存映射装配（资产删除后空串）。
type auditSearchItem struct {
	ID             int64           `json:"id"`
	CreatedAt      time.Time       `json:"created_at"`
	ActorType      string          `json:"actor_type"`
	ActorID        string          `json:"actor_id"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id"`
	ServerID       string          `json:"server_id,omitempty"`
	ServerName     string          `json:"server_name,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	IP             string          `json:"ip,omitempty"`
	Detail         json.RawMessage `json:"detail"`
}

// runAuditSearch 实现 audit_search 工具（AuditStore.List 扩展时间过滤）。
func (r *Runner) runAuditSearch(ctx context.Context, raw json.RawMessage) toolOutcome {
	var args auditSearchArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	filter := store.AuditFilter{Limit: 50}
	if args.ServerID != nil && *args.ServerID != "" {
		filter.ServerID = *args.ServerID
	}
	if args.Action != nil {
		if len(*args.Action) < 1 || len(*args.Action) > 100 {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "action must be 1..100 characters"}, Status: store.ToolFailed}
		}
		filter.Action = *args.Action
	}
	if args.ResourceType != nil {
		if len(*args.ResourceType) < 1 || len(*args.ResourceType) > 100 {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "resource_type must be 1..100 characters"}, Status: store.ToolFailed}
		}
		filter.ResourceType = *args.ResourceType
	}
	for _, pair := range []struct {
		raw  *string
		dest *time.Time
		name string
	}{
		{args.From, &filter.FromTime, "from"},
		{args.To, &filter.ToTime, "to"},
	} {
		if pair.raw == nil || *pair.raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, *pair.raw)
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: pair.name + " must be an ISO8601 date-time"}, Status: store.ToolFailed}
		}
		*pair.dest = t
	}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > 200 {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "limit must be between 1 and 200"}, Status: store.ToolFailed}
		}
		filter.Limit = *args.Limit
	}
	items, hasMore, err := r.st.AuditLogs().List(ctx, filter)
	if err != nil {
		return toolOutcome{Fatal: err}
	}

	// server_name 装配（一次 List，内存映射；上限受资产表规模约束）。
	names := map[string]string{}
	if servers, err := r.st.Servers().List(ctx); err == nil {
		for _, s := range servers {
			names[s.ID] = s.Name
		}
	}
	out := make([]auditSearchItem, 0, len(items))
	for _, a := range items {
		out = append(out, auditSearchItem{
			ID: a.ID, CreatedAt: a.CreatedAt, ActorType: a.ActorType, ActorID: a.ActorID,
			Action: a.Action, ResourceType: a.ResourceType, ResourceID: a.ResourceID,
			ServerID: deref(a.ServerID), ServerName: names[deref(a.ServerID)],
			ConversationID: deref(a.ConversationID), IP: a.IP, Detail: a.Detail,
		})
	}
	res, _ := json.Marshal(map[string]any{"items": out, "has_more": hasMore})
	return toolOutcome{Result: res}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
