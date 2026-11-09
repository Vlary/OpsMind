// runner_monitor_test.go 覆盖五期 M1 三个只读工具（G37 的 M1 部分）：
// get_metrics / list_alerts / audit_search 的参数校验、结果结构与
// server/越界错误路径。走 runXxx 直测（工具执行体），完整 fake-LLM 循环
// 由既有 G06 框架覆盖（executeToolCall 分发与 tool.result 事件同构）。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"aiops/internal/store"
)

// monitorTestStore 包装 *testStore：覆盖 Metrics/AlertEvents/AuditLogs 三个
// M1 依赖的子存储（其余复用 testStore 现有 fake）。
type monitorTestStore struct {
	*testStore

	metrics []store.ServerMetric
	alerts  []store.AlertEvent
	audits  []store.AuditLog
}

func (s *monitorTestStore) Metrics() store.MetricStore { return &fakeMetricStore{s: s} }
func (s *monitorTestStore) AlertEvents() store.AlertEventStore {
	return &fakeAlertEventStore{s: s}
}
func (s *monitorTestStore) AuditLogs() store.AuditStore { return &fakeAuditListStore{s: s} }

type fakeMetricStore struct{ s *monitorTestStore }

func (f *fakeMetricStore) BatchInsert(context.Context, []store.InsertMetricParams) error {
	panic("not used in tool tests")
}
func (f *fakeMetricStore) Latest(context.Context) ([]store.ServerMetricLatest, error) {
	return nil, nil
}
func (f *fakeMetricStore) LatestByServer(context.Context, string) (store.ServerMetricLatest, error) {
	return store.ServerMetricLatest{}, store.ErrNotFound
}
func (f *fakeMetricStore) Window(_ context.Context, w store.MetricWindow) ([]store.ServerMetric, error) {
	if w.Limit > 1440 {
		return nil, store.ErrInvalid
	}
	var out []store.ServerMetric
	for _, m := range f.s.metrics {
		if w.ServerID == "" || m.ServerID == w.ServerID {
			if !m.CreatedAt.Before(w.From) && !m.CreatedAt.After(w.To) {
				out = append(out, m)
			}
		}
	}
	return out, nil
}
func (f *fakeMetricStore) DeleteBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

type fakeAlertEventStore struct{ s *monitorTestStore }

