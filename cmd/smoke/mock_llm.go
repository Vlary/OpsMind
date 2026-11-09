package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// mockModel 是假供应商对外暴露的唯一模型。
const mockModel = "mock-model"

// mockLLM 是 OpenAI 兼容的本地假供应商（CONTRACT.md 9.3 补充）。
//
// 行为固定为两轮：当最后一条上游消息不是 tool 结果时返回一次
// list_servers 工具调用（arguments 为 {}）；否则返回最终文本
// "echo:<最后一条 user 内容>"。这样同一进程内多个会话都能各自完成
// 工具调用 → 最终回答的循环，而不依赖全局调用计数。
type mockLLM struct {
	srv *httptest.Server

	mu      sync.Mutex
	streams int
}

func newMockLLM() *mockLLM {
	m := &mockLLM{}
	mux := http.NewServeMux()
	mux.HandleFunc("/models", m.handleModels)
	mux.HandleFunc("/chat/completions", m.handleChatCompletions)
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockLLM) URL() string { return m.srv.URL }

func (m *mockLLM) Close() { m.srv.Close() }

// Streams 返回已处理的 chat/completions 请求数（便于失败时定位）。
func (m *mockLLM) Streams() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams
}

func (m *mockLLM) handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + mockModel + `","object":"model","owned_by":"mock"}]}`))
}

type mockChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role      string          `json:"role"`
		Content   string          `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
	} `json:"messages"`
}

func (m *mockLLM) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req mockChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid request"}}`, http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.streams++
	m.mu.Unlock()

	lastRole := ""
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if lastRole == "" {
			lastRole = req.Messages[i].Role
		}
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Content
			break
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeFrame := func(payload string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}

	if lastRole != "tool" {
		// 第一轮：固定返回 list_servers 工具调用，arguments 为 {}。
		writeFrame(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"` + mockModel + `","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_mock_1","type":"function","function":{"name":"list_servers","arguments":"{}"}}]},"finish_reason":null}]}`)
		writeFrame(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"` + mockModel + `","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeFrame(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"` + mockModel + `","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
		writeFrame(`[DONE]`)
		return
	}

	// 第二轮：最终文本 echo:<最后一条 user 内容>。
	content := "echo:" + lastUser
	completionTokens := len([]rune(content))
	chunk, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"model":   mockModel,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil}},
	})
	writeFrame(string(chunk))
	writeFrame(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":"` + mockModel + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	usageChunk, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"model":   mockModel,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     21,
			"completion_tokens": completionTokens,
			"total_tokens":      21 + completionTokens,
		},
	})
	writeFrame(string(usageChunk))
	writeFrame(`[DONE]`)
}
