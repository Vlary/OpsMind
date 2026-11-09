package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/audit"
	"aiops/internal/store"
)

// writeError 是 handler 的统一错误出口。
func writeError(c *gin.Context, code, message string) {
	apierr.Write(c, apierr.New(code, message))
}

// writeErrorStatus 用于错误码表未覆盖但契约要求特定状态码的场景（例如 415）。
func writeErrorStatus(c *gin.Context, status int, code, message string) {
	err := apierr.New(code, message)
	err.RequestID = c.GetString("request_id")
	c.AbortWithStatusJSON(status, gin.H{"error": err})
}

// bindJSONStrict 严格解析 JSON：拒绝未知字段与尾随数据（OpenAPI additionalProperties=false）。
func bindJSONStrict(c *gin.Context, dst any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(c, apierr.ValidationFailed, "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(c, apierr.ValidationFailed, "invalid request body")
		return false
	}
	return true
}

// handleStoreError 把 store 哨兵错误映射为 HTTP 错误，并记录内部错误。
func (d *Deps) handleStoreError(c *gin.Context, err error, notFoundCode string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(c, notFoundCode, "resource not found")
	case errors.Is(err, store.ErrConflict):
		writeError(c, apierr.Conflict, "resource conflict")
	case errors.Is(err, store.ErrInvalid):
		writeError(c, apierr.ValidationFailed, "invalid request")
	default:
		d.logHandlerError(c, "store operation failed", err)
		writeError(c, apierr.Internal, "internal server error")
	}
}

// logHandlerError 只记录错误与 request_id，绝不记录请求体；敏感字段由 audit/logx 脱敏。
func (d *Deps) logHandlerError(c *gin.Context, msg string, err error) {
	if d.Log == nil {
		return
	}
	d.Log.ErrorContext(c.Request.Context(), "http_handler_error",
		"message", msg,
		"error", err.Error(),
		"request_id", requestIDOf(c),
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
	)
}

func requestIDOf(c *gin.Context) string {
	return c.GetString("request_id")
}

// queryLimit 解析 limit：缺省 50，合法范围 1..200。
func queryLimit(c *gin.Context) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return 50, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 200 {
		writeError(c, apierr.ValidationFailed, "limit must be between 1 and 200")
		return 0, false
	}
	return n, true
}

// queryBeforeSeq 解析 before_seq：缺省 0，最小 0。
func queryBeforeSeq(c *gin.Context) (int64, bool) {
	raw := c.Query("before_seq")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		writeError(c, apierr.ValidationFailed, "before_seq must be a non-negative integer")
		return 0, false
	}
	return n, true
}

// queryBeforeID 解析 before_id：缺省 0，最小 1。
func queryBeforeID(c *gin.Context) (int64, bool) {
	raw := c.Query("before_id")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		writeError(c, apierr.ValidationFailed, "before_id must be a positive integer")
		return 0, false
	}
	return n, true
}

// queryConversationLimit 解析会话列表 limit：缺省 200（保持旧契约首屏
// 上限不变），合法范围 1..200。
func queryConversationLimit(c *gin.Context) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return 200, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 200 {
		writeError(c, apierr.ValidationFailed, "limit must be between 1 and 200")
		return 0, false
	}
	return n, true
}

// queryConversationCursor 解析会话列表游标：before_last_message_at 与
// before_id 必须成对出现（RFC3339 时间 + uuid）；均缺省表示第一页。
func queryConversationCursor(c *gin.Context) (store.ConversationListCursor, bool) {
	at := c.Query("before_last_message_at")
	id := c.Query("before_id")
	if at == "" && id == "" {
		return store.ConversationListCursor{}, true
	}
	if at == "" || id == "" {
		writeError(c, apierr.ValidationFailed, "before_last_message_at and before_id must be provided together")
		return store.ConversationListCursor{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		writeError(c, apierr.ValidationFailed, "before_last_message_at must be an RFC3339 timestamp")
		return store.ConversationListCursor{}, false
	}
	if _, err := uuid.Parse(id); err != nil {
		writeError(c, apierr.ValidationFailed, "before_id must be a uuid")
		return store.ConversationListCursor{}, false
	}
	return store.ConversationListCursor{LastMessageAt: ts, ID: id}, true
}

// auditEnqueue 以请求上下文写入审计；失败只记录日志，不影响主响应。
func (d *Deps) auditEnqueue(c *gin.Context, e audit.Event) {
	if d.Audit == nil {
		return
	}
	if e.RequestID == "" {
		e.RequestID = requestIDOf(c)
	}
	if e.IP == "" {
		e.IP = d.requestIP(c)
	}
	if err := d.Audit.Enqueue(c.Request.Context(), e); err != nil {
		if d.Log != nil {
			d.Log.Warn("audit_enqueue_failed",
				slog.String("action", e.Action),
				slog.String("request_id", requestIDOf(c)),
				slog.Any("error", err),
			)
		}
	}
}

// requestIP 返回客户端 IP；TrustProxy 由 gin 的 trusted proxies 配置控制。
func (d *Deps) requestIP(c *gin.Context) string {
	return c.ClientIP()
}

// userFromContext 返回 requireAuth 注入的用户。
func userFromContext(c *gin.Context) (store.User, bool) {
	v, ok := c.Get("user")
	if !ok {
		return store.User{}, false
	}
	u, ok := v.(store.User)
	return u, ok
}
