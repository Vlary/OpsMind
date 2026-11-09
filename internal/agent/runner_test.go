package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/llm"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 假 store
// ---------------------------------------------------------------------------

type testStore struct {
	store.Store
	mu            sync.Mutex
	providers     map[string]store.Provider
	conversations map[string]store.Conversation
	messages      []store.Message
	toolCalls     []store.ToolCall
	servers       map[string]store.Server
	databases     map[string]store.Database
	auditLogs     []store.CreateAuditParams
}

func newTestStore() *testStore {
	return &testStore{
		providers:     map[string]store.Provider{},
		conversations: map[string]store.Conversation{},
		servers:       map[string]store.Server{},
		databases:     map[string]store.Database{},
	}
}

func (s *testStore) Ping(context.Context) error { return nil }
func (s *testStore) Close()                     {}
func (s *testStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(s)
}
func (s *testStore) Providers() store.ProviderStore         { return &providerRepo{s: s} }
func (s *testStore) Conversations() store.ConversationStore { return &conversationRepo{s: s} }
func (s *testStore) Messages() store.MessageStore           { return &messageRepo{s: s} }
func (s *testStore) ToolCalls() store.ToolCallStore         { return &toolCallRepo{s: s} }
func (s *testStore) Servers() store.ServerStore             { return &serverRepo{s: s} }
func (s *testStore) Databases() store.DatabaseStore         { return &databaseRepo{s: s} }
func (s *testStore) Approvals() store.ApprovalStore         { return nil }
func (s *testStore) AuditLogs() store.AuditStore            { return s }
func (s *testStore) Settings() store.SettingStore           { return nil }
func (s *testStore) Users() store.UserStore                 { return nil }
func (s *testStore) Sessions() store.SessionStore           { return nil }

func (s *testStore) Append(_ context.Context, p store.CreateAuditParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditLogs = append(s.auditLogs, p)
	return nil
}

func (s *testStore) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	return nil, false, nil
}

func (s *testStore) LastHash(context.Context) (string, error) { return "", nil }

type providerRepo struct {
	store.ProviderStore
	s *testStore
}

func (r *providerRepo) Get(_ context.Context, id string) (store.Provider, error) {
	p, ok := r.s.providers[id]
	if !ok {
		return store.Provider{}, store.ErrNotFound
	}
	return p, nil
}

func (r *providerRepo) List(context.Context) ([]store.Provider, error) {
	out := make([]store.Provider, 0, len(r.s.providers))
	for _, p := range r.s.providers {
		out = append(out, p)
	}
	return out, nil
}

type conversationRepo struct {
	store.ConversationStore
	s *testStore
}

func (r *conversationRepo) Get(_ context.Context, id string) (store.Conversation, error) {
	c, ok := r.s.conversations[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	return c, nil
}

func (r *conversationRepo) BindModel(_ context.Context, id, providerID, model string, _ time.Time) (store.Conversation, error) {
	c, ok := r.s.conversations[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	c.ProviderID = &providerID
	c.Model = model
	r.s.conversations[id] = c
	return c, nil
}

func (r *conversationRepo) Touch(context.Context, string, time.Time) error { return nil }

type messageRepo struct {
	store.MessageStore
	s *testStore
}

func (r *messageRepo) Create(_ context.Context, p store.CreateMessageParams) (store.Message, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	m := store.Message{
		ID:                  fmt.Sprintf("msg-%d", len(r.s.messages)+1),
		Seq:                 int64(len(r.s.messages) + 1),
		ConversationID:      p.ConversationID,
		TurnID:              p.TurnID,
		Role:                p.Role,
		Content:             p.Content,
		Reasoning:           p.Reasoning,
		ReasoningDurationMS: p.ReasoningDurationMS,
		Status:              p.Status,
		ErrorCode:           p.ErrorCode,
		Model:               p.Model,
		PromptTokens:        p.PromptTokens,
		CompletionTokens:    p.CompletionTokens,
		CreatedAt:           p.CreatedAt,
	}
	r.s.messages = append(r.s.messages, m)
	return m, nil
}

func (r *messageRepo) ListPage(_ context.Context, conversationID string, _ int64, _ int) ([]store.Message, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]store.Message, 0, len(r.s.messages))
	for _, m := range r.s.messages {
		if m.ConversationID == conversationID {
			out = append(out, m)
		}
	}
	return out, false, nil
}

