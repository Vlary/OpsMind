// fakes_test.go 是 monitor 单测的共享 fake：store（嵌入接口 + 覆盖 M1 子
// 存储）、SSH/WinRM 执行器、审计 sink、时钟。
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// ---------------------------------------------------------------------------
// fake store
// ---------------------------------------------------------------------------

// fakeMonitorStore 嵌入 store.Store（未覆盖的方法 panic），按需覆盖 M1 子
// 存储。alertEvents.Create 模拟部分唯一索引（同 (rule,server) 至多一条
// firing，重复返回 ErrConflict）。
type fakeMonitorStore struct {
	store.Store

	mu        sync.Mutex
	servers   map[string]store.Server
	metrics   []store.InsertMetricParams
	latest    map[string]store.ServerMetricLatest
	rules     map[string]store.AlertRule
	alerts    []store.AlertEvent
	channels  map[string]store.NotificationChannel
	settings  map[string]store.Setting
	nextEvent int64
	windowErr error
}

func newFakeMonitorStore() *fakeMonitorStore {
	return &fakeMonitorStore{
		servers:  map[string]store.Server{},
		latest:   map[string]store.ServerMetricLatest{},
		rules:    map[string]store.AlertRule{},
		channels: map[string]store.NotificationChannel{},
		settings: map[string]store.Setting{},
	}
}

func (f *fakeMonitorStore) upsertServer(s store.Server) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[s.ID] = s
}

