package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/cryptox"
	"aiops/internal/dbproxy"
	"aiops/internal/llm"
	"aiops/internal/sqlguard"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 补充 fake：可配置的 SSH 结果 / dbproxy 执行器 / 会失败的审批服务
// ---------------------------------------------------------------------------

// hostKeySSH 可配置执行结果，用于覆盖主机密钥回写与私钥解密路径。
type hostKeySSH struct {
	sshx.Executor
	result   sshx.RunResult
	execErr  error
	targets  []sshx.Target
	commands []string
}

func (f *hostKeySSH) Execute(_ context.Context, target sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.targets = append(f.targets, target)
	f.commands = append(f.commands, req.Command)
	if f.execErr != nil {
		return sshx.RunResult{}, f.execErr
	}
	if f.result.Combined == "" {
		f.result.Combined = "ok"
	}
	return f.result, nil
}

func (f *hostKeySSH) Test(context.Context, sshx.Target) (sshx.TestResult, error) {
	return sshx.TestResult{LatencyMS: 5, ServerVersion: "OpenSSH_9.0"}, nil
}
func (f *hostKeySSH) Invalidate(string) {}
func (f *hostKeySSH) Close() error      { return nil }

type fakeDBExec struct {
	dbproxy.Executor
	queryFn func(ctx context.Context, db store.Database, password string, req dbproxy.QueryRequest) (dbproxy.QueryResult, error)
	calls   []dbproxy.QueryRequest
}

func (f *fakeDBExec) Test(context.Context, store.Database, string) (dbproxy.TestResult, error) {
	return dbproxy.TestResult{LatencyMS: 2, ServerVersion: "PostgreSQL 16"}, nil
}
func (f *fakeDBExec) Query(ctx context.Context, db store.Database, password string, req dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
	f.calls = append(f.calls, req)
	return f.queryFn(ctx, db, password, req)
}
func (f *fakeDBExec) Invalidate(string) {}
func (f *fakeDBExec) Close() error      { return nil }

// errApprovals 让 Wait 返回错误（ctx 仍存活），覆盖审批等待失败路径。
type errApprovals struct {
	fakeApprovals
	waitErr error
}

func (f *errApprovals) Wait(context.Context, string) (approvals.Decision, error) {
	return approvals.Decision{}, f.waitErr
}

