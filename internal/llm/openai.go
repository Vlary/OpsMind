package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenAI 是 OpenAI 兼容适配器（OpenAI、DeepSeek、DashScope、Moonshot、
// 智谱、Gemini 兼容端点、Ollama、自定义端点）。
type OpenAI struct {
	cfg    ProviderConfig
	client *http.Client
	base   string
}

// NewOpenAI 构造 OpenAI 兼容客户端。
func NewOpenAI(cfg ProviderConfig, httpClient *http.Client) (*OpenAI, error) {
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = defaultClient()
	}
	return &OpenAI{cfg: cfg, client: httpClient, base: base}, nil
}

// openAIRequest 是 /chat/completions 请求体。
type openAIRequest struct {
	Model         string            `json:"model"`
	Messages      []openAIMessage   `json:"messages"`
	Tools         []openAITool      `json:"tools,omitempty"`
	Temperature   float64           `json:"temperature"`
	MaxTokens     int               `json:"max_tokens"`
	Stream        bool              `json:"stream"`
	StreamOptions *openAIStreamOpts `json:"stream_options,omitempty"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func buildOpenAIRequest(req Request) openAIRequest {
	temperature := req.Temperature
	if temperature == 0 {
		temperature = 0.2
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	msgs := make([]openAIMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := openAIMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, openAIToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: openAIFunctionCall{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			})
		}
		msgs = append(msgs, om)
	}
	tools := make([]openAITool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return openAIRequest{
		Model:         req.Model,
		Messages:      msgs,
		Tools:         tools,
		Temperature:   temperature,
		MaxTokens:     maxTokens,
		Stream:        true,
		StreamOptions: &openAIStreamOpts{IncludeUsage: true},
	}
}

func (c *OpenAI) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
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
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	return req, nil
}

// Stream 同步读取上游 SSE；emit 串行调用，不启动 goroutine。
func (c *OpenAI) Stream(ctx context.Context, req Request, emit func(StreamEvent) error) error {
	body, err := json.Marshal(buildOpenAIRequest(req))
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrInvalidConfig)
	}

	emitted := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := doWithRetry(ctx, c.client, c.cfg.APIKey, func() (*http.Request, error) {
			r, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
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
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return timeoutError(ctx.Err())
		}
		// 已经向前端发出字节时不得重试，避免重复 token。
		if emitted || !errors.Is(err, ErrStream) || attempt == maxAttempts-1 {
			return err
		}
	}
	return fmt.Errorf("%w: stream failed", ErrStream)
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type toolAccumulator struct {
	id   string
	name string
	args strings.Builder
}

// consume 解析 OpenAI SSE。工具调用参数累积后一次性 emit；JSON 非法时返回错误。
func (c *OpenAI) consume(body io.Reader, emit func(StreamEvent) error) error {
	accs := map[int]*toolAccumulator{}
	order := make([]int, 0, 4)
	finish := ""
	var usage *Usage
	done := false

	emitToolCalls := func() error {
		sort.Ints(order)
		for _, idx := range order {
			acc := accs[idx]
			if acc == nil || acc.name == "" {
				continue
			}
			args := strings.TrimSpace(acc.args.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return fmt.Errorf("%w: tool arguments are not valid JSON", ErrStream)
			}
			if err := emit(StreamEvent{
				Type: EventToolCall,
				ToolCall: &ToolCall{
					ID:        acc.id,
					Name:      acc.name,
					Arguments: json.RawMessage(args),
				},
			}); err != nil {
				return err
			}
		}
		return nil
	}

	err := readSSE(body, func(frame sseFrame) error {
		data := strings.TrimSpace(frame.Data)
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			done = true
			if err := emitToolCalls(); err != nil {
				return err
			}
			return emit(StreamEvent{Type: EventDone, FinishReason: normalizeFinish(finish), Usage: usage})
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("%w: malformed chunk", ErrStream)
		}
		if chunk.Error != nil {
			msg := sanitizeText(chunk.Error.Message, c.cfg.APIKey, 256)
			if msg == "" {
				msg = "upstream stream error"
			}
			return fmt.Errorf("%w: %s", ErrUpstream, msg)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if d := choice.Delta.ReasoningContent; d != "" {
				if err := emit(StreamEvent{Type: EventReasoning, Delta: d}); err != nil {
					return err
				}
			}
			if d := choice.Delta.Reasoning; d != "" {
				if err := emit(StreamEvent{Type: EventReasoning, Delta: d}); err != nil {
					return err
				}
			}
			if d := choice.Delta.Content; d != "" {
				if err := emit(StreamEvent{Type: EventToken, Delta: d}); err != nil {
					return err
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc, ok := accs[tc.Index]
				if !ok {
					acc = &toolAccumulator{}
					accs[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name += tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	// 上游未发送 [DONE] 就断开：按正常结束处理，但仍派发已累积内容。
	if err := emitToolCalls(); err != nil {
		return err
	}
	return emit(StreamEvent{Type: EventDone, FinishReason: normalizeFinish(finish), Usage: usage})
}

func normalizeFinish(reason string) string {
	switch reason {
	case "":
		return "stop"
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	default:
		return reason
	}
}

// ListModels 调用 GET {base}/models，排序去重。
func (c *OpenAI) ListModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	resp, err := doWithRetry(ctx, c.client, c.cfg.APIKey, func() (*http.Request, error) {
		return c.newRequest(ctx, http.MethodGet, "/models", nil)
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

// Test 调用 /models 验证连通性，不发起计费聊天。
func (c *OpenAI) Test(ctx context.Context) error {
	if _, err := c.ListModels(ctx); err != nil {
		return err
	}
	return nil
}

func sortUnique(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	sort.Strings(in)
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

var _ Client = (*OpenAI)(nil)
