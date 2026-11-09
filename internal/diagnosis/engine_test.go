// engine_test.go 覆盖契约 G33（诊断引擎）与防 OOM 内存守卫：
// fake LLM/SSH/store 驱动完整状态机——safe 闸（needs_approval 拒绝且伪结果
// 回给模型）、报告解析失败重试一次、轮数耗尽、并发 2 + 队列上限 ErrBusy、
// awaiting_fix 过期扫描、报告越界截断、forbidden 建议过滤、停机收敛。
package diagnosis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/llm"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeDiagStore struct {
	store.Store

	mu        sync.Mutex
	servers   map[string]store.Server
	sessions  map[string]*store.DiagnosisSession
	order     []string
	providers []store.Provider
	nextSeq   int64
}

func newFakeDiagStore() *fakeDiagStore {
	return &fakeDiagStore{
		servers:  map[string]store.Server{},
		sessions: map[string]*store.DiagnosisSession{},
	}
}

func (f *fakeDiagStore) Servers() store.ServerStore { return (*fakeServersDiag)(f) }

type fakeServersDiag fakeDiagStore

func (f *fakeServersDiag) Get(_ context.Context, id string) (store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return store.Server{}, store.ErrNotFound
}
func (f *fakeServersDiag) List(context.Context) ([]store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Server, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeServersDiag) Create(context.Context, store.CreateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServersDiag) Update(context.Context, string, store.UpdateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServersDiag) Delete(context.Context, string) error { return errors.New("not implemented") }
func (f *fakeServersDiag) UpdateHostKey(context.Context, string, string, string, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServersDiag) UpdateContainerRuntime(context.Context, string, store.ContainerRuntime, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}

func (f *fakeDiagStore) DiagnosisSessions() store.DiagnosisSessionStore {
	return (*fakeDiagSessions)(f)
}

type fakeDiagSessions fakeDiagStore

func (f *fakeDiagSessions) Create(_ context.Context, p store.CreateDiagnosisParams) (store.DiagnosisSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("diag-%03d", f.nextSeq)
	f.nextSeq++
	now := time.Now().UTC()
	sess := &store.DiagnosisSession{
		ID: id, ServerID: p.ServerID, AlertEventID: p.AlertEventID,
		ConversationID: p.ConversationID, Trigger: p.Trigger,
		Status: store.DiagnosisPending, ExpiresAt: now.Add(30 * time.Minute),
		CreatedAt: now, UpdatedAt: now,
	}
	f.sessions[id] = sess
	f.order = append(f.order, id)
	return *sess, nil
}

func (f *fakeDiagSessions) Get(_ context.Context, id string) (store.DiagnosisSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[id]; ok {
		return *s, nil
	}
	return store.DiagnosisSession{}, store.ErrNotFound
}

