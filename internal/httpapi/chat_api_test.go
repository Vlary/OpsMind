package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/agent"
	"aiops/internal/llm"
	"aiops/internal/store"
)

func seedProvider(t *testing.T, env *testEnv) {
	t.Helper()
	env.st.mu.Lock()
	env.st.providers["p1"] = store.Provider{
		ID: "p1", Name: "primary", Preset: store.PresetCustom,
		Kind: store.ProviderOpenAICompatible, BaseURL: "http://127.0.0.1:9/v1",
		DefaultModel: "m-1", Models: []string{"m-1"},
	}
	env.st.mu.Unlock()
}

func TestChatValidationBeforeStream(t *testing.T) {
	env := newTestEnv(t)
	conv := createConversation(t, env)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty content", `{"conversation_id":"` + conv + `","content":""}`, "content is required"},
		{"content too long", `{"conversation_id":"` + conv + `","content":"` + strings.Repeat("字", 16001) + `"}`, "content is too long"},
		{"missing conversation_id", `{"content":"hi"}`, "conversation_id is required"},
		{"whitespace conversation_id", `{"conversation_id":"  ","content":"hi"}`, "conversation_id is required"},
		{"unknown field", `{"conversation_id":"` + conv + `","content":"hi","x":1}`, "invalid request body"},
		{"malformed json", `{`, "invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/chat", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
				t.Fatalf("code = %s", code)
			}
		})
	}

	// 错误 Content-Type → 415。
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type = %d", rec.Code)
	}
}