func fakeServer(id string, os store.ServerOS, mods ...func(*store.Server)) store.Server {
	s := store.Server{
		ID: id, Name: "srv-" + id, DeviceKind: store.DeviceKindServer, OS: os,
		Host: "127.0.0.1", Port: 22, Username: "root", AuthMethod: store.AuthMethodPassword,
	}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func (f *fakeMonitorStore) Servers() store.ServerStore { return (*fakeServers)(f) }

type fakeServers fakeMonitorStore

func (f *fakeServers) List(context.Context) ([]store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Server, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (f *fakeServers) Get(_ context.Context, id string) (store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return store.Server{}, store.ErrNotFound
}
func (f *fakeServers) Create(context.Context, store.CreateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServers) Update(context.Context, string, store.UpdateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServers) Delete(context.Context, string) error { return errors.New("not implemented") }
func (f *fakeServers) UpdateHostKey(context.Context, string, string, string, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServers) UpdateContainerRuntime(context.Context, string, store.ContainerRuntime, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}

func (f *fakeMonitorStore) Metrics() store.MetricStore { return (*fakeMetrics)(f) }

type fakeMetrics fakeMonitorStore

func (f *fakeMetrics) BatchInsert(_ context.Context, items []store.InsertMetricParams) error {
	if len(items) > BatchInsertMax {
		return store.ErrInvalid
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metrics = append(f.metrics, items...)
	for _, it := range items {
		name := ""
		if s, ok := f.servers[it.ServerID]; ok {
			name = s.Name
		}
		f.latest[it.ServerID] = store.ServerMetricLatest{
			ServerID: it.ServerID, ServerName: name, CreatedAt: it.CreatedAt,
			CPU: it.CPU, Mem: it.Mem, Disk: it.Disk, Load1: it.Load1,
			NetIn: it.NetIn, NetOut: it.NetOut, UpdatedAt: it.CreatedAt,
		}
	}
	return nil
}

func (f *fakeMetrics) Latest(context.Context) ([]store.ServerMetricLatest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.ServerMetricLatest, 0, len(f.latest))
	for _, m := range f.latest {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out, nil
}

func (f *fakeMetrics) LatestByServer(_ context.Context, serverID string) (store.ServerMetricLatest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.latest[serverID]; ok {
		return m, nil
	}
	return store.ServerMetricLatest{}, store.ErrNotFound
}

func (f *fakeMetrics) Window(_ context.Context, w store.MetricWindow) ([]store.ServerMetric, error) {
	if f.windowErr != nil {
		return nil, f.windowErr
	}
	if w.Limit > 1440 {
		return nil, store.ErrInvalid
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ServerMetric
	for _, it := range f.metrics {
		if w.ServerID != "" && it.ServerID != w.ServerID {
			continue
		}
		if !it.CreatedAt.Before(w.From) && !it.CreatedAt.After(w.To) {
			out = append(out, store.ServerMetric{
				ServerID: it.ServerID, CreatedAt: it.CreatedAt, CPU: it.CPU, Mem: it.Mem,
				Disk: it.Disk, Load1: it.Load1, NetIn: it.NetIn, NetOut: it.NetOut,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > w.Limit {
		out = out[len(out)-w.Limit:]
	}
	return out, nil
}

func (f *fakeMetrics) DeleteBefore(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.metrics[:0]
	var n int64
	for _, it := range f.metrics {
		if it.CreatedAt.Before(cutoff) {
			n++
			continue
		}
		kept = append(kept, it)
	}
	f.metrics = kept
	return n, nil
}

func (f *fakeMonitorStore) AlertRules() store.AlertRuleStore { return (*fakeRules)(f) }

type fakeRules fakeMonitorStore

func (f *fakeRules) List(context.Context) ([]store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.AlertRule, 0, len(f.rules))
	for _, r := range f.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (f *fakeRules) Get(_ context.Context, id string) (store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rules[id]; ok {
		return r, nil
	}
	return store.AlertRule{}, store.ErrNotFound
}
func (f *fakeRules) Create(_ context.Context, p store.CreateAlertRuleParams) (store.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rules {
		if r.Name == p.Name {
			return store.AlertRule{}, store.ErrConflict
		}
	}
	r := store.AlertRule{
		ID: fmt.Sprintf("rule-%s", p.Name), Name: p.Name, Metric: p.Metric,
		Comparator: p.Comparator, Threshold: p.Threshold, DurationWindows: p.DurationWindows,
		CooldownSeconds: p.CooldownSeconds, Severity: p.Severity, SilentUntil: p.SilentUntil,
		NotifyChannelID: p.NotifyChannelID, Enabled: p.Enabled,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.rules[r.ID] = r
	return r, nil
}
func (f *fakeRules) Update(context.Context, string, store.UpdateAlertRuleParams) (store.AlertRule, error) {
	return store.AlertRule{}, errors.New("not implemented")
}
func (f *fakeRules) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeMonitorStore) AlertEvents() store.AlertEventStore { return (*fakeEvents)(f) }

type fakeEvents fakeMonitorStore

func (f *fakeEvents) Create(_ context.Context, rule store.AlertRule, serverID string, value float64, message string, firedAt time.Time) (store.AlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.alerts {
		if ev.RuleID == rule.ID && ev.ServerID == serverID && ev.Status == store.AlertEventFiring {
			return store.AlertEvent{}, store.ErrConflict // 模拟部分唯一索引
		}
	}
	f.nextEvent++
	ev := store.AlertEvent{
		ID: f.nextEvent, RuleID: rule.ID, RuleName: rule.Name, ServerID: serverID,
		ServerName: "srv-" + serverID, Metric: rule.Metric, Status: store.AlertEventFiring,
		Severity: rule.Severity, Value: value, Message: message, FiredAt: firedAt,
		NotifyStatus: store.NotifyNone, CreatedAt: firedAt, UpdatedAt: firedAt,
	}
	f.alerts = append(f.alerts, ev)
	return ev, nil
}

func (f *fakeEvents) List(_ context.Context, flt store.AlertEventFilter) ([]store.AlertEvent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AlertEvent
	for _, ev := range f.alerts {
		if flt.Status != "" && ev.Status != flt.Status {
			continue
		}
		if flt.Severity != "" && ev.Severity != flt.Severity {
			continue
		}
		if flt.ServerID != "" && ev.ServerID != flt.ServerID {
			continue
		}
		if flt.BeforeID > 0 && ev.ID >= flt.BeforeID {
			continue
		}
		out = append(out, ev)
	}
	// fired_at DESC, id DESC
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FiredAt.Equal(out[j].FiredAt) {
			return out[i].FiredAt.After(out[j].FiredAt)
		}
		return out[i].ID > out[j].ID
	})
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

func (f *fakeEvents) Get(_ context.Context, id int64) (store.AlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.alerts {
		if ev.ID == id {
			return ev, nil
		}
	}
	return store.AlertEvent{}, store.ErrNotFound
}

func (f *fakeEvents) Resolve(_ context.Context, id int64, at time.Time) (store.AlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.alerts {
		if f.alerts[i].ID != id {
			continue
		}
		if f.alerts[i].Status != store.AlertEventFiring {
			return store.AlertEvent{}, store.ErrConflict
		}
		f.alerts[i].Status = store.AlertEventResolved
		f.alerts[i].ResolvedAt = &at
		return f.alerts[i], nil
	}
	return store.AlertEvent{}, store.ErrNotFound
}

func (f *fakeEvents) SetNotifyStatus(_ context.Context, id int64, status store.NotifyStatus, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.alerts {
		if f.alerts[i].ID == id {
			f.alerts[i].NotifyStatus = status
			f.alerts[i].LastNotifiedAt = &at
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeEvents) CountFiring(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, ev := range f.alerts {
		if ev.Status == store.AlertEventFiring {
			n++
		}
	}
	return n, nil
}

func (f *fakeEvents) CountFiringByServer(_ context.Context, serverID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, ev := range f.alerts {
		if ev.Status == store.AlertEventFiring && ev.ServerID == serverID {
			n++
		}
	}
	return n, nil
}

func (f *fakeMonitorStore) NotificationChannels() store.NotificationChannelStore {
	return (*fakeChannels)(f)
}

type fakeChannels fakeMonitorStore

func (f *fakeChannels) List(context.Context) ([]store.NotificationChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NotificationChannel, 0, len(f.channels))
	for _, ch := range f.channels {
		out = append(out, ch)
	}
	return out, nil
}
func (f *fakeChannels) Get(_ context.Context, id string) (store.NotificationChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.channels[id]; ok {
		return ch, nil
	}
	return store.NotificationChannel{}, store.ErrNotFound
}
func (f *fakeChannels) Create(_ context.Context, p store.CreateNotificationChannelParams) (store.NotificationChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("ch-%s", p.Name)
	if _, dup := f.channels[id]; dup {
		return store.NotificationChannel{}, store.ErrConflict
	}
	ch := store.NotificationChannel{
		ID: id, Name: p.Name, Kind: p.Kind, ConfigEncrypted: p.ConfigJSON,
		ConfigHint: p.ConfigHint, HasConfig: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.channels[id] = ch
	return ch, nil
}
func (f *fakeChannels) Update(context.Context, string, store.UpdateNotificationChannelParams) (store.NotificationChannel, error) {
	return store.NotificationChannel{}, errors.New("not implemented")
}
func (f *fakeChannels) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.channels[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.channels, id)
	return nil
}

func (f *fakeMonitorStore) Settings() store.SettingStore { return (*fakeSettings)(f) }

type fakeSettings fakeMonitorStore

func (f *fakeSettings) Get(_ context.Context, key string) (store.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.settings[key]; ok {
		return s, nil
	}
	return store.Setting{}, store.ErrNotFound
}
func (f *fakeSettings) Set(_ context.Context, key string, value json.RawMessage, at time.Time) (store.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := store.Setting{Key: key, Value: value, UpdatedAt: at}
	f.settings[key] = s
	return s, nil
}
func (f *fakeSettings) List(context.Context) ([]store.Setting, error) { return nil, nil }

// newTestCipher 返回测试用 AES-GCM cipher（32 字节零密钥）。
func newTestCipher(t *testing.T) *cryptox.Cipher {
	t.Helper()
	c, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("new test cipher: %v", err)
	}
	return c
}

// withTestCreds 把空密码加密为合法密文（生产语义：凭据一律密文存储）。
func withTestCreds(t *testing.T, c *cryptox.Cipher, srv *store.Server) {
	t.Helper()
	enc, err := c.Encrypt("")
	if err != nil {
		t.Fatalf("encrypt empty password: %v", err)
	}
	srv.PasswordEncrypted = enc
}

// ---------------------------------------------------------------------------
// fake executors
// ---------------------------------------------------------------------------

// fakeSSHExecutor 按服务器返回固定输出或错误。
type fakeSSHExecutor struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func (f *fakeSSHExecutor) Test(context.Context, sshx.Target) (sshx.TestResult, error) {
	return sshx.TestResult{}, nil
}
func (f *fakeSSHExecutor) Execute(_ context.Context, target sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, target.ServerID+"|"+req.Command)
	if err, ok := f.errs[target.ServerID]; ok {
		return sshx.RunResult{}, err
	}
	return sshx.RunResult{ExitCode: 0, Combined: f.outputs[target.ServerID]}, nil
}
func (f *fakeSSHExecutor) Invalidate(string) {}
func (f *fakeSSHExecutor) Close() error      { return nil }

// fakeWinRMExecutor 同形。
type fakeWinRMExecutor struct{ fakeSSHExecutor }

func (f *fakeWinRMExecutor) Test(context.Context, winrmx.Target) (winrmx.TestResult, error) {
	return winrmx.TestResult{}, nil
}
func (f *fakeWinRMExecutor) Execute(_ context.Context, target winrmx.Target, req winrmx.RunRequest) (winrmx.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, target.ServerID+"|"+req.Command)
	if err, ok := f.errs[target.ServerID]; ok {
		return winrmx.RunResult{}, err
	}
	return winrmx.RunResult{ExitCode: 0, Combined: f.outputs[target.ServerID]}, nil
}

// ---------------------------------------------------------------------------
// fake audit sink
// ---------------------------------------------------------------------------

type fakeAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *fakeAuditSink) Enqueue(_ context.Context, e audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *fakeAuditSink) of(action string) []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Event
	for _, e := range s.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// quietLogger 返回丢弃输出的 slog。
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