func (f *fakeAlertEventStore) Create(context.Context, store.AlertRule, string, float64, string, time.Time) (store.AlertEvent, error) {
	panic("not used in tool tests")
}
func (f *fakeAlertEventStore) List(_ context.Context, flt store.AlertEventFilter) ([]store.AlertEvent, bool, error) {
	var out []store.AlertEvent
	for _, ev := range f.s.alerts {
		if flt.Status != "" && ev.Status != flt.Status {
			continue
		}
		if flt.Severity != "" && ev.Severity != flt.Severity {
			continue
		}
		out = append(out, ev)
	}
	limit := flt.Limit
	if limit <= 0 {
		limit = 50
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}
func (f *fakeAlertEventStore) Get(context.Context, int64) (store.AlertEvent, error) {
	return store.AlertEvent{}, store.ErrNotFound
}
func (f *fakeAlertEventStore) Resolve(context.Context, int64, time.Time) (store.AlertEvent, error) {
	return store.AlertEvent{}, store.ErrConflict
}
func (f *fakeAlertEventStore) SetNotifyStatus(context.Context, int64, store.NotifyStatus, time.Time) error {
	return nil
}
func (f *fakeAlertEventStore) CountFiring(context.Context) (int64, error) { return 0, nil }
func (f *fakeAlertEventStore) CountFiringByServer(context.Context, string) (int64, error) {
	return 0, nil
}

type fakeAuditListStore struct{ s *monitorTestStore }

func (f *fakeAuditListStore) Append(context.Context, store.CreateAuditParams) error { return nil }
func (f *fakeAuditListStore) LastHash(context.Context) (string, error)              { return "", nil }
func (f *fakeAuditListStore) List(_ context.Context, flt store.AuditFilter) ([]store.AuditLog, bool, error) {
	var out []store.AuditLog
	for _, a := range f.s.audits {
		if flt.Action != "" && a.Action != flt.Action {
			continue
		}
		if flt.ResourceType != "" && a.ResourceType != flt.ResourceType {
			continue
		}
		if flt.ServerID != "" && (a.ServerID == nil || *a.ServerID != flt.ServerID) {
			continue
		}
		if !flt.FromTime.IsZero() && a.CreatedAt.Before(flt.FromTime) {
			continue
		}
		if !flt.ToTime.IsZero() && a.CreatedAt.After(flt.ToTime) {
			continue
		}
		out = append(out, a)
	}
	limit := flt.Limit
	if limit <= 0 {
		limit = 50
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

func newMonitorTestStore(t *testing.T) *monitorTestStore {
	t.Helper()
	base := seed(false)
	st := &monitorTestStore{testStore: base}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		st.metrics = append(st.metrics, store.ServerMetric{
			ServerID: "s1", CreatedAt: now.Add(time.Duration(i-10) * time.Minute),
			CPU: 10 + float64(i), Mem: 40, Disk: 50, Load1: 0.5, NetIn: 1000, NetOut: 2000,
		})
	}
	st.alerts = append(st.alerts, store.AlertEvent{
		ID: 12, RuleID: "r1", RuleName: "cpu-high", ServerID: "s1", ServerName: "web-1",
		Metric: store.AlertMetricCPU, Status: store.AlertEventFiring, Severity: store.AlertSeverityWarning,
		Value: 95.5, Message: "cpu_pct 95.5 > 90.0 持续 3 个采样点", FiredAt: now,
		NotifyStatus: store.NotifySent,
	})
	s1 := "s1"
	st.audits = append(st.audits, store.AuditLog{
		ID: 123, CreatedAt: now, ActorType: "user", ActorID: "u1",
		Action: "command.executed", ResourceType: "server", ResourceID: "s1",
		ServerID: &s1, Detail: json.RawMessage(`{"command":"df -h","exit_code":0}`),
	})
	return st
}

func TestToolGetMetricsHappyPath(t *testing.T) {
	st := newMonitorTestStore(t)
	r := newRunner(st, nil, nil, nil, nil)
	out := r.runGetMetrics(context.Background(), json.RawMessage(`{"server_id":"s1","minutes":15}`))
	if out.Err != nil {
		t.Fatalf("err = %v", out.Err)
	}
	var res struct {
		ServerID  string           `json:"server_id"`
		Minutes   int              `json:"minutes"`
		Points    []map[string]any `json:"points"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ServerID != "s1" || res.Minutes != 15 || len(res.Points) != 5 {
		t.Fatalf("res = %+v", res)
	}
	first := res.Points[0]
	if first["cpu_pct"] != float64(10) {
		t.Fatalf("first point = %v", first)
	}
}

func TestToolGetMetricsMetricFilter(t *testing.T) {
	st := newMonitorTestStore(t)
	r := newRunner(st, nil, nil, nil, nil)
	out := r.runGetMetrics(context.Background(), json.RawMessage(`{"server_id":"s1","metric":"mem_pct"}`))
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	var res struct {
		Points []map[string]any `json:"points"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Points) == 0 {
		t.Fatal("no points")
	}
	p := res.Points[0]
	if _, wrong := p["cpu_pct"]; wrong {
		t.Fatal("metric filter must drop other series")
	}
	if p["mem_pct"] != float64(40) {
		t.Fatalf("mem_pct = %v", p["mem_pct"])
	}
}

func TestToolGetMetricsArgValidation(t *testing.T) {
	st := newMonitorTestStore(t)
	r := newRunner(st, nil, nil, nil, nil)
	cases := []struct {
		name string
		args string
	}{
		{"minutes too small", `{"server_id":"s1","minutes":10}`},
		{"minutes too large", `{"server_id":"s1","minutes":1441}`},
		{"bad metric", `{"server_id":"s1","metric":"bogus"}`},
		{"missing server", `{"minutes":60}`},
		{"unknown server", `{"server_id":"ghost","minutes":60}`},
		{"unknown field", `{"server_id":"s1","bogus":1}`},
	}
	for _, tc := range cases {
		out := r.runGetMetrics(context.Background(), json.RawMessage(tc.args))
		if out.Err == nil || out.Err.Code != codeToolArgsInvalid && out.Err.Code != "SERVER_NOT_FOUND" {
			t.Fatalf("case %s: err = %+v", tc.name, out.Err)
		}
		if out.Status != store.ToolFailed {
			t.Fatalf("case %s: status = %s", tc.name, out.Status)
		}
	}
}

func TestToolListAlerts(t *testing.T) {
	st := newMonitorTestStore(t)
	r := newRunner(st, nil, nil, nil, nil)
	out := r.runListAlerts(context.Background(), json.RawMessage(`{"status":"firing","severity":"warning"}`))
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	var res struct {
		Alerts  []store.AlertEvent `json:"alerts"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Alerts) != 1 || res.Alerts[0].RuleName != "cpu-high" || res.Alerts[0].Value != 95.5 {
		t.Fatalf("alerts = %+v", res.Alerts)
	}
	// 过滤不命中。
	out = r.runListAlerts(context.Background(), json.RawMessage(`{"status":"resolved"}`))
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Alerts) != 0 {
		t.Fatalf("resolved filter should be empty, got %d", len(res.Alerts))
	}
	// 越界参数。
	out = r.runListAlerts(context.Background(), json.RawMessage(`{"limit":201}`))
	if out.Err == nil || out.Err.Code != codeToolArgsInvalid {
		t.Fatalf("limit 201 must be TOOL_ARGS_INVALID, got %+v", out.Err)
	}
	out = r.runListAlerts(context.Background(), json.RawMessage(`{"status":"bogus"}`))
	if out.Err == nil || out.Err.Code != codeToolArgsInvalid {
		t.Fatalf("bad status must be TOOL_ARGS_INVALID, got %+v", out.Err)
	}
}

func TestToolAuditSearch(t *testing.T) {
	st := newMonitorTestStore(t)
	r := newRunner(st, nil, nil, nil, nil)
	// 时间窗用相对值（now-24h..now+1h）：固定日期窗口会随日历过期
	//（六期回归发现：2026-10-28 00:00Z 之后该用例必失败）。
	fromISO := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	toISO := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	query := fmt.Sprintf(`{"server_id":"s1","action":"command.executed","from":%q,"to":%q}`, fromISO, toISO)
	out := r.runAuditSearch(context.Background(), json.RawMessage(query))
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	var res struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %v", res.Items)
	}
	item := res.Items[0]
	if item["action"] != "command.executed" {
		t.Fatalf("item = %v", item)
	}
	if item["server_name"] != "web-1" {
		t.Fatalf("server_name = %v", item["server_name"])
	}
	// hash/prev_hash 不得出现（内部字段）。
	if _, has := item["hash"]; has {
		t.Fatal("hash must not be exposed")
	}
	if _, has := item["prev_hash"]; has {
		t.Fatal("prev_hash must not be exposed")
	}
	// 时间过滤：from 晚于记录时间 → 空（动态未来时间，避免跨时区脆弱）。
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	out = r.runAuditSearch(context.Background(), json.RawMessage(`{"from":"`+future+`"}`))
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("time filter should exclude the row, got %d", len(res.Items))
	}
	// 畸形时间 / 越界 limit / 未知字段。
	for _, bad := range []string{
		`{"from":"not-a-time"}`,
		`{"limit":0}`,
		`{"limit":201}`,
		`{"action":"x","bogus":1}`,
	} {
		out := r.runAuditSearch(context.Background(), json.RawMessage(bad))
		if out.Err == nil || out.Err.Code != codeToolArgsInvalid {
			t.Fatalf("args %s must be TOOL_ARGS_INVALID, got %+v", bad, out.Err)
		}
	}
}

// TestMonitorToolDefinitionsRegistered 断言三个工具进入定义表与名单。
func TestMonitorToolDefinitionsRegistered(t *testing.T) {
	defs := ToolDefinitions()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range []string{ToolGetMetrics, ToolListAlerts, ToolAuditSearch} {
		if !names[want] {
			t.Fatalf("tool %s missing from definitions", want)
		}
		if !knownTool(want) {
			t.Fatalf("tool %s missing from ToolNames", want)
		}
	}
}
