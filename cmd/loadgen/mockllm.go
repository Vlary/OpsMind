package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// mockLLM 是内嵌的 OpenAI 兼容假供应商，真实监听 127.0.0.1 随机端口，
// 供被测服务端（独立进程）回调。
//
// 行为固定两轮：
//  1. 最后一条上游消息不是 tool 结果时，返回一次 ssh_exec 工具调用
//     （指向 bootstrap 创建的假服务器）——服务端风险路由会对它发出
//     approval_required，loadgen 自动批准后 SSH 失败（端口不可达），
//     工具结果回填；
//  2. 收到 tool 结果后，按 tokenDelay 逐 token 流出最终文本，模拟
//     真实的逐 token SSE。
type mockLLM struct {
	srv    *http.Server
	addr   string
	model  string
	delay  time.Duration
	tokens int

	serverID atomic.Value // string：ssh_exec 目标

	requests atomic.Int64
}

const mockModelName = "loadgen-mock-model"

func newMockLLM(tokenDelay time.Duration, tokenCount int) (*mockLLM, error) {
	m := &mockLLM{model: mockModelName, delay: tokenDelay, tokens: tokenCount}
	mux := http.NewServeMux()
	mux.HandleFunc("/models", m.handleModels)
	mux.HandleFunc("/chat/completions", m.handleChatCompletions)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m.addr = ln.Addr().String()
	m.srv = &http.Server{Handler: mux}
	go func() { _ = m.srv.Serve(ln) }()
	return m, nil
}

// URL 返回假供应商 base_url。
func (m *mockLLM) URL() string { return "http://" + m.addr }

// Requests 返回已处理的 chat/completions 请求数。
func (m *mockLLM) Requests() int64 { return m.requests.Load() }

// SetServerID 注入 ssh_exec 的目标服务器 ID。
func (m *mockLLM) SetServerID(id string) { m.serverID.Store(id) }

func (m *mockLLM) Close() { _ = m.srv.Close() }

func (m *mockLLM) handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","owned_by":"loadgen"}]}`, m.model)
}

type mockMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type mockChatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []mockMessage `json:"messages"`
}

func (m *mockLLM) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req mockChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid request"}}`, http.StatusBadRequest)
		return
	}
	m.requests.Add(1)

	lastRole := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "" {
			lastRole = req.Messages[i].Role
			break
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeFrame := func(payload string) {
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunkHeader := func(delta, finish any) string {
		chunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-loadgen", "object": "chat.completion.chunk", "model": m.model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return string(chunk)
	}

	if lastRole != "tool" {
		// 第一轮：ssh_exec 工具调用。uptime 是 safe 命令，但假服务器
		// auto_exec_enabled=false，服务端必然发出 approval_required。
		serverID, _ := m.serverID.Load().(string)
		if serverID == "" {
			// bootstrap 尚未完成时的兜底：无参 list_servers。
			writeFrame(chunkHeader(map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_list", "type": "function",
					"function": map[string]any{"name": "list_servers", "arguments": "{}"},
				}},
			}, nil))
		} else {
			args, _ := json.Marshal(map[string]any{
				"server_id": serverID, "command": "uptime", "timeout_seconds": 5,
			})
			writeFrame(chunkHeader(map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_ssh", "type": "function",
					"function": map[string]any{"name": "ssh_exec", "arguments": string(args)},
				}},
			}, nil))
		}
		writeFrame(chunkHeader(map[string]any{}, "tool_calls"))
		writeFrame(`{"id":"chatcmpl-loadgen","object":"chat.completion.chunk","model":"` + m.model + `","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
		writeFrame("[DONE]")
		return
	}

	// 第二轮：逐 token 流出最终文本。
	for i := 0; i < m.tokens; i++ {
		writeFrame(chunkHeader(map[string]any{"content": fmt.Sprintf("t%d ", i)}, nil))
		time.Sleep(m.delay)
	}
	writeFrame(chunkHeader(map[string]any{}, "stop"))
	usage, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-loadgen", "object": "chat.completion.chunk", "model": m.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": 21, "completion_tokens": m.tokens, "total_tokens": 21 + m.tokens,
		},
	})
	writeFrame(string(usage))
	writeFrame("[DONE]")
}
