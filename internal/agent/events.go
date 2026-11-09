package agent

import (
	"encoding/json"
	"time"

	"aiops/internal/llm"
	"aiops/internal/store"
)

// EventType 是 agent 产出的事件类型。
type EventType string

const (
	EventToken            EventType = "token"
	EventReasoning        EventType = "reasoning"
	EventToolCall         EventType = "tool_call"
	EventToolResult       EventType = "tool_result"
	EventApprovalRequired EventType = "approval_required"
	EventApprovalResolved EventType = "approval_resolved"
	EventDone             EventType = "done"
	EventError            EventType = "error"
)

// TokenPayload 是 token/reasoning 事件负载。
type TokenPayload struct {
	Delta string `json:"delta"`
}

// ToolCallPayload 是工具调用事件负载。
type ToolCallPayload struct {
	ToolCallID     string          `json:"tool_call_id"`
	ProviderCallID string          `json:"provider_call_id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
}

// ToolError 是工具错误。
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ToolResultPayload 是工具结果事件负载。
type ToolResultPayload struct {
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	OK         bool            `json:"ok"`
	Result     json.RawMessage `json:"result"`
	Error      *ToolError      `json:"error"`
	DurationMS int64           `json:"duration_ms"`
	// ApprovalMode 携带本工具调用判定时的有效门限（六期 BE-P，契约 6.2：
	// 仅命令执行类工具 ssh_exec/container_action/batch_execute 非空，UI
	// 展示「在 <mode> 门限下执行」）；其余工具空串（序列化省略，前端判空）。
	ApprovalMode string `json:"approval_mode,omitempty"`
}

// ApprovalResolvedPayload 是审批结果事件负载。
type ApprovalResolvedPayload struct {
	ApprovalID string               `json:"approval_id"`
	Status     store.ApprovalStatus `json:"status"`
	DecidedAt  *time.Time           `json:"decided_at"`
	// EditedCommand 是 decide 批准且用户改写时非空（六期 BE-P，契约 6.2）；
	// 等待方（runner）执行该命令。
	EditedCommand string `json:"edited_command"`
}

// DonePayload 是完成事件负载。
type DonePayload struct {
	TurnID       string    `json:"turn_id"`
	MessageID    string    `json:"message_id"`
	FinishReason string    `json:"finish_reason"`
	Usage        llm.Usage `json:"usage"`
}

// ErrorPayload 是错误事件负载。
type ErrorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// Event 是 agent 产出的统一事件。
type Event struct {
	Type             EventType
	Token            *TokenPayload
	ToolCall         *ToolCallPayload
	ToolResult       *ToolResultPayload
	ApprovalRequired *store.Approval
	ApprovalResolved *ApprovalResolvedPayload
	Done             *DonePayload
	Error            *ErrorPayload
}

// RunInfo 是 Prepare 的解析结果。
type RunInfo struct {
	Conversation store.Conversation
	Provider     store.Provider
	Model        string
}

// RunInput 是 Run 入参。
type RunInput struct {
	UserID         string
	ConversationID string
	ProviderID     string
	Model          string
	Content        string
	RequestID      string
}
