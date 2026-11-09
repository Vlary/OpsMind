package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/llm"
	"aiops/internal/store"
)

// errStore 在 testStore 之上按需注入仓储错误。
type errStore struct {
	*testStore
	convGetErr     error
	provGetErr     error
	provListErr    error
	msgCreateErr   error
	serversListErr error
	dbListErr      error
	hostKeyErr     error
}

type errConversationRepo struct {
	*conversationRepo
	s *errStore
}

func (r *errConversationRepo) Get(ctx context.Context, id string) (store.Conversation, error) {
	if r.s.convGetErr != nil {
		return store.Conversation{}, r.s.convGetErr
	}
	return r.conversationRepo.Get(ctx, id)
}

type errProviderRepo struct {
	*providerRepo
	s *errStore
}

func (r *errProviderRepo) Get(ctx context.Context, id string) (store.Provider, error) {
	if r.s.provGetErr != nil {
		return store.Provider{}, r.s.provGetErr
	}
	return r.providerRepo.Get(ctx, id)
}
func (r *errProviderRepo) List(ctx context.Context) ([]store.Provider, error) {
	if r.s.provListErr != nil {
		return nil, r.s.provListErr
	}
	return r.providerRepo.List(ctx)
}

type errMessageRepo struct {
	*messageRepo
	s *errStore
}

func (r *errMessageRepo) Create(ctx context.Context, p store.CreateMessageParams) (store.Message, error) {
	if r.s.msgCreateErr != nil {
		return store.Message{}, r.s.msgCreateErr
	}
	return r.messageRepo.Create(ctx, p)
}

type errServerRepo struct {
	*serverRepo
	s *errStore
}

func (r *errServerRepo) List(ctx context.Context) ([]store.Server, error) {
	if r.s.serversListErr != nil {
		return nil, r.s.serversListErr
	}
	return r.serverRepo.List(ctx)
}
func (r *errServerRepo) UpdateHostKey(ctx context.Context, id, fingerprint, publicKey string, at time.Time) (store.Server, error) {
	if r.s.hostKeyErr != nil {
		return store.Server{}, r.s.hostKeyErr
	}
	return r.serverRepo.UpdateHostKey(ctx, id, fingerprint, publicKey, at)
}

type errDatabaseRepo struct {
	*databaseRepo
	s *errStore
}

func (r *errDatabaseRepo) List(ctx context.Context) ([]store.Database, error) {
	if r.s.dbListErr != nil {
		return nil, r.s.dbListErr
	}
	return r.databaseRepo.List(ctx)
}

func (e *errStore) Conversations() store.ConversationStore {
	return &errConversationRepo{conversationRepo: &conversationRepo{s: e.testStore}, s: e}
}

func (e *errStore) Providers() store.ProviderStore {
	return &errProviderRepo{providerRepo: &providerRepo{s: e.testStore}, s: e}
}
func (e *errStore) Messages() store.MessageStore {
	return &errMessageRepo{messageRepo: &messageRepo{s: e.testStore}, s: e}
}
func (e *errStore) Servers() store.ServerStore {
	return &errServerRepo{serverRepo: &serverRepo{s: e.testStore}, s: e}
}
func (e *errStore) Databases() store.DatabaseStore {
	return &errDatabaseRepo{databaseRepo: &databaseRepo{s: e.testStore}, s: e}
}

func newErrStore(base *testStore) *errStore { return &errStore{testStore: base} }

// WithTx 把 errStore 自身传给回调，保证注入的错误在事务内同样生效。
func (e *errStore) WithTx(ctx context.Context, fn func(store.Store) error) error { return fn(e) }

// newErrRunnerForTest 用 errStore 构造 Runner（带错误注入）。
func newErrRunnerForTest(t *testing.T, st *errStore, client llm.Client, cfg RunnerConfig) *Runner {
	t.Helper()
	r := NewRunner(st, nil, &fakeSSH{}, nil, nil, audit.NewWriter(st, testLogger(), time.Now), nil, cfg, time.Now, testLogger())
	r.SetLLMClient(client)
	return r
}

func collectEvents(_ []Event) {}