type toolCallRepo struct {
	store.ToolCallStore
	s *testStore
}

func (r *toolCallRepo) Create(_ context.Context, p store.CreateToolCallParams) (store.ToolCall, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	tc := store.ToolCall{
		ID:             fmt.Sprintf("tc-%d", len(r.s.toolCalls)+1),
		Seq:            int64(len(r.s.toolCalls) + 1),
		ConversationID: p.ConversationID,
		TurnID:         p.TurnID,
		ProviderCallID: p.ProviderCallID,
		Name:           p.Name,
		Arguments:      p.Arguments,
		RiskLevel:      p.RiskLevel,
		Status:         p.Status,
		// 六期打磨 sprint 1：门限快照随创建行落库（迁移 00010）。
		ApprovalMode: string(p.ApprovalMode),
		StartedAt:    p.StartedAt,
	}
	r.s.toolCalls = append(r.s.toolCalls, tc)
	return tc, nil
}

func (r *toolCallRepo) SetStatus(_ context.Context, id string, status store.ToolCallStatus, errorCode, errorMessage string) (store.ToolCall, error) {
	return r.update(id, func(tc *store.ToolCall) {
		tc.Status = status
		tc.ErrorCode = errorCode
		tc.ErrorMessage = errorMessage
	})
}

func (r *toolCallRepo) Finish(_ context.Context, p store.FinishToolCallParams) (store.ToolCall, error) {
	return r.update(p.ID, func(tc *store.ToolCall) {
		tc.Status = p.Status
		tc.Result = p.Result
		tc.ErrorCode = p.ErrorCode
		tc.ErrorMessage = p.ErrorMessage
		tc.FinishedAt = &p.FinishedAt
	})
}

func (r *toolCallRepo) update(id string, fn func(*store.ToolCall)) (store.ToolCall, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for i := range r.s.toolCalls {
		if r.s.toolCalls[i].ID == id {
			fn(&r.s.toolCalls[i])
			return r.s.toolCalls[i], nil
		}
	}
	return store.ToolCall{}, store.ErrNotFound
}

func (r *toolCallRepo) ListPage(_ context.Context, conversationID string, _ int64, _ int) ([]store.ToolCall, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]store.ToolCall, 0, len(r.s.toolCalls))
	for _, tc := range r.s.toolCalls {
		if tc.ConversationID == conversationID {
			out = append(out, tc)
		}
	}
	return out, false, nil
}

func (r *toolCallRepo) ByID(_ context.Context, id string) (store.ToolCall, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, tc := range r.s.toolCalls {
		if tc.ID == id {
			return tc, nil
		}
	}
	return store.ToolCall{}, store.ErrNotFound
}

type serverRepo struct {
	store.ServerStore
	s *testStore
}

func (r *serverRepo) List(context.Context) ([]store.Server, error) {
	out := make([]store.Server, 0, len(r.s.servers))
	for _, srv := range r.s.servers {
		out = append(out, srv)
	}
	return out, nil
}

func (r *serverRepo) Get(_ context.Context, id string) (store.Server, error) {
	srv, ok := r.s.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	return srv, nil
}

func (r *serverRepo) UpdateHostKey(_ context.Context, id, fingerprint, publicKey string, at time.Time) (store.Server, error) {
	srv, ok := r.s.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	srv.HostKeyFingerprint = fingerprint
	srv.HostKeyPublic = publicKey
	srv.HostKeyVerifiedAt = &at
	r.s.servers[id] = srv
	return srv, nil
}

// UpdateContainerRuntime 写回容器运行时探测缓存（四期 C1，契约 2.9 约定 1）。
func (r *serverRepo) UpdateContainerRuntime(_ context.Context, id string, runtime store.ContainerRuntime, _ time.Time) (store.Server, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	srv, ok := r.s.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	if srv.DeviceKind == store.DeviceKindNetworkDevice && runtime != store.ContainerRuntimeNone {
		return store.Server{}, store.ErrInvalid
	}
	srv.ContainerRuntime = runtime
	r.s.servers[id] = srv
	return srv, nil
}

