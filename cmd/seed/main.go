// Command seed 通过运行中的 OpsMind API 播种一套演示数据：两台示例服务器、
// 一个指向假 LLM（web/e2e/fixtures/fake-llm.mjs）的供应商和一个演示会话，
// 免去演示前逐页手工录入。幂等：同名资产已存在时跳过。
//
// 用法（后端需已启动且完成数据库迁移）：
//
//	make seed                                # 仅资产（服务器/供应商/会话）
//	make seed CHAT=1                         # 额外在演示会话发一条消息（需假 LLM）
//	go run ./cmd/seed -base-url http://localhost:8080 -password <管理员密码> -chat
//
// 无 Key 演示链路：先启动假 LLM（node web/e2e/fixtures/fake-llm.mjs），
// 再 make seed CHAT=1；供应商 base_url 默认指向 http://127.0.0.1:18081。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"
)

func main() {
	baseURL := flag.String("base-url", envDefault("http://localhost:8080", "SEED_BASE_URL"), "后端地址（同时作为写请求 Origin，需在 AIOPS_ALLOWED_ORIGINS 白名单内）")
	password := flag.String("password", os.Getenv("ADMIN_PASSWORD"), "管理员密码（默认取 ADMIN_PASSWORD 环境变量）")
	llmBaseURL := flag.String("llm-base-url", envDefault("http://127.0.0.1:18081", "SEED_LLM_BASE_URL"), "演示供应商指向的 OpenAI 兼容地址（默认为 e2e 假 LLM 端口）")
	chat := flag.Bool("chat", false, "在演示会话发送一条示例消息（需要 LLM 可达）")
	flag.Parse()

	if strings.TrimSpace(*password) == "" {
		fmt.Fprintln(os.Stderr, "SEED_FAILED: 管理员密码为空：用 -password 或设置 ADMIN_PASSWORD")
		os.Exit(1)
	}

	client, err := newSeedClient(*baseURL, *password)
	if err != nil {
		fail("%v", err)
	}
	if err := runSeed(client, seedOptions{llmBaseURL: strings.TrimRight(*llmBaseURL, "/"), chat: *chat}); err != nil {
		fail("%v", err)
	}
	fmt.Println("SEED_OK")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "SEED_FAILED: "+format+"\n", args...)
	os.Exit(1)
}

func envDefault(fallback, key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

type seedOptions struct {
	llmBaseURL string
	chat       bool
}

// seedClient 是带会话 Cookie 与固定 Origin 的最小 API 客户端。
type seedClient struct {
	base   string
	origin string
	hc     *http.Client
}

func newSeedClient(baseURL, password string) (*seedClient, error) {
	base := strings.TrimRight(baseURL, "/")
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("创建 cookie jar: %w", err)
	}
	c := &seedClient{
		base:   base,
		origin: base,
		hc:     &http.Client{Jar: jar, Timeout: 60 * time.Second},
	}
	if err := c.do("POST", "/api/auth/login", map[string]string{"password": password}, nil, http.StatusOK); err != nil {
		return nil, fmt.Errorf("登录失败（检查 -password / ADMIN_PASSWORD）: %w", err)
	}
	return c, nil
}

// do 发送 JSON 请求并把响应解析到 out（out 可为 nil）。
func (c *seedClient) do(method, path string, body any, out any, wantStatus ...int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求体: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("构造请求 %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("读取响应 %s %s: %w", method, path, err)
	}
	okStatus := false
	for _, want := range wantStatus {
		if resp.StatusCode == want {
			okStatus = true
			break
		}
	}
	if !okStatus {
		snippet := data
		if len(snippet) > 2048 {
			snippet = snippet[:2048]
		}
		return fmt.Errorf("%s %s: 状态码 %d，响应 %s", method, path, resp.StatusCode, string(snippet))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析响应 %s %s: %w", method, path, err)
		}
	}
	return nil
}

// namedItem 是 {id, name} 形式的资产（servers / providers）。
type namedItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func listNamed(c *seedClient, path string) ([]namedItem, error) {
	var page struct {
		Items []namedItem `json:"items"`
	}
	if err := c.do("GET", path, nil, &page, http.StatusOK); err != nil {
		return nil, err
	}
	return page.Items, nil
}

func findNamed(items []namedItem, name string) (namedItem, bool) {
	for _, item := range items {
		if item.Name == name {
			return item, true
		}
	}
	return namedItem{}, false
}