func (f *fakeDiagSessions) List(_ context.Context, status store.DiagnosisStatus, serverID string, limit int) ([]store.DiagnosisSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.DiagnosisSession{}
	for _, id := range f.order {
		s := f.sessions[id]
		if status != "" && s.Status != status {
			continue
		}
		if serverID != "" && s.ServerID != serverID {
			continue
		}
		out = append(out, *s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeDiagSessions) UpdateStatus(_ context.Context, p store.UpdateDiagnosisStatusParams) (store.DiagnosisSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[p.ID]
	if !ok {
		return store.DiagnosisSession{}, store.ErrNotFound
	}
	if p.From != "" && s.Status != p.From {
		return store.DiagnosisSession{}, store.ErrConflict
	}
	s.Status = p.To
	if p.To == store.DiagnosisDiagnosing && s.StartedAt == nil {
		t := p.Timestamp
		s.StartedAt = &t
	}
	switch p.To {
	case store.DiagnosisFailed, store.DiagnosisDone, store.DiagnosisExpired:
		t := p.Timestamp
		s.FinishedAt = &t
	case store.DiagnosisAwaitingFix:
		s.ExpiresAt = p.Timestamp.Add(30 * time.Minute)
	}
	if p.Report != nil {
		s.Report = p.Report
	}
	if p.ErrorCode != "" {
		s.ErrorCode = p.ErrorCode
	}
	return *s, nil
}

func (f *fakeDiagSessions) ExpireStale(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, s := range f.sessions {
		if s.Status == store.DiagnosisAwaitingFix && s.ExpiresAt.Before(now) {
			s.Status = store.DiagnosisExpired
			t := now
			s.FinishedAt = &t
			n++
		}
	}
	return n, nil
}

func (f *fakeDiagStore) Metrics() store.MetricStore { return (*fakeDiagMetrics)(f) }

type fakeDiagMetrics fakeDiagStore

func (f *fakeDiagMetrics) BatchInsert(context.Context, []store.InsertMetricParams) error {
	return errors.New("not implemented")
}
func (f *fakeDiagMetrics) Latest(context.Context) ([]store.ServerMetricLatest, error) {
	return nil, nil
}
func (f *fakeDiagMetrics) LatestByServer(context.Context, string) (store.ServerMetricLatest, error) {
	return store.ServerMetricLatest{}, store.ErrNotFound
}
func (f *fakeDiagMetrics) Window(context.Context, store.MetricWindow) ([]store.ServerMetric, error) {
	return nil, nil
}
func (f *fakeDiagMetrics) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeDiagStore) AlertEvents() store.AlertEventStore { return (*fakeDiagEvents)(f) }

type fakeDiagEvents fakeDiagStore

func (f *fakeDiagEvents) Create(context.Context, store.AlertRule, string, float64, string, time.Time) (store.AlertEvent, error) {
	return store.AlertEvent{}, errors.New("not implemented")
}
func (f *fakeDiagEvents) List(context.Context, store.AlertEventFilter) ([]store.AlertEvent, bool, error) {
	return nil, false, nil
}
func (f *fakeDiagEvents) Get(context.Context, int64) (store.AlertEvent, error) {
	return store.AlertEvent{}, store.ErrNotFound
}
func (f *fakeDiagEvents) Resolve(context.Context, int64, time.Time) (store.AlertEvent, error) {
	return store.AlertEvent{}, errors.New("not implemented")
}
func (f *fakeDiagEvents) SetNotifyStatus(context.Context, int64, store.NotifyStatus, time.Time) error {
	return nil
}
func (f *fakeDiagEvents) CountFiring(context.Context) (int64, error) { return 0, nil }
func (f *fakeDiagEvents) CountFiringByServer(context.Context, string) (int64, error) {
	return 0, nil
}

func (f *fakeDiagStore) Providers() store.ProviderStore { return (*fakeDiagProviders)(f) }

type fakeDiagProviders fakeDiagStore

func (f *fakeDiagProviders) List(context.Context) ([]store.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Provider(nil), f.providers...), nil
}
func (f *fakeDiagProviders) Get(context.Context, string) (store.Provider, error) {
	return store.Provider{}, store.ErrNotFound
}
func (f *fakeDiagProviders) Create(context.Context, store.CreateProviderParams) (store.Provider, error) {
	return store.Provider{}, errors.New("not implemented")
}
func (f *fakeDiagProviders) Update(context.Context, string, store.UpdateProviderParams) (store.Provider, error) {
	return store.Provider{}, errors.New("not implemented")
}
func (f *fakeDiagProviders) Delete(context.Context, string) error {
	return errors.New("not implemented")
}

// fakeSSHExec 记录执行的命令并按脚本回放输出。
type fakeSSHExec struct {
	mu       sync.Mutex
	commands []string
	reply    map[string]string
	failAll  bool
}

func (f *fakeSSHExec) Test(context.Context, sshx.Target) (sshx.TestResult, error) {
	return sshx.TestResult{}, nil
}

func (f *fakeSSHExec) Execute(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.mu.Lock()
	f.commands = append(f.commands, req.Command)
	reply, ok := f.reply[req.Command]
	f.mu.Unlock()
	if f.failAll {
		return sshx.RunResult{}, errors.New("ssh unreachable")
	}
	if !ok {
		reply = "ok"
	}
	return sshx.RunResult{ExitCode: 0, Combined: reply}, nil
}
func (f *fakeSSHExec) Invalidate(string) {}
func (f *fakeSSHExec) Close() error      { return nil }

func (f *fakeSSHExec) executedCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// fakeLLM 按脚本逐轮回放输出。
type fakeLLM struct {
	mu     sync.Mutex
	script []string
	calls  int
}

func (f *fakeLLM) Stream(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	f.mu.Unlock()
	text := "not a diagnosis"
	if idx < len(f.script) {
		text = f.script[idx]
	}
	if err := emit(llm.StreamEvent{Type: llm.EventToken, Delta: text}); err != nil {
		return err
	}
	return emit(llm.StreamEvent{Type: llm.EventDone})
}
func (f *fakeLLM) ListModels(context.Context) ([]string, error) { return []string{"fake"}, nil }
func (f *fakeLLM) Test(context.Context) error                   { return nil }