type databaseRepo struct {
	store.DatabaseStore
	s *testStore
}

func (r *databaseRepo) List(context.Context) ([]store.Database, error) {
	out := make([]store.Database, 0, len(r.s.databases))
	for _, d := range r.s.databases {
		out = append(out, d)
	}
	return out, nil
}

func (r *databaseRepo) Get(_ context.Context, id string) (store.Database, error) {
	d, ok := r.s.databases[id]
	if !ok {
		return store.Database{}, store.ErrNotFound
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// 假 SSH / 审批 / LLM
// ---------------------------------------------------------------------------

type fakeSSH struct {
	sshx.Executor
	calls   []sshx.RunRequest
	output  string
	execErr error
	// onExec 测试钩子：Execute 时回调（用于推进假时钟，模拟真实执行耗时）。
	onExec func()
}

func (f *fakeSSH) Execute(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.calls = append(f.calls, req)
	if f.onExec != nil {
		f.onExec()
	}
	if f.execErr != nil {
		return sshx.RunResult{}, f.execErr
	}
	output := f.output
	if output == "" {
		output = "ok"
	}
	return sshx.RunResult{ExitCode: 0, Combined: output, DurationMS: 3}, nil
}

type fakeApprovals struct {
	waitStatus store.ApprovalStatus
	created    []approvals.CreateRequest
	cancelled  []string
	// waitEdited 模拟 decide 批准时的用户改写命令（六期 BE-P）。
	waitEdited string
	// onWait 测试钩子：Wait 时回调（用于推进假时钟，模拟人工审批等待）。
	onWait func()
}

func (f *fakeApprovals) Create(_ context.Context, req approvals.CreateRequest) (store.Approval, error) {
	f.created = append(f.created, req)
	return store.Approval{
		ID:             fmt.Sprintf("ap-%d", len(f.created)),
		ConversationID: req.ConversationID,
		ToolCallID:     req.ToolCallID,
		ServerID:       req.ServerID,
		ServerName:     req.ServerName,
		Command:        req.Command,
		RiskLevel:      req.RiskLevel,
		Reason:         req.Reason,
		Status:         store.ApprovalPending,
	}, nil
}

func (f *fakeApprovals) Wait(_ context.Context, approvalID string) (approvals.Decision, error) {
	if f.onWait != nil {
		f.onWait()
	}
	status := f.waitStatus
	if status == "" {
		status = store.ApprovalExpired
	}
	now := time.Now()
	return approvals.Decision{ApprovalID: approvalID, Status: status, DecidedAt: &now, EditedCommand: f.waitEdited}, nil
}

func (f *fakeApprovals) Cancel(_ context.Context, approvalID, reason, _ string) (store.Approval, error) {
	f.cancelled = append(f.cancelled, approvalID+":"+reason)
	return store.Approval{ID: approvalID, Status: store.ApprovalExpired}, nil
}

type scriptStep struct {
	events []llm.StreamEvent
	err    error
}

type fakeLLM struct {
	steps []scriptStep
	calls []llm.Request
}

func (f *fakeLLM) Stream(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	idx := len(f.calls)
	f.calls = append(f.calls, req)
	if idx >= len(f.steps) {
		return fmt.Errorf("unexpected llm call %d", idx+1)
	}
	for _, ev := range f.steps[idx].events {
		if err := emit(ev); err != nil {
			return err
		}
	}
	return f.steps[idx].err
}

func (f *fakeLLM) ListModels(context.Context) ([]string, error) { return []string{"m1"}, nil }
func (f *fakeLLM) Test(context.Context) error                   { return nil }

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func toolCallEvents(name string, args string) []llm.StreamEvent {
	return []llm.StreamEvent{
		{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "provider-" + name, Name: name, Arguments: json.RawMessage(args)}},
		{Type: llm.EventDone, FinishReason: "tool_calls", Usage: &llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
	}
}

func finalEvents(text string, usage llm.Usage) []llm.StreamEvent {
	events := []llm.StreamEvent{}
	if text != "" {
		events = append(events, llm.StreamEvent{Type: llm.EventToken, Delta: text})
	}
	events = append(events, llm.StreamEvent{Type: llm.EventDone, FinishReason: "stop", Usage: &usage})
	return events
}

func newRunnerForTest(t *testing.T, st *testStore, client llm.Client, ap ApprovalService, cfg RunnerConfig) (*Runner, *fakeSSH) {
	t.Helper()
	ssh := &fakeSSH{}
	r := NewRunner(st, nil, ssh, nil, nil, audit.NewWriter(st, testLogger(), time.Now), nil, cfg, time.Now, testLogger())
	r.SetLLMClient(client)
	if ap != nil {
		r.SetApprovalService(ap)
	}
	return r, ssh
}

func seed(unbound bool) *testStore {
	st := newTestStore()
	st.providers["p1"] = store.Provider{
		ID: "p1", Name: "test", Preset: store.PresetCustom,
		Kind: store.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1",
		DefaultModel: "m1", Models: []string{"m1"},
	}
	st.conversations["c1"] = store.Conversation{ID: "c1", Title: "t"}
	if !unbound {
		pid := "p1"
		c := st.conversations["c1"]
		c.ProviderID = &pid
		c.Model = "m1"
		st.conversations["c1"] = c
	}
	st.servers["s1"] = store.Server{
		ID: "s1", Name: "web-1", Host: "127.0.0.1", Port: 22,
		Username: "root", AuthMethod: store.AuthMethodPassword,
		AutoExecEnabled: false, Tags: []string{"prod"},
	}
	st.databases["d1"] = store.Database{
		ID: "d1", Name: "orders", Engine: store.EnginePostgres,
		Host: "db", Port: 5432, DatabaseName: "orders", Username: "ro",
	}
	return st
}

func eventTypes(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, string(ev.Type))
	}
	return out
}