// ---------------------------------------------------------------------------
// RunError / NewRunner / 辅助函数
// ---------------------------------------------------------------------------

func TestRunErrorErrorString(t *testing.T) {
	e := &RunError{Code: "X", Message: "m", Err: errors.New("cause")}
	if got := e.Error(); got != "X: m" {
		t.Fatalf("Error() = %q", got)
	}
	e2 := runErr("Y", "n", true)
	if e2.Error() != "Y: n" || !e2.Retryable {
		t.Fatalf("runErr = %+v", e2)
	}
}

func TestNewRunnerDefaultsAndNow(t *testing.T) {
	st := seed(false)
	ap := approvals.NewService(nil, nil, time.Minute, nil, nil)
	r := NewRunner(st, nil, nil, nil, ap, nil, nil, RunnerConfig{}, nil, nil)
	if r.approvals == nil {
		t.Fatal("non-nil approvals service must be wired")
	}
	if r.cfg.MaxIterations != 12 || r.cfg.ContextMessages != 40 {
		t.Fatalf("cfg defaults = %+v", r.cfg)
	}
	if r.cfg.LLMCallTimeout != 180*time.Second || r.cfg.SSHDefaultTimeout != 60*time.Second {
		t.Fatalf("timeout defaults = %+v", r.cfg)
	}
	if r.now().IsZero() {
		t.Fatal("default clock must return current time")
	}
	if got := stateRequestID(nil); got != "" {
		t.Fatalf("stateRequestID(nil) = %q", got)
	}

	// Store 未配置时 Prepare 失败关闭。
	r2 := NewRunner(nil, nil, nil, nil, nil, nil, nil, RunnerConfig{}, time.Now, testLogger())
	if _, err := r2.Prepare(context.Background(), "c1", "", ""); err == nil ||
		!strings.Contains(err.Error(), "storage is not configured") {
		t.Fatalf("Prepare without store err = %v", err)
	}
}

