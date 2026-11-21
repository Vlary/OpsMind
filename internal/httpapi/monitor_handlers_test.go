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
	"math"
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
		// 与生产 UPSERT 同语义：旧时刻点不得覆盖已更新的 latest
		// （第 10 轮 medium）。
		if cur, ok := f.st.latest[it.ServerID]; ok && it.CreatedAt.Before(cur.CreatedAt) {
			continue
		}
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
	if w.Limit > store.MetricWindowLimitMax {
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
	// 与 Postgres 实现同语义：单机窗口取最新 limit 个点后升序返回。
	if w.Limit > 0 && len(out) > w.Limit {
		out = out[len(out)-w.Limit:]
	}
	return out, nil
}
func (f *memMetrics) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }

// memP95 是 nearest-rank 95 分位（SQL percentile_cont 的测试近似；handler
// 单测只断言 null/非 null 与桶数，精确插值由 PG 集成测试覆盖）。
func memP95(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	idx := int(math.Ceil(0.95*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// Trend 是生产 date_bin 聚合的内存等价实现（测试用）：同一 origin
// （alignTrendBucket）、[From,To) 半开窗口、只返回有样本桶。
func (f *memMetrics) Trend(_ context.Context, q store.MetricTrendQuery) ([]store.MetricTrendPoint, error) {
	if q.BucketSeconds < 60 || !q.To.After(q.From) {
		return nil, store.ErrInvalid
	}
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	type agg struct {
		cpu, mem []float64
		servers  map[string]struct{}
		first    time.Time
		last     time.Time
	}
	groups := make(map[int64]*agg, 32)
	for _, it := range f.st.metrics {
		if it.CreatedAt.Before(q.From) || !it.CreatedAt.Before(q.To) {
			continue
		}
		// 走查第 16 轮 medium：Now 非零时排除未来样本，与生产 SQL 同口径。
		if !q.Now.IsZero() && it.CreatedAt.After(q.Now) {
			continue
		}
		key := alignTrendBucket(it.CreatedAt, q.BucketSeconds).Unix()
		g, ok := groups[key]
		if !ok {
			g = &agg{servers: map[string]struct{}{}}
			groups[key] = g
		}
		g.cpu = append(g.cpu, it.CPU)
		g.mem = append(g.mem, it.Mem)
		g.servers[it.ServerID] = struct{}{}
		if g.first.IsZero() || it.CreatedAt.Before(g.first) {
			g.first = it.CreatedAt
		}
		if g.last.IsZero() || it.CreatedAt.After(g.last) {
			g.last = it.CreatedAt
		}
	}
	out := make([]store.MetricTrendPoint, 0, len(groups))
	for key, g := range groups {
		var sumCPU, sumMem, maxCPU, maxMem float64
		for i := range g.cpu {
			sumCPU += g.cpu[i]
			sumMem += g.mem[i]
			if g.cpu[i] > maxCPU {
				maxCPU = g.cpu[i]
			}
			if g.mem[i] > maxMem {
				maxMem = g.mem[i]
			}
		}
		n := float64(len(g.cpu))
		out = append(out, store.MetricTrendPoint{
			BucketStart: time.Unix(key, 0).UTC(),
			CPUP95:      memP95(g.cpu), CPUAvg: sumCPU / n, CPUMax: maxCPU,
			MemP95: memP95(g.mem), MemAvg: sumMem / n, MemMax: maxMem,
			Servers: len(g.servers), Samples: int64(len(g.cpu)),
			FirstSample: g.first, LastSample: g.last,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketStart.Before(out[j].BucketStart) })
	return out, nil
}

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
		if flt.RuleID != "" && ev.RuleID != flt.RuleID {
			continue
		}
		if flt.FiredAfter != nil && ev.FiredAt.Before(*flt.FiredAfter) {
			continue
		}
		// 上界半开 [after, before)：与 stats 桶同口径，边界事件不重复、
		// 桶末 500µs 的事件不会被漏掉（走查第 13 轮 medium）。
		if flt.FiredBefore != nil && !ev.FiredAt.Before(*flt.FiredBefore) {
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

func (f *memEvents) CountFiringBySeverity(context.Context) (store.FiringSeverityCounts, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	var out store.FiringSeverityCounts
	for _, ev := range f.st.events {
		if ev.Status != store.AlertEventFiring {
			continue
		}
		switch ev.Severity {
		case store.AlertSeverityInfo:
			out.Info++
		case store.AlertSeverityWarning:
			out.Warning++
		case store.AlertSeverityCritical:
			out.Critical++
		}
	}
	return out, nil
}

// Stats 是两条 SQL 的内存等价实现（时间桶分级 + top rules）。
func (f *memEvents) Stats(_ context.Context, q store.AlertEventStatsQuery) (store.AlertEventStats, error) {
	if q.TopRules < 1 || q.TopRules > 20 || q.BucketSeconds < 60 || !q.To.After(q.From) {
		return store.AlertEventStats{}, store.ErrInvalid
	}
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := store.AlertEventStats{
		Timeline: []store.AlertEventSeverityBucket{},
		TopRules: []store.AlertRuleStat{},
	}
	buckets := map[int64]*store.AlertEventSeverityBucket{}
	distinct := map[string]struct{}{}
	type ruleAgg struct {
		stat    store.AlertRuleStat
		servers map[string]struct{}
	}
	rules := map[string]*ruleAgg{}
	for _, ev := range f.st.events {
		if ev.FiredAt.Before(q.From) || !ev.FiredAt.Before(q.To) {
			continue
		}
		out.Total++
		distinct[ev.RuleID] = struct{}{}
		start := alignTrendBucket(ev.FiredAt, q.BucketSeconds).Unix()
		b, ok := buckets[start]
		if !ok {
			b = &store.AlertEventSeverityBucket{BucketStart: time.Unix(start, 0).UTC()}
			buckets[start] = b
		}
		switch ev.Severity {
		case store.AlertSeverityInfo:
			b.Info++
		case store.AlertSeverityWarning:
			b.Warning++
		case store.AlertSeverityCritical:
			b.Critical++
		}
		ra, ok := rules[ev.RuleID]
		if !ok {
			name, sev := ev.RuleName, ev.Severity
			if r, ok := f.st.rules[ev.RuleID]; ok {
				name, sev = r.Name, r.Severity
			}
			ra = &ruleAgg{
				stat:    store.AlertRuleStat{RuleID: ev.RuleID, RuleName: name, Severity: sev},
				servers: map[string]struct{}{},
			}
			rules[ev.RuleID] = ra
		}
		ra.stat.Count++
		ra.servers[ev.ServerID] = struct{}{}
		if ev.FiredAt.After(ra.stat.LastFiredAt) {
			ra.stat.LastFiredAt = ev.FiredAt
		}
	}
	out.DistinctRules = int64(len(distinct))
	for _, b := range buckets {
		out.Timeline = append(out.Timeline, *b)
	}
	sort.Slice(out.Timeline, func(i, j int) bool {
		return out.Timeline[i].BucketStart.Before(out.Timeline[j].BucketStart)
	})
	for _, ra := range rules {
		ra.stat.ServerCount = int64(len(ra.servers))
		out.TopRules = append(out.TopRules, ra.stat)
	}
	sort.Slice(out.TopRules, func(i, j int) bool {
		if out.TopRules[i].Count != out.TopRules[j].Count {
			return out.TopRules[i].Count > out.TopRules[j].Count
		}
		if !out.TopRules[i].LastFiredAt.Equal(out.TopRules[j].LastFiredAt) {
			return out.TopRules[i].LastFiredAt.After(out.TopRules[j].LastFiredAt)
		}
		return out.TopRules[i].RuleID < out.TopRules[j].RuleID
	})
	if len(out.TopRules) > q.TopRules {
		out.TopRules = out.TopRules[:q.TopRules]
	}
	return out, nil
}

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
	return newMonitorTestEnvAt(t, time.Now().UTC())
}

// newMonitorTestEnvAt 固定时钟版本：图表桶对齐断言需要确定性的 now。
func newMonitorTestEnvAt(t *testing.T, now time.Time) (*gin.Engine, *Deps) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetMonitorState()
	resetInspectionState()
	st := newMemStore()
	d := &Deps{Store: st, Cipher: testCipherForMonitor(t), Clock: func() time.Time { return now }}
	r := gin.New()
	protected := r.Group("/api")
	protected.Use(withTestAdminIdentity())
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
	// 走查第 15 轮 medium：overview 的 servers[] 必须带 os（Windows 资产
	// 不能被前端按 /proc 口径标注）。
	monState.mu.Lock()
	row := monState.latest[srvID]
	row.OS = store.ServerOSWindows
	monState.latest[srvID] = row
	monState.mu.Unlock()
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
		Servers    []store.ServerMetricLatest `json:"servers"`
		ServerTime time.Time                  `json:"server_time"`
		Firing     struct {
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
	if res.Servers[0].OS != store.ServerOSWindows {
		t.Fatalf("servers[0].os = %q, want windows", res.Servers[0].OS)
	}
	// 走查第 14 轮 medium：前端按 server_time 校正浏览器时钟；字段必须存在
	// 且贴近本次请求时刻（不是零值、不是未来）。
	if res.ServerTime.IsZero() || time.Since(res.ServerTime).Abs() > time.Minute {
		t.Fatalf("server_time = %v, want ~now", res.ServerTime)
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
		Servers    []store.ServerMetricLatest `json:"servers"`
		ServerTime time.Time                  `json:"server_time"`
		Firing     struct {
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

// TestServerMetricsThirtySecondFullDay：采集间隔 30s（产品允许的下限）时
// 24h 窗口内有 2880 点。接口必须全部返回且 truncated=false；旧实现固定
// LIMIT 1440，只返回最近 12h，页面把其余 12h 误标为「无采样」（第 10 轮
// high）。只有窗口内确实多出更早的点时才报 truncated。
func TestServerMetricsThirtySecondFullDay(t *testing.T) {
	now := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	r, d := newMonitorTestEnvAt(t, now)
	srvID := uuid.NewString()
	st := d.Store.(*memStore)
	st.mu.Lock()
	st.servers[srvID] = store.Server{
		ID: srvID, Name: "fast-01", DeviceKind: store.DeviceKindServer, OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root", AuthMethod: store.AuthMethodPassword,
	}
	st.mu.Unlock()

	// 2880 点 × 30s：末点=now，首点=now-23h59.5m（24h 窗口内）。
	base := now.Add(-time.Duration(store.MetricWindowLimitMax-1) * 30 * time.Second)
	points := make([]store.InsertMetricParams, 0, store.MetricWindowLimitMax)
	for i := 0; i < store.MetricWindowLimitMax; i++ {
		points = append(points, store.InsertMetricParams{
			ServerID: srvID, CreatedAt: base.Add(time.Duration(i) * 30 * time.Second),
			CPU: float64(i % 100), Mem: 40, Disk: 50, Load1: 0.5, NetIn: 1024, NetOut: 2048,
		})
	}
	if err := d.Store.Metrics().BatchInsert(context.Background(), points); err != nil {
		t.Fatal(err)
	}

	fetch := func() metricsResponse {
		t.Helper()
		w := doJSON(t, r, http.MethodGet, "/api/servers/"+srvID+"/metrics?minutes=1440", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
		}
		var res metricsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := fetch()
	if len(res.Points) != store.MetricWindowLimitMax {
		t.Fatalf("points = %d, want %d", len(res.Points), store.MetricWindowLimitMax)
	}
	if res.Truncated {
		t.Fatal("truncated = true with a complete 30s/24h window")
	}
	first, _ := res.Points[0]["created_at"].(string)
	last, _ := res.Points[len(res.Points)-1]["created_at"].(string)
	if first != base.Format(time.RFC3339) || last != now.Format(time.RFC3339) {
		t.Fatalf("range = %s..%s, want %s..%s", first, last,
			base.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	// 窗口内再加一个更早的点（2881 个）：上限丢弃最旧一个，truncated=true，
	// 返回的仍是最新 2880 点。
	if err := d.Store.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{{
		ServerID: srvID, CreatedAt: base.Add(-15 * time.Second),
		CPU: 1, Mem: 2, Disk: 3, Load1: 0.1, NetIn: 1, NetOut: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	res = fetch()
	if len(res.Points) != store.MetricWindowLimitMax {
		t.Fatalf("points after extra = %d, want %d", len(res.Points), store.MetricWindowLimitMax)
	}
	if !res.Truncated {
		t.Fatal("truncated = false although an older point exists beyond the window limit")
	}
	oldest, _ := res.Points[0]["created_at"].(string)
	if oldest != base.Format(time.RFC3339) {
		t.Fatalf("oldest returned = %s, want %s", oldest, base.Format(time.RFC3339))
	}
}

// TestServerMetricsSixtySecondFullDayNotTruncated：60s 采集的 24h 窗口正好
// 1440 点，低于 2880 硬上限，truncated 必须为 false——不能按点数猜截断。
// 30s 满窗（2880 点）与真正超限的场景见
// TestServerMetricsThirtySecondFullDay。
func TestServerMetricsSixtySecondFullDayNotTruncated(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	r, d := newMonitorTestEnvAt(t, now)
	srvID := uuid.NewString()
	st := d.Store.(*memStore)
	st.mu.Lock()
	st.servers[srvID] = store.Server{
		ID: srvID, Name: "batch-04", DeviceKind: store.DeviceKindServer, OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root", AuthMethod: store.AuthMethodPassword,
	}
	st.mu.Unlock()

	// 1440 个分钟点：now-1439m .. now（窗口下界 now-1440m 内）。
	points := make([]store.InsertMetricParams, 0, 1440)
	for i := 0; i < 1440; i++ {
		points = append(points, store.InsertMetricParams{
			ServerID: srvID, CreatedAt: now.Add(-time.Duration(i) * time.Minute),
			CPU: float64(i % 100), Mem: 40, Disk: 50, Load1: 0.5, NetIn: 1024, NetOut: 2048,
		})
	}
	if err := d.Store.Metrics().BatchInsert(context.Background(), points); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, r, http.MethodGet, "/api/servers/"+srvID+"/metrics?minutes=1440", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res metricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != 1440 {
		t.Fatalf("points = %d, want 1440", len(res.Points))
	}
	if res.Truncated {
		t.Fatal("truncated = true with exactly 1440 points and no older sample")
	}
	oldest, ok := res.Points[0]["created_at"].(string)
	if !ok {
		t.Fatalf("created_at missing: %v", res.Points[0])
	}
	if want := now.Add(-1439 * time.Minute).Format(time.RFC3339); oldest != want {
		t.Fatalf("oldest returned = %s, want %s", oldest, want)
	}
}

// ---------------------------------------------------------------------------
// 图表聚合（GET /api/monitor/trend / GET /api/alert-events/stats）
// ---------------------------------------------------------------------------

// trendFixedNow 是图表测试的固定时钟：24h 窗口对齐后为
// [2026-11-18T13:00Z, 2026-11-19T13:00Z)，7d 窗口到 18:00Z。
var trendFixedNow = time.Date(2026, 11, 19, 12, 34, 56, 0, time.UTC)

type trendPointJSON struct {
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

type monitorTrendJSON struct {
	Window        string           `json:"window"`
	BucketSeconds int              `json:"bucket_seconds"`
	From          time.Time        `json:"from"`
	To            time.Time        `json:"to"`
	FirstSample   *time.Time       `json:"first_sample"`
	LastSample    *time.Time       `json:"last_sample"`
	Points        []trendPointJSON `json:"points"`
}

func decodeTrend(t *testing.T, w *httptest.ResponseRecorder) monitorTrendJSON {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res monitorTrendJSON
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	return res
}

// TestMonitorTrendBucketsAndNulls：24h→24 桶、7d→28 桶；有样本桶给聚合值，
// 无样本桶六指标为 null（前端断线不连点）；空数据仍是完整 null 桶而非报错。
func TestMonitorTrendBucketsAndNulls(t *testing.T) {
	r, d := newMonitorTestEnvAt(t, trendFixedNow)
	srv1, srv2 := uuid.NewString(), uuid.NewString()
	base := time.Date(2026, 11, 18, 14, 10, 0, 0, time.UTC) // 24h 窗口第 2 个桶
	if err := d.Store.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{
		{ServerID: srv1, CreatedAt: base, CPU: 10, Mem: 20},
		{ServerID: srv2, CreatedAt: base.Add(10 * time.Minute), CPU: 30, Mem: 40},
	}); err != nil {
		t.Fatal(err)
	}

	res := decodeTrend(t, doJSON(t, r, http.MethodGet, "/api/monitor/trend", nil))
	if res.Window != "24h" || res.BucketSeconds != 3600 || len(res.Points) != 24 {
		t.Fatalf("window/buckets = %+v len=%d", res, len(res.Points))
	}
	if want := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC); !res.From.Equal(want) {
		t.Fatalf("from = %s, want %s", res.From, want)
	}
	if want := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC); !res.To.Equal(want) {
		t.Fatalf("to = %s, want %s", res.To, want)
	}
	if !res.Points[0].BucketStart.Equal(res.From) {
		t.Fatalf("first bucket = %s, want from", res.Points[0].BucketStart)
	}
	for i := 1; i < len(res.Points); i++ {
		if !res.Points[i].BucketStart.Equal(res.Points[i-1].BucketStart.Add(time.Hour)) {
			t.Fatalf("bucket %d not contiguous: %s", i, res.Points[i].BucketStart)
		}
	}
	// 无样本桶：null + 0。
	empty := res.Points[0]
	if empty.CPUP95 != nil || empty.CPUAvg != nil || empty.CPUMax != nil ||
		empty.MemP95 != nil || empty.MemAvg != nil || empty.MemMax != nil {
		t.Fatalf("empty bucket must be null: %+v", empty)
	}
	if empty.Servers != 0 || empty.Samples != 0 {
		t.Fatalf("empty bucket counts = %d/%d", empty.Servers, empty.Samples)
	}
	// 有样本桶：第 2 个（14:00–15:00）。
	p := res.Points[1]
	if p.CPUP95 == nil || p.CPUAvg == nil || p.CPUMax == nil ||
		p.MemP95 == nil || p.MemAvg == nil || p.MemMax == nil {
		t.Fatalf("populated bucket has null: %+v", p)
	}
	if *p.CPUAvg != 20 || *p.CPUMax != 30 || *p.MemAvg != 30 || *p.MemMax != 40 {
		t.Fatalf("aggregates = %+v", p)
	}
	if p.Servers != 2 || p.Samples != 2 {
		t.Fatalf("servers/samples = %d/%d", p.Servers, p.Samples)
	}
	// 走查第 17 轮 low：首末采样时刻来自实际样本，而非桶边界。
	if res.FirstSample == nil || !res.FirstSample.Equal(base) {
		t.Fatalf("first_sample = %v, want %s", res.FirstSample, base)
	}
	if res.LastSample == nil || !res.LastSample.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("last_sample = %v, want %s", res.LastSample, base.Add(10*time.Minute))
	}

	// 7d：28 桶 × 21600s。
	res = decodeTrend(t, doJSON(t, r, http.MethodGet, "/api/monitor/trend?window=7d", nil))
	if res.Window != "7d" || res.BucketSeconds != 21600 || len(res.Points) != 28 {
		t.Fatalf("7d window = %+v len=%d", res.Window, len(res.Points))
	}

	// 空数据：完整 24 个 null 桶，不是错误、不是空数组。
	r2, _ := newMonitorTestEnvAt(t, trendFixedNow)
	res = decodeTrend(t, doJSON(t, r2, http.MethodGet, "/api/monitor/trend", nil))
	if len(res.Points) != 24 {
		t.Fatalf("empty points = %d", len(res.Points))
	}
	for i, p := range res.Points {
		if p.CPUP95 != nil || p.Samples != 0 {
			t.Fatalf("empty point %d = %+v", i, p)
		}
	}
	if res.FirstSample != nil || res.LastSample != nil {
		t.Fatalf("empty first/last sample = %v/%v, want nil", res.FirstSample, res.LastSample)
	}
}

// TestMonitorTrendClampsFutureSamples：走查第 16 轮 medium——当前桶的终点
// 是未来时刻，时钟超前机器写入的未来样本不得进入 P95/峰值/台数/样本数；
// 上界含 now（now 当刻的样本仍算），但排除 now 之后。
func TestMonitorTrendClampsFutureSamples(t *testing.T) {
	r, d := newMonitorTestEnvAt(t, trendFixedNow)
	srv := uuid.NewString()
	bucketStart := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if err := d.Store.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{
		{ServerID: srv, CreatedAt: bucketStart.Add(10 * time.Minute), CPU: 40, Mem: 41},
		{ServerID: srv, CreatedAt: trendFixedNow, CPU: 50, Mem: 51},
		// 服务端 now + 10 分钟：未来样本（错误时钟写入），必须排除。
		{ServerID: srv, CreatedAt: trendFixedNow.Add(10 * time.Minute), CPU: 100, Mem: 100},
	}); err != nil {
		t.Fatal(err)
	}
	res := decodeTrend(t, doJSON(t, r, http.MethodGet, "/api/monitor/trend", nil))
	// 桶数不变：仍是 24 个桶，最后一桶仍以当前桶起点开始。
	if len(res.Points) != 24 {
		t.Fatalf("points = %d, want 24", len(res.Points))
	}
	last := res.Points[len(res.Points)-1]
	if !last.BucketStart.Equal(bucketStart) {
		t.Fatalf("last bucket = %s, want %s", last.BucketStart, bucketStart)
	}
	if last.CPUMax == nil || *last.CPUMax != 50 {
		t.Fatalf("cpu_max = %v, want 50 (future 100 must be excluded)", last.CPUMax)
	}
	if last.CPUAvg == nil || *last.CPUAvg != 45 {
		t.Fatalf("cpu_avg = %v, want 45", last.CPUAvg)
	}
	if last.Servers != 1 || last.Samples != 2 {
		t.Fatalf("servers/samples = %d/%d, want 1/2", last.Servers, last.Samples)
	}
	// 被钳制的未来样本也不能进入 last_sample。
	if res.FirstSample == nil || !res.FirstSample.Equal(bucketStart.Add(10*time.Minute)) {
		t.Fatalf("first_sample = %v, want %s", res.FirstSample, bucketStart.Add(10*time.Minute))
	}
	if res.LastSample == nil || !res.LastSample.Equal(trendFixedNow) {
		t.Fatalf("last_sample = %v, want %s", res.LastSample, trendFixedNow)
	}
}

// TestMonitorTrendWindowValidation：window 只接受 24h|7d，其它值 400。
func TestMonitorTrendWindowValidation(t *testing.T) {
	r, _ := newMonitorTestEnvAt(t, trendFixedNow)
	w := doJSON(t, r, http.MethodGet, "/api/monitor/trend?window=30d", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s", body.Error.Code)
	}
	// 显式 24h 与缺省等价。
	if res := decodeTrend(t, doJSON(t, r, http.MethodGet, "/api/monitor/trend?window=24h", nil)); res.Window != "24h" {
		t.Fatalf("window = %s", res.Window)
	}
}

type alertStatsJSON struct {
	Window        string    `json:"window"`
	BucketSeconds int       `json:"bucket_seconds"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	ServerTime    time.Time `json:"server_time"`
	Total         int64     `json:"total"`
	DistinctRules int64     `json:"distinct_rules"`
	Timeline      []struct {
		BucketStart time.Time `json:"bucket_start"`
		Info        int64     `json:"info"`
		Warning     int64     `json:"warning"`
		Critical    int64     `json:"critical"`
	} `json:"severity_timeline"`
	TopRules []struct {
		RuleID      string    `json:"rule_id"`
		RuleName    string    `json:"rule_name"`
		Severity    string    `json:"severity"`
		Count       int64     `json:"count"`
		ServerCount int64     `json:"server_count"`
		LastFiredAt time.Time `json:"last_fired_at"`
	} `json:"top_rules"`
}

// TestAlertEventStatsContract：total/distinct_rules、分级时间轴（空桶补零）、
// top_rules 排序与 top_rules 上限。
func TestAlertEventStatsContract(t *testing.T) {
	r, d := newMonitorTestEnvAt(t, trendFixedNow)
	ruleA := store.AlertRule{ID: uuid.NewString(), Name: "cpu-high", Severity: store.AlertSeverityWarning}
	ruleB := store.AlertRule{ID: uuid.NewString(), Name: "mem-high", Severity: store.AlertSeverityCritical}
	ruleC := store.AlertRule{ID: uuid.NewString(), Name: "disk-note", Severity: store.AlertSeverityInfo}
	monState.mu.Lock()
	for _, rule := range []store.AlertRule{ruleA, ruleB, ruleC} {
		monState.rules[rule.ID] = rule
	}
	monState.mu.Unlock()

	ctx := context.Background()
	seed := []struct {
		rule store.AlertRule
		srv  string
		at   time.Time
	}{
		{ruleA, uuid.NewString(), time.Date(2026, 11, 18, 13, 30, 0, 0, time.UTC)},
		{ruleA, uuid.NewString(), time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)},
		{ruleB, uuid.NewString(), time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)},
		{ruleC, uuid.NewString(), time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC)},
	}
	for _, s := range seed {
		if _, err := d.Store.AlertEvents().Create(ctx, s.rule, s.srv, 95, "m", s.at); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	w := doJSON(t, r, http.MethodGet, "/api/alert-events/stats", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res alertStatsJSON
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Window != "24h" || res.BucketSeconds != 3600 || len(res.Timeline) != 24 {
		t.Fatalf("window/timeline = %+v len=%d", res, len(res.Timeline))
	}
	// 走查第 14 轮 low：to 对齐到当前桶结束，前端用 server_time 判断
	// 最后一桶是否未走完；固定时钟下两者应同源（to > server_time）。
	if !res.ServerTime.Equal(trendFixedNow) {
		t.Fatalf("server_time = %v, want %v", res.ServerTime, trendFixedNow)
	}
	if !res.To.After(res.ServerTime) {
		t.Fatalf("to = %v must be after server_time %v", res.To, res.ServerTime)
	}
	if res.Total != 4 || res.DistinctRules != 3 {
		t.Fatalf("total/distinct = %d/%d", res.Total, res.DistinctRules)
	}
	// 空桶补零 + 分级落桶：13:30→桶 0(warning)，10:00→桶 21(warning)，
	// 12:00/12:30→桶 23(critical+info)。
	if res.Timeline[0].Warning != 1 || res.Timeline[21].Warning != 1 ||
		res.Timeline[23].Critical != 1 || res.Timeline[23].Info != 1 {
		t.Fatalf("timeline = %+v", res.Timeline)
	}
	var sumInfo, sumWarn, sumCrit int64
	for _, b := range res.Timeline {
		sumInfo += b.Info
		sumWarn += b.Warning
		sumCrit += b.Critical
	}
	if sumInfo != 1 || sumWarn != 2 || sumCrit != 1 {
		t.Fatalf("severity totals = %d/%d/%d", sumInfo, sumWarn, sumCrit)
	}
	if len(res.TopRules) != 3 {
		t.Fatalf("top_rules = %+v", res.TopRules)
	}
	if res.TopRules[0].RuleID != ruleA.ID || res.TopRules[0].Count != 2 ||
		res.TopRules[0].ServerCount != 2 || res.TopRules[0].Severity != "warning" {
		t.Fatalf("top[0] = %+v", res.TopRules[0])
	}
	lastA := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if !res.TopRules[0].LastFiredAt.Equal(lastA) {
		t.Fatalf("top[0].last_fired_at = %s, want %s", res.TopRules[0].LastFiredAt, lastA)
	}
	// 并列 1 条按 last_fired_at DESC：ruleC(12:30) 先于 ruleB(12:00)。
	if res.TopRules[1].RuleID != ruleC.ID || res.TopRules[2].RuleID != ruleB.ID {
		t.Fatalf("tie order = %+v", res.TopRules)
	}

	// top_rules=1：只返回最高频规则。
	w = doJSON(t, r, http.MethodGet, "/api/alert-events/stats?top_rules=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("top_rules=1 code = %d", w.Code)
	}
	var limited alertStatsJSON
	_ = json.Unmarshal(w.Body.Bytes(), &limited)
	if len(limited.TopRules) != 1 || limited.TopRules[0].RuleID != ruleA.ID {
		t.Fatalf("limited top = %+v", limited.TopRules)
	}

	// 空窗口：total=0、全零时间轴、top_rules=[]。
	r2, _ := newMonitorTestEnvAt(t, trendFixedNow)
	w = doJSON(t, r2, http.MethodGet, "/api/alert-events/stats?window=7d", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("empty stats code = %d", w.Code)
	}
	var empty alertStatsJSON
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Total != 0 || empty.DistinctRules != 0 || len(empty.Timeline) != 28 || len(empty.TopRules) != 0 {
		t.Fatalf("empty stats = %+v", empty)
	}
	if empty.TopRules == nil {
		t.Fatal("top_rules must be [] not null")
	}
}

// TestAlertEventStatsValidation：top_rules 1..20，越界 400。
func TestAlertEventStatsValidation(t *testing.T) {
	r, _ := newMonitorTestEnvAt(t, trendFixedNow)
	for _, q := range []string{"top_rules=0", "top_rules=21", "window=1h"} {
		w := doJSON(t, r, http.MethodGet, "/api/alert-events/stats?"+q, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s code = %d, want 400", q, w.Code)
		}
	}
}

// TestAlertEventsRuleAndFiredRangeFilter：rule_id + fired_after/fired_before
// 过滤（半开 [after, before)）；两参数必须成对、after<before、跨度 ≤7d。
// 走查第 13 轮 medium：上界不再含边界，桶末 500µs 的事件必须命中。
func TestAlertEventsRuleAndFiredRangeFilter(t *testing.T) {
	r, d := newMonitorTestEnvAt(t, trendFixedNow)
	ruleA := store.AlertRule{ID: uuid.NewString(), Name: "cpu-high", Severity: store.AlertSeverityWarning}
	ruleB := store.AlertRule{ID: uuid.NewString(), Name: "mem-high", Severity: store.AlertSeverityWarning}
	monState.mu.Lock()
	monState.rules[ruleA.ID] = ruleA
	monState.rules[ruleB.ID] = ruleB
	monState.mu.Unlock()

	t1 := time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	// 桶 [t1, t2) 的最后一刻：t2-500µs 必须计入，恰在 t2 的事件归下一桶。
	lastInstant := t2.Add(-500 * time.Microsecond)
	ctx := context.Background()
	for _, s := range []struct {
		rule store.AlertRule
		at   time.Time
	}{{ruleA, t1}, {ruleA, lastInstant}, {ruleA, t2}, {ruleB, t3}} {
		if _, err := d.Store.AlertEvents().Create(ctx, s.rule, uuid.NewString(), 90, "m", s.at); err != nil {
			t.Fatal(err)
		}
	}
	countItems := func(path string) int {
		t.Helper()
		w := doJSON(t, r, http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s code = %d body=%s", path, w.Code, w.Body.String())
		}
		var body struct {
			Items []store.AlertEvent `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return len(body.Items)
	}
	if n := countItems("/api/alert-events?rule_id=" + ruleA.ID); n != 3 {
		t.Fatalf("rule filter items = %d", n)
	}
	// 半开区间：t1..t2 命中 t1 与桶末 t2-500µs，不含恰好落在 t2 的边界事件。
	rangeQ := "?fired_after=" + t1.Format(time.RFC3339) + "&fired_before=" + t2.Format(time.RFC3339)
	if n := countItems("/api/alert-events" + rangeQ); n != 2 {
		t.Fatalf("range items = %d", n)
	}
	if n := countItems("/api/alert-events?rule_id=" + ruleA.ID + "&" + rangeQ[1:]); n != 2 {
		t.Fatalf("rule+range items = %d", n)
	}
	// 下一桶 [t2, t3) 只命中边界事件（t2）：相邻桶不重不漏。
	if n := countItems("/api/alert-events?rule_id=" + ruleA.ID + "&fired_after=" +
		t2.Format(time.RFC3339) + "&fired_before=" + t3.Format(time.RFC3339)); n != 1 {
		t.Fatalf("next bucket items = %d", n)
	}
	// 微秒精度终点：原样解析，不截断到毫秒；包含桶末事件。
	if n := countItems("/api/alert-events?rule_id=" + ruleA.ID + "&fired_after=" +
		t1.Format(time.RFC3339Nano) + "&fired_before=" + lastInstant.Add(time.Microsecond).Format(time.RFC3339Nano)); n != 2 {
		t.Fatalf("microsecond range items = %d", n)
	}
	// ruleB 的事件恰在 t3：半开上界要用 t3+1s 才包含（正好验证边界不含）。
	if n := countItems("/api/alert-events?rule_id=" + ruleB.ID + "&fired_after=" +
		t2.Format(time.RFC3339) + "&fired_before=" + t3.Add(time.Second).Format(time.RFC3339)); n != 1 {
		t.Fatalf("ruleB+range items = %d", n)
	}
	// 参数校验。
	bad := []string{
		"?fired_after=" + t1.Format(time.RFC3339),
		"?fired_before=" + t2.Format(time.RFC3339),
		"?fired_after=" + t2.Format(time.RFC3339) + "&fired_before=" + t2.Format(time.RFC3339),
		"?fired_after=" + t2.Format(time.RFC3339) + "&fired_before=" + t1.Format(time.RFC3339),
		"?fired_after=" + t1.Format(time.RFC3339) + "&fired_before=" +
			t1.Add(7*24*time.Hour+time.Second).Format(time.RFC3339),
		"?fired_after=not-a-time&fired_before=" + t2.Format(time.RFC3339),
		"?rule_id=not-a-uuid",
	}
	for _, q := range bad {
		w := doJSON(t, r, http.MethodGet, "/api/alert-events"+q, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s code = %d, want 400", q, w.Code)
		}
	}
}

// TestMonitorOverviewFiringSeverityAggregation：firing 分级走一次 GROUP BY
// 聚合（不再分页读事件），resolved 不计入。
func TestMonitorOverviewFiringSeverityAggregation(t *testing.T) {
	r, d := newMonitorTestEnvAt(t, trendFixedNow)
	ctx := context.Background()
	ruleInfo := store.AlertRule{ID: uuid.NewString(), Name: "info-rule", Severity: store.AlertSeverityInfo}
	ruleWarn := store.AlertRule{ID: uuid.NewString(), Name: "warn-rule", Severity: store.AlertSeverityWarning}
	ruleCrit := store.AlertRule{ID: uuid.NewString(), Name: "crit-rule", Severity: store.AlertSeverityCritical}
	monState.mu.Lock()
	for _, rule := range []store.AlertRule{ruleInfo, ruleWarn, ruleCrit} {
		monState.rules[rule.ID] = rule
	}
	monState.mu.Unlock()
	seed := []struct {
		rule store.AlertRule
		n    int
	}{{ruleInfo, 2}, {ruleWarn, 1}, {ruleCrit, 3}}
	for _, s := range seed {
		for i := 0; i < s.n; i++ {
			if _, err := d.Store.AlertEvents().Create(ctx, s.rule, uuid.NewString(), 90, "m", trendFixedNow); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 一条 resolved critical：不计入 firing。
	resolved, err := d.Store.AlertEvents().Create(ctx, ruleCrit, uuid.NewString(), 90, "m", trendFixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.AlertEvents().Resolve(ctx, resolved.ID, trendFixedNow); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, r, http.MethodGet, "/api/monitor/overview", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		Firing struct {
			Info     int64 `json:"info"`
			Warning  int64 `json:"warning"`
			Critical int64 `json:"critical"`
			Total    int64 `json:"total"`
		} `json:"firing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Firing.Info != 2 || res.Firing.Warning != 1 || res.Firing.Critical != 3 || res.Firing.Total != 6 {
		t.Fatalf("firing = %+v", res.Firing)
	}
}
