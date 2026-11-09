package llm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 适配器错误。调用方用 errors.Is 区分并映射为 HTTP 错误码；
// 错误文本只包含状态码与脱敏后的上游片段，绝不包含 API Key。
var (
	ErrUpstream      = errors.New("llm: upstream request failed")
	ErrTimeout       = errors.New("llm: upstream request timed out")
	ErrStream        = errors.New("llm: invalid stream response")
	ErrInvalidConfig = errors.New("llm: invalid provider config")
)

// maxAttempts 是单次调用允许的最大尝试次数（首次 + 2 次重试）。
const maxAttempts = 3

// retryBaseDelay 是首次重试的等待时间，之后按 2 倍指数递增。
// 声明为变量以便测试缩短等待。
var retryBaseDelay = 200 * time.Millisecond

// defaultClient 在调用方未提供 http.Client 时使用；不设置全局 Timeout，
// 否则会截断长流。超时由 context 与各阶段超时控制。
func defaultClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// normalizeBaseURL 校验并去掉尾部 "/"。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: base_url is required", ErrInvalidConfig)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%w: base_url must be an absolute http(s) URL", ErrInvalidConfig)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: base_url must not contain userinfo, query or fragment", ErrInvalidConfig)
	}
	return strings.TrimRight(raw, "/"), nil
}

// retryableStatus 判断状态码是否值得重试：只重试 5xx。
func retryableStatus(code int) bool { return code >= 500 }

// isRetryableError 判断请求错误是否属于网络层可重试错误。
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// sleepContext 可被 context 取消的等待。
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// timeoutError 把 context 错误包装为 ErrTimeout。
func timeoutError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, context.DeadlineExceeded)
	}
	return err
}

// doWithRetry 发起请求；仅网络错误与 5xx 重试，最多 maxAttempts 次。
// build 每次尝试都会重新构造请求（body 可重放）。secret 仅用于脱敏上游错误片段。
func doWithRetry(ctx context.Context, client *http.Client, secret string, build func() (*http.Request, error)) (*http.Response, error) {
	if client == nil {
		client = defaultClient()
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, retryBaseDelay<<uint(attempt-1)); err != nil {
				return nil, timeoutError(err)
			}
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, timeoutError(ctx.Err())
			}
			// 网络层错误必须携带脱敏后的底层原因（dial 拒绝 / DNS / TLS），
			// 供 httpapi 层透出给运维排障；只写 "network error" 无法区分故障类型。
			lastErr = fmt.Errorf("%w: network error: %s", ErrUpstream, sanitizeText(err.Error(), secret, 256))
			if !isRetryableError(err) {
				return nil, lastErr
			}
			continue
		}
		if retryableStatus(resp.StatusCode) {
			lastErr = fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, readSnippet(resp.Body, secret, 512))
			resp.Body.Close()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snippet := readSnippet(resp.Body, secret, 512)
			resp.Body.Close()
			return nil, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, snippet)
		}
		return resp, nil
	}
	return nil, lastErr
}

// readSnippet 读取响应体的一小段用于错误信息，先脱敏再截断。
func readSnippet(r io.Reader, secret string, max int) string {
	if r == nil {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(r, int64(max*2)))
	return sanitizeText(string(buf), secret, max)
}

var (
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
	skPattern     = regexp.MustCompile(`sk-[A-Za-z0-9._\-]{6,}`)
	spacePattern  = regexp.MustCompile(`\s+`)
)

// sanitizeText 脱敏并截断一段文本；不返回换行，便于单行日志。
func sanitizeText(s, secret string, max int) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	s = bearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = skPattern.ReplaceAllString(s, "[REDACTED]")
	s = strings.TrimSpace(spacePattern.ReplaceAllString(s, " "))
	if max > 0 && len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// sseFrame 是一条解析后的 SSE 事件。
type sseFrame struct {
	Event string
	Data  string
}

// readSSE 逐行读取 SSE；遇到空行派发事件。函数同步执行，不启动 goroutine。
// 注释行（":" 开头）与 id/retry 字段被忽略；多行 data 用 "\n" 连接。
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data []string
	hasData := false

	dispatch := func() error {
		if !hasData {
			event, data, hasData = "", nil, false
			return nil
		}
		frame := sseFrame{Event: event, Data: strings.Join(data, "\n")}
		event, data, hasData = "", nil, false
		return fn(frame)
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			switch {
			case line == "":
				if derr := dispatch(); derr != nil {
					return derr
				}
			case strings.HasPrefix(line, ":"):
				// 注释/心跳，忽略。
			default:
				field, value, ok := strings.Cut(line, ":")
				if !ok {
					field, value = line, ""
				}
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					event = value
				case "data":
					data = append(data, value)
					hasData = true
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: read failed", ErrStream)
			}
			if derr := dispatch(); derr != nil {
				return derr
			}
			return nil
		}
	}
}