func indexOf(types []string, want string) int {
	for i, t := range types {
		if t == want {
			return i
		}
	}
	return -1
}

func runInput() RunInput {
	return RunInput{
		UserID: "u1", ConversationID: "c1", ProviderID: "p1", Model: "m1",
		Content: "check servers", RequestID: "req-1",
	}
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestRunToolCallThenFinalAnswer(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolListServers, `{}`)},
		{events: finalEvents("final answer", llm.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})

	var events []Event
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	types := eventTypes(events)
	callIdx, resultIdx := indexOf(types, "tool_call"), indexOf(types, "tool_result")
	tokenIdx, doneIdx := indexOf(types, "token"), indexOf(types, "done")
	if callIdx < 0 || resultIdx < 0 || tokenIdx < 0 || doneIdx < 0 {
		t.Fatalf("events = %v", types)
	}
	if !(callIdx < resultIdx && resultIdx < tokenIdx && tokenIdx < doneIdx) {
		t.Fatalf("event order = %v", types)
	}
	if indexOf(types, "error") >= 0 {
		t.Fatalf("unexpected error event: %v", types)
	}

	if len(st.messages) != 2 {
		t.Fatalf("messages = %d", len(st.messages))
	}
	assistant := st.messages[1]
	if assistant.Role != store.RoleAssistant || assistant.Status != store.MessageComplete {
		t.Errorf("assistant = %+v", assistant)
	}
	if assistant.Content != "final answer" {
		t.Errorf("assistant content = %q", assistant.Content)
	}
	if assistant.PromptTokens != 8 || assistant.CompletionTokens != 3 {
		t.Errorf("usage = %d/%d", assistant.PromptTokens, assistant.CompletionTokens)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolSucceeded {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
	if !strings.Contains(string(st.toolCalls[0].Result), "web-1") {
		t.Errorf("tool result = %s", st.toolCalls[0].Result)
	}

	if len(client.calls) != 2 {
		t.Fatalf("llm calls = %d", len(client.calls))
	}
	second := client.calls[1].Messages
	if len(second) == 0 || second[0].Role != llm.RoleSystem {
		t.Fatalf("system prompt missing: %+v", second)
	}
	var sawAssistantCalls, sawToolResult bool
	for _, m := range second {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) == 1 && m.ToolCalls[0].Name == ToolListServers {
			sawAssistantCalls = true
		}
		if m.Role == llm.RoleTool && m.ToolCallID == "provider-"+ToolListServers && strings.Contains(m.Content, "servers") {
			sawToolResult = true
		}
	}
	if !sawAssistantCalls || !sawToolResult {
		t.Errorf("tool round-trip missing: assistant=%v tool=%v", sawAssistantCalls, sawToolResult)
	}
	if !strings.Contains(second[0].Content, "UNTRUSTED DATA") {
		t.Errorf("system prompt does not mark tool output untrusted")
	}
}

