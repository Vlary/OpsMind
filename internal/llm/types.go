// Package llm 是供应商客户端、两套协议适配与 Registry 缓存。
package llm

import (
	"context"
	"encoding/json"

	"aiops/internal/store"
)

// Role 是 LLM 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 是模型请求的工具调用。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message 是上游消息。
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
}

// Tool 是发送给模型的工具定义。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request 是一次流式请求。
type Request struct {
	Model       string
	Messages    []Message
	Tools       []Tool
	Temperature float64
	MaxTokens   int
}

// StreamEventType 是流事件类型。
type StreamEventType string

const (
	EventToken     StreamEventType = "token"
	EventReasoning StreamEventType = "reasoning"
	EventToolCall  StreamEventType = "tool_call"
	EventDone      StreamEventType = "done"
)

// Usage 是 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamEvent 是内部统一流事件。
type StreamEvent struct {
	Type         StreamEventType
	Delta        string
	ToolCall     *ToolCall
	FinishReason string
	Usage        *Usage
}

// Client 是供应商客户端接口。
type Client interface {
	Stream(ctx context.Context, req Request, emit func(StreamEvent) error) error
	ListModels(ctx context.Context) ([]string, error)
	Test(ctx context.Context) error
}

// ProviderConfig 是构造客户端的配置。
type ProviderConfig struct {
	ID           string
	Name         string
	Kind         store.ProviderKind
	BaseURL      string
	APIKey       string
	DefaultModel string
}

// TestResult 是供应商连接测试结果。
type TestResult struct {
	LatencyMS  int64 `json:"latency_ms"`
	ModelCount int   `json:"model_count"`
}
