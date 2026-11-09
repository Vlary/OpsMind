package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIStreamAssemblesFragmentedToolCalls(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization header = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []string{
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"ssh_exec\",\"arguments\":\"{\\\"server_id\\\":\"}}]}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"s1\\\",\\\"command\\\":\\\"ls -la\\\"}\"}}]}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n",
			"data: [DONE]\n\n",
		}
		// 以不自然的切分点写出，验证跨 chunk 的行重组。
		payload := strings.Join(frames, "")
		for i := 0; i < len(payload); i += 7 {
			end := i + 7
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write([]byte(payload[i:end]))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	client, err := NewOpenAI(ProviderConfig{BaseURL: server.URL, APIKey: "test-key"}, server.Client())
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	var events []StreamEvent
	err = client.Stream(context.Background(), Request{
		Model:    "m1",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, func(ev StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var reasoning, content strings.Builder
	var calls []*ToolCall
	var done *StreamEvent
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case EventReasoning:
			reasoning.WriteString(ev.Delta)
		case EventToken:
			content.WriteString(ev.Delta)
		case EventToolCall:
			calls = append(calls, ev.ToolCall)
		case EventDone:
			done = &events[i]
		}
	}
	if reasoning.String() != "think " {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if content.String() != "hello world" {
		t.Errorf("content = %q", content.String())
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "ssh_exec" {
		t.Errorf("tool call = %+v", calls[0])
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if args["server_id"] != "s1" || args["command"] != "ls -la" {
		t.Errorf("arguments = %v", args)
	}
	if done == nil {
		t.Fatal("missing done event")
	}
	if done.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", done.FinishReason)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", done.Usage)
	}
	if gotBody["stream"] != true {
		t.Errorf("stream = %v", gotBody["stream"])
	}
	opts, _ := gotBody["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Errorf("stream_options = %v", gotBody["stream_options"])
	}
}

func TestOpenAIRetriesOnlyNetworkAnd5xx(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = old }()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("temporary upstream failure"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, _ := NewOpenAI(ProviderConfig{BaseURL: server.URL, APIKey: "k"}, server.Client())
	var content strings.Builder
	err := client.Stream(context.Background(), Request{Model: "m"}, func(ev StreamEvent) error {
		if ev.Type == EventToken {
			content.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if content.String() != "ok" {
		t.Errorf("content = %q", content.String())
	}
}

func TestOpenAIDoesNotRetry4xx(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = old }()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer server.Close()

	client, _ := NewOpenAI(ProviderConfig{BaseURL: server.URL, APIKey: "k"}, server.Client())
	err := client.Stream(context.Background(), Request{Model: "m"}, func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestOpenAIErrorIsSanitized(t *testing.T) {
	const secret = "super-secret-api-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key sk-abcdef123456 and ` + secret + `"}}`))
	}))
	defer server.Close()

	client, _ := NewOpenAI(ProviderConfig{BaseURL: server.URL, APIKey: secret}, server.Client())
	err := client.Stream(context.Background(), Request{Model: "m"}, func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "sk-abcdef") {
		t.Fatalf("error leaks secret: %q", msg)
	}
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("error = %v, want ErrUpstream", err)
	}
}

func TestOpenAIInvalidToolArgumentsAreRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"ssh_exec\",\"arguments\":\"{not-json\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, _ := NewOpenAI(ProviderConfig{BaseURL: server.URL, APIKey: "k"}, server.Client())
	err := client.Stream(context.Background(), Request{Model: "m"}, func(StreamEvent) error { return nil })
	if err == nil || !errors.Is(err, ErrStream) {
		t.Fatalf("error = %v, want ErrStream", err)
	}
}

func TestOpenAIListModelsSortsAndDeduplicates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"b"},{"id":"a"},{"id":"b"}]}`))
	}))
	defer server.Close()

	client, _ := NewOpenAI(ProviderConfig{BaseURL: server.URL + "/", APIKey: "k"}, server.Client())
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if strings.Join(models, ",") != "a,b" {
		t.Errorf("models = %v", models)
	}
}

func TestOpenAIEmitErrorAborts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, _ := NewOpenAI(ProviderConfig{BaseURL: server.URL, APIKey: "k"}, server.Client())
	sentinel := errors.New("client gone")
	err := client.Stream(context.Background(), Request{Model: "m"}, func(StreamEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
}
