package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/agent"
	"aiops/internal/apierr"
	"aiops/internal/audit"
)

// chatRequest 对应 OpenAPI ChatRequest。
type chatRequest struct {
	ConversationID string `json:"conversation_id"`
	Content        string `json:"content"`
	ProviderID     string `json:"provider_id"`
	Model          string `json:"model"`
}

// handleChat 处理 POST /api/chat。
func (d *Deps) handleChat(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var req chatRequest
	if !decodeJSONBody(c, &req) {
		return
	}
	if msg := validateChatContent(req.Content); msg != "" {
		writeError(c, "VALIDATION_FAILED", msg)
		return
	}
	if strings.TrimSpace(req.ConversationID) == "" {
		writeError(c, "VALIDATION_FAILED", "conversation_id is required")
		return
	}
	d.streamChat(c, req.ConversationID, req.ProviderID, req.Model, req.Content)
}

func validateChatContent(content string) string {
	n := utf8.RuneCountInString(content)
	if n < 1 {
		return "content is required"
	}
	if n > 16000 {
		return "content is too long"
	}
	return ""
}

// streamChat 是所有会话入口共用的 SSE 管道：
// Prepare → StreamGuard → SSE 头 → Runner.Run → 关闭心跳。
func (d *Deps) streamChat(c *gin.Context, conversationID, providerID, model, content string) {
	if d.Agent == nil {
		writeError(c, "INTERNAL", "agent is not configured")
		return
	}
	if d.Streams == nil {
		writeError(c, "INTERNAL", "stream guard is not configured")
		return
	}
	userID := c.GetString("user_id")
	if userID == "" {
		writeError(c, "AUTH_REQUIRED", "authentication required")
		return
	}
	requestID := c.GetString("request_id")
	requestCtx := c.Request.Context()

	if _, err := d.Agent.Prepare(requestCtx, conversationID, providerID, model); err != nil {
		writeAgentError(c, err)
		return
	}

	release, err := d.Streams.TryAcquire(conversationID)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrStreamActive):
			writeError(c, "STREAM_ACTIVE", "conversation already has an active stream")
		case errors.Is(err, agent.ErrTooManyStreams):
			writeError(c, "TOO_MANY_STREAMS", "too many concurrent streams")
		default:
			writeError(c, "INTERNAL", "failed to acquire stream")
		}
		return
	}
	defer release()

	// 服务关闭时（RegisterOnShutdown → StreamGuard.ShutdownActive）主动取消
	// 活跃流：http.Server.Shutdown 不会断开活跃请求，绑请求 ctx 的 SSE 长流
	// 若不取消，排空窗口内永远等不完，随后资源关闭会让在途轮次的最终落库
	// （WithoutCancel+5s 路径）失败。取消后 Runner 走既有的 abort 落库路径。
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	d.Streams.Watch(conversationID, cancel)

	d.enqueueAudit(ctx, c, "chat.started", "conversation", conversationID, &conversationID, nil, map[string]any{
		"provider_id": providerID, "model": model,
	})

	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.Flush()

	writer := newSSEWriter(c.Writer)
	defer writer.Close()

	runErr := d.Agent.Run(ctx, agent.RunInput{
		UserID:         userID,
		ConversationID: conversationID,
		ProviderID:     providerID,
		Model:          model,
		Content:        content,
		RequestID:      requestID,
	}, writer.WriteEvent)

	if runErr != nil {
		code, _, _ := agentErrorInfo(runErr)
		action := "chat.failed"
		if code == "CLIENT_DISCONNECTED" || code == "STREAM_TIMEOUT" {
			action = "chat.cancelled"
		}
		d.enqueueAudit(ctx, c, action, "conversation", conversationID, &conversationID, nil, map[string]any{
			"code": code,
		})
		return
	}
	d.enqueueAudit(ctx, c, "chat.finished", "conversation", conversationID, &conversationID, nil, nil)
}

func writeAgentError(c *gin.Context, err error) {
	code, message, _ := agentErrorInfo(err)
	if message == "" {
		message = "internal server error"
	}
	c.AbortWithStatusJSON(statusForCode(code), gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": c.GetString("request_id"),
	}})
}