func TestRunApprovalDeniedIsFedBackToModel(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("command was not executed", llm.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalDenied}
	r, ssh := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	var events []Event
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	types := eventTypes(events)
	required, resolved := indexOf(types, "approval_required"), indexOf(types, "approval_resolved")
	result := indexOf(types, "tool_result")
	if required < 0 || resolved < 0 || result < 0 || !(required < resolved && resolved < result) {
		t.Fatalf("event order = %v", types)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("command executed despite denial: %+v", ssh.calls)
	}
	if len(ap.created) != 1 || ap.created[0].Command != "ls -la" {
		t.Fatalf("approvals = %+v", ap.created)
	}
	if len(st.toolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(st.toolCalls))
	}
	tc := st.toolCalls[0]
	if tc.Status != store.ToolDenied || tc.ErrorCode != "APPROVAL_DENIED" {
		t.Errorf("tool call = %+v", tc)
	}
	var denied *Event
	for i := range events {
		if events[i].Type == EventToolResult {
			denied = &events[i]
		}
	}
	if denied == nil || denied.ToolResult.OK || denied.ToolResult.Error == nil || denied.ToolResult.Error.Code != "APPROVAL_DENIED" {
		t.Fatalf("tool result = %+v", denied)
	}
	if !strings.Contains(string(st.messages[len(st.messages)-1].Content), "command was not executed") {
		t.Errorf("final assistant content = %q", st.messages[len(st.messages)-1].Content)
	}
}

func TestRunStopsAtMaxIterations(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolListServers, `{}`)},
		{events: toolCallEvents(ToolListServers, `{}`)},
		{events: finalEvents("should not be called", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{MaxIterations: 2})

	var events []Event
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var re *RunError
	if !errors.As(err, &re) || re.Code != "AGENT_MAX_ITERATIONS" {
		t.Fatalf("error = %v", err)
	}
	if len(client.calls) != 2 {
		t.Errorf("llm calls = %d, want 2", len(client.calls))
	}
	if len(st.toolCalls) != 1 {
		t.Errorf("tool calls = %d, want 1 (second round must not execute)", len(st.toolCalls))
	}
	types := eventTypes(events)
	if indexOf(types, "error") < 0 {
		t.Fatalf("missing error event: %v", types)
	}
	if indexOf(types, "done") >= 0 {
		t.Fatalf("done must not follow error: %v", types)
	}
	last := st.messages[len(st.messages)-1]
	if last.Status != store.MessageError || last.ErrorCode != "AGENT_MAX_ITERATIONS" {
		t.Errorf("assistant error message = %+v", last)
	}
}

func TestRunUnknownToolReturnsToolNotFound(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents("drop_database", `{}`)},
		{events: finalEvents("I cannot do that", llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})

	var events []Event
	if err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var result *Event
	for i := range events {
		if events[i].Type == EventToolResult {
			result = &events[i]
		}
	}
	if result == nil || result.ToolResult.OK {
		t.Fatalf("tool result = %+v", result)
	}
	if result.ToolResult.Error == nil || result.ToolResult.Error.Code != "TOOL_NOT_FOUND" {
		t.Fatalf("error = %+v", result.ToolResult.Error)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed {
		t.Errorf("tool calls = %+v", st.toolCalls)
	}
	// 失败底层消息必须落库：刷新/重进会话后工具卡与 SSE 实时信息一致。
	if st.toolCalls[0].ErrorCode != "TOOL_NOT_FOUND" || st.toolCalls[0].ErrorMessage != "unknown tool" {
		t.Errorf("error code/message = %q / %q", st.toolCalls[0].ErrorCode, st.toolCalls[0].ErrorMessage)
	}
	if len(st.messages) != 2 || st.messages[1].Status != store.MessageComplete {
		t.Fatalf("messages = %+v", st.messages)
	}
}

