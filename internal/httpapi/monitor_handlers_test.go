// monitor_handlers_test.go 实现五期 M1 的监控/告警 REST 测试：包级补丁给
// *memStore 挂 Metrics/AlertRules/AlertEvents/NotificationChannels（同 W3/W4
// 对 TerminalSessions/RdpSessions 的补丁先例），并覆盖 S22①②③ 的
// handler 行为（渠道创建响应无密文回显、规则 CRUD 校验、事件列表、
// overview 结构、collector PUT/GET 回读、metrics 时间序列）。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/cryptox"
	"aiops/internal/monitor"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// memStore 的 M1 子存储补丁
// ---------------------------------------------------------------------------

// monitorState 是 memStore 的 M1 数据面（与 fakes_test.go 的字段隔离）。
type monitorState struct {
	mu           sync.Mutex
	metrics      []store.InsertMetricParams
	latest       map[string]store.ServerMetricLatest
	rules        map[string]store.AlertRule
	events       []store.AlertEvent
	channels     map[string]store.NotificationChannel
	nextEventID  int64
	auditWritten []string
}

var monState = func() *monitorState {
	return &monitorState{
		latest:   map[string]store.ServerMetricLatest{},
		rules:    map[string]store.AlertRule{},
		channels: map[string]store.NotificationChannel{},
	}
}()

func resetMonitorState() {
	monState = &monitorState{
		latest:   map[string]store.ServerMetricLatest{},
		rules:    map[string]store.AlertRule{},
		channels: map[string]store.NotificationChannel{},
	}
}

func (m *memStore) Metrics() store.MetricStore { return &memMetrics{st: monState} }

type memMetrics struct{ st *monitorState }

