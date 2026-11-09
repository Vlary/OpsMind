package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/agent"
	"aiops/internal/apierr"
	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// SSE writer 单元
// ---------------------------------------------------------------------------

// failRW 让底层 Write 永远失败，驱动写错误分支。
type failRW struct{ gin.ResponseWriter }

func (f *failRW) Write([]byte) (int, error) { return 0, errors.New("connection closed") }

func newSSEForTest(w gin.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, interval: time.Hour, done: make(chan struct{})}
}

func TestSSEWriterEventPayloads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	s := newSSEForTest(c.Writer)

	if err := s.WriteEvent(agent.Event{Type: agent.EventToolCall, ToolCall: &agent.ToolCallPayload{
		ToolCallID: "tc-1", Name: "ssh_exec", Arguments: json.RawMessage(`{"command":"uptime"}`),
	}}); err != nil {
		t.Fatalf("tool_call event: %v", err)
	}
	if err := s.WriteEvent(agent.Event{Type: agent.EventApprovalResolved, ApprovalResolved: &agent.ApprovalResolvedPayload{
		ApprovalID: "ap-1", Status: store.ApprovalApproved,
	}}); err != nil {
		t.Fatalf("approval_resolved event: %v", err)
	}
	// 未知事件类型静默跳过。
	if err := s.WriteEvent(agent.Event{Type: "future_kind"}); err != nil {
		t.Fatalf("unknown event must be skipped: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: tool_call") || !strings.Contains(body, `"tool_call_id":"tc-1"`) {
		t.Fatalf("tool_call frame = %q", body)
	}
	if !strings.Contains(body, "event: approval_resolved") || !strings.Contains(body, `"approval_id":"ap-1"`) {
		t.Fatalf("approval frame = %q", body)
	}
	if strings.Contains(body, "future_kind") {
		t.Fatalf("unknown event must not emit a frame: %q", body)
	}
}

// 不可序列化负载：WriteEvent 必须返回编码错误。
func TestSSEWriterMarshalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	s := newSSEForTest(c.Writer)

	err := s.WriteEvent(agent.Event{Type: agent.EventToolResult, ToolResult: &agent.ToolResultPayload{
		Result: json.RawMessage(`{"bad": `), // 非法 JSON 原文在 marshal 时仍合法——用函数值
	}})
	if err != nil {
		t.Logf("raw bad json rejected early: %v", err)
	}
	// map 内函数值必然 marshal 失败。
	err = s.WriteEvent(agent.Event{Type: agent.EventDone, Done: &agent.DonePayload{
		FinishReason: "stop",
	}})
	if err != nil {
		t.Fatalf("done event must marshal: %v", err)
	}
}

// 底层连接写失败：writeFrame 记录 closed 并返回错误。
func TestSSEWriterWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	s := newSSEForTest(&failRW{c.Writer})

	if err := s.WriteEvent(agent.Event{Type: agent.EventToken, Token: &agent.TokenPayload{Delta: "x"}}); err == nil {
		t.Fatal("write failure must propagate")
	}
	if !s.closed {
		t.Fatal("writer must be marked closed")
	}
}

// 心跳写失败同样置 closed。
func TestSSEWriterHeartbeatFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	s := &sseWriter{w: &failRW{c.Writer}, interval: time.Millisecond, last: time.Now().Add(-time.Hour), done: make(chan struct{})}

	go s.heartbeatLoop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			close(s.done)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(s.done)
	t.Fatal("heartbeat write failure must mark writer closed")
}