func TestRunInvalidToolArguments(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls","unexpected":1}`)},
		{events: finalEvents("bad args", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	var events []Event
	if err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatal("ssh must not execute with invalid args")
	}
	var result *Event
	for i := range events {
		if events[i].Type == EventToolResult {
			result = &events[i]
		}
	}
	if result == nil || result.ToolResult.Error == nil || result.ToolResult.Error.Code != "TOOL_ARGS_INVALID" {
		t.Fatalf("result = %+v", result)
	}
	// 参数不合契约（未知字段）但 command 仍可提取时，risk_level 必须按真实
	// 命令分类（"ls" → safe），而不是无脑落列默认值。
	if len(st.toolCalls) != 1 {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
	if got := st.toolCalls[0].RiskLevel; got != string(risk.LevelSafe) {
		t.Fatalf("risk_level = %q, want %q (classified from extractable command)", got, risk.LevelSafe)
	}
}

// 参数解析失败且 command 是危险命令时，risk_level 不得记成默认 safe：
// 审计与前端展示的风险级别必须与命令实际风险一致。
func TestRunInvalidToolArgumentsClassifiesCommand(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"rm -rf / --no-preserve-root","timeout_seconds":"ten"}`)},
		{events: finalEvents("bad args", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	var events []Event
	if err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatal("ssh must not execute with invalid args")
	}
	var result *Event
	for i := range events {
		if events[i].Type == EventToolResult {
			result = &events[i]
		}
	}
	if result == nil || result.ToolResult.Error == nil || result.ToolResult.Error.Code != "TOOL_ARGS_INVALID" {
		t.Fatalf("result = %+v", result)
	}
	if len(st.toolCalls) != 1 {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
	if got := st.toolCalls[0].RiskLevel; got != string(risk.LevelForbidden) {
		t.Fatalf("risk_level = %q, want %q (command must be classified even when args are invalid)", got, risk.LevelForbidden)
	}
}

// 参数解析失败且 command 完全无法提取时，risk_level 只能保持列默认 safe；
// 该调用必然 failed，不产生审批或执行。
func TestRunInvalidToolArgumentsWithoutCommand(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":42}`)},
		{events: finalEvents("bad args", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	var events []Event
	if err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var result *Event
	for i := range events {
		if events[i].Type == EventToolResult {
			result = &events[i]
		}
	}
	if result == nil || result.ToolResult.Error == nil || result.ToolResult.Error.Code != "TOOL_ARGS_INVALID" {
		t.Fatalf("result = %+v", result)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
	if got := st.toolCalls[0].RiskLevel; got != string(risk.LevelSafe) {
		t.Fatalf("risk_level = %q, want column default safe when command is unextractable", got)
	}
}

func TestPrepareRules(t *testing.T) {
	st := seed(true)
	r, _ := newRunnerForTest(t, st, &fakeLLM{}, nil, RunnerConfig{})

	info, err := r.Prepare(context.Background(), "c1", "", "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if info.Provider.ID != "p1" || info.Model != "m1" {
		t.Errorf("info = %+v", info)
	}

	locked := "p1"
	c := st.conversations["c1"]
	c.ProviderID = &locked
	c.Model = "m1"
	st.conversations["c1"] = c
	if _, err := r.Prepare(context.Background(), "c1", "p2", ""); err == nil {
		t.Fatal("expected model locked error")
	} else {
		var re *RunError
		if !errors.As(err, &re) || re.Code != "CONVERSATION_MODEL_LOCKED" {
			t.Fatalf("error = %v", err)
		}
	}
	if _, err := r.Prepare(context.Background(), "c1", "p1", "other-model"); err == nil {
		t.Fatal("expected model locked error for model change")
	}

	if _, err := r.Prepare(context.Background(), "missing", "", ""); err == nil {
		t.Fatal("expected conversation not found")
	} else {
		var re *RunError
		if !errors.As(err, &re) || re.Code != "CONVERSATION_NOT_FOUND" {
			t.Fatalf("error = %v", err)
		}
	}

	empty := newTestStore()
	r2, _ := newRunnerForTest(t, empty, &fakeLLM{}, nil, RunnerConfig{})
	empty.conversations["c1"] = store.Conversation{ID: "c1", Title: "t"}
	if _, err := r2.Prepare(context.Background(), "c1", "", ""); err == nil {
		t.Fatal("expected no provider error")
	} else {
		var re *RunError
		if !errors.As(err, &re) || re.Code != "NO_PROVIDER_CONFIGURED" {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestToolDefinitionsShape(t *testing.T) {
	defs := ToolDefinitions()
	if len(defs) != 18 { // 五期：M1 追加 3（get_metrics/list_alerts/audit_search）、M2 追加 3（run_diagnosis/run_inspection/get_inspection_report）、M3 追加 1（batch_execute）——契约 7.1 全量 18。
		t.Fatalf("tools = %d", len(defs))
	}
	want := map[string]bool{
		ToolListServers: false, ToolServerFacts: false, ToolSSHExec: false,
		ToolListDatabases: false, ToolDBQuery: false, ToolAskUser: false,
		ToolContainerList: false, ToolContainerInspect: false, ToolContainerAction: false,
		ToolContainerLogs: false, ToolImageList: false,
		ToolGetMetrics: false, ToolListAlerts: false, ToolAuditSearch: false,
		ToolBatchExecute: false,
		ToolRunDiagnosis: false, ToolRunInspection: false, ToolGetInspectionReport: false,
	}
	for _, d := range defs {
		if _, ok := want[d.Name]; !ok {
			t.Errorf("unexpected tool %q", d.Name)
		}
		want[d.Name] = true
		var schema map[string]any
		if err := json.Unmarshal(d.Parameters, &schema); err != nil {
			t.Fatalf("%s schema: %v", d.Name, err)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s schema = %v", d.Name, schema)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestRunCancelsApprovalWhenStreamEmitFails(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		if ev.Type == EventApprovalRequired {
			return errors.New("client gone")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected emit failure")
	}
	if len(ap.created) != 1 {
		t.Fatalf("approvals created = %d, want 1", len(ap.created))
	}
	if len(ap.cancelled) != 1 || ap.cancelled[0] != "ap-1:stream_cancelled" {
		t.Fatalf("approval cancel = %v, want ap-1:stream_cancelled", ap.cancelled)
	}
}

func TestToolMessageContentIsTruncated(t *testing.T) {
	st := seed(false)
	srv := st.servers["s1"]
	srv.AutoExecEnabled = true
	st.servers["s1"] = srv

	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{ToolResultBytes: 1024})
	ssh.output = strings.Repeat("x", 256*1024)

	if err := r.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("llm calls = %d, want 2", len(client.calls))
	}
	var toolMsg string
	for _, m := range client.calls[1].Messages {
		if m.Role == llm.RoleTool {
			toolMsg = m.Content
		}
	}
	if toolMsg == "" {
		t.Fatal("tool message missing from second LLM request")
	}
	if len(toolMsg) > 2048 {
		t.Fatalf("tool message length = %d, want <= 2048", len(toolMsg))
	}
	if !strings.Contains(toolMsg, `"truncated":true`) {
		preview := toolMsg
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Fatalf("tool message is not the truncation marker: %q", preview)
	}
}

// TestAuditEventSurvivesCancelledContext 回归：SSE 客户端断开后 runCtx 已取消，
// 本轮工具审计事件（tool.call/command.executed/…）仍必须入队落库。
// Enqueue 的 select 在「channel 可发送」与「ctx.Done」同时就绪时按 Go 语义
// 伪随机选择；auditEvent 必须用 WithoutCancel 的 ctx 投递，否则约半数事件
// 被静默丢弃。
func TestAuditEventSurvivesCancelledContext(t *testing.T) {
	st := newTestStore()
	writer := audit.NewWriter(st, testLogger(), time.Now)
	writer.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	r := NewRunner(st, nil, &fakeSSH{}, nil, nil, writer, nil,
		RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const rounds = 50
	for i := 0; i < rounds; i++ {
		r.auditEvent(ctx, "tool.call", "model", "conversation:c1", "tool_call",
			fmt.Sprintf("tc-%d", i), nil, nil, nil)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	if err := writer.Flush(flushCtx); err != nil {
		t.Fatalf("flush audit writer: %v", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.auditLogs) != rounds {
		t.Fatalf("persisted audit events = %d, want %d (events silently dropped on cancelled ctx)", len(st.auditLogs), rounds)
	}
	for i, e := range st.auditLogs {
		if e.Action != "tool.call" {
			t.Fatalf("audit[%d].Action = %q, want tool.call", i, e.Action)
		}
	}
}

// TestClassifySSHErrorIncludesCause 第 6 轮打磨：SSH 执行失败的
// message 必须拼接底层原因（连接拒绝、认证失败等），模型与前端
// 工具卡才能区分故障类型；host key 与 timeout 分支维持既有文案。
func TestClassifySSHErrorIncludesCause(t *testing.T) {
	dialErr := errors.New("sshx: dial [127.0.0.1]:1: dial tcp 127.0.0.1:1: connect: connection refused")
	code, msg := classifySSHError(context.Background(), dialErr)
	if code != codeSSHExecFailed {
		t.Fatalf("code = %q, want %q", code, codeSSHExecFailed)
	}
	if !strings.Contains(msg, "ssh command failed") || !strings.Contains(msg, "connection refused") {
		t.Fatalf("message = %q, want underlying dial error surfaced", msg)
	}

	code, msg = classifySSHError(context.Background(), &sshx.HostKeyError{ServerID: "s1"})
	if code != codeSSHHostKeyChanged || msg != "server host key changed" {
		t.Fatalf("host key branch changed: code=%q msg=%q", code, msg)
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	code, msg = classifySSHError(expired, context.DeadlineExceeded)
	if code != codeSSHTimeout || msg != "ssh command timed out" {
		t.Fatalf("timeout branch changed: code=%q msg=%q", code, msg)
	}
}

// TestRunSSHExecFailureSurfacesUnderlyingError 走查场景回归：审批通过后
// SSH 执行失败，tool_result 事件的 error.message 必须包含底层原因，
// 否则前端工具卡只剩无信息量的「ssh command failed」。
func TestRunSSHExecFailureSurfacesUnderlyingError(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("ssh failed as expected", llm.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, ssh := newRunnerForTest(t, st, client, ap, RunnerConfig{})
	ssh.execErr = errors.New("sshx: dial [127.0.0.1]:22: dial tcp 127.0.0.1:22: connect: connection refused")

	var toolResult *ToolResultPayload
	if err := r.Run(context.Background(), runInput(), func(ev Event) error {
		if ev.Type == EventToolResult {
			toolResult = ev.ToolResult
		}
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if toolResult == nil || toolResult.OK || toolResult.Error == nil {
		t.Fatalf("tool result = %+v, want failed with error", toolResult)
	}
	if toolResult.Error.Code != codeSSHExecFailed {
		t.Fatalf("error code = %q, want %q", toolResult.Error.Code, codeSSHExecFailed)
	}
	if !strings.Contains(toolResult.Error.Message, "ssh command failed") {
		t.Errorf("message = %q, want prefix", toolResult.Error.Message)
	}
	if !strings.Contains(toolResult.Error.Message, "connection refused") {
		t.Errorf("message = %q, want underlying dial error surfaced", toolResult.Error.Message)
	}

	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed {
		t.Fatalf("persisted tool calls = %+v", st.toolCalls)
	}
	if st.toolCalls[0].ErrorCode != codeSSHExecFailed {
		t.Errorf("persisted error code = %q, want %q", st.toolCalls[0].ErrorCode, codeSSHExecFailed)
	}
}