// ---------------------------------------------------------------------------
// 测试装配
// ---------------------------------------------------------------------------

func newTestEngine(t *testing.T, fs *fakeDiagStore, client llm.Client, ssh *fakeSSHExec) *Engine {
	t.Helper()
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	e := NewEngine(DefaultEngineConfig(), fs, nil, ssh, nil, cipher, nil, nil, nil)
	e.SetLLMClient(client)
	return e
}

// fakeLinuxServer 返回带可用密码凭据的 linux 服务器（cipher 可解密）。
func fakeLinuxServer(t *testing.T, e *Engine, id string) store.Server {
	t.Helper()
	enc, err := e.cipher.Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return store.Server{ID: id, Name: "srv-" + id, OS: store.ServerOSLinux,
		AuthMethod: store.AuthMethodPassword, PasswordEncrypted: enc}
}

func waitTerminal(t *testing.T, fs *fakeDiagStore, id string) store.DiagnosisSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sess, err := fs.DiagnosisSessions().Get(context.Background(), id)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if isTerminalForTest(sess.Status) {
			return sess
		}
		time.Sleep(5 * time.Millisecond)
	}
	sess, _ := fs.DiagnosisSessions().Get(context.Background(), id)
	t.Fatalf("session not terminal: status=%s", sess.Status)
	return sess
}

func isTerminalForTest(s store.DiagnosisStatus) bool {
	switch s {
	case store.DiagnosisAwaitingFix, store.DiagnosisDone, store.DiagnosisExpired, store.DiagnosisFailed:
		return true
	}
	return false
}

const goodReport = `{"action":"report","report":{"summary":"CPU 高由失控进程导致","severity":"critical","root_cause":"transcode worker 死循环","evidence":[{"source":"command","command":"ps aux --sort=-%cpu","output_excerpt":"PID 8121 380%","note":"top process"}],"suggestions":[{"command":"systemctl restart transcode-worker","reason":"重启 worker"}],"confidence":"high"}}`

func probeJSON(cmd string) string {
	b, _ := json.Marshal(map[string]any{"action": "probe", "command": cmd})
	return string(b)
}

func mustStart(t *testing.T, e *Engine) {
	t.Helper()
	e.Start(context.Background())
	t.Cleanup(e.Stop)
}

// ---------------------------------------------------------------------------
// G33：safe 闸与成功路径
// ---------------------------------------------------------------------------

// TestEngineSafeGateOnlyExecutesSafe 验证 safe 命令执行、needs_approval 与
// forbidden 拒绝且拒绝原因回给模型（脚本第二轮仍能产出报告——模型收到了
// 伪结果）。
func TestEngineSafeGateOnlyExecutesSafe(t *testing.T) {
	fs := newFakeDiagStore()
	fs.providers = []store.Provider{{ID: "p1", DefaultModel: "fake", CreatedAt: time.Now()}}

	ssh := &fakeSSHExec{reply: map[string]string{"uptime": " 12:00 up 3 days"}}
	client := &fakeLLM{script: []string{
		probeJSON("uptime"),                  // safe：执行
		probeJSON("systemctl restart nginx"), // needs_approval：拒绝
		probeJSON("rm -rf /"),                // forbidden：拒绝
		goodReport,                           // 报告
	}}

	e := newTestEngine(t, fs, client, ssh)
	fs.servers["srv-1"] = fakeLinuxServer(t, e, "srv-1")
	mustStart(t, e)

	sess, err := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	final := waitTerminal(t, fs, sess.ID)

	if final.Status != store.DiagnosisAwaitingFix {
		t.Fatalf("status = %s, want awaiting_fix (error_code=%s)", final.Status, final.ErrorCode)
	}
	// 仅 uptime 真正执行（safe 闸）。
	cmds := ssh.executedCommands()
	if len(cmds) != 1 || cmds[0] != "uptime" {
		t.Fatalf("executed commands = %v, want [uptime]", cmds)
	}
	// 报告固化。
	var rep DiagnosisReport
	if err := json.Unmarshal(final.Report, &rep); err != nil {
		t.Fatalf("report unparsable: %v", err)
	}
	if rep.Summary == "" || len(rep.Suggestions) != 1 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	// restart 建议 risk_level 预估为 needs_approval。
	if rep.Suggestions[0].RiskLevel != string(risk.LevelNeedsApproval) {
		t.Fatalf("suggestion risk_level = %s, want needs_approval", rep.Suggestions[0].RiskLevel)
	}
}