// ensure 幂等地确保名为 name 的资产存在：已存在返回其 id；不存在则创建。
func ensure(c *seedClient, kind, listPath, name string, create func() (string, error)) (string, error) {
	items, err := listNamed(c, listPath)
	if err != nil {
		return "", fmt.Errorf("列出%s: %w", kind, err)
	}
	if hit, ok := findNamed(items, name); ok {
		fmt.Printf("跳过 %s「%s」（已存在，id=%s）\n", kind, name, hit.ID)
		return hit.ID, nil
	}
	id, err := create()
	if err != nil {
		return "", fmt.Errorf("创建%s「%s」: %w", kind, name, err)
	}
	fmt.Printf("已创建 %s「%s」（id=%s）\n", kind, name, id)
	return id, nil
}

func runSeed(c *seedClient, opts seedOptions) error {
	// 1) 示例服务器：192.0.2.x 是文档保留地址，SSH 不会真的连通，
	//    用于演示资产列表与风险预览，不产生真实执行。
	for _, server := range []struct {
		name string
		host string
		tags []string
	}{
		{name: "demo-web-1", host: "192.0.2.10", tags: []string{"demo", "web"}},
		{name: "demo-db-1", host: "192.0.2.11", tags: []string{"demo", "db"}},
	} {
		server := server
		_, err := ensure(c, "服务器", "/api/servers", server.name, func() (string, error) {
			var created namedItem
			if err := c.do("POST", "/api/servers", map[string]any{
				"name":        server.name,
				"host":        server.host,
				"port":        22,
				"username":    "root",
				"auth_method": "password",
				"password":    "demo-password-not-connectable",
				"tags":        server.tags,
			}, &created, http.StatusCreated); err != nil {
				return "", err
			}
			return created.ID, nil
		})
		if err != nil {
			return err
		}
	}

	// 2) 演示供应商：默认指向 e2e 假 LLM，演示无需真实 API Key。
	if _, err := ensure(c, "供应商", "/api/providers", "demo-provider", func() (string, error) {
		var created namedItem
		if err := c.do("POST", "/api/providers", map[string]any{
			"name":          "demo-provider",
			"preset":        "custom",
			"kind":          "openai_compatible",
			"base_url":      opts.llmBaseURL,
			"api_key":       "sk-demo-not-a-real-key",
			"default_model": "demo-model",
			"models":        []string{"demo-model"},
		}, &created, http.StatusCreated); err != nil {
			return "", err
		}
		return created.ID, nil
	}); err != nil {
		return err
	}

	// 3) 演示会话（列表项字段是 title 不是 name，单独处理）。
	conversationID, err := ensureConversation(c, "演示：资产巡检")
	if err != nil {
		return err
	}

	// 4) 可选：在演示会话发一条消息走通完整链路（需 LLM 可达）。
	if opts.chat {
		if err := seedChat(c, conversationID); err != nil {
			return err
		}
	}
	return nil
}

func ensureConversation(c *seedClient, title string) (string, error) {
	var page struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := c.do("GET", "/api/conversations", nil, &page, http.StatusOK); err != nil {
		return "", fmt.Errorf("列出会话: %w", err)
	}
	for _, item := range page.Items {
		if item.Title == title {
			fmt.Printf("跳过 会话「%s」（已存在，id=%s）\n", title, item.ID)
			return item.ID, nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.do("POST", "/api/conversations", map[string]string{"title": title}, &created, http.StatusCreated); err != nil {
		return "", fmt.Errorf("创建会话「%s」: %w", title, err)
	}
	fmt.Printf("已创建 会话「%s」（id=%s）\n", title, created.ID)
	return created.ID, nil
}

// seedChat 发送一条演示消息并等待 SSE 结束（done 或 error）。
func seedChat(c *seedClient, conversationID string) error {
	encoded, err := json.Marshal(map[string]any{
		"conversation_id": conversationID,
		"content":         "__scenario_list__ 列出所有已配置的服务器",
	})
	if err != nil {
		return fmt.Errorf("编码消息: %w", err)
	}
	req, err := http.NewRequest("POST", c.base+"/api/chat", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("构造聊天请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", c.origin)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("发送演示消息（确认假 LLM 已启动）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("演示消息失败：状态码 %d，响应 %s", resp.StatusCode, string(data))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		if event, ok := strings.CutPrefix(scanner.Text(), "event:"); ok {
			if strings.TrimSpace(event) == "error" {
				return fmt.Errorf("演示消息收到 error 事件（确认假 LLM 已启动并返回 SSE）")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取演示消息响应: %w", err)
	}
	fmt.Println("已在演示会话发送示例消息")
	return nil
}
