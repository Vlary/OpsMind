package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/agent"
	"aiops/internal/store"
)

func newSSETestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	return c, rec
}

func TestSSEWriterFramesEventsAsSingleLineData(t *testing.T) {
	c, rec := newSSETestContext()
	sw := &sseWriter{w: c.Writer, interval: time.Hour, last: time.Now(), done: make(chan struct{})}

	if err := sw.WriteEvent(agent.Event{
		Type:  agent.EventToken,
		Token: &agent.TokenPayload{Delta: "line1\nline2"},
	}); err != nil {
		t.Fatalf("WriteEvent token: %v", err)
	}
	want := "event: token\ndata: {\"delta\":\"line1\\nline2\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}

	if err := sw.WriteEvent(agent.Event{
		Type: agent.EventToolResult,
		ToolResult: &agent.ToolResultPayload{
			ToolCallID: "tc-1", Name: "ssh_exec", OK: false,
			Result: nil, Error: &agent.ToolError{Code: "APPROVAL_DENIED", Message: "denied"},
		},
	}); err != nil {
		t.Fatalf("WriteEvent tool_result: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: tool_result\n") {
		t.Errorf("missing tool_result frame: %q", body)
	}
	if !strings.Contains(body, `"result":null`) {
		t.Errorf("result should be null on error: %q", body)
	}

	if err := sw.WriteEvent(agent.Event{
		Type:             agent.EventApprovalRequired,
		ApprovalRequired: &store.Approval{ID: "ap-1", ConversationID: testConvPtr("c1"), ToolCallID: testConvPtr("tc-1"), Status: store.ApprovalPending},
	}); err != nil {
		t.Fatalf("WriteEvent approval: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "event: approval_required\n") {
		t.Errorf("missing approval frame")
	}

	sw.Close()
	if err := sw.WriteEvent(agent.Event{Type: agent.EventDone, Done: &agent.DonePayload{}}); err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestSSEWriterHeartbeat(t *testing.T) {
	c, rec := newSSETestContext()
	sw := &sseWriter{
		w:        c.Writer,
		interval: 10 * time.Millisecond,
		last:     time.Now(),
		done:     make(chan struct{}),
	}
	go sw.heartbeatLoop()
	time.Sleep(45 * time.Millisecond)
	sw.Close()
	sw.mu.Lock()
	body := rec.Body.String()
	sw.mu.Unlock()
	if !strings.Contains(body, ": ping\n\n") {
		t.Fatalf("heartbeat missing: %q", body)
	}
}

func TestSSEWriterSerializesEventAndHeartbeat(t *testing.T) {
	c, rec := newSSETestContext()
	sw := &sseWriter{
		w:        c.Writer,
		interval: time.Millisecond,
		last:     time.Now(),
		done:     make(chan struct{}),
	}
	go sw.heartbeatLoop()
	for i := 0; i < 50; i++ {
		if err := sw.WriteEvent(agent.Event{Type: agent.EventToken, Token: &agent.TokenPayload{Delta: "x"}}); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	sw.Close()
	body := rec.Body.String()
	// 心跳不能插入事件帧内部：每个 event 后必须紧跟 data 与空行。
	for _, frame := range strings.Split(body, "\n\n") {
		if frame == "" || strings.HasPrefix(frame, ": ping") {
			continue
		}
		if !strings.HasPrefix(frame, "event: ") || !strings.Contains(frame, "\ndata: ") {
			t.Fatalf("interleaved frame: %q", frame)
		}
	}
}

func TestDecodeJSONBodyRejectsUnknownFieldsAndWrongType(t *testing.T) {
	c, _ := newSSETestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"content":"x","bogus":1}`))
	c.Request.Header.Set("Content-Type", "application/json")
	var req chatRequest
	if decodeJSONBody(c, &req) {
		t.Fatal("expected unknown field rejection")
	}
	if c.Writer.Status() != http.StatusBadRequest {
		t.Errorf("status = %d", c.Writer.Status())
	}

	c2, _ := newSSETestContext()
	c2.Request = httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"content":"x"}`))
	c2.Request.Header.Set("Content-Type", "text/plain")
	if decodeJSONBody(c2, &chatRequest{}) {
		t.Fatal("expected content type rejection")
	}
	if c2.Writer.Status() != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d", c2.Writer.Status())
	}
}

func TestRegisterRoutesInstallsBE3Endpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&Deps{}).RegisterRoutes(r.Group("/api"))

	routes := map[string]bool{}
	for _, route := range r.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	want := []string{
		"GET /api/providers/presets",
		"GET /api/providers",
		"POST /api/providers",
		"GET /api/providers/:id",
		"PATCH /api/providers/:id",
		"DELETE /api/providers/:id",
		"POST /api/providers/:id/test",
		"POST /api/providers/:id/models/refresh",
		"POST /api/chat",
	}
	for _, key := range want {
		if !routes[key] {
			t.Errorf("missing route %s", key)
		}
	}
	// 契约 2.5 与 api/openapi.yaml 均未定义会话路径式发送入口，
	// 不允许存在 POST /api/conversations/:id/messages 之类的隐藏可写路由。
	if routes["POST /api/conversations/:id/messages"] {
		t.Error("unexpected route POST /api/conversations/:id/messages (not in contract)")
	}
}
