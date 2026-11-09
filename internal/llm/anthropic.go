package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicVersion 固定请求头版本。
const anthropicVersion = "2023-06-01"

// Anthropic 是 Anthropic Messages 适配器。
type Anthropic struct {
	cfg    ProviderConfig
	client *http.Client
	base   string
}

// NewAnthropic 构造 Anthropic 客户端。
func NewAnthropic(cfg ProviderConfig, httpClient *http.Client) (*Anthropic, error) {
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("%w: api key is required", ErrInvalidConfig)
	}
	if httpClient == nil {
		httpClient = defaultClient()
	}
	return &Anthropic{cfg: cfg, client: httpClient, base: base}, nil
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// buildAnthropicRequest 把统一消息转换为 Messages API 结构：
// system 独立顶层；tool 结果作为 user 的 tool_result block。
func buildAnthropicRequest(req Request) (anthropicRequest, error) {
	temperature := req.Temperature
	if temperature == 0 {
		temperature = 0.2
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	out := anthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Stream:      true,
	}
	systems := make([]string, 0, 1)
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				systems = append(systems, s)
			}
		case RoleUser:
			out.Messages = appendAnthropicBlocks(out.Messages, "user", []anthropicBlock{{Type: "text", Text: m.Content}})
		case RoleAssistant:
			blocks := make([]anthropicBlock, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Arguments
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			if len(blocks) > 0 {
				out.Messages = appendAnthropicBlocks(out.Messages, "assistant", blocks)
			}
		case RoleTool:
			out.Messages = appendAnthropicBlocks(out.Messages, "user", []anthropicBlock{{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}})
		default:
			return anthropicRequest{}, fmt.Errorf("%w: unsupported role %q", ErrInvalidConfig, m.Role)
		}
	}
	if len(systems) > 0 {
		out.System = strings.Join(systems, "\n\n")
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return out, nil
}

// appendAnthropicBlocks 合并相邻同角色消息（Anthropic 要求严格交替）。
func appendAnthropicBlocks(msgs []anthropicMessage, role string, blocks []anthropicBlock) []anthropicMessage {
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, anthropicMessage{Role: role, Content: blocks})
}

func (c *Anthropic) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrInvalidConfig)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	return req, nil
}

// Stream 同步读取 Messages 流；emit 串行调用。
func (c *Anthropic) Stream(ctx context.Context, req Request, emit func(StreamEvent) error) error {
	payload, err := buildAnthropicRequest(req)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrInvalidConfig)
	}

	emitted := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := doWithRetry(ctx, c.client, c.cfg.APIKey, func() (*http.Request, error) {
			r, err := c.newRequest(ctx, http.MethodPost, "/v1/messages", body)
			if err != nil {
				return nil, err
			}
			r.Header.Set("Accept", "text/event-stream")
			return r, nil
		})
		if err != nil {
			return err
		}
		err = c.consume(resp.Body, func(ev StreamEvent) error {
			emitted = true
			return emit(ev)
		})
		resp.Body.Close()
		// 只要向调用方发过事件，就不必（也不能）重试。
		if err == nil || emitted || ctx.Err() != nil {
			if ctx.Err() != nil && err != nil {
				return timeoutError(ctx.Err())
			}
			return err
		}
		if !errors.Is(err, ErrStream) || attempt == maxAttempts-1 {
			return err
		}
	}
	return fmt.Errorf("%w: stream failed", ErrStream)
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicBlockAcc struct {
	kind string
	id   string
	name string
	args strings.Builder
}

// consume 解析 Anthropic SSE。
func (c *Anthropic) consume(body io.Reader, emit func(StreamEvent) error) error {
	blocks := map[int]*anthropicBlockAcc{}
	var usage Usage
	finish := ""
	done := false

	err := readSSE(body, func(frame sseFrame) error {
		data := strings.TrimSpace(frame.Data)
		if data == "" {
			return nil
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("%w: malformed event", ErrStream)
		}
		typ := ev.Type
		if typ == "" {
			typ = frame.Event
		}
		switch typ {
		case "ping":
			return nil
		case "error":
			msg := "upstream stream error"
			if ev.Error != nil {
				msg = sanitizeText(ev.Error.Message, c.cfg.APIKey, 256)
			}
			return fmt.Errorf("%w: %s", ErrUpstream, msg)
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				usage.PromptTokens = ev.Message.Usage.InputTokens
				usage.CompletionTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				blocks[ev.Index] = &anthropicBlockAcc{
					kind: ev.ContentBlock.Type,
					id:   ev.ContentBlock.ID,
					name: ev.ContentBlock.Name,
				}
				if ev.ContentBlock.Type == "text" && ev.ContentBlock.Text != "" {
					if err := emit(StreamEvent{Type: EventToken, Delta: ev.ContentBlock.Text}); err != nil {
						return err
					}
				}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					return emit(StreamEvent{Type: EventToken, Delta: ev.Delta.Text})
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					return emit(StreamEvent{Type: EventReasoning, Delta: ev.Delta.Thinking})
				}
			case "input_json_delta":
				if acc, ok := blocks[ev.Index]; ok {
					acc.args.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			acc, ok := blocks[ev.Index]
			if !ok || acc.kind != "tool_use" || acc.name == "" {
				return nil
			}
			args := strings.TrimSpace(acc.args.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return fmt.Errorf("%w: tool arguments are not valid JSON", ErrStream)
			}
			return emit(StreamEvent{
				Type: EventToolCall,
				ToolCall: &ToolCall{
					ID:        acc.id,
					Name:      acc.name,
					Arguments: json.RawMessage(args),
				},
			})
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				finish = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					usage.PromptTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.CompletionTokens = ev.Usage.OutputTokens
				}
			}
		case "message_stop":
			done = true
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			return emit(StreamEvent{Type: EventDone, FinishReason: normalizeFinish(finish), Usage: &usage})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return emit(StreamEvent{Type: EventDone, FinishReason: normalizeFinish(finish), Usage: &usage})
}

// ListModels 调用 GET {base}/v1/models，排序去重。
func (c *Anthropic) ListModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	resp, err := doWithRetry(ctx, c.client, c.cfg.APIKey, func() (*http.Request, error) {
		return c.newRequest(ctx, http.MethodGet, "/v1/models?limit=1000", nil)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: malformed model list", ErrStream)
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if s := strings.TrimSpace(m.ID); s != "" {
			models = append(models, s)
		}
	}
	return sortUnique(models), nil
}

// Test 调用 /v1/models 验证连通性。
func (c *Anthropic) Test(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}

var _ Client = (*Anthropic)(nil)
