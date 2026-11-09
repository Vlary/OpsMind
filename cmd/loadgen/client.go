package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"time"
)

// loadClient 是一个绑定独立源 IP 的 API 客户端（独立 cookie jar = 独立会话）。
//
// 源 IP 错开是因为服务端登录限流按 IP 计令牌（internal/auth/limiter.go:
// capacity=5、refill=12s），N 个 worker 若都从 127.0.0.1 登录必然触发
// AUTH_RATE_LIMITED；Linux 环回 /8 均路由到本机，各 worker 用
// 127.0.0.(2+i) 即可互不挤占限流桶。
type loadClient struct {
	base   string
	origin string
	hc     *http.Client
	rec    *opRecorder

	// http5xx 由调用方在 onResp 里集中累计；这里只透传状态码。
	onResp func(status int, method, path string)
}

// loopbackIP 返回 worker i 的专属源 IP（127.0.0.2..127.0.0.254）。
func loopbackIP(i int) net.IP {
	last := 2 + i%253
	return net.IPv4(127, 0, 0, byte(last))
}

func newLoadClient(base string, workerID int, rec *opRecorder, onResp func(int, string, string)) (*loadClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	localIP := loopbackIP(workerID)
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			LocalAddr: &net.TCPAddr{IP: localIP},
		}).DialContext,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &loadClient{
		base:   base,
		origin: "http://localhost:5173",
		hc:     &http.Client{Jar: jar, Timeout: 120 * time.Second, Transport: transport},
		rec:    rec,
		onResp: onResp,
	}, nil
}

// doJSON 发送缓冲 JSON 请求并读回完整响应体。
func (c *loadClient) doJSON(ctx context.Context, op, method, path string, body any, out any) error {
	_, raw, err := c.raw(ctx, op, method, path, body)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s: decode response: %w", op, err)
		}
	}
	return nil
}

// raw 发送请求，返回状态码与响应体；非 2xx 视为错误。
func (c *loadClient) raw(ctx context.Context, op, method, path string, body any) (int, []byte, error) {
	start := time.Now()
	status, raw, err := c.roundtrip(ctx, method, path, body)
	c.rec.record(op, time.Since(start))
	if err != nil {
		c.rec.recordErr(op)
		return status, nil, err
	}
	if status < 200 || status >= 300 {
		c.rec.recordErr(op)
		return status, raw, fmt.Errorf("%s: status %d body %s", op, status, snippet(raw))
	}
	return status, raw, nil
}

func (c *loadClient) roundtrip(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", c.origin) // 写请求必须携带被允许的 Origin
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if c.onResp != nil {
		c.onResp(resp.StatusCode, method, path)
	}
	return resp.StatusCode, raw, nil
}

func snippet(b []byte) string {
	if len(b) > 512 {
		return string(b[:512]) + "...(truncated)"
	}
	return string(b)
}

// login 登录并对 AUTH_RATE_LIMITED 做固定退避重试（源 IP 已错开，正常不会触发）。
func (c *loadClient) login(ctx context.Context, op, password string) error {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		start := time.Now()
		status, raw, err := c.roundtrip(ctx, http.MethodPost, "/api/auth/login", map[string]string{"password": password})
		c.rec.record(op, time.Since(start))
		if err != nil {
			return err
		}
		if status == http.StatusOK {
			return nil
		}
		if status == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("login rate limited (attempt %d)", attempt+1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		return fmt.Errorf("login: status %d body %s", status, snippet(raw))
	}
	return lastErr
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// sseEvent 是解析后的一个 SSE 事件帧。
type sseEvent struct {
	Name string
	Data string
}

// openSSE 发起 SSE 请求并回调逐事件读取；返回整体耗时。
// onEvent 返回非 nil 时中断读取（用于提前失败）。
func (c *loadClient) openSSE(ctx context.Context, op, path string, body any, onEvent func(sseEvent) error) error {
	start := time.Now()
	defer func() { c.rec.record(op, time.Since(start)) }()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		c.rec.recordErr(op)
		return err
	}
	defer resp.Body.Close()
	if c.onResp != nil {
		c.onResp(resp.StatusCode, http.MethodPost, path)
	}
	if resp.StatusCode != http.StatusOK {
		c.rec.recordErr(op)
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: status %d body %s", op, resp.StatusCode, snippet(raw))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		c.rec.recordErr(op)
		return fmt.Errorf("%s: want text/event-stream, got %q", op, ct)
	}

	err = scanSSE(resp.Body, onEvent)
	if err != nil && err != errStreamDone {
		c.rec.recordErr(op)
	}
	if err == errStreamDone {
		return nil
	}
	return err
}