func agentErrorInfo(err error) (code, message string, retryable bool) {
	var re *agent.RunError
	if errors.As(err, &re) {
		return re.Code, re.Message, re.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "LLM_TIMEOUT", "request timed out", true
	}
	if errors.Is(err, context.Canceled) {
		return "CLIENT_DISCONNECTED", "client disconnected", false
	}
	return "INTERNAL", "internal server error", false
}

// statusForCode 覆盖 SSE 通道中新增的错误码；其余复用 apierr 的映射。
func statusForCode(code string) int {
	switch code {
	case "CLIENT_DISCONNECTED", "STREAM_TIMEOUT", "APPROVAL_STREAM_CANCELLED", "SERVER_SHUTTING_DOWN":
		return http.StatusOK
	default:
		return apierr.Status(code)
	}
}

// sseWriter 串行化事件与心跳。一个流只有一个 writer。
type sseWriter struct {
	mu       sync.Mutex
	w        gin.ResponseWriter
	interval time.Duration
	last     time.Time
	closed   bool
	done     chan struct{}
	closeOne sync.Once
}

func newSSEWriter(w gin.ResponseWriter) *sseWriter {
	writer := &sseWriter{
		w:        w,
		interval: 15 * time.Second,
		last:     time.Now(),
		done:     make(chan struct{}),
	}
	go writer.heartbeatLoop()
	return writer
}

// WriteEvent 把 agent 事件编码为单行 data 的 SSE 帧并立即 flush。
func (s *sseWriter) WriteEvent(ev agent.Event) error {
	var payload any
	switch ev.Type {
	case agent.EventToken, agent.EventReasoning:
		payload = ev.Token
	case agent.EventToolCall:
		payload = ev.ToolCall
	case agent.EventToolResult:
		payload = ev.ToolResult
	case agent.EventApprovalRequired:
		payload = ev.ApprovalRequired
	case agent.EventApprovalResolved:
		payload = ev.ApprovalResolved
	case agent.EventDone:
		payload = ev.Done
	case agent.EventError:
		payload = ev.Error
	default:
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.writeFrame(string(ev.Type), data)
}

func (s *sseWriter) writeFrame(event string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sse: stream is closed")
	}
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, '\n')
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	if _, err := s.w.Write(frame); err != nil {
		s.closed = true
		return err
	}
	s.last = time.Now()
	s.w.Flush()
	return nil
}

func (s *sseWriter) heartbeatLoop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.closed && time.Since(s.last) >= s.interval {
				if _, err := s.w.Write([]byte(": ping\n\n")); err != nil {
					s.closed = true
				} else {
					s.last = time.Now()
					s.w.Flush()
				}
			}
			s.mu.Unlock()
		}
	}
}

// Close 停止心跳；幂等。不会再写任何字节。
func (s *sseWriter) Close() {
	s.closeOne.Do(func() {
		close(s.done)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	})
}

// decodeJSONBody 严格要求 application/json、未知字段与尾随内容。
func decodeJSONBody(c *gin.Context, dst any) bool {
	if c.ContentType() != "application/json" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": gin.H{
			"code":       "VALIDATION_FAILED",
			"message":    "content type must be application/json",
			"request_id": c.GetString("request_id"),
		}})
		return false
	}
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(c, "VALIDATION_FAILED", "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(c, "VALIDATION_FAILED", "invalid request body")
		return false
	}
	return true
}

// enqueueAudit 写一条审计；失败只记录日志，不影响响应。
func (d *Deps) enqueueAudit(ctx context.Context, c *gin.Context, action, resourceType, resourceID string, conversationID, serverID *string, detail map[string]any) {
	if d.Audit == nil {
		return
	}
	e := audit.Event{
		ActorType:      "user",
		ActorID:        c.GetString("user_id"),
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		RequestID:      c.GetString("request_id"),
		ConversationID: conversationID,
		ServerID:       serverID,
		IP:             c.ClientIP(),
		Detail:         detail,
	}
	// chat.cancelled 审计时请求 ctx 已取消；Enqueue 的 select 在 channel 可
	// 发送与 ctx.Done 同时就绪时伪随机选择，必须用 WithoutCancel 保证事件落库。
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.Audit.Enqueue(enqueueCtx, e); err != nil && d.Log != nil {
		d.Log.Warn("audit_enqueue_failed", "action", action, "error", err)
	}
}