func newRunner(st store.Store, ssh sshx.Executor, db dbproxy.Executor, ap ApprovalService, client llm.Client) *Runner {
	r := NewRunner(st, nil, ssh, db, nil, nil, nil, RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
	r.SetLLMClient(client)
	if ap != nil {
		r.SetApprovalService(ap)
	}
	return r
}

func runCollect(r *Runner, in RunInput) ([]Event, error) {
	var events []Event
	err := r.Run(context.Background(), in, func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

func lastToolResult(events []Event) *ToolResultPayload {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EventToolResult {
			return events[i].ToolResult
		}
	}
	return nil
}

func errorCodeOf(t *testing.T, events []Event) string {
	t.Helper()
	for _, ev := range events {
		if ev.Type == EventError {
			if ev.Error == nil {
				t.Fatal("error event without payload")
			}
			return ev.Error.Code
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// RunError / 错误分类辅助
// ---------------------------------------------------------------------------

func TestRunErrorMethodsAndIsEmitFailure(t *testing.T) {
	inner := errors.New("upstream boom")
	re := &RunError{Code: "LLM_UPSTREAM_ERROR", Message: "failed", Retryable: false, Err: inner}
	if got := re.Error(); got != "LLM_UPSTREAM_ERROR: failed" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(re, inner) {
		t.Fatal("Unwrap must expose inner error")
	}
	if !isEmitFailure(context.Canceled) {
		t.Fatal("context.Canceled is an emit failure")
	}
	if !isEmitFailure(fmt.Errorf("wrap: %w", errEmitFailed)) {
		t.Fatal("wrapped errEmitFailed is an emit failure")
	}
	if isEmitFailure(errors.New("plain")) {
		t.Fatal("plain error is not an emit failure")
	}
}

func TestClassifyLLMErrorMatrix(t *testing.T) {
	r := &Runner{}
	cases := []struct {
		name        string
		ctxErr      error
		runCtxErr   error
		err         error
		wantCode    string
		wantRetry   bool
		wantMessage string
	}{
		{name: "stream timeout", runCtxErr: context.DeadlineExceeded, err: context.DeadlineExceeded, wantCode: codeStreamTimeout, wantMessage: "stream exceeded maximum duration"},
		{name: "client disconnected", ctxErr: context.Canceled, err: errors.New("broken pipe"), wantCode: codeClientDisconnected},
		{name: "llm timeout", err: llm.ErrTimeout, wantCode: codeLLMTimeout, wantRetry: true},
		{name: "ctx deadline", err: context.DeadlineExceeded, wantCode: codeLLMTimeout, wantRetry: true},
		{name: "llm stream error", err: llm.ErrStream, wantCode: codeLLMStream},
		{name: "invalid config", err: llm.ErrInvalidConfig, wantCode: codeLLMUpstream, wantMessage: "language model is misconfigured"},
		{name: "generic upstream", err: errors.New("502 bad gateway"), wantCode: codeLLMUpstream, wantMessage: "language model request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.ctxErr != nil {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			runCtx := context.Background()
			if tc.runCtxErr != nil {
				c, cancel := context.WithDeadline(runCtx, time.Now().Add(-time.Second))
				defer cancel()
				runCtx = c
			}
			code, msg, retryable := r.classifyLLMError(ctx, runCtx, tc.err)
			if code != tc.wantCode || retryable != tc.wantRetry {
				t.Fatalf("classifyLLMError = %q,%v, want %q,%v", code, retryable, tc.wantCode, tc.wantRetry)
			}
			if tc.wantMessage != "" && msg != tc.wantMessage {
				t.Fatalf("message = %q, want %q", msg, tc.wantMessage)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Run 入口校验与 clientFor 失败
// ---------------------------------------------------------------------------

func TestRunValidationBeforePersist(t *testing.T) {
	st := seed(true)

	r := newRunner(st, &hostKeySSH{}, nil, nil, &fakeLLM{})
	if err := r.Run(context.Background(), runInput(), nil); err == nil {
		t.Fatal("nil emit must fail")
	} else {
		var re *RunError
		if !errors.As(err, &re) || re.Code != codeInternal {
			t.Fatalf("nil emit error = %v", err)
		}
	}

	// 空 store。
	var events []Event
	r2 := newRunner(nil, &hostKeySSH{}, nil, nil, &fakeLLM{})
	err := r2.Run(context.Background(), runInput(), func(ev Event) error { events = append(events, ev); return nil })
	var re *RunError
	if !errors.As(err, &re) || re.Code != codeInternal {
		t.Fatalf("nil store error = %v", err)
	}
	if errorCodeOf(t, events) != codeInternal {
		t.Fatalf("events = %v", eventTypes(events))
	}

	// 空会话 ID。
	r3 := newRunner(st, &hostKeySSH{}, nil, nil, &fakeLLM{})
	in := runInput()
	in.ConversationID = "  "
	events = nil
	err = r3.Run(context.Background(), in, func(ev Event) error { events = append(events, ev); return nil })
	if !errors.As(err, &re) || re.Code != codeValidationFailed {
		t.Fatalf("empty conversation error = %v", err)
	}
	if errorCodeOf(t, events) != codeValidationFailed {
		t.Fatalf("events = %v", eventTypes(events))
	}

	// 空内容。
	in = runInput()
	in.Content = " "
	err = r3.Run(context.Background(), in, func(ev Event) error { events = append(events, ev); return nil })
	if !errors.As(err, &re) || re.Code != codeValidationFailed {
		t.Fatalf("empty content error = %v", err)
	}

	// 会话不存在。
	in = runInput()
	in.ConversationID = "nope"
	events = nil
	err = r3.Run(context.Background(), in, func(ev Event) error { events = append(events, ev); return nil })
	if !errors.As(err, &re) || re.Code != codeConversationNotFound {
		t.Fatalf("missing conversation error = %v", err)
	}
	if errorCodeOf(t, events) != codeConversationNotFound {
		t.Fatalf("events = %v", eventTypes(events))
	}

	// emit 回调失败时错误事件投递也失败 → CLIENT_DISCONNECTED 包装错误。
	r4 := newRunner(st, &hostKeySSH{}, nil, nil, &fakeLLM{})
	err = r4.Run(context.Background(), runInput(), func(Event) error { return errors.New("client gone") })
	if !errors.As(err, &re) || re.Code != codeClientDisconnected {
		t.Fatalf("emit failure error = %v", err)
	}
}

// 未注入 LLM client 且 registry 为 nil：clientFor 失败必须落库并回报 PROVIDER_NOT_FOUND。
func TestRunClientForFailure(t *testing.T) {
	st := seed(true)
	r := NewRunner(st, nil, &hostKeySSH{}, nil, nil, nil, nil,
		RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
	// 不调用 SetLLMClient，也不配置 registry。

	var events []Event
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	var re *RunError
	if !errors.As(err, &re) || re.Code != codeProviderNotFound {
		t.Fatalf("error = %v, want PROVIDER_NOT_FOUND", err)
	}
	if errorCodeOf(t, events) != codeProviderNotFound {
		t.Fatalf("events = %v", eventTypes(events))
	}
	if len(st.messages) != 2 {
		t.Fatalf("messages = %d, want user + error assistant", len(st.messages))
	}
	last := st.messages[len(st.messages)-1]
	if last.Status != store.MessageError || last.ErrorCode != codeProviderNotFound {
		t.Fatalf("persisted error message = %+v", last)
	}
}

// ---------------------------------------------------------------------------
// ask_user / list_databases / server_facts 工具
// ---------------------------------------------------------------------------

func TestRunAskUserEndsTurnWithQuestion(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolAskUser, `{"question":"Which environment?"}`)},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, nil, client)

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	types := eventTypes(events)
	if indexOf(types, "done") < 0 || indexOf(types, "error") >= 0 {
		t.Fatalf("events = %v", types)
	}
	res := lastToolResult(events)
	if res == nil || !res.OK || res.Error != nil {
		t.Fatalf("tool result = %+v", res)
	}
	if !strings.Contains(string(res.Result), `"awaiting_user"`) {
		t.Fatalf("result = %s", res.Result)
	}
	// 问题必须作为 token 发给前端并写入 assistant 消息。
	sawQuestion := false
	for _, ev := range events {
		if ev.Type == EventToken && ev.Token != nil && ev.Token.Delta == "Which environment?" {
			sawQuestion = true
		}
	}
	if !sawQuestion {
		t.Fatalf("question token missing: %v", types)
	}
	var done *Event
	for i := range events {
		if events[i].Type == EventDone {
			done = &events[i]
		}
	}
	if done == nil || done.Done.FinishReason != "ask_user" {
		t.Fatalf("done = %+v", done)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolAwaitingUser {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
	assistant := st.messages[len(st.messages)-1]
	if !strings.Contains(assistant.Content, "Which environment?") {
		t.Fatalf("assistant content = %q", assistant.Content)
	}
	if len(client.calls) != 1 {
		t.Fatalf("ask_user must end the turn without a second LLM call, calls = %d", len(client.calls))
	}
}

func TestRunAskUserRejectsInvalidQuestion(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolAskUser, `{"question":"   "}`)},
		{events: finalEvents("need more info", llm.Usage{})},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, nil, client)

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || res.OK || res.Error == nil || res.Error.Code != codeToolArgsInvalid {
		t.Fatalf("tool result = %+v", res)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolFailed {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
}

func TestRunListDatabasesTool(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolListDatabases, `{}`)},
		{events: finalEvents("found orders", llm.Usage{})},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, nil, client)

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || !res.OK {
		t.Fatalf("tool result = %+v", res)
	}
	if !strings.Contains(string(res.Result), `"orders"`) || !strings.Contains(string(res.Result), `"postgres"`) {
		t.Fatalf("result = %s", res.Result)
	}
}

const factsOutput = `hostname=web-1
os=Debian GNU/Linux 12
kernel=6.1.0-18-amd64
arch=x86_64
uptime_seconds=123456
cpu_count=8
load_1=0.10
load_5=0.20
load_15=0.30
memory_total_bytes=16777216000
memory_available_bytes=8388608000
ip=10.0.0.5 192.168.1.5
disk=/|/dev/sda1|1000000|500000|500000
disk=/data|/dev/sdb1|2000000|100000|1900000
disk=malformed-line
unknown_key=ignored`

func TestRunServerFactsRecordsHostKey(t *testing.T) {
	st := seed(false)
	sshExec := &hostKeySSH{result: sshx.RunResult{
		ExitCode: 0, Combined: factsOutput, DurationMS: 12,
		FirstSeen: true, Fingerprint: "SHA256:abc123", HostKey: "ssh-ed25519 AAAA abc123",
	}}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"s1"}`)},
		{events: finalEvents("facts collected", llm.Usage{})},
	}}
	r := newRunner(st, sshExec, nil, nil, client)

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || !res.OK {
		t.Fatalf("tool result = %+v", res)
	}
	var payload struct {
		Facts serverFacts `json:"facts"`
	}
	if err := json.Unmarshal(res.Result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, res.Result)
	}
	f := payload.Facts
	if f.Hostname != "web-1" || f.CPUCount != 8 || f.UptimeSeconds != 123456 {
		t.Fatalf("facts = %+v", f)
	}
	if f.Load1 != 0.10 || f.MemoryTotalBytes != 16777216000 {
		t.Fatalf("facts numeric fields = %+v", f)
	}
	if len(f.IPAddresses) != 2 || f.IPAddresses[0] != "10.0.0.5" {
		t.Fatalf("ips = %v", f.IPAddresses)
	}
	if len(f.Disks) != 2 || f.Disks[0].Mount != "/" || f.Disks[0].SizeBytes != 1000000 {
		t.Fatalf("disks = %+v (malformed line must be dropped, mount sorted first)", f.Disks)
	}
	// 首次发现的主机密钥必须回写。
	srv := st.servers["s1"]
	if srv.HostKeyFingerprint != "SHA256:abc123" || srv.HostKeyPublic != "ssh-ed25519 AAAA abc123" {
		t.Fatalf("host key not recorded: %+v", srv)
	}
	if srv.HostKeyVerifiedAt == nil {
		t.Fatal("host key verified_at must be set")
	}
	if len(sshExec.commands) != 1 || !strings.Contains(sshExec.commands[0], "hostname") {
		t.Fatalf("ssh command = %v", sshExec.commands)
	}
}

func TestRunServerFactsErrorBranches(t *testing.T) {
	// server_facts 缺 server_id。
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{}`)},
		{events: finalEvents("need server id", llm.Usage{})},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, nil, client)
	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != codeToolArgsInvalid {
		t.Fatalf("missing server_id result = %+v", res)
	}

	// 服务器不存在。
	client = &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"missing"}`)},
		{events: finalEvents("no such server", llm.Usage{})},
	}}
	r = newRunner(st, &hostKeySSH{}, nil, nil, client)
	events, err = runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != "SERVER_NOT_FOUND" {
		t.Fatalf("missing server result = %+v", res)
	}

	// 私钥凭据解不开（未配置 cipher）。
	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	encKey, err := cipher.Encrypt("PRIVATE KEY MATERIAL")
	if err != nil {
		t.Fatal(err)
	}
	st.servers["s2"] = store.Server{
		ID: "s2", Name: "key-host", Host: "127.0.0.1", Port: 22, Username: "root",
		AuthMethod: store.AuthMethodPrivateKey, PrivateKeyEncrypted: encKey,
	}
	client = &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"s2"}`)},
		{events: finalEvents("creds unavailable", llm.Usage{})},
	}}
	rNoCipher := NewRunner(st, nil, &hostKeySSH{}, nil, nil, nil, nil,
		RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
	rNoCipher.SetLLMClient(client)
	events, err = runCollect(rNoCipher, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != codeInternal {
		t.Fatalf("no-cipher result = %+v", res)
	}

	// 配置 cipher 后私钥目标可解出（验证 serverTarget 私钥分支）。
	sshExec := &hostKeySSH{result: sshx.RunResult{ExitCode: 0, Combined: "hostname=x"}}
	rCipher := NewRunner(st, nil, sshExec, nil, nil, nil, cipher,
		RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
	rCipher.SetLLMClient(&fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"s2"}`)},
		{events: finalEvents("ok", llm.Usage{})},
	}})
	events, err = runCollect(rCipher, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := lastToolResult(events); res == nil || !res.OK {
		t.Fatalf("private-key result = %+v", res)
	}
	if len(sshExec.targets) != 1 || sshExec.targets[0].PrivateKey != "PRIVATE KEY MATERIAL" {
		t.Fatalf("target = %+v", sshExec.targets)
	}
	if sshExec.targets[0].AuthMethod != store.AuthMethodPrivateKey || sshExec.targets[0].Username != "root" {
		t.Fatalf("target fields = %+v", sshExec.targets[0])
	}

	// SSH 执行失败。
	sshFail := &hostKeySSH{execErr: errors.New("sshx: dial tcp: connect: connection refused")}
	rFail := newRunner(st, sshFail, nil, nil, &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"s1"}`)},
		{events: finalEvents("failed", llm.Usage{})},
	}})
	events, err = runCollect(rFail, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != codeSSHExecFailed {
		t.Fatalf("ssh failure result = %+v", res)
	}
}