// classifyLLMError 的分支矩阵（ctx/超时/上游错误）。
func TestClassifyLLMErrorContexts(t *testing.T) {
	r := NewRunner(nil, nil, nil, nil, nil, nil, nil, RunnerConfig{}, time.Now, testLogger())

	// runCtx 超时 → STREAM_TIMEOUT。
	runCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	code, _, _ := r.classifyLLMError(context.Background(), runCtx, errors.New("x"))
	cancel()
	if code != codeStreamTimeout {
		t.Fatalf("runCtx timeout code = %s", code)
	}

	// 外层 ctx 取消 → CLIENT_DISCONNECTED。
	ctx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	code, _, _ = r.classifyLLMError(ctx, context.Background(), errors.New("x"))
	if code != codeClientDisconnected {
		t.Fatalf("ctx cancelled code = %s", code)
	}

	// 上游错误族。
	for _, tc := range []struct {
		err  error
		want string
	}{{llm.ErrTimeout, codeLLMTimeout}, {llm.ErrStream, codeLLMStream}, {llm.ErrInvalidConfig, codeLLMUpstream}} {
		code, _, _ := r.classifyLLMError(context.Background(), context.Background(), tc.err)
		if code != tc.want {
			t.Errorf("classify(%v) = %s, want %s", tc.err, code, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Prepare 的失败分支
// ---------------------------------------------------------------------------

func TestPrepareFailureBranches(t *testing.T) {
	ctx := context.Background()

	// 空 conversation_id。
	st := seed(false)
	r, _ := newRunnerForTest(t, st, &fakeLLM{}, nil, RunnerConfig{})
	if _, err := r.Prepare(ctx, "", "", ""); err == nil || !strings.Contains(err.Error(), "conversation_id is required") {
		t.Fatalf("empty conversation err = %v", err)
	}

	// 会话读取失败（非 NotFound）。
	est := newErrStore(seed(true))
	est.convGetErr = errors.New("db down")
	r2 := newErrRunnerForTest(t, est, &fakeLLM{}, RunnerConfig{})
	if _, err := r2.Prepare(ctx, "c1", "", ""); err == nil || !strings.Contains(err.Error(), "failed to load conversation") {
		t.Fatalf("conv store err = %v", err)
	}

	// 会话绑定的 provider 不存在。
	st2 := seed(true) // c1 未绑定
	pid := "missing"
	c := st2.conversations["c1"]
	c.ProviderID = &pid
	st2.conversations["c1"] = c
	r3, _ := newRunnerForTest(t, st2, &fakeLLM{}, nil, RunnerConfig{})
	if _, err := r3.Prepare(ctx, "c1", "", ""); err == nil || !strings.Contains(err.Error(), "provider not found") {
		t.Fatalf("bound provider missing err = %v", err)
	}

	// 会话绑定 provider 但 model 为空。
	st3 := seed(true)
	c2 := st3.conversations["c1"]
	c2.ProviderID = &pid
	c2.Model = ""
	st3.conversations["c1"] = c2
	st3.providers["missing"] = st3.providers["p1"]
	r4, _ := newRunnerForTest(t, st3, &fakeLLM{}, nil, RunnerConfig{})
	if _, err := r4.Prepare(ctx, "c1", "", ""); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("model required err = %v", err)
	}

	// provider 读取失败（非 NotFound）。
	est2 := newErrStore(seed(true))
	est2.provGetErr = errors.New("provider db down")
	r5 := newErrRunnerForTest(t, est2, &fakeLLM{}, RunnerConfig{})
	if _, err := r5.Prepare(ctx, "c1", "p1", ""); err == nil || !strings.Contains(err.Error(), "failed to load provider") {
		t.Fatalf("provider store err = %v", err)
	}

	// 未指定 provider 时列举失败。
	est3 := newErrStore(seed(true))
	est3.provListErr = errors.New("list down")
	r6 := newErrRunnerForTest(t, est3, &fakeLLM{}, RunnerConfig{})
	if _, err := r6.Prepare(ctx, "c1", "", ""); err == nil || !strings.Contains(err.Error(), "failed to list providers") {
		t.Fatalf("provider list err = %v", err)
	}

	// 默认 provider 没有 DefaultModel。
	st4 := seed(true)
	p := st4.providers["p1"]
	p.DefaultModel = ""
	st4.providers["p1"] = p
	r7, _ := newRunnerForTest(t, st4, &fakeLLM{}, nil, RunnerConfig{})
	if _, err := r7.Prepare(ctx, "c1", "", ""); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("default model missing err = %v", err)
	}

	// 会话已绑定时请求其他 provider → MODEL_LOCKED。
	st5 := seed(false)
	r8, _ := newRunnerForTest(t, st5, &fakeLLM{}, nil, RunnerConfig{})
	if _, err := r8.Prepare(ctx, "c1", "other", ""); err == nil || !strings.Contains(err.Error(), "bound to another provider") {
		t.Fatalf("model locked err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Run 的失败与边界分支
// ---------------------------------------------------------------------------

func TestRunStoreFailureBranches(t *testing.T) {
	ctx := context.Background()

	// 会话读取失败 → INTERNAL。
	est := newErrStore(seed(true))
	est.convGetErr = errors.New("conv db down")
	r := newErrRunnerForTest(t, est, &fakeLLM{steps: []scriptStep{{}}}, RunnerConfig{})
	err := r.Run(ctx, runInput(), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "failed to load conversation") {
		t.Fatalf("run conv err = %v", err)
	}

	// 用户消息写入失败 → INTERNAL。
	est2 := newErrStore(seed(false))
	est2.msgCreateErr = errors.New("message db down")
	r2 := newErrRunnerForTest(t, est2, &fakeLLM{steps: []scriptStep{{}}}, RunnerConfig{})
	err = r2.Run(ctx, runInput(), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "failed to store message") {
		t.Fatalf("run message err = %v", err)
	}
}

// FinishReason=length 且无文本 → LLM_TRUNCATED；有文本 → finish=length 但正常完成。
func TestRunLengthFinishReasons(t *testing.T) {
	ctx := context.Background()

	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{{
		events: []llm.StreamEvent{
			{Type: llm.EventDone, FinishReason: "length", Usage: &llm.Usage{TotalTokens: 4}},
		},
	}}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	var got []Event
	err := r.Run(ctx, runInput(), func(ev Event) error { got = append(got, ev); return nil })
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("empty length err = %v", err)
	}
	if idx := indexOf(eventTypes(got), string(EventError)); idx < 0 {
		t.Fatal("error event must be emitted")
	}

	st2 := seed(false)
	client2 := &fakeLLM{steps: []scriptStep{{
		events: []llm.StreamEvent{
			{Type: llm.EventToken, Delta: "partial"},
			{Type: llm.EventDone, FinishReason: "length", Usage: &llm.Usage{TotalTokens: 4}},
		},
	}}}
	r2, _ := newRunnerForTest(t, st2, client2, nil, RunnerConfig{})
	var got2 []Event
	if err := r2.Run(ctx, runInput(), func(ev Event) error { got2 = append(got2, ev); return nil }); err != nil {
		t.Fatalf("textual length run: %v", err)
	}
	idx := indexOf(eventTypes(got2), string(EventDone))
	if idx < 0 || got2[idx].Done.FinishReason != "length" {
		t.Fatalf("done event = %+v", got2)
	}
	// 助手消息落库。
	found := false
	for _, m := range st2.messages {
		if m.Role == store.RoleAssistant && m.Content == "partial" {
			found = true
		}
	}
	if !found {
		t.Fatal("assistant message must be persisted")
	}
}

// Provider 未给 call ID 时本地生成。
func TestRunGeneratesToolCallIDs(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: []llm.StreamEvent{
			{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{Name: "list_servers"}}, // 无 ID
			{Type: llm.EventDone, FinishReason: "tool_calls", Usage: &llm.Usage{}},
		}},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	if err := r.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(st.toolCalls) != 1 || !strings.HasPrefix(st.toolCalls[0].ProviderCallID, "call_") {
		t.Fatalf("toolCalls = %+v", st.toolCalls)
	}
}

// 工具列表读取失败时以工具错误返回给模型，运行继续。
func TestRunListServersDatabasesStoreErrors(t *testing.T) {
	est := newErrStore(seed(true))
	est.serversListErr = errors.New("servers db down")
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents("list_servers", `{}`)},
		{events: finalEvents("after-server-err", llm.Usage{})},
	}}
	r := newErrRunnerForTest(t, est, client, RunnerConfig{})
	err := r.Run(context.Background(), runInput(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("run must survive list_servers failure: %v", err)
	}
	if len(est.toolCalls) != 1 || est.toolCalls[0].Status != store.ToolFailed {
		t.Fatalf("toolCalls = %+v", est.toolCalls)
	}

	est2 := newErrStore(seed(true))
	est2.dbListErr = errors.New("databases db down")
	client2 := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents("list_databases", `{}`)},
		{events: finalEvents("after-db-err", llm.Usage{})},
	}}
	r2 := newErrRunnerForTest(t, est2, client2, RunnerConfig{})
	if err := r2.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("run must survive list_databases failure: %v", err)
	}
	if len(est2.toolCalls) != 1 || est2.toolCalls[0].Status != store.ToolFailed {
		t.Fatalf("toolCalls = %+v", est2.toolCalls)
	}
}