func (f *memMetrics) BatchInsert(_ context.Context, items []store.InsertMetricParams) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.metrics = append(f.st.metrics, items...)
	for _, it := range items {
		f.st.latest[it.ServerID] = store.ServerMetricLatest{
			ServerID: it.ServerID, CreatedAt: it.CreatedAt, UpdatedAt: it.CreatedAt,
			CPU: it.CPU, Mem: it.Mem, Disk: it.Disk, Load1: it.Load1, NetIn: it.NetIn, NetOut: it.NetOut,
		}
	}
	return nil
}
func (f *memMetrics) Latest(context.Context) ([]store.ServerMetricLatest, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := make([]store.ServerMetricLatest, 0, len(f.st.latest))
	for _, m := range f.st.latest {
		out = append(out, m)
	}
	return out, nil
}
func (f *memMetrics) LatestByServer(_ context.Context, id string) (store.ServerMetricLatest, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if m, ok := f.st.latest[id]; ok {
		return m, nil
	}
	return store.ServerMetricLatest{}, store.ErrNotFound
}
func (f *memMetrics) Window(_ context.Context, w store.MetricWindow) ([]store.ServerMetric, error) {
	if w.Limit > 1440 {
		return nil, store.ErrInvalid
	}
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	var out []store.ServerMetric
	for _, it := range f.st.metrics {
		if w.ServerID != "" && it.ServerID != w.ServerID {
			continue
		}
		if it.CreatedAt.Before(w.From) || it.CreatedAt.After(w.To) {
			continue
		}
		out = append(out, store.ServerMetric{
			ServerID: it.ServerID, CreatedAt: it.CreatedAt, CPU: it.CPU, Mem: it.Mem,
			Disk: it.Disk, Load1: it.Load1, NetIn: it.NetIn, NetOut: it.NetOut,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (f *memMetrics) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (m *memStore) AlertRules() store.AlertRuleStore { return &memRules{st: monState} }

type memRules struct{ st *monitorState }

func (f *memRules) List(context.Context) ([]store.AlertRule, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := make([]store.AlertRule, 0, len(f.st.rules))
	for _, r := range f.st.rules {
		out = append(out, r)
	}
	return out, nil
}
func (f *memRules) Get(_ context.Context, id string) (store.AlertRule, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if r, ok := f.st.rules[id]; ok {
		return r, nil
	}
	return store.AlertRule{}, store.ErrNotFound
}
func (f *memRules) Create(_ context.Context, p store.CreateAlertRuleParams) (store.AlertRule, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	for _, r := range f.st.rules {
		if r.Name == p.Name {
			return store.AlertRule{}, store.ErrConflict
		}
	}
	id := uuid.NewString()
	r := store.AlertRule{
		ID: id, Name: p.Name, Metric: p.Metric, Comparator: p.Comparator,
		Threshold: p.Threshold, DurationWindows: p.DurationWindows,
		CooldownSeconds: p.CooldownSeconds, Severity: p.Severity,
		SilentUntil: p.SilentUntil, NotifyChannelID: p.NotifyChannelID,
		Enabled: p.Enabled, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.st.rules[id] = r
	return r, nil
}
func (f *memRules) Update(_ context.Context, id string, p store.UpdateAlertRuleParams) (store.AlertRule, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	r, ok := f.st.rules[id]
	if !ok {
		return store.AlertRule{}, store.ErrNotFound
	}
	if p.Name != nil {
		// 模拟生产 alert_rules.name UNIQUE（migrations 00007）：改名撞
		// 其他规则名返回 ErrConflict（pgstore 的 UPDATE 同样命中唯一索引）。
		for _, other := range f.st.rules {
			if other.ID != id && other.Name == *p.Name {
				return store.AlertRule{}, store.ErrConflict
			}
		}
		r.Name = *p.Name
	}
	if p.Enabled != nil {
		r.Enabled = *p.Enabled
	}
	f.st.rules[id] = r
	return r, nil
}
func (f *memRules) Delete(_ context.Context, id string) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if _, ok := f.st.rules[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.st.rules, id)
	return nil
}

func (m *memStore) AlertEvents() store.AlertEventStore { return &memEvents{st: monState} }

type memEvents struct{ st *monitorState }

func (f *memEvents) Create(_ context.Context, rule store.AlertRule, serverID string, value float64, message string, firedAt time.Time) (store.AlertEvent, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	for _, ev := range f.st.events {
		if ev.RuleID == rule.ID && ev.ServerID == serverID && ev.Status == store.AlertEventFiring {
			return store.AlertEvent{}, store.ErrConflict
		}
	}
	f.st.nextEventID++
	ev := store.AlertEvent{
		ID: f.st.nextEventID, RuleID: rule.ID, RuleName: rule.Name, ServerID: serverID,
		Metric: rule.Metric, Status: store.AlertEventFiring, Severity: rule.Severity,
		Value: value, Message: message, FiredAt: firedAt, NotifyStatus: store.NotifyNone,
	}
	f.st.events = append(f.st.events, ev)
	return ev, nil
}
func (f *memEvents) List(_ context.Context, flt store.AlertEventFilter) ([]store.AlertEvent, bool, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	var out []store.AlertEvent
	for _, ev := range f.st.events {
		if flt.Status != "" && ev.Status != flt.Status {
			continue
		}
		if flt.Severity != "" && ev.Severity != flt.Severity {
			continue
		}
		if flt.ServerID != "" && ev.ServerID != flt.ServerID {
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
func (f *memEvents) Get(context.Context, int64) (store.AlertEvent, error) {
	return store.AlertEvent{}, store.ErrNotFound
}
func (f *memEvents) Resolve(_ context.Context, id int64, at time.Time) (store.AlertEvent, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	for i := range f.st.events {
		if f.st.events[i].ID == id && f.st.events[i].Status == store.AlertEventFiring {
			f.st.events[i].Status = store.AlertEventResolved
			f.st.events[i].ResolvedAt = &at
			return f.st.events[i], nil
		}
	}
	return store.AlertEvent{}, store.ErrConflict
}
func (f *memEvents) SetNotifyStatus(_ context.Context, id int64, status store.NotifyStatus, at time.Time) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	for i := range f.st.events {
		if f.st.events[i].ID == id {
			f.st.events[i].NotifyStatus = status
			f.st.events[i].LastNotifiedAt = &at
		}
	}
	return nil
}
func (f *memEvents) CountFiring(context.Context) (int64, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	var n int64
	for _, ev := range f.st.events {
		if ev.Status == store.AlertEventFiring {
			n++
		}
	}
	return n, nil
}
func (f *memEvents) CountFiringByServer(context.Context, string) (int64, error) { return 0, nil }

func (m *memStore) NotificationChannels() store.NotificationChannelStore {
	return &memChannels{st: monState}
}

type memChannels struct{ st *monitorState }

func (f *memChannels) List(context.Context) ([]store.NotificationChannel, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := make([]store.NotificationChannel, 0, len(f.st.channels))
	for _, ch := range f.st.channels {
		out = append(out, ch)
	}
	return out, nil
}
func (f *memChannels) Get(_ context.Context, id string) (store.NotificationChannel, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if ch, ok := f.st.channels[id]; ok {
		return ch, nil
	}
	return store.NotificationChannel{}, store.ErrNotFound
}
func (f *memChannels) Create(_ context.Context, p store.CreateNotificationChannelParams) (store.NotificationChannel, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	id := uuid.NewString()
	ch := store.NotificationChannel{
		ID: id, Name: p.Name, Kind: p.Kind, ConfigEncrypted: p.ConfigJSON,
		ConfigHint: p.ConfigHint, HasConfig: p.ConfigJSON != "",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.st.channels[id] = ch
	return ch, nil
}
func (f *memChannels) Update(context.Context, string, store.UpdateNotificationChannelParams) (store.NotificationChannel, error) {
	return store.NotificationChannel{}, store.ErrNotFound
}
func (f *memChannels) Delete(_ context.Context, id string) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if _, ok := f.st.channels[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.st.channels, id)
	return nil
}

// BatchJobs 桩：M3（五期）向 store.Store 增挂 BatchJobStore 后 memStore
// 需要满足接口；真实 fake 与 handler 测试见 batch_handlers_test.go
//（M3 落地时替换本桩——memStore.BatchJobs 由该文件提供）。

// ---------------------------------------------------------------------------
// handler 测试
// ---------------------------------------------------------------------------

// newMonitorTestEnv 构造挂了 M1 路由的测试引擎。同时重置巡检数据面
// （overview 的 last_inspection 读 InspectionReports，需与 monitor 状态
// 一同归零，避免同包其他用例残留报告污染概要断言）。
func newMonitorTestEnv(t *testing.T) (*gin.Engine, *Deps) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetMonitorState()
	resetInspectionState()
	st := newMemStore()
	d := &Deps{Store: st, Cipher: testCipherForMonitor(t), Clock: func() time.Time { return time.Now().UTC() }}
	r := gin.New()
	protected := r.Group("/api")
	d.RegisterMonitorRoutes(protected)
	d.RegisterAlertRoutes(protected)
	return r, d
}

func testCipherForMonitor(t *testing.T) *cryptox.Cipher {
	t.Helper()
	c, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestChannelCreateNoSecretEcho(t *testing.T) {
	r, _ := newMonitorTestEnv(t)
	w := doJSON(t, r, http.MethodPost, "/api/notification-channels", map[string]any{
		"name": "hook", "kind": "webhook",
		"config": map[string]any{"url": "https://ops.example/hook", "secret": "topsecret"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var ch store.NotificationChannel
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil {
		t.Fatal(err)
	}
	if ch.ConfigHint == "" {
		t.Fatal("config_hint missing")
	}
	raw := w.Body.String()
	if bytes.Contains([]byte(raw), []byte("topsecret")) {
		t.Fatal("secret leaked in response")
	}
	if bytes.Contains([]byte(raw), []byte("config_enc")) {
		t.Fatal("encrypted config must not be echoed")
	}
}

func TestChannelCreateInvalidConfig(t *testing.T) {
	r, _ := newMonitorTestEnv(t)
	w := doJSON(t, r, http.MethodPost, "/api/notification-channels", map[string]any{
		"name": "bad", "kind": "webhook", "config": map[string]any{"url": "not-a-url"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	var errBody struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errBody)
	if errBody.Error.Code != "NOTIFICATION_CONFIG_INVALID" {
		t.Fatalf("code = %s", errBody.Error.Code)
	}
}

func TestAlertRuleCRUDAndValidation(t *testing.T) {
	r, _ := newMonitorTestEnv(t)
	// 创建。
	w := doJSON(t, r, http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "cpu-high", "metric": "cpu_pct", "comparator": "gt",
		"threshold": 90, "duration_windows": 3, "cooldown_seconds": 300,
		"severity": "warning", "enabled": true,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create code = %d body=%s", w.Code, w.Body.String())
	}
	var rule store.AlertRule
	if err := json.Unmarshal(w.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.ID == "" || rule.DurationWindows != 3 {
		t.Fatalf("rule = %+v", rule)
	}
	// 非法阈值：cpu_pct > 100。
	w = doJSON(t, r, http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "bad", "metric": "cpu_pct", "comparator": "gt", "threshold": 101,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("threshold>100 code = %d, want 400", w.Code)
	}
	// PATCH enabled=false。
	w = doJSON(t, r, http.MethodPatch, "/api/alert-rules/"+rule.ID, map[string]any{"enabled": false})
	if w.Code != http.StatusOK {
		t.Fatalf("patch code = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rule); err != nil || rule.Enabled {
		t.Fatalf("patched rule = %+v err=%v", rule, err)
	}
	// 列表 & 删除。
	if w = doJSON(t, r, http.MethodGet, "/api/alert-rules", nil); w.Code != http.StatusOK {
		t.Fatalf("list code = %d", w.Code)
	}
	if w = doJSON(t, r, http.MethodDelete, "/api/alert-rules/"+rule.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", w.Code)
	}
}

// TestAlertRuleDuplicateNameConflict 锁定 p5 sprint 6 修复：规则重名
// 创建/编辑改名的 409 必须携带具体中文 message「规则名称已存在」（对齐
// servers p3 sprint 1 的「服务器名称已存在」口径），不再回落通用
// "resource conflict"（前端 CONFLICT 映射靠括号内 message 透出原因）。
func TestAlertRuleDuplicateNameConflict(t *testing.T) {
	r, _ := newMonitorTestEnv(t)
	create := func(name string) *httptest.ResponseRecorder {
		body := map[string]any{
			"name": name, "metric": "cpu_pct", "comparator": "gt", "threshold": 90,
			"duration_windows": 3, "cooldown_seconds": 300, "severity": "warning", "enabled": true,
		}
		return doJSON(t, r, http.MethodPost, "/api/alert-rules", body)
	}
	w := create("cpu-high")
	if w.Code != http.StatusCreated {
		t.Fatalf("create code = %d body=%s", w.Code, w.Body.String())
	}
	var first store.AlertRule
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	// ① 重名创建 → 409 CONFLICT + 中文 message。
	w = create("cpu-high")
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate create code = %d, want 409", w.Code)
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "CONFLICT" || errBody.Error.Message != "规则名称已存在" {
		t.Fatalf("duplicate error = %+v", errBody.Error)
	}

	// ② 编辑改名撞其他规则名 → 同口径 409。
	w = create("mem-high")
	if w.Code != http.StatusCreated {
		t.Fatalf("second create code = %d body=%s", w.Code, w.Body.String())
	}
	var second store.AlertRule
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	w = doJSON(t, r, http.MethodPatch, "/api/alert-rules/"+second.ID, map[string]any{"name": "cpu-high"})
	if w.Code != http.StatusConflict {
		t.Fatalf("rename conflict code = %d, want 409", w.Code)
	}
	errBody = struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "CONFLICT" || errBody.Error.Message != "规则名称已存在" {
		t.Fatalf("rename error = %+v", errBody.Error)
	}
}

func TestAlertEventsListEmpty(t *testing.T) {
	r, _ := newMonitorTestEnv(t)
	w := doJSON(t, r, http.MethodGet, "/api/alert-events", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var res struct {
		Items []store.AlertEvent `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Items == nil {
		t.Fatal("items must be [] not null")
	}
}

func TestMonitorOverviewShape(t *testing.T) {
	r, d := newMonitorTestEnv(t)
	// 注入一台 latest + 一条 firing。
	srvID := uuid.NewString()
	_ = d.Store.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{{
		ServerID: srvID, CreatedAt: time.Now(), CPU: 10, Mem: 20, Disk: 30,
	}})
	_, err := d.Store.AlertEvents().Create(context.Background(), store.AlertRule{
		ID: "r1", Name: "cpu", Severity: store.AlertSeverityCritical,
	}, srvID, 99, "m", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, r, http.MethodGet, "/api/monitor/overview", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		Servers []store.ServerMetricLatest `json:"servers"`
		Firing  struct {
			Critical int64 `json:"critical"`
			Total    int64 `json:"total"`
		} `json:"firing"`
		LastInspection *struct {
			ReportID  string `json:"report_id"`
			AvgScore  int    `json:"avg_score"`
			CreatedAt string `json:"created_at"`
		} `json:"last_inspection"`
		Collector struct {
			Enabled         bool `json:"enabled"`
			IntervalSeconds int  `json:"interval_seconds"`
		} `json:"collector"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Servers) != 1 || res.Firing.Critical != 1 || res.Firing.Total != 1 {
		t.Fatalf("res = %+v", res)
	}
	if !res.Collector.Enabled || res.Collector.IntervalSeconds != 60 {
		t.Fatalf("collector defaults missing: %+v", res.Collector)
	}
	// p5 sprint 6：无报告时 last_inspection 必须为 null（契约 nullable，
	// 前端据此显示占位符 —）。
	if res.LastInspection != nil {
		t.Fatalf("last_inspection = %+v, want null when no report", res.LastInspection)
	}

	// 插入两条报告（先旧后新）：概要卡应装配最近一条（created_at DESC）。
	now := time.Now().UTC()
	for _, score := range []int{72, 95} {
		if _, err := d.Store.InspectionReports().Create(context.Background(),
			store.CreateInspectionReportParams{
				Trigger: store.InspectionTriggerManual, ServerCount: 1,
				AvgScore: score, StartedAt: now, FinishedAt: &now,
			}); err != nil {
			t.Fatal(err)
		}
	}
	w = doJSON(t, r, http.MethodGet, "/api/monitor/overview", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	res = struct {
		Servers []store.ServerMetricLatest `json:"servers"`
		Firing  struct {
			Critical int64 `json:"critical"`
			Total    int64 `json:"total"`
		} `json:"firing"`
		LastInspection *struct {
			ReportID  string `json:"report_id"`
			AvgScore  int    `json:"avg_score"`
			CreatedAt string `json:"created_at"`
		} `json:"last_inspection"`
		Collector struct {
			Enabled         bool `json:"enabled"`
			IntervalSeconds int  `json:"interval_seconds"`
		} `json:"collector"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.LastInspection == nil {
		t.Fatal("last_inspection missing after inspection ran")
	}
	if res.LastInspection.AvgScore != 95 {
		t.Fatalf("last_inspection.avg_score = %d, want 95 (latest report)", res.LastInspection.AvgScore)
	}
	if res.LastInspection.ReportID == "" || res.LastInspection.CreatedAt == "" {
		t.Fatalf("last_inspection incomplete: %+v", res.LastInspection)
	}
}

func TestCollectorPutGetRoundTrip(t *testing.T) {
	r, _ := newMonitorTestEnv(t)
	w := doJSON(t, r, http.MethodPut, "/api/monitor/collector", map[string]any{
		"enabled": false, "interval_seconds": 120,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("put code = %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/api/monitor/collector", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get code = %d", w.Code)
	}
	var cs monitor.CollectorSetting
	if err := json.Unmarshal(w.Body.Bytes(), &cs); err != nil {
		t.Fatal(err)
	}
	if cs.Enabled || cs.IntervalSeconds != 120 {
		t.Fatalf("round trip mismatch: %+v", cs)
	}
	// 越界 interval。
	w = doJSON(t, r, http.MethodPut, "/api/monitor/collector", map[string]any{
		"enabled": true, "interval_seconds": 10,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("interval=10 code = %d, want 400", w.Code)
	}
}

func TestServerMetricsEndpoint(t *testing.T) {
	r, d := newMonitorTestEnv(t)
	// memStore 的 Create 生成非 UUID id；直接落一行 UUID 服务器（handler 校验
	// id 必须为合法 uuid，与生产 store 的 gen_random_uuid 一致）。
	srvID := uuid.NewString()
	srv := store.Server{
		ID: srvID, Name: "web-1", DeviceKind: store.DeviceKindServer, OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root", AuthMethod: store.AuthMethodPassword,
	}
	st := d.Store.(*memStore)
	st.mu.Lock()
	st.servers[srvID] = srv
	st.mu.Unlock()
	now := time.Now().UTC()
	_ = d.Store.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{{
		ServerID: srvID, CreatedAt: now, CPU: 11, Mem: 22, Disk: 33, Load1: 0.4, NetIn: 5, NetOut: 6,
	}})

	w := doJSON(t, r, http.MethodGet, "/api/servers/"+srvID+"/metrics?minutes=60", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res metricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 1 {
		t.Fatalf("points = %d", len(res.Points))
	}
	if res.Points[0]["cpu_pct"] != float64(11) {
		t.Fatalf("point = %v", res.Points[0])
	}

	// metric 过滤。
	w = doJSON(t, r, http.MethodGet, "/api/servers/"+srvID+"/metrics?minutes=15&metric=mem_pct", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("filter code = %d", w.Code)
	}
	var filtered metricsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &filtered)
	if len(filtered.Points) != 1 {
		t.Fatalf("filtered points = %d", len(filtered.Points))
	}
	if _, has := filtered.Points[0]["cpu_pct"]; has {
		t.Fatal("metric filter must drop other series")
	}
	if filtered.Points[0]["mem_pct"] != float64(22) {
		t.Fatalf("mem_pct = %v", filtered.Points[0]["mem_pct"])
	}

	// 越界 minutes。
	w = doJSON(t, r, http.MethodGet, "/api/servers/"+srvID+"/metrics?minutes=5", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("minutes=5 code = %d", w.Code)
	}
	// 不存在的服务器。
	w = doJSON(t, r, http.MethodGet, "/api/servers/"+uuid.NewString()+"/metrics", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("ghost server code = %d", w.Code)
	}
}