// TestEngineReportInvalidRetryThenFailed 非法报告 → 修正重试一次成功；
// 两次非法 → failed(DIAGNOSIS_REPORT_INVALID)。
func TestEngineReportInvalidRetryThenFailed(t *testing.T) {
	fs := newFakeDiagStore()
	fs.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	fs.providers = []store.Provider{{ID: "p1", DefaultModel: "fake"}}

	t.Run("retry succeeds", func(t *testing.T) {
		client := &fakeLLM{script: []string{
			`{"action":"report","report":{"summary":"x","severity":"bogus","confidence":"high"}}`, // severity 非法
			goodReport,
		}}
		e := newTestEngine(t, fs, client, &fakeSSHExec{})
		mustStart(t, e)
		sess, _ := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
		final := waitTerminal(t, fs, sess.ID)
		if final.Status != store.DiagnosisAwaitingFix {
			t.Fatalf("status = %s (%s)", final.Status, final.ErrorCode)
		}
	})

	t.Run("twice invalid fails", func(t *testing.T) {
		client := &fakeLLM{script: []string{
			`{"action":"report","report":{"summary":"x","severity":"bogus","confidence":"high"}}`,
			`{"action":"report","report":{"summary":"y","severity":"info","confidence":"nope"}}`,
		}}
		e := newTestEngine(t, fs, client, &fakeSSHExec{})
		mustStart(t, e)
		sess, _ := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
		final := waitTerminal(t, fs, sess.ID)
		if final.Status != store.DiagnosisFailed || final.ErrorCode != "DIAGNOSIS_REPORT_INVALID" {
			t.Fatalf("status=%s error_code=%s, want failed/DIAGNOSIS_REPORT_INVALID", final.Status, final.ErrorCode)
		}
	})
}

// TestEngineMaxRounds 轮数耗尽（6 轮 probe 无报告）→ failed。
func TestEngineMaxRounds(t *testing.T) {
	fs := newFakeDiagStore()
	fs.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	fs.providers = []store.Provider{{ID: "p1", DefaultModel: "fake"}}

	script := make([]string, 8)
	for i := range script {
		script[i] = probeJSON("uptime")
	}
	e := newTestEngine(t, fs, &fakeLLM{script: script}, &fakeSSHExec{})
	mustStart(t, e)
	sess, _ := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
	final := waitTerminal(t, fs, sess.ID)
	if final.Status != store.DiagnosisFailed || final.ErrorCode != "DIAGNOSIS_MAX_ROUNDS" {
		t.Fatalf("status=%s error_code=%s, want failed/DIAGNOSIS_MAX_ROUNDS", final.Status, final.ErrorCode)
	}
}

// TestEngineNoProvider 无供应商 → failed(NO_PROVIDER_CONFIGURED)。
func TestEngineNoProvider(t *testing.T) {
	fs := newFakeDiagStore()
	fs.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	e := NewEngine(DefaultEngineConfig(), fs, nil, &fakeSSHExec{}, nil, nil, nil, nil, nil)
	mustStart(t, e)
	sess, _ := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
	final := waitTerminal(t, fs, sess.ID)
	if final.Status != store.DiagnosisFailed || final.ErrorCode != "NO_PROVIDER_CONFIGURED" {
		t.Fatalf("status=%s error_code=%s", final.Status, final.ErrorCode)
	}
}

// TestEngineQueueLimitSmall 队列上限（缩小的队列）→ ErrBusy。
func TestEngineQueueLimitSmall(t *testing.T) {
	fs := newFakeDiagStore()
	fs.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	e := NewEngine(EngineConfig{QueueSize: 1, MaxConcurrent: 1}, fs, nil, &fakeSSHExec{}, nil, nil, nil, nil, nil)
	// 不 Start（worker 不消费）：队列容量 1，第二个 Create 必须 ErrBusy。
	if _, err := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second create err = %v, want ErrBusy", err)
	}
}