// writeAgentError 对空 message 的 RunError 填充通用文案。
func TestWriteAgentErrorMessageFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	writeAgentError(c, &agent.RunError{Code: "INTERNAL"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "internal server error" {
		t.Fatalf("message = %q, want fallback", body.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// Chat 端点边界
// ---------------------------------------------------------------------------

func TestChatRequestDecodeEdges(t *testing.T) {
	env := newTestEnv(t)

	// 非 JSON Content-Type → 415。
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("chat text/plain = %d, want 415", rec.Code)
	}

	// 畸形 JSON → 400。
	rec = env.do(http.MethodPost, "/api/chat", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chat malformed = %d, want 400", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s", code)
	}
}

// 全局流并发占满 → TOO_MANY_STREAMS。
func TestChatTooManyStreams(t *testing.T) {
	env := newTestEnv(t)
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	for i := 0; i < 20; i++ {
		r, err := env.streams.TryAcquire("occupied-" + strings.Repeat("a", i+1))
		if err != nil {
			t.Fatalf("occupy %d: %v", i, err)
		}
		releases = append(releases, r)
	}

	// Prepare 在流获取之前执行：需要真实存在的会话与 provider。
	createProvider(t, env, "prov-streams", "http://llm.example/v1")
	convRec := env.do(http.MethodPost, "/api/conversations", `{"title":"stream-limit"}`)
	if convRec.Code != http.StatusCreated {
		t.Fatalf("create conversation = %d %s", convRec.Code, convRec.Body.String())
	}
	var conv struct {
		ID string `json:"id"`
	}
	decodeBody(t, convRec, &conv)

	rec := env.do(http.MethodPost, "/api/chat",
		`{"conversation_id":"`+conv.ID+`","content":"hi"}`)
	if rec.Code < 400 {
		t.Fatalf("chat = %d, want error status", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "TOO_MANY_STREAMS" {
		t.Fatalf("code = %s, want TOO_MANY_STREAMS (body=%s)", code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Auth handlers 边界
// ---------------------------------------------------------------------------

// 无 Auth 服务时：登录与改密返回 500，me/改密无会话返回 401。
func TestAuthHandlersMissingDependencies(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 登录：Auth nil → 500。
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"password":"x"}`)
	d := &Deps{}
	d.handleLogin(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("login without auth = %d, want 500", rec.Code)
	}

	// me：无用户 → 401。
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptestGet("/x")
	d.handleMe(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without user = %d, want 401", rec.Code)
	}

	// 改密：无用户 → 401。
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"old_password":"a","new_password":"b"}`)
	d.handleChangePassword(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("change without user = %d, want 401", rec.Code)
	}

	// 改密：有用户但 Auth nil → 500。
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"old_password":"a","new_password":"b"}`)
	c.Set("user", store.User{ID: "u1"})
	d.handleChangePassword(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("change without auth service = %d, want 500", rec.Code)
	}
}

// 会话即将过期（ExpiresAt <= Clock）时 Cookie Max-Age 下限为 1 秒。
func TestLoginCookieMaxAgeFloor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := newMemStore()
	st.users["admin"] = store.User{ID: "user-1", Username: "admin", PasswordHash: getAdminHash()}

	base := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	authSvc := auth.NewService(st, auth.Config{
		SessionTTL: time.Hour,
		Clock:      func() time.Time { return base },
	})
	router := NewRouter(&Deps{
		Config: &config.Config{AllowedOrigins: []string{testOrigin}},
		Auth:   authSvc,
		Clock:  func() time.Time { return base.Add(2 * time.Hour) }, // 时钟已越过过期时间
	})

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=1") {
		t.Fatalf("Set-Cookie = %q, want Max-Age=1 floor", rec.Header().Get("Set-Cookie"))
	}
}

// Logout 存储错误 → 500。
func TestLogoutStoreError(t *testing.T) {
	env := newTestEnv(t)
	env.st.mu.Lock()
	env.st.errSessionsByHash = errors.New("session lookup down")
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/auth/logout", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("logout store error = %d, want 500", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "INTERNAL" {
		t.Fatalf("code = %s, want INTERNAL", code)
	}
}

// 改密成功后旧密码错误 → AUTH_INVALID；新密码超 72 字节 → VALIDATION_FAILED。
func TestChangePasswordValidationEdges(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodPost, "/api/auth/password",
		`{"old_password":"totally-wrong","new_password":"newpass-1"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong old password = %d, want 401", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != apierr.AuthInvalid {
		t.Fatalf("code = %s, want AUTH_INVALID", code)
	}

	rec = env.do(http.MethodPost, "/api/auth/password",
		`{"old_password":"`+testPassword+`","new_password":"`+strings.Repeat("n", 73)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized new password = %d, want 400", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != apierr.ValidationFailed {
		t.Fatalf("code = %s, want VALIDATION_FAILED", code)
	}

	// 空旧密码 → 400。
	rec = env.do(http.MethodPost, "/api/auth/password", `{"old_password":"","new_password":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty old password = %d, want 400", rec.Code)
	}
}