// ---------------------------------------------------------------------------
// serverTarget / recordHostKey / 审计
// ---------------------------------------------------------------------------

func TestServerTargetCredentialFailures(t *testing.T) {
	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}

	// 无 cipher：密文存在时 decrypt 报错，通过 runSSHExec 表现为凭据不可用。
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents("ssh_exec", `{"server_id":"s1","command":"uptime"}`)},
		{events: finalEvents("after-cred-err", llm.Usage{})},
	}}
	r := NewRunner(st, nil, &fakeSSH{}, nil, nil, audit.NewWriter(st, testLogger(), time.Now), nil,
		RunnerConfig{}, time.Now, testLogger())
	r.SetLLMClient(client)
	if err := r.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed ||
		!strings.Contains(string(st.toolCalls[0].ErrorCode), "") {
		t.Fatalf("toolCalls = %+v", st.toolCalls)
	}

	// 非法 auth_method 直接拒绝。
	r2 := NewRunner(st, nil, &fakeSSH{}, nil, nil, nil, cipher, RunnerConfig{}, time.Now, testLogger())
	srv := st.servers["s1"]
	srv.AuthMethod = store.AuthMethod("weird")
	if _, err := r2.serverTarget(srv); err == nil || !strings.Contains(err.Error(), "unsupported auth method") {
		t.Fatalf("bad auth method err = %v", err)
	}

	// 损坏密文解密失败。
	srv.AuthMethod = store.AuthMethodPassword
	srv.PasswordEncrypted = "bogus"
	if _, err := r2.serverTarget(srv); err == nil {
		t.Fatal("bogus ciphertext must fail")
	}
	srv.AuthMethod = store.AuthMethodPrivateKey
	srv.PrivateKeyEncrypted = "bogus"
	if _, err := r2.serverTarget(srv); err == nil {
		t.Fatal("bogus key must fail")
	}
}