// TestEngineExpireStale awaiting_fix 过期 → expired（扫描器路径）。
func TestEngineExpireStale(t *testing.T) {
	fs := newFakeDiagStore()
	fs.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	fs.providers = []store.Provider{{ID: "p1", DefaultModel: "fake"}}
	e := newTestEngine(t, fs, &fakeLLM{script: []string{goodReport}}, &fakeSSHExec{})
	mustStart(t, e)
	sess, _ := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
	waitTerminal(t, fs, sess.ID)

	// 把 expires_at 拨到过去，直接调 store 的 ExpireStale（引擎每 30s 扫描
	// 同一入口）。
	fs.mu.Lock()
	fs.sessions[sess.ID].ExpiresAt = time.Now().Add(-time.Minute)
	fs.mu.Unlock()
	n, err := fs.DiagnosisSessions().ExpireStale(context.Background(), time.Now())
	if err != nil || n != 1 {
		t.Fatalf("ExpireStale = (%d, %v), want (1, nil)", n, err)
	}
	got, _ := fs.DiagnosisSessions().Get(context.Background(), sess.ID)
	if got.Status != store.DiagnosisExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
}

// TestEngineStopFailsDiagnosing 停机时进行中的会话 → failed(SERVER_SHUTTING_DOWN)。
func TestEngineStopFailsDiagnosing(t *testing.T) {
	fs := newFakeDiagStore()
	fs.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	fs.providers = []store.Provider{{ID: "p1", DefaultModel: "fake"}}
	e := newTestEngine(t, fs, &fakeLLM{}, &fakeSSHExec{})
	e.Start(context.Background())
	sess, _ := e.Create(context.Background(), store.CreateDiagnosisParams{ServerID: "srv-1"})
	// 手动置 diagnosing（模拟进行中）。
	_, _ = fs.DiagnosisSessions().UpdateStatus(context.Background(), store.UpdateDiagnosisStatusParams{
		ID: sess.ID, From: store.DiagnosisPending, To: store.DiagnosisDiagnosing, Timestamp: time.Now(),
	})
	e.Stop()
	got, _ := fs.DiagnosisSessions().Get(context.Background(), sess.ID)
	if got.Status != store.DiagnosisFailed || got.ErrorCode != "SERVER_SHUTTING_DOWN" {
		t.Fatalf("status=%s error_code=%s, want failed/SERVER_SHUTTING_DOWN", got.Status, got.ErrorCode)
	}
}

// ---------------------------------------------------------------------------
// 报告 schema（G33：越界截断）
// ---------------------------------------------------------------------------

func TestReportNormalizeTruncates(t *testing.T) {
	rep := DiagnosisReport{
		Summary: strings.Repeat("s", 5000), Severity: "info", RootCause: "r",
		Confidence: "high",
	}
	for i := 0; i < 30; i++ { // evidence 30 → 20
		rep.Evidence = append(rep.Evidence, DiagnosisEvidence{
			Source: "command", OutputExcerpt: strings.Repeat("x", 8192), Note: "n",
		})
	}
	for i := 0; i < 15; i++ { // suggestions 15 → 10；含 forbidden 过滤
		rep.Suggestions = append(rep.Suggestions, FixSuggestion{
			Command: fmt.Sprintf("echo %d", i), Reason: "r",
		})
	}
	rep.Suggestions = append(rep.Suggestions, FixSuggestion{Command: "rm -rf /", Reason: "evil"})
	rep.Normalize(risk.OSLinux)

	if len(rep.Evidence) != MaxEvidenceCount {
		t.Fatalf("evidence = %d, want %d", len(rep.Evidence), MaxEvidenceCount)
	}
	for _, ev := range rep.Evidence {
		if len(ev.OutputExcerpt) > MaxExcerptBytes {
			t.Fatalf("excerpt %d > %d", len(ev.OutputExcerpt), MaxExcerptBytes)
		}
	}
	// 6 条合法（echo 0..4 + 5..9 中截到 10 前的）且无 forbidden。
	if len(rep.Suggestions) != 10 {
		t.Fatalf("suggestions = %d, want 10", len(rep.Suggestions))
	}
	for _, s := range rep.Suggestions {
		if s.Command == "rm -rf /" {
			t.Fatalf("forbidden suggestion survived filter")
		}
	}
	if len(rep.Summary) > 2048 {
		t.Fatalf("summary not truncated")
	}
}

