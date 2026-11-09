// Package logx 提供 JSON slog logger 与脱敏辅助。
//
// 只依赖标准库；不 import httpapi/store。
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// New 构建 JSON 格式的 slog.Logger，最小级别由参数决定。
func New(level slog.Level) *slog.Logger {
	return NewWithWriter(os.Stdout, level)
}

// NewWithWriter 允许测试注入输出目标。
func NewWithWriter(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}

type requestIDKey struct{}

// WithRequestID 把 request_id 放入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID 从 context 取 request_id，不存在时返回空串。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}
