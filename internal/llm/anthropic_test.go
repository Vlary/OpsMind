package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicStreamParsesThinkingAndToolUse(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "anthropic-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"pondering\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"ssh_exec\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"server_id\\\":\\\"s1\\\",\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"command\\\":\\\"ls\\\"}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		payload := strings.Join(frames, "")
		for i := 0; i < len(payload); i += 11 {
			end := i + 11
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write([]byte(payload[i:end]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	client, err := NewAnthropic(ProviderConfig{BaseURL: server.URL, APIKey: "anthropic-key"}, server.Client())
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	var events []StreamEvent
	err = client.Stream(context.Background(), Request{
		Model: "claude",
		Messages: []Message{
			{Role: RoleSystem, Content: "system rules"},
			{Role: RoleUser, Content: "help"},
			{Role: RoleAssistant, Content: "", ToolCalls: []ToolCall{{ID: "toolu_0", Name: "list_servers", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, ToolCallID: "toolu_0", Name: "list_servers", Content: `{"servers":[]}`},
		},
		Tools: []Tool{{Name: "ssh_exec", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, func(ev StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text, thinking strings.Builder
	var calls []*ToolCall
	var done *StreamEvent
	for i := range events {
		switch events[i].Type {
		case EventToken:
			text.WriteString(events[i].Delta)
		case EventReasoning:
			thinking.WriteString(events[i].Delta)
		case EventToolCall:
			calls = append(calls, events[i].ToolCall)
		case EventDone:
			done = &events[i]
		}
	}
	if text.String() != "Hi" || thinking.String() != "pondering" {
		t.Errorf("text=%q thinking=%q", text.String(), thinking.String())
	}
	if len(calls) != 1 || calls[0].ID != "toolu_1" || calls[0].Name != "ssh_exec" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if args["server_id"] != "s1" || args["command"] != "ls" {
		t.Errorf("args = %v", args)
	}
	if done == nil || done.FinishReason != "tool_use" {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage == nil || done.Usage.PromptTokens != 12 || done.Usage.CompletionTokens != 4 || done.Usage.TotalTokens != 16 {
		t.Errorf("usage = %+v", done.Usage)
	}

	if body["system"] != "system rules" {
		t.Errorf("system = %v", body["system"])
	}
	if body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	rawMessages, _ := body["messages"].([]any)
	if len(rawMessages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, merged user tool_result)", len(rawMessages))
	}
	last, _ := rawMessages[2].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("tool result role = %v", last["role"])
	}
	blocks, _ := last["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("tool result blocks = %d", len(blocks))
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "toolu_0" {
		t.Errorf("block = %v", block)
	}
}

func TestAnthropicErrorEventIsSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"bad key sk-supersecret123\"}}\n\n"))
	}))
	defer server.Close()

	client, _ := NewAnthropic(ProviderConfig{BaseURL: server.URL, APIKey: "anthropic-key"}, server.Client())
	err := client.Stream(context.Background(), Request{Model: "claude"}, func(StreamEvent) error { return nil })
	if err == nil || !errors.Is(err, ErrUpstream) {
		t.Fatalf("error = %v, want ErrUpstream", err)
	}
	if strings.Contains(err.Error(), "sk-supersecret123") {
		t.Fatalf("error leaks key-like token: %v", err)
	}
}

func TestNewAnthropicRequiresAPIKey(t *testing.T) {
	if _, err := NewAnthropic(ProviderConfig{BaseURL: "https://api.anthropic.com"}, nil); err == nil {
		t.Fatal("expected error for missing api key")
	}
}
