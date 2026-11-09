package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strings"
	"time"
)

const (
	// smokeOrigin 是所有写请求固定携带的 Origin（CONTRACT.md 9.3）。
	smokeOrigin = "http://localhost:5173"
	// smokeAdminPassword 是冒烟专用管理员密码（测试库专用，非生产凭据）。
	smokeAdminPassword = "smoke-admin-password-9f3c"
	// bodySnippet 是失败信息中允许打印的响应体字节数（CONTRACT.md 9.3）。
	bodySnippet = 2048
)

// apiErrorBody 对应统一错误体。
type apiErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// apiResponse 是缓冲后的 HTTP 响应。
type apiResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *apiResponse) snippet() string {
	if len(r.Body) <= bodySnippet {
		return string(r.Body)
	}
	return string(r.Body[:bodySnippet]) + "...(truncated)"
}

// fail 统一构造失败信息：步骤、状态码、响应体前 2KB。
func (r *apiResponse) fail(step, format string, args ...any) error {
	return fmt.Errorf("%s: %s (status=%d body=%s)", step, fmt.Sprintf(format, args...), r.Status, r.snippet())
}

// decodeError 解析统一错误体；成功返回 false。
func (r *apiResponse) decodeError() (apiErrorBody, bool) {
	var e apiErrorBody
	if err := json.Unmarshal(r.Body, &e); err != nil {
		return e, false
	}
	return e, e.Error.Code != ""
}

// smokeHTTP 是带 CookieJar 与固定 Origin 的 HTTP 客户端。
type smokeHTTP struct {
	base   string
	origin string
	hc     *http.Client
}

func newSmokeHTTP(baseURL string) *smokeHTTP {
	jar, _ := cookiejar.New(nil)
	return &smokeHTTP{
		base:   strings.TrimRight(baseURL, "/"),
		origin: smokeOrigin,
		hc:     &http.Client{Jar: jar, Timeout: 60 * time.Second},
	}
}

// do 发送缓冲请求。body 为 nil 表示无请求体；omitOrigin=true 时不带 Origin。
func (c *smokeHTTP) do(step, method, path string, body any, omitOrigin bool) (*apiResponse, error) {
	resp, err := c.open(step, method, path, body, omitOrigin)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", step, err)
	}
	return &apiResponse{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// open 发起请求并返回未读取的响应（SSE 用）。
func (c *smokeHTTP) open(step, method, path string, body any, omitOrigin bool) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s: encode request: %w", step, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", step, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !omitOrigin {
		req.Header.Set("Origin", c.origin)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s: %w", step, method, path, err)
	}
	return resp, nil
}

// expectStatus 断言状态码；失败时包含响应体。
func expectStatus(step string, resp *apiResponse, want int) error {
	if resp.Status != want {
		if e, ok := resp.decodeError(); ok {
			return resp.fail(step, "want status %d, got %d code=%s message=%q", want, resp.Status, e.Error.Code, e.Error.Message)
		}
		return resp.fail(step, "want status %d, got %d", want, resp.Status)
	}
	return nil
}

// expectErrorCode 断言状态码与统一错误码。
func expectErrorCode(step string, resp *apiResponse, wantStatus int, wantCode string) error {
	if resp.Status != wantStatus {
		return resp.fail(step, "want status %d, got %d", wantStatus, resp.Status)
	}
	e, ok := resp.decodeError()
	if !ok {
		return resp.fail(step, "response is not a JSON error body")
	}
	if e.Error.Code != wantCode {
		return resp.fail(step, "want error code %s, got %s", wantCode, e.Error.Code)
	}
	return nil
}

// decodeJSON 解析响应体。
func decodeJSON(step string, resp *apiResponse, dst any) error {
	if err := json.Unmarshal(resp.Body, dst); err != nil {
		return resp.fail(step, "invalid json: %v", err)
	}
	return nil
}

// containsAny 返回第一个出现在 body 中的敏感值；未出现返回空串。
func containsAny(body []byte, secrets ...string) string {
	for _, s := range secrets {
		if s != "" && bytes.Contains(body, []byte(s)) {
			return s
		}
	}
	return ""
}

// assertNoSecrets 断言响应体不包含明文凭据或密文标记。
func assertNoSecrets(step string, body []byte, secrets ...string) error {
	if hit := containsAny(body, secrets...); hit != "" {
		return fmt.Errorf("%s: response leaks sensitive value %q", step, hit)
	}
	if bytes.Contains(body, []byte("v1:")) {
		return fmt.Errorf("%s: response leaks ciphertext (v1: prefix)", step)
	}
	return nil
}

// sseEvent 是解析后的一个 SSE 事件。
type sseEvent struct {
	Name string
	Data string
}

var legalSSEEvents = map[string]bool{
	"token":             true,
	"reasoning":         true,
	"tool_call":         true,
	"tool_result":       true,
	"approval_required": true,
	"approval_resolved": true,
	"done":              true,
	"error":             true,
}

// readSSE 读取完整 SSE 流；心跳注释被忽略。
func readSSE(step string, r io.Reader) ([]sseEvent, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var (
		events []sseEvent
		name   string
		data   []string
	)
	flush := func() {
		if name == "" && len(data) == 0 {
			return
		}
		events = append(events, sseEvent{Name: name, Data: strings.Join(data, "\n")})
		name = ""
		data = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// 心跳注释。
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: read sse stream: %w", step, err)
	}
	flush()
	return events, nil
}

// validateSSE 断言事件名合法、包含 token、以 done 结尾且没有 error。
func validateSSE(step string, events []sseEvent, wantContent string) error {
	if len(events) == 0 {
		return fmt.Errorf("%s: empty sse stream", step)
	}
	var tokens strings.Builder
	doneCount := 0
	for i, ev := range events {
		if !legalSSEEvents[ev.Name] {
			return fmt.Errorf("%s: illegal sse event %q at index %d", step, ev.Name, i)
		}
		switch ev.Name {
		case "error":
			return fmt.Errorf("%s: sse error event: %s", step, ev.Data)
		case "token":
			var payload struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
				return fmt.Errorf("%s: token event has invalid json: %v", step, err)
			}
			tokens.WriteString(payload.Delta)
		case "done":
			doneCount++
			if i != len(events)-1 {
				return fmt.Errorf("%s: done event is not last (index %d of %d)", step, i, len(events))
			}
		}
	}
	if doneCount != 1 {
		return fmt.Errorf("%s: want exactly one done event, got %d", step, doneCount)
	}
	if wantContent != "" && tokens.String() != wantContent {
		return fmt.Errorf("%s: want token stream %q, got %q", step, wantContent, tokens.String())
	}
	return nil
}

// p95 返回排序后的第 95 个样本（1-based），输入长度必须为 100。
func p95(durations []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[94]
}
