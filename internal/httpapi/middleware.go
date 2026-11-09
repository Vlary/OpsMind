package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/auth"
	"aiops/internal/logx"
)

const (
	jsonBodyLimit = 1 << 20  // 1MB
	chatBodyLimit = 64 << 10 // 64KB
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// recovery 捕获 panic，写统一 500；不打印请求体。
func (d *Deps) recovery() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		if d.Log != nil {
			d.Log.Error("panic_recovered",
				"request_id", c.GetString("request_id"),
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"panic", fmt.Sprint(recovered),
				"stack", string(debug.Stack()),
			)
		}
		if c.Writer.Written() {
			c.Abort()
			return
		}
		apierr.Write(c, apierr.New(apierr.Internal, "internal server error"))
	})
}

// requestID 接受合法的 X-Request-ID，否则生成 16 位小写十六进制。
func (d *Deps) requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(logx.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf)
}

// accessLog 输出结构化访问日志；不记录请求体或查询串。
func (d *Deps) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if d.Log == nil {
			return
		}
		d.Log.Info("http_request",
			"request_id", c.GetString("request_id"),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"user_id", c.GetString("user_id"),
			"ip", c.ClientIP(),
		)
	}
}

// securityHeaders 设置基础安全响应头。
func (d *Deps) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Next()
	}
}

// originCheck 对 /api/* 的写请求校验 Origin 精确匹配。
func (d *Deps) originCheck() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isWriteMethod(c.Request.Method) || !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Next()
			return
		}
		origin := c.GetHeader("Origin")
		if d.Config == nil || !d.Config.OriginAllowed(origin) {
			writeError(c, apierr.OriginForbidden, "origin not allowed")
			return
		}
		c.Next()
	}
}

// jsonContentType 要求 /api/* 的 JSON 写请求使用 application/json（无 body 的请求除外）。
func (d *Deps) jsonContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isWriteMethod(c.Request.Method) || !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Next()
			return
		}
		hasBody := c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0
		if !hasBody {
			c.Next()
			return
		}
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeErrorStatus(c, http.StatusUnsupportedMediaType, apierr.ValidationFailed, "content type must be application/json")
			return
		}
		c.Next()
	}
}

// bodyLimit 限制请求体大小；超限时后续 Bind 返回错误并映射为 400。
func (d *Deps) bodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		}
		c.Next()
	}
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// requireAuth 读取 opsmind_session Cookie 并注入 user/user_id。
func (d *Deps) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Auth == nil {
			writeError(c, apierr.Internal, "authentication unavailable")
			return
		}
		raw, err := c.Cookie(sessionCookieName)
		if err != nil || raw == "" {
			writeError(c, apierr.AuthRequired, "authentication required")
			return
		}
		user, err := d.Auth.Authenticate(c.Request.Context(), raw)
		if err != nil {
			if errors.Is(err, auth.ErrAuthRequired) {
				writeError(c, apierr.AuthRequired, "authentication required")
				return
			}
			d.logHandlerError(c, "authenticate failed", err)
			writeError(c, apierr.Internal, "internal server error")
			return
		}
		c.Set("user", user)
		c.Set("user_id", user.ID)
		c.Next()
	}
}