func TestChatStreamHappyPath(t *testing.T) {
	env := newTestEnv(t)
	seedProvider(t, env)
	conv := createConversation(t, env)

	rec := env.do(http.MethodPost, "/api/chat",
		`{"conversation_id":"`+conv+`","content":"检查服务器"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	for _, want := range []string{"event: token\n", "event: done\n", "assistant reply"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected error event:\n%s", body)
	}
	// done 帧必须携带 finish_reason。
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("done payload missing finish reason:\n%s", body)
	}

	// 用户与 assistant 消息均已落库。
	msgs, _, err := env.st.Messages().ListPage(context.Background(), conv, 0, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages = %v (%v)", msgs, err)
	}
	// ListPage 按新到旧返回：第一条是 assistant，第二条是 user。
	if msgs[0].Role != store.RoleAssistant || msgs[1].Role != store.RoleUser {
		t.Fatalf("roles = %v,%v", msgs[0].Role, msgs[1].Role)
	}
	if msgs[0].Content != "assistant reply" {
		t.Fatalf("assistant content = %q", msgs[0].Content)
	}

	// 审计：chat.started 与 chat.finished。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.auditW.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	started, finished := false, false
	for _, l := range env.st.auditLogs {
		if l.Action == "chat.started" && l.ResourceID == conv {
			started = true
		}
		if l.Action == "chat.finished" {
			finished = true
		}
	}
	if !started || !finished {
		t.Fatalf("audit started=%v finished=%v logs=%+v", started, finished, env.st.auditLogs)
	}
}

func TestChatStreamErrorPath(t *testing.T) {
	env := newTestEnv(t)
	seedProvider(t, env)
	conv := createConversation(t, env)

	env.llmClient.set(&stubLLMClient{fn: func(context.Context, llm.Request, func(llm.StreamEvent) error) error {
		return errors.New("upstream 503")
	}})
	rec := env.do(http.MethodPost, "/api/chat", `{"conversation_id":"`+conv+`","content":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") || !strings.Contains(body, "LLM_UPSTREAM_ERROR") {
		t.Fatalf("error event missing:\n%s", body)
	}
	if strings.Contains(body, "event: done") {
		t.Fatalf("done must not follow error:\n%s", body)
	}
	// 失败轮次仍要落 error 状态的 assistant 消息。
	msgs, _, _ := env.st.Messages().ListPage(context.Background(), conv, 0, 10)
	if len(msgs) != 2 || msgs[0].Status != store.MessageError || msgs[0].ErrorCode != "LLM_UPSTREAM_ERROR" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestChatPrepareErrorsBeforeSSE(t *testing.T) {
	env := newTestEnv(t)

	// 会话不存在 → 404 JSON（SSE 之前）。
	rec := env.do(http.MethodPost, "/api/chat", `{"conversation_id":"ghost","content":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing conversation = %d body = %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "CONVERSATION_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}

	// 未配置任何供应商 → 422 NO_PROVIDER_CONFIGURED。
	conv := createConversation(t, env)
	rec = env.do(http.MethodPost, "/api/chat", `{"conversation_id":"`+conv+`","content":"hi"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no provider = %d body = %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "NO_PROVIDER_CONFIGURED" {
		t.Fatalf("code = %s", code)
	}
}

func TestChatStreamActiveConflict(t *testing.T) {
	env := newTestEnv(t)
	seedProvider(t, env)
	conv := createConversation(t, env)

	release, err := env.streams.TryAcquire(conv)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	rec := env.do(http.MethodPost, "/api/chat", `{"conversation_id":"`+conv+`","content":"hi"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "STREAM_ACTIVE" {
		t.Fatalf("code = %s", code)
	}
}

func TestStreamChatGuardBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Agent 未装配 → 500。
	c, rec := withParam(httptestPost("/api/chat", `{}`), "", "")
	c.Set("user_id", "u1")
	(&Deps{}).streamChat(c, "c1", "", "", "hi")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil agent = %d body = %s", rec.Code, rec.Body.String())
	}

	// StreamGuard 未装配 → 500。
	d := &Deps{Agent: agent.NewRunner(nil, nil, nil, nil, nil, nil, nil, agent.RunnerConfig{}, time.Now, nil)}
	c, rec = withParam(httptestPost("/api/chat", `{}`), "", "")
	c.Set("user_id", "u1")
	d.streamChat(c, "c1", "", "", "hi")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil streams = %d body = %s", rec.Code, rec.Body.String())
	}

	// 未认证（user_id 空）→ 401。
	env := newTestEnv(t)
	c, rec = withParam(httptestPost("/api/chat", `{}`), "", "")
	d2 := &Deps{Agent: d.Agent, Streams: env.streams}
	d2.streamChat(c, "c1", "", "", "hi")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d body = %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", code)
	}
}

func TestAgentErrorInfoAndStatus(t *testing.T) {
	// RunError 映射。
	re := &agent.RunError{Code: "LLM_TIMEOUT", Message: "timed out", Retryable: true}
	code, msg, retryable := agentErrorInfo(re)
	if code != "LLM_TIMEOUT" || msg != "timed out" || !retryable {
		t.Fatalf("agentErrorInfo = %q %q %v", code, msg, retryable)
	}
	if got := statusForCode(code); got != http.StatusGatewayTimeout {
		t.Fatalf("status = %d", got)
	}

	// DeadlineExceeded。
	code, _, retryable = agentErrorInfo(context.DeadlineExceeded)
	if code != "LLM_TIMEOUT" || !retryable {
		t.Fatalf("deadline = %q %v", code, retryable)
	}
	// Canceled。
	code, _, _ = agentErrorInfo(context.Canceled)
	if code != "CLIENT_DISCONNECTED" {
		t.Fatalf("canceled = %q", code)
	}
	if got := statusForCode(code); got != http.StatusOK {
		t.Fatalf("client disconnected status = %d", got)
	}
	// 未知 → INTERNAL。
	code, _, _ = agentErrorInfo(errors.New("boom"))
	if code != "INTERNAL" {
		t.Fatalf("generic = %q", code)
	}
	if got := statusForCode("APPROVAL_STREAM_CANCELLED"); got != http.StatusOK {
		t.Fatalf("stream cancel status = %d", got)
	}
	if got := statusForCode("NO_SUCH_CODE"); got != http.StatusInternalServerError {
		t.Fatalf("fallback status = %d", got)
	}

	// writeAgentError 的空 message 兜底。
	gin.SetMode(gin.TestMode)
	c, rec := withParam(httptestPost("/api/chat", `{}`), "", "")
	writeAgentError(c, re)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("writeAgentError status = %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, rec, &body)
	if body.Error.Code != "LLM_TIMEOUT" || body.Error.Message != "timed out" {
		t.Fatalf("body = %+v", body)
	}
}