func TestReportSerializeBudget(t *testing.T) {
	rep := DiagnosisReport{
		Summary: "s", Severity: "info", RootCause: "r", Confidence: "high",
		Evidence: make([]DiagnosisEvidence, MaxEvidenceCount),
	}
	for i := range rep.Evidence {
		rep.Evidence[i] = DiagnosisEvidence{Source: "log", OutputExcerpt: strings.Repeat("e", MaxExcerptBytes), Note: "n"}
	}
	rep.Suggestions = []FixSuggestion{{Command: "systemctl restart x", Reason: "r", RiskLevel: "needs_approval"}}
	raw, err := rep.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if len(raw) > ReportMaxBytes {
		t.Fatalf("serialized %d > %d", len(raw), ReportMaxBytes)
	}
	// suggestions 必须保留（契约 11 节 #8：超限截 evidence 保 suggestions）。
	if !strings.Contains(string(raw), "systemctl restart x") {
		t.Fatalf("suggestion dropped during evidence truncation")
	}
}

func TestParseReportRejects(t *testing.T) {
	bad := []string{
		`{"summary":"x","severity":"info","confidence":"high","unknown":1}`, // 未知字段
		`{"summary":"x","severity":"warn","confidence":"high"}`,             // 枚举
		`{"summary":"","severity":"info","confidence":"high"}`,              // 空 summary
		`not json`,
	}
	for _, in := range bad {
		if _, err := ParseReport([]byte(in)); err == nil {
			t.Fatalf("ParseReport(%q) should fail", in)
		}
	}
	// 超 64KB 输入拒绝。
	big := `{"summary":"` + strings.Repeat("a", ReportMaxBytes) + `","severity":"info","confidence":"high"}`
	if _, err := ParseReport([]byte(big)); err == nil {
		t.Fatalf("oversized payload should fail")
	}
}

// ---------------------------------------------------------------------------
// 内存守卫（防 OOM 铁律 #6：核心循环分配预算）
// ---------------------------------------------------------------------------

// TestNormalizeAllocBudget 断言 Normalize（核心固化路径）对标准报告的
// 分配有界（重复运行不随输入规模之外的因子增长）。
func TestNormalizeAllocBudget(t *testing.T) {
	rep := DiagnosisReport{
		Summary: "s", Severity: "info", RootCause: "r", Confidence: "high",
		Evidence:    []DiagnosisEvidence{{Source: "command", OutputExcerpt: "out", Note: "n"}},
		Suggestions: []FixSuggestion{{Command: "systemctl restart x", Reason: "r"}},
	}
	allocs := testing.AllocsPerRun(20, func() {
		r := rep // 拷贝后原地截断（避免跨运行累积）
		r.Normalize(risk.OSLinux)
	})
	if allocs > 40 {
		t.Fatalf("Normalize allocs = %.0f, want <= 40 (evidence 截断/过滤路径分配有界)", allocs)
	}
}

// TestProbeCommandLimit 候选命令超 16KB → 拒绝（解析器输入限制）。
func TestProbeCommandLimit(t *testing.T) {
	e := &Engine{clock: time.Now}
	res := e.runProbe(context.Background(),
		store.DiagnosisSession{Trigger: store.DiagnosisTriggerManual},
		store.Server{ID: "s1", OS: store.ServerOSLinux},
		strings.Repeat("a", 20000))
	if res.Executed || res.Rejected == "" {
		t.Fatalf("oversized probe should be rejected: %+v", res)
	}
}

// TestSSHTargetEmptyPassphraseShortCircuits 覆盖六期打磨 sprint 4 的走查
// high：私钥认证 + 未填 passphrase（openapi 4.3 该字段可选）时诊断引擎
// 不再报 "diagnosis: decrypt passphrase failed"——此前 decrypt 对空串无
// 短路，cryptox 返回 ErrInvalidCiphertext，AI 诊断整条路径失败。
func TestSSHTargetEmptyPassphraseShortCircuits(t *testing.T) {
	e := newTestEngine(t, newFakeDiagStore(), &fakeLLM{}, &fakeSSHExec{})
	key, err := e.cipher.Encrypt("KEY-BODY")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	srv := store.Server{
		ID: "srv-pk", Name: "srv-pk", OS: store.ServerOSLinux,
		AuthMethod: store.AuthMethodPrivateKey, PrivateKeyEncrypted: key,
		PassphraseEncrypted: "",
	}
	target, err := e.sshTarget(srv)
	if err != nil {
		t.Fatalf("sshTarget with empty passphrase: %v", err)
	}
	if target.PrivateKey != "KEY-BODY" || target.Passphrase != "" {
		t.Fatalf("target = %+v", target)
	}
	// 非法密文仍报错（不因空串短路放宽）。
	if _, err := decrypt(e.cipher, "bogus"); err == nil {
		t.Fatal("invalid ciphertext must still fail")
	}
}