// ---------------------------------------------------------------------------
// db_query 工具
// ---------------------------------------------------------------------------

func TestRunDBQueryMatrix(t *testing.T) {
	newCase := func(t *testing.T) *testStore {
		t.Helper()
		st := seed(true)
		st.databases["d1"] = store.Database{
			ID: "d1", Name: "orders", Engine: store.EnginePostgres,
			Host: "db", Port: 5432, DatabaseName: "orders", Username: "ro",
		}
		st.databases["oracle"] = store.Database{ID: "oracle", Name: "legacy", Engine: "oracle"}
		return st
	}

	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	encPwd, err := cipher.Encrypt("s3cret")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("readonly select succeeds with encrypted password", func(t *testing.T) {
		st := newCase(t)
		st.databases["d1"] = func() store.Database {
			d := st.databases["d1"]
			d.PasswordEncrypted = encPwd
			return d
		}()
		var gotPassword string
		db := &fakeDBExec{queryFn: func(_ context.Context, _ store.Database, password string, _ dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
			gotPassword = password
			return dbproxy.QueryResult{Columns: []dbproxy.Column{{Name: "id", Type: "int8"}}, Rows: [][]any{{int64(1)}}, RowCount: 1}, nil
		}}
		r := NewRunner(st, nil, &hostKeySSH{}, db, nil, nil, cipher,
			RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
		r.SetLLMClient(&fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1","sql":"SELECT id FROM orders"}`)},
			{events: finalEvents("queried", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil || !res.OK {
			t.Fatalf("result = %+v", res)
		}
		if gotPassword != "s3cret" {
			t.Fatalf("decrypted password = %q", gotPassword)
		}
		if !strings.Contains(string(res.Result), `"row_count":1`) {
			t.Fatalf("result = %s", res.Result)
		}
		if len(db.calls) != 1 || db.calls[0].MaxRows != 1000 {
			t.Fatalf("query calls = %+v", db.calls)
		}
	})

	t.Run("write statement rejected before executor", func(t *testing.T) {
		st := newCase(t)
		db := &fakeDBExec{queryFn: func(context.Context, store.Database, string, dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
			t.Fatal("executor must not be called for non-readonly SQL")
			return dbproxy.QueryResult{}, nil
		}}
		r := newRunner(st, &hostKeySSH{}, db, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1","sql":"DELETE FROM orders"}`)},
			{events: finalEvents("rejected", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil || res.Error == nil || res.Error.Code != codeSQLNotReadOnly {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("invalid args", func(t *testing.T) {
		st := newCase(t)
		r := newRunner(st, &hostKeySSH{}, &fakeDBExec{}, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1"}`)},
			{events: finalEvents("need sql", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != codeToolArgsInvalid {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("sql too large", func(t *testing.T) {
		st := newCase(t)
		big := `{"database_id":"d1","sql":"SELECT '` + strings.Repeat("x", MaxSQLBytes+2) + `'"}`
		r := newRunner(st, &hostKeySSH{}, &fakeDBExec{}, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, big)},
			{events: finalEvents("too big", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != codeToolArgsInvalid {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("database not found", func(t *testing.T) {
		st := newCase(t)
		r := newRunner(st, &hostKeySSH{}, &fakeDBExec{}, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"missing","sql":"SELECT 1"}`)},
			{events: finalEvents("missing", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != "DATABASE_NOT_FOUND" {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("unsupported engine", func(t *testing.T) {
		st := newCase(t)
		r := newRunner(st, &hostKeySSH{}, &fakeDBExec{}, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"oracle","sql":"SELECT 1 FROM DUAL"}`)},
			{events: finalEvents("unsupported", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil || res.Error == nil || res.Error.Code != codeSQLQueryFailed {
			t.Fatalf("result = %+v", res)
		}
		if res.Error.Message != "unsupported database engine" {
			t.Fatalf("message = %q", res.Error.Message)
		}
	})

	t.Run("executor not configured", func(t *testing.T) {
		st := newCase(t)
		r := newRunner(st, &hostKeySSH{}, nil, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1","sql":"SELECT 1"}`)},
			{events: finalEvents("no executor", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil || res.Error == nil || res.Error.Code != codeSQLQueryFailed {
			t.Fatalf("result = %+v", res)
		}
		if res.Error.Message != "database executor is not configured" {
			t.Fatalf("message = %q", res.Error.Message)
		}
	})

	t.Run("password undecryptable without cipher", func(t *testing.T) {
		st := newCase(t)
		d := st.databases["d1"]
		d.PasswordEncrypted = encPwd
		st.databases["d1"] = d
		r := newRunner(st, &hostKeySSH{}, &fakeDBExec{}, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1","sql":"SELECT 1"}`)},
			{events: finalEvents("no cipher", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil || res.Error == nil || res.Error.Code != codeInternal || res.Error.Message != "database credentials are unavailable" {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("executor not readonly error", func(t *testing.T) {
		st := newCase(t)
		db := &fakeDBExec{queryFn: func(context.Context, store.Database, string, dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
			return dbproxy.QueryResult{}, &dbproxy.NotReadOnlyError{Result: sqlguard.Result{Allowed: false, RuleID: "SQL_GUARD_DML"}}
		}}
		r := newRunner(st, &hostKeySSH{}, db, nil, &fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1","sql":"SELECT pg_sleep(0)"}`)},
			{events: finalEvents("guard hit", llm.Usage{})},
		}})
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res := lastToolResult(events); res == nil || res.Error == nil || res.Error.Code != codeSQLNotReadOnly {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("query timeout surfaces sql timeout", func(t *testing.T) {
		st := newCase(t)
		db := &fakeDBExec{queryFn: func(ctx context.Context, _ store.Database, _ string, _ dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
			<-ctx.Done()
			return dbproxy.QueryResult{}, ctx.Err()
		}}
		r := NewRunner(st, nil, &hostKeySSH{}, db, nil, nil, nil,
			RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
		r.SetLLMClient(&fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolDBQuery, `{"database_id":"d1","sql":"SELECT * FROM orders WHERE id IN (SELECT id FROM orders)"}`)},
			{events: finalEvents("timeout", llm.Usage{})},
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		var events []Event
		// 查询超时是工具级错误：回填给模型后本轮仍正常收尾。
		if err := r.Run(ctx, runInput(), func(ev Event) error { events = append(events, ev); return nil }); err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil || res.Error == nil || res.Error.Code != codeSQLTimeout {
			t.Fatalf("result = %+v", res)
		}
	})
}

// ---------------------------------------------------------------------------
// 审批等待失败 / 审批服务缺失
// ---------------------------------------------------------------------------

func TestRunApprovalWaitFailure(t *testing.T) {
	st := seed(false)
	ap := &errApprovals{waitErr: errors.New("waiter stopped")}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("approval wait broke", llm.Usage{})},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, ap, client)

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || res.Error == nil || res.Error.Code != codeInternal || res.Error.Message != "approval wait failed" {
		t.Fatalf("result = %+v", res)
	}
	// 等待失败后审批必须被取消，不能留下可批准的 pending。
	if len(ap.cancelled) != 1 {
		t.Fatalf("cancelled = %v", ap.cancelled)
	}
}

func TestRunApprovalExpiredTimesOut(t *testing.T) {
	st := seed(false)
	ap := &fakeApprovals{waitStatus: store.ApprovalExpired}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("approval expired", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || res.Error == nil || res.Error.Code != codeApprovalTimeout {
		t.Fatalf("result = %+v", res)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].Status != store.ToolExpired {
		t.Fatalf("tool calls = %+v", st.toolCalls)
	}
}

// TestRunToolResultDurationExcludesApprovalWait 走查 B3 回归：需审批的
// 工具调用把「等待审批」全程计入 tool_result 耗时，超时未执行的命令在
// 卡片上显示 45s 级耗时，易被误读为执行了很久才失败。耗时必须只统计
// 真实执行段。
func TestRunToolResultDurationExcludesApprovalWait(t *testing.T) {
	mustRun := func(t *testing.T, r *Runner) *ToolResultPayload {
		t.Helper()
		events, err := runCollect(r, runInput())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		res := lastToolResult(events)
		if res == nil {
			t.Fatal("no tool_result event")
		}
		return res
	}

	t.Run("expired 审批等待 45s 不计入耗时", func(t *testing.T) {
		st := seed(false)
		now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		ap := &fakeApprovals{
			waitStatus: store.ApprovalExpired,
			onWait:     func() { now = now.Add(45 * time.Second) },
		}
		r := NewRunner(st, nil, &fakeSSH{}, nil, nil, nil, nil,
			RunnerConfig{StreamMaxDuration: time.Minute}, clock, testLogger())
		r.SetLLMClient(&fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
			{events: finalEvents("approval expired", llm.Usage{})},
		}})
		r.SetApprovalService(ap)

		res := mustRun(t, r)
		if res.Error == nil || res.Error.Code != codeApprovalTimeout {
			t.Fatalf("result = %+v", res)
		}
		// 修复前为 45_000ms 级（审批等待全程）；命令从未执行，等待段必须剔除。
		if res.DurationMS != 0 {
			t.Fatalf("DurationMS = %d, want 0 (approval wait must not count as execution)", res.DurationMS)
		}
	})

	t.Run("approved 等待 45s 后执行 300ms 只计执行段", func(t *testing.T) {
		st := seed(false)
		now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		ap := &fakeApprovals{
			waitStatus: store.ApprovalApproved,
			onWait:     func() { now = now.Add(45 * time.Second) },
		}
		sshExec := &fakeSSH{onExec: func() { now = now.Add(300 * time.Millisecond) }}
		r := NewRunner(st, nil, sshExec, nil, nil, nil, nil,
			RunnerConfig{StreamMaxDuration: time.Minute}, clock, testLogger())
		r.SetLLMClient(&fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
			{events: finalEvents("done", llm.Usage{})},
		}})
		r.SetApprovalService(ap)

		res := mustRun(t, r)
		if !res.OK {
			t.Fatalf("result = %+v", res)
		}
		// 修复前为 45_300ms（等待+执行）；只允许统计执行段 300ms。
		if res.DurationMS != 300 {
			t.Fatalf("DurationMS = %d, want 300 (execution segment only)", res.DurationMS)
		}
	})

	t.Run("auto_exec 免审批 全程计时不受影响", func(t *testing.T) {
		st := seed(false)
		srv := st.servers["s1"]
		srv.AutoExecEnabled = true
		st.servers["s1"] = srv
		now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		ap := &fakeApprovals{}
		sshExec := &fakeSSH{onExec: func() { now = now.Add(120 * time.Millisecond) }}
		r := NewRunner(st, nil, sshExec, nil, nil, nil, nil,
			RunnerConfig{StreamMaxDuration: time.Minute}, clock, testLogger())
		r.SetLLMClient(&fakeLLM{steps: []scriptStep{
			{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
			{events: finalEvents("done", llm.Usage{})},
		}})
		r.SetApprovalService(ap)

		res := mustRun(t, r)
		if !res.OK {
			t.Fatalf("result = %+v", res)
		}
		if len(ap.created) != 0 {
			t.Fatalf("auto-exec safe command must not create approval: %+v", ap.created)
		}
		// 无审批等待：从调用开始计时，执行段 120ms 正常展示。
		if res.DurationMS != 120 {
			t.Fatalf("DurationMS = %d, want 120", res.DurationMS)
		}
	})
}

func TestRunApprovalServiceMissing(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la"}`)},
		{events: finalEvents("no approval service", llm.Usage{})},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, nil, client) // approvals == nil

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || res.Error == nil || res.Error.Code != codeInternal || res.Error.Message != "approval service is not configured" {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunSSHExecArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing server_id", `{"command":"ls"}`, codeToolArgsInvalid},
		{"missing command", `{"server_id":"s1"}`, codeToolArgsInvalid},
		{"timeout out of range", `{"server_id":"s1","command":"ls","timeout_seconds":301}`, codeToolArgsInvalid},
		{"timeout zero", `{"server_id":"s1","command":"ls","timeout_seconds":0}`, codeToolArgsInvalid},
		{"command too large", `{"server_id":"s1","command":"` + strings.Repeat("a", MaxCommandBytes+2) + `"}`, codeCommandInvalid},
		{"server missing", `{"server_id":"ghost","command":"ls"}`, "SERVER_NOT_FOUND"},
		{"forbidden command", `{"server_id":"s1","command":"rm -rf / --no-preserve-root"}`, codeCommandForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := seed(false)
			r := newRunner(st, &hostKeySSH{}, nil, nil, &fakeLLM{steps: []scriptStep{
				{events: toolCallEvents(ToolSSHExec, tc.args)},
				{events: finalEvents("handled", llm.Usage{})},
			}})
			events, err := runCollect(r, runInput())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			res := lastToolResult(events)
			if res == nil || res.Error == nil || res.Error.Code != tc.want {
				t.Fatalf("result = %+v, want code %s", res, tc.want)
			}
		})
	}
}

// ssh 无执行器配置（r.ssh == nil）。
func TestRunSSHExecWithoutExecutor(t *testing.T) {
	st := seed(false)
	srv := st.servers["s1"]
	srv.AutoExecEnabled = true // safe 命令直接放行，避开审批
	st.servers["s1"] = srv
	r := NewRunner(st, nil, nil, nil, nil, nil, nil,
		RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
	r.SetLLMClient(&fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls"}`)},
		{events: finalEvents("no ssh", llm.Usage{})},
	}})
	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || res.Error == nil || res.Error.Code != codeSSHExecFailed || res.Error.Message != "ssh executor is not configured" {
		t.Fatalf("result = %+v", res)
	}
}

// auto_exec + safe 命令跳过审批；host key 首见回写。
func TestRunSSHExecAutoExecSkipsApproval(t *testing.T) {
	st := seed(false)
	srv := st.servers["s1"]
	srv.AutoExecEnabled = true
	st.servers["s1"] = srv
	ap := &fakeApprovals{}
	sshExec := &hostKeySSH{result: sshx.RunResult{
		ExitCode: 0, Combined: "total 0", DurationMS: 8,
		FirstSeen: true, Fingerprint: "SHA256:new", HostKey: "ssh-ed25519 BBBB",
	}}
	r := newRunner(st, sshExec, nil, ap, &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"ls -la","timeout_seconds":5}`)},
		{events: finalEvents("done", llm.Usage{})},
	}})

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if len(ap.created) != 0 {
		t.Fatalf("auto-exec safe command must not create approval: %+v", ap.created)
	}
	if !strings.Contains(string(res.Result), `"approval_id":null`) {
		t.Fatalf("result = %s", res.Result)
	}
	if st.servers["s1"].HostKeyFingerprint != "SHA256:new" {
		t.Fatalf("host key not recorded: %+v", st.servers["s1"])
	}
	if len(sshExec.commands) != 1 || sshExec.commands[0] != "ls -la" {
		t.Fatalf("commands = %v", sshExec.commands)
	}
}

// ---------------------------------------------------------------------------
// StreamGuard / 纯函数
// ---------------------------------------------------------------------------

func TestStreamGuardActiveCountAndShutdown(t *testing.T) {
	g := NewStreamGuard(0) // 默认 max 20
	release, err := g.TryAcquire("c1")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Active("c1") || g.Count() != 1 {
		t.Fatalf("active=%v count=%d", g.Active("c1"), g.Count())
	}
	if g.Active("c2") {
		t.Fatal("c2 must not be active")
	}
	// 同一会话重复占用。
	if _, err := g.TryAcquire("c1"); !errors.Is(err, ErrStreamActive) {
		t.Fatalf("second acquire err = %v", err)
	}
	// 全局上限。
	for i := 0; i < 19; i++ {
		if _, err := g.TryAcquire(fmt.Sprintf("other-%d", i)); err != nil {
			t.Fatalf("acquire other-%d: %v", i, err)
		}
	}
	if _, err := g.TryAcquire("overflow"); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("overflow err = %v", err)
	}
	if g.Count() != 20 {
		t.Fatalf("count = %d, want 20", g.Count())
	}
	// release 幂等。
	release()
	release()
	if g.Count() != 19 || g.Active("c1") {
		t.Fatalf("after release count=%d active=%v", g.Count(), g.Active("c1"))
	}

	// Watch + ShutdownActive：已登记的取消函数被调用。
	cancelled := false
	g.Watch("other-0", func() { cancelled = true })
	g.ShutdownActive()
	if !cancelled {
		t.Fatal("ShutdownActive must invoke registered cancel")
	}
	// 关闭后再 Watch：立即取消。
	lateCancelled := false
	g.Watch("late", func() { lateCancelled = true })
	if !lateCancelled {
		t.Fatal("Watch after shutdown must cancel immediately")
	}
	// 重复 ShutdownActive 幂等。
	g.ShutdownActive()
}

func TestParseServerFactsUnit(t *testing.T) {
	f := parseServerFacts("uptime_seconds=not-a-number\ncpu_count=oops\nload_1=zzz\ndisk=a|b|c\nnoequals\n\nip=")
	if f.UptimeSeconds != 0 || f.CPUCount != 0 || f.Load1 != 0 {
		t.Fatalf("malformed numbers must stay zero: %+v", f)
	}
	if len(f.Disks) != 0 || len(f.IPAddresses) != 0 {
		t.Fatalf("malformed disk/ip dropped: %+v", f)
	}
	if f.Hostname != "" {
		t.Fatalf("hostname = %q", f.Hostname)
	}
}

func TestToolMessageContentBranches(t *testing.T) {
	if got := toolMessageContent(toolOutcome{Err: &ToolError{Code: "X", Message: "boom"}}, 100); !strings.Contains(got, `"code":"X"`) {
		t.Fatalf("err content = %s", got)
	}
	if got := toolMessageContent(toolOutcome{}, 100); got != "{}" {
		t.Fatalf("empty result content = %q", got)
	}
	big := json.RawMessage(`{"data":"` + strings.Repeat("x", 400) + `"}`)
	got := toolMessageContent(toolOutcome{Result: big}, 100)
	if len(got) > 200 || !strings.Contains(got, `"truncated":true`) {
		preview := string(got)
		if len(preview) > 120 {
			preview = preview[:120]
		}
		t.Fatalf("truncated content = %s...", preview)
	}
}

func TestSQLAndActorHelpers(t *testing.T) {
	for _, tc := range []struct {
		engine store.DatabaseEngine
		ok     bool
	}{
		{store.EnginePostgres, true},
		{store.EngineMySQL, true},
		{store.EngineSQLite, true},
		{"oracle", false},
	} {
		if _, ok := sqlEngine(tc.engine); ok != tc.ok {
			t.Errorf("sqlEngine(%q) ok = %v, want %v", tc.engine, ok, tc.ok)
		}
	}
	if conversationActor("") != "" {
		t.Fatal("empty conversation actor must be empty")
	}
	if conversationActor("c1") != "conversation:c1" {
		t.Fatal("conversationActor prefix wrong")
	}
	if serverIDFromArgs(ToolDBQuery, sshExecArgs{ServerID: "s1"}, "") != nil {
		t.Fatal("non-ssh tools have no server id")
	}
	if serverIDFromArgs(ToolSSHExec, sshExecArgs{}, "") != nil {
		t.Fatal("empty server id must yield nil")
	}
	id := serverIDFromArgs(ToolSSHExec, sshExecArgs{ServerID: "s1"}, "")
	if id == nil || *id != "s1" {
		t.Fatal("server id not extracted")
	}
}

// ---------------------------------------------------------------------------
// buildMessages 上下文裁剪
// ---------------------------------------------------------------------------

func TestBuildMessagesTrimsToBudget(t *testing.T) {
	st := seed(true)
	// 预置 3 条历史 user 消息（首条超大，应被字节预算裁掉）。
	for i, content := range []string{strings.Repeat("old-", 400), "middle message", "latest message"} {
		st.messages = append(st.messages, store.Message{
			ID: fmt.Sprintf("pre-%d", i+1), Seq: int64(i + 1),
			ConversationID: "c1", Role: store.RoleUser, Content: content,
		})
	}
	r := &Runner{st: st, cfg: RunnerConfig{ContextMessages: 40, ContextBytes: 100}}
	userMsg := store.Message{ID: "msg-new", ConversationID: "c1", Role: store.RoleUser, Content: "hello"}

	msgs, err := r.buildMessages(context.Background(), "c1", userMsg, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Role != llm.RoleSystem {
		t.Fatalf("system prompt missing: %+v", msgs)
	}
	var contents []string
	for _, m := range msgs[1:] {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "\x00")
	if strings.Contains(joined, "old-") {
		t.Fatalf("oversized history must be trimmed: %q", joined)
	}
	if !strings.Contains(joined, "hello") {
		t.Fatalf("current user message missing: %q", joined)
	}

	// ContextMessages=1：只保留最近一组。
	r2 := &Runner{st: st, cfg: RunnerConfig{ContextMessages: 1, ContextBytes: 204800}}
	msgs2, err := r2.buildMessages(context.Background(), "c1", userMsg, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 2 { // system + 当前用户消息
		t.Fatalf("msgs = %d, want 2 (system + latest)", len(msgs2))
	}
}

// ToolCalls().ListPage 报错时 buildMessages 仍能构造（错误被忽略）。
func TestBuildMessagesIgnoresToolCallErrors(t *testing.T) {
	st := seed(true)
	r := &Runner{st: st, cfg: RunnerConfig{ContextMessages: 40, ContextBytes: 204800}}
	userMsg := store.Message{ID: "msg-new", ConversationID: "c1", Role: store.RoleUser, Content: "hi"}
	msgs, err := r.buildMessages(context.Background(), "c1", userMsg, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 2 {
		t.Fatalf("msgs = %+v", msgs)
	}
}

// pagedToolCallRepo 实现 postgres 的 ListPage 语义：seq < beforeSeq 的最新
// limit 条、时间正序、hasMore —— 现有 toolCallRepo 忽略分页参数，无法驱动
// buildMessages 的翻页窗口逻辑。
type pagedToolCallRepo struct {
	store.ToolCallStore
	s *testStore
}

func (r *pagedToolCallRepo) ListPage(_ context.Context, conversationID string, beforeSeq int64, limit int) ([]store.ToolCall, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var desc []store.ToolCall
	for i := len(r.s.toolCalls) - 1; i >= 0; i-- {
		tc := r.s.toolCalls[i]
		if tc.ConversationID != conversationID {
			continue
		}
		if beforeSeq > 0 && tc.Seq >= beforeSeq {
			continue
		}
		desc = append(desc, tc)
		if len(desc) > limit {
			break
		}
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, hasMore, nil
}

type pagedStore struct {
	*testStore
}

func (s *pagedStore) ToolCalls() store.ToolCallStore { return &pagedToolCallRepo{s: s.testStore} }

// tool_calls 拉取窗口必须覆盖消息窗口内最旧 assistant 轮次：总调用数超过
// 单页（200）时继续翻页，最旧组的 tool_calls 与 tool 消息不得因窗口截断而
// 残缺（回归：固定 200 条 tool_calls 窗口与消息窗口失配）。
func TestBuildMessagesToolCallWindowCoversMessageWindow(t *testing.T) {
	st := seed(true)
	const callsPerTurn = 120 // 两轮共 240 条 > 单页 200，最旧轮跨页。

	st.messages = append(st.messages,
		store.Message{ID: "m-old", Seq: 1, ConversationID: "c1", TurnID: "turn-old", Role: store.RoleAssistant, Content: "old answer"},
		store.Message{ID: "m-mid", Seq: 2, ConversationID: "c1", TurnID: "turn-old", Role: store.RoleUser, Content: "mid question"},
		store.Message{ID: "m-new", Seq: 3, ConversationID: "c1", TurnID: "turn-new", Role: store.RoleAssistant, Content: "new answer"},
	)
	now := time.Now()
	for i := 0; i < callsPerTurn; i++ {
		st.toolCalls = append(st.toolCalls, store.ToolCall{
			ID: fmt.Sprintf("old-%d", i), Seq: int64(i + 1),
			ConversationID: "c1", TurnID: "turn-old",
			ProviderCallID: fmt.Sprintf("call-old-%d", i), Name: "list_servers",
			Arguments: json.RawMessage(`{}`), RiskLevel: "safe",
			Status: store.ToolSucceeded, Result: json.RawMessage(`{"ok":true}`),
			StartedAt: now,
		})
	}
	for i := 0; i < callsPerTurn; i++ {
		st.toolCalls = append(st.toolCalls, store.ToolCall{
			ID: fmt.Sprintf("new-%d", i), Seq: int64(callsPerTurn + i + 1),
			ConversationID: "c1", TurnID: "turn-new",
			ProviderCallID: fmt.Sprintf("call-new-%d", i), Name: "list_servers",
			Arguments: json.RawMessage(`{}`), RiskLevel: "safe",
			Status: store.ToolSucceeded, Result: json.RawMessage(`{"ok":true}`),
			StartedAt: now,
		})
	}

	r := &Runner{st: &pagedStore{testStore: st}, cfg: RunnerConfig{ContextMessages: 500, ContextBytes: 8 << 20}}
	userMsg := store.Message{ID: "msg-new", ConversationID: "c1", Role: store.RoleUser, Content: "hello"}
	msgs, err := r.buildMessages(context.Background(), "c1", userMsg, 40)
	if err != nil {
		t.Fatal(err)
	}

	assertTurnComplete := func(content, idPrefix string) {
		t.Helper()
		idx := -1
		for i, m := range msgs {
			if m.Role == llm.RoleAssistant && m.Content == content {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("assistant message %q missing from context", content)
		}
		calls := msgs[idx].ToolCalls
		if len(calls) != callsPerTurn {
			t.Fatalf("%s: tool_calls = %d, want %d (window truncated)", content, len(calls), callsPerTurn)
		}
		for i, call := range calls {
			if want := fmt.Sprintf("%s-%d", idPrefix, i); call.ID != want {
				t.Fatalf("%s: tool_calls[%d].ID = %s, want %s (wrong seq order)", content, i, call.ID, want)
			}
			tm := msgs[idx+1+i]
			if tm.Role != llm.RoleTool || tm.ToolCallID != call.ID {
				t.Fatalf("%s: tool message %d mismatched: role=%s id=%q want id=%q", content, i, tm.Role, tm.ToolCallID, call.ID)
			}
		}
	}
	assertTurnComplete("old answer", "call-old")
	assertTurnComplete("new answer", "call-new")
}