// recordHostKey 落库失败只记日志。
func TestRecordHostKeyStoreFailure(t *testing.T) {
	est := newErrStore(seed(true))
	est.hostKeyErr = errors.New("hostkey db down")
	r := NewRunner(est, nil, nil, nil, nil, nil, nil, RunnerConfig{}, time.Now, testLogger())
	r.recordHostKey(context.Background(), est.servers["s1"], true, "fp", "key")
}

// 审计 writer 关闭后 auditEvent 只记 Warn。
func TestAuditEventEnqueueFailureLogs(t *testing.T) {
	st := seed(false)
	w := audit.NewWriter(st, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(st, nil, nil, nil, nil, w, nil, RunnerConfig{}, time.Now, testLogger())
	r.auditEvent(context.Background(), "x.y", "model", "conversation:c1", "conversation", "c1", nil, nil, nil)
}

// ---------------------------------------------------------------------------
// tools.go 辅助
// ---------------------------------------------------------------------------

func TestDecodeStrictArgsEdges(t *testing.T) {
	var dst struct {
		A string `json:"a"`
	}
	// 空 raw 视作 {}。
	if err := decodeStrictArgs(nil, &dst); err != nil || dst.A != "" {
		t.Fatalf("nil args err = %v", err)
	}
	if err := decodeStrictArgs(json.RawMessage("  "), &dst); err != nil {
		t.Fatalf("blank args err = %v", err)
	}
	// 尾随数据拒绝。
	if err := decodeStrictArgs(json.RawMessage(`{"a":"1"}{"a":"2"}`), &dst); err == nil {
		t.Fatal("trailing object must be rejected")
	}
	// 未知字段拒绝。
	if err := decodeStrictArgs(json.RawMessage(`{"a":"1","b":2}`), &dst); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	// 正常路径。
	if err := decodeStrictArgs(json.RawMessage(`{"a":"x"}`), &dst); err != nil || dst.A != "x" {
		t.Fatalf("ok path = %q, %v", dst.A, err)
	}
}

func TestTruncateResultEdges(t *testing.T) {
	raw := json.RawMessage(`{"k":"` + strings.Repeat("v", 64) + `"}`)
	// max<=0：原样返回。
	if got := truncateResult(raw, 0); string(got) != string(raw) {
		t.Fatalf("max=0 = %s", got)
	}
	// 小于上限：原样返回。
	if got := truncateResult(raw, len(raw)+10); string(got) != string(raw) {
		t.Fatalf("under limit = %s", got)
	}
	// 超限：包装 truncated。
	got := truncateResult(raw, 16)
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("truncated payload not json: %v (%s)", err, got)
	}
	if body["truncated"] != true {
		t.Fatalf("body = %v", body)
	}
	if int(body["original_bytes"].(float64)) != len(raw) {
		t.Fatalf("original_bytes = %v", body["original_bytes"])
	}
}

func TestErrorResultShape(t *testing.T) {
	got := errorResult("TOOL_NOT_FOUND", "no such tool")
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "TOOL_NOT_FOUND" || body.Error.Message != "no such tool" {
		t.Fatalf("body = %+v", body)
	}
}