// scanSSE 逐行解析 SSE 流（event:/data: 帧，`:ping` 心跳忽略）。
func scanSSE(r io.Reader, onEvent func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var (
		name string
		data []string
	)
	flush := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		ev := sseEvent{Name: name, Data: joinLines(data)}
		name, data = "", nil
		if onEvent == nil {
			return nil
		}
		return onEvent(ev)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case bytes.HasPrefix([]byte(line), []byte(":")):
			// 心跳注释。
		case bytes.HasPrefix([]byte(line), []byte("event:")):
			name = trimSSEField(line, "event:")
		case bytes.HasPrefix([]byte(line), []byte("data:")):
			data = append(data, trimSSEField(line, "data:"))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sse stream: %w", err)
	}
	return flush()
}

func trimSSEField(line, prefix string) string {
	v := line[len(prefix):]
	for len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return v
}

func joinLines(lines []string) string {
	var b bytes.Buffer
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 业务 API 封装
// ---------------------------------------------------------------------------

type idBody struct {
	ID string `json:"id"`
}

func (c *loadClient) createConversation(ctx context.Context, op, title string) (string, error) {
	var conv idBody
	if err := c.doJSON(ctx, op, http.MethodPost, "/api/conversations", map[string]string{"title": title}, &conv); err != nil {
		return "", err
	}
	if conv.ID == "" {
		return "", fmt.Errorf("%s: empty conversation id", op)
	}
	return conv.ID, nil
}

func (c *loadClient) deleteConversation(ctx context.Context, op, id string) error {
	_, _, err := c.raw(ctx, op, http.MethodDelete, "/api/conversations/"+id, nil)
	return err
}

func (c *loadClient) renameConversation(ctx context.Context, op, id, title string) error {
	_, _, err := c.raw(ctx, op, http.MethodPatch, "/api/conversations/"+id, map[string]string{"title": title})
	return err
}

func (c *loadClient) createServer(ctx context.Context, op, name string) (string, error) {
	body := map[string]any{
		"name": name, "host": "127.0.0.1", "port": 1, "username": "root",
		"auth_method": "password", "password": "loadgen-fake", "tags": []string{"loadgen"},
		"auto_exec_enabled": false,
	}
	var srv idBody
	if err := c.doJSON(ctx, op, http.MethodPost, "/api/servers", body, &srv); err != nil {
		return "", err
	}
	return srv.ID, nil
}

func (c *loadClient) patchServer(ctx context.Context, op, id, name string) error {
	_, _, err := c.raw(ctx, op, http.MethodPatch, "/api/servers/"+id, map[string]any{
		"name": name, "tags": []string{"loadgen", "churn"},
	})
	return err
}

func (c *loadClient) deleteServer(ctx context.Context, op, id string) error {
	_, _, err := c.raw(ctx, op, http.MethodDelete, "/api/servers/"+id, nil)
	return err
}

func (c *loadClient) createProvider(ctx context.Context, op, name, baseURL string) (string, error) {
	body := map[string]any{
		"name": name, "preset": "custom", "kind": "openai_compatible",
		"base_url": baseURL, "api_key": "sk-loadgen-0000", "default_model": mockModelName,
		"models": []string{mockModelName},
	}
	var p idBody
	if err := c.doJSON(ctx, op, http.MethodPost, "/api/providers", body, &p); err != nil {
		return "", err
	}
	return p.ID, nil
}

func (c *loadClient) deleteProvider(ctx context.Context, op, id string) error {
	_, _, err := c.raw(ctx, op, http.MethodDelete, "/api/providers/"+id, nil)
	return err
}

// auditPage 拉取一页审计（limit<=200，服务端在 internal/httpapi/response.go 校验）。
func (c *loadClient) auditPage(ctx context.Context, op string, before int64) (int, bool, int64, error) {
	q := url.Values{}
	q.Set("limit", "200")
	if before > 0 {
		q.Set("before_id", strconv.FormatInt(before, 10))
	}
	var page struct {
		Items        []json.RawMessage `json:"items"`
		HasMore      bool              `json:"has_more"`
		NextBeforeID *int64            `json:"next_before_id"`
	}
	if err := c.doJSON(ctx, op, http.MethodGet, "/api/audit?"+q.Encode(), nil, &page); err != nil {
		return 0, false, 0, err
	}
	next := int64(0)
	if page.NextBeforeID != nil {
		next = *page.NextBeforeID
	}
	return len(page.Items), page.HasMore, next, nil
}

// decideApproval 批准一个审批（POST /api/approvals/:id/decide）。
func (c *loadClient) decideApproval(ctx context.Context, op, approvalID string) error {
	_, _, err := c.raw(ctx, op, http.MethodPost, "/api/approvals/"+approvalID+"/decide", map[string]string{
		"decision": "approve",
	})
	return err
}
