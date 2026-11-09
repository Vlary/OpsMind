// Package agent 实现提示词、工具循环、风险路由与审批编排。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/dbproxy"
	"aiops/internal/llm"
	"aiops/internal/netdev"
	"aiops/internal/risk"
	"aiops/internal/sqlguard"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// 运行错误码（与 CONTRACT.md 9.6 一致，agent 包不 import apierr）。
const (
	codeConversationNotFound = "CONVERSATION_NOT_FOUND"
	codeModelLocked          = "CONVERSATION_MODEL_LOCKED"
	codeNoProvider           = "NO_PROVIDER_CONFIGURED"
	codeProviderNotFound     = "PROVIDER_NOT_FOUND"
	codeValidationFailed     = "VALIDATION_FAILED"
	codeMaxIterations        = "AGENT_MAX_ITERATIONS"
	codeLLMUpstream          = "LLM_UPSTREAM_ERROR"
	codeLLMTimeout           = "LLM_TIMEOUT"
	codeLLMTruncated         = "LLM_OUTPUT_TRUNCATED"
	codeLLMStream            = "LLM_STREAM_ERROR"
	codeStreamTimeout        = "STREAM_TIMEOUT"
	codeClientDisconnected   = "CLIENT_DISCONNECTED"
	codeInternal             = "INTERNAL"
	codeToolNotFound         = "TOOL_NOT_FOUND"
	codeToolArgsInvalid      = "TOOL_ARGS_INVALID"
	codeCommandForbidden     = "COMMAND_FORBIDDEN"
	codeCommandInvalid       = "COMMAND_INVALID"
	codeApprovalDenied       = "APPROVAL_DENIED"
	codeApprovalTimeout      = "APPROVAL_TIMEOUT"
	// codeApprovalCancelled 表示审批因回答流中断被连带取消（六期打磨
	// sprint 3）：审批落库为 expired + note=stream_cancelled，对应 tool_call
	// 必须收敛为 expired 并带该码——否则历史工具卡永久停在
	// awaiting_approval「待审批」，与同会话审批卡的「已取消」自相矛盾。
	// 与 APPROVAL_STREAM_CANCELLED（SSE 通道错误码）区分：后者描述响应流本身。
	codeApprovalCancelled = "APPROVAL_CANCELLED"
	codeSSHHostKeyChanged = "SSH_HOST_KEY_CHANGED"
	codeSSHTimeout        = "SSH_TIMEOUT"
	codeSSHExecFailed     = "SSH_EXEC_FAILED"
	codeSQLNotReadOnly    = "SQL_NOT_READONLY"
	codeSQLTimeout        = "SQL_TIMEOUT"
	codeSQLQueryFailed    = "SQL_QUERY_FAILED"

	// N1：网络设备 CLI（契约 9.6）。
	codeNetCLILoginFailed = "NETCLI_LOGIN_FAILED"
	codeNetCLIExecFailed  = "NETCLI_EXEC_FAILED"

	// C1 四期：容器管理（契约 9.6）。
	codeContainerUnsupported = "CONTAINER_UNSUPPORTED"
	codeContainerNotFound    = "CONTAINER_NOT_FOUND"
	codeContainerCLIFailed   = "CONTAINER_CLI_FAILED"

	// C1 四期：审批规则与来源（契约 2.9/9.7）。
	ruleContainerChange = "CONTAINER_CHANGE"
)

// RunError 是 Runner 返回的错误，携带 SSE 错误码与可重试标记。
type RunError struct {
	Code      string
	Message   string
	Retryable bool
	Err       error
}

func (e *RunError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message
	}
	return e.Code + ": " + e.Message
}

func (e *RunError) Unwrap() error { return e.Err }

func runErr(code, message string, retryable bool) *RunError {
	return &RunError{Code: code, Message: message, Retryable: retryable}
}

// ApprovalService 是 Runner 需要的审批能力。生产实现是 *approvals.Service；
// 单测可注入假实现。
type ApprovalService interface {
	Create(ctx context.Context, req approvals.CreateRequest) (store.Approval, error)
	Wait(ctx context.Context, approvalID string) (approvals.Decision, error)
	Cancel(ctx context.Context, approvalID, reason, requestID string) (store.Approval, error)
}

// Runner 是智能体执行器。
type Runner struct {
	st        store.Store
	registry  *llm.Registry
	ssh       sshx.Executor
	winrm     winrmx.Executor // W1：WinRM 认证（password_ntlm/password_basic）的执行通道
	netcli    netdev.Executor // N1：网络设备（device_kind=network_device）的执行通道
	db        dbproxy.Executor
	approvals ApprovalService
	audit     *audit.Writer
	cipher    *cryptox.Cipher
	cfg       RunnerConfig
	clock     func() time.Time
	log       *slog.Logger

	// client 非 nil 时绕过 registry，仅用于单测注入。
	client llm.Client

	// M3 五期：批量编排引擎（batchx.Service 经 BatchDispatcher 接口注入，
	// agent 不 import batchx——契约 2.3 依赖表）。
	batch BatchDispatcher

	// M2 五期：诊断引擎与巡检管理器（经 AttachPhase5 注入的最小接口，
	// agent 不 import diagnosis/inspection——契约 2.3 依赖表）。
	phase5Diagnosis  DiagnosisStarter
	phase5Inspection InspectionRunner
}

// RunnerConfig 是执行器上限配置。
type RunnerConfig struct {
	MaxIterations     int
	ContextMessages   int
	ContextBytes      int
	ToolResultBytes   int
	LLMCallTimeout    time.Duration
	SSHDefaultTimeout time.Duration
	SSHMaxTimeout     time.Duration
	StreamMaxDuration time.Duration
}

// NewRunner 构造执行器。
func NewRunner(
	st store.Store,
	registry *llm.Registry,
	ssh sshx.Executor,
	db dbproxy.Executor,
	approvalsSvc *approvals.Service,
	auditWriter *audit.Writer,
	cipher *cryptox.Cipher,
	cfg RunnerConfig,
	clock func() time.Time,
	log *slog.Logger,
) *Runner {
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 12
	}
	if cfg.ContextMessages == 0 {
		cfg.ContextMessages = 40
	}
	if cfg.ContextBytes == 0 {
		cfg.ContextBytes = 204800
	}
	if cfg.ToolResultBytes == 0 {
		cfg.ToolResultBytes = 65536
	}
	if cfg.LLMCallTimeout == 0 {
		cfg.LLMCallTimeout = 180 * time.Second
	}
	if cfg.SSHDefaultTimeout == 0 {
		cfg.SSHDefaultTimeout = 60 * time.Second
	}
	if cfg.SSHMaxTimeout == 0 {
		cfg.SSHMaxTimeout = 300 * time.Second
	}
	if cfg.StreamMaxDuration == 0 {
		cfg.StreamMaxDuration = 30 * time.Minute
	}
	if clock == nil {
		clock = time.Now
	}
	r := &Runner{
		st: st, registry: registry, ssh: ssh, db: db, audit: auditWriter,
		cipher: cipher, cfg: cfg, clock: clock, log: log,
	}
	// 避免把 nil *approvals.Service 装入非 nil 接口。
	if approvalsSvc != nil {
		r.approvals = approvalsSvc
	}
	return r
}

// SetLLMClient 注入假客户端，仅供单元测试使用。
func (r *Runner) SetLLMClient(client llm.Client) { r.client = client }

// SetApprovalService 注入审批服务（生产环境由 NewRunner 设置）。
func (r *Runner) SetApprovalService(svc ApprovalService) { r.approvals = svc }

// SetWinRMExecutor 注入 WinRM 执行通道（W1，契约 2.7：server.os=windows 且
// 认证为 password_ntlm/password_basic 时路由到 winrmx）。未注入时 WinRM
// 认证的服务器执行失败返回 SSH_EXEC_FAILED，不回退到 SSH。
func (r *Runner) SetWinRMExecutor(ex winrmx.Executor) { r.winrm = ex }

// SetNetCLIExecutor 注入网络设备执行通道（N1，契约 2.8：
// device_kind=network_device 时路由到 netdev）。未注入时网络设备执行失败
// 返回 SSH_EXEC_FAILED，不回退到 SSH 通道。
func (r *Runner) SetNetCLIExecutor(ex netdev.Executor) { r.netcli = ex }

// isNetworkDevice 报告资产是否为网络设备（契约 2.8 路由入口）。
func isNetworkDevice(srv store.Server) bool {
	return srv.DeviceKind == store.DeviceKindNetworkDevice
}

// netRiskBrand 把 store.NetdevBrand 映射为 risk.NetBrand（契约 7.3C 分册
// 路由键）；未知值失败关闭到 generic。
func netRiskBrand(b store.NetdevBrand) risk.NetBrand {
	switch b {
	case store.NetdevBrandCisco:
		return risk.NetBrandCisco
	case store.NetdevBrandHuawei:
		return risk.NetBrandHuawei
	case store.NetdevBrandH3C:
		return risk.NetBrandH3C
	case store.NetdevBrandJunos:
		return risk.NetBrandJunos
	default:
		return risk.NetBrandGeneric
	}
}

// riskOS 把服务器 OS 映射为风险分册（契约 7.2：windows 走 PowerShell 分册；
// network 由 ClassifyNet(generic) 兜底——知道厂商的调用方必须直接调
// ClassifyNet，见 netRiskClassify）。
func riskOS(srv store.Server) risk.OS {
	switch srv.OS {
	case store.ServerOSWindows:
		return risk.OSWindows
	case store.ServerOSNetwork:
		return risk.OSNetwork
	default:
		return risk.OSLinux
	}
}

// netRiskClassify 是命令风险分类入口（契约 2.8.7/7.3C）：网络设备按厂商
// 分册整体分类单条命令；其余资产按 os 分册。
func netRiskClassify(command string, srv store.Server) risk.Decision {
	if isNetworkDevice(srv) {
		return risk.ClassifyNet(command, netRiskBrand(srv.NetdevBrand))
	}
	return risk.Classify(command, riskOS(srv))
}

// winRMChannel 判断服务器是否走 WinRM 通道（仅 Windows + WinRM 认证）。
func winRMChannel(srv store.Server) bool {
	return srv.OS == store.ServerOSWindows &&
		(srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic)
}

func (r *Runner) now() time.Time {
	if r.clock != nil {
		return r.clock().UTC()
	}
	return time.Now().UTC()
}

// Prepare 在写 SSE 头之前做同步校验与供应商解析；不写库。
func (r *Runner) Prepare(ctx context.Context, conversationID, requestedProviderID, requestedModel string) (RunInfo, error) {
	if r.st == nil {
		return RunInfo{}, runErr(codeInternal, "storage is not configured", false)
	}
	if strings.TrimSpace(conversationID) == "" {
		return RunInfo{}, runErr(codeValidationFailed, "conversation_id is required", false)
	}
	conv, err := r.st.Conversations().Get(ctx, conversationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RunInfo{}, runErr(codeConversationNotFound, "conversation not found", false)
		}
		return RunInfo{}, runErr(codeInternal, "failed to load conversation", false)
	}
	return r.resolve(ctx, conv, requestedProviderID, requestedModel)
}

// resolve 按 CONTRACT.md 5.8 的规则解析供应商与模型。
func (r *Runner) resolve(ctx context.Context, conv store.Conversation, requestedProviderID, requestedModel string) (RunInfo, error) {
	info := RunInfo{Conversation: conv}

	if conv.ProviderID != nil && *conv.ProviderID != "" {
		if requestedProviderID != "" && requestedProviderID != *conv.ProviderID {
			return RunInfo{}, runErr(codeModelLocked, "conversation is bound to another provider", false)
		}
		if requestedModel != "" && conv.Model != "" && requestedModel != conv.Model {
			return RunInfo{}, runErr(codeModelLocked, "conversation is bound to another model", false)
		}
		provider, err := r.st.Providers().Get(ctx, *conv.ProviderID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return RunInfo{}, runErr(codeProviderNotFound, "provider not found", false)
			}
			return RunInfo{}, runErr(codeInternal, "failed to load provider", false)
		}
		model := requestedModel
		if model == "" {
			model = conv.Model
		}
		if model == "" {
			return RunInfo{}, runErr(codeValidationFailed, "model is required", false)
		}
		info.Provider = provider
		info.Model = model
		return info, nil
	}

	providerID := requestedProviderID
	if providerID == "" {
		providers, err := r.st.Providers().List(ctx)
		if err != nil {
			return RunInfo{}, runErr(codeInternal, "failed to list providers", false)
		}
		if len(providers) == 0 {
			return RunInfo{}, runErr(codeNoProvider, "no LLM provider is configured", false)
		}
		providerID = providers[0].ID
	}
	provider, err := r.st.Providers().Get(ctx, providerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RunInfo{}, runErr(codeProviderNotFound, "provider not found", false)
		}
		return RunInfo{}, runErr(codeInternal, "failed to load provider", false)
	}
	model := requestedModel
	if model == "" {
		model = provider.DefaultModel
	}
	if model == "" {
		return RunInfo{}, runErr(codeValidationFailed, "model is required", false)
	}
	info.Provider = provider
	info.Model = model
	return info, nil
}

type runState struct {
	conv      store.Conversation
	turnID    string
	model     string
	requestID string
	content   strings.Builder
	reasoning strings.Builder
	usage     llm.Usage
	// 六期打磨 sprint 2（契约 8.6 第 13 条）：本轮 reasoning 首/尾 delta 的
	// 服务端时刻。首条只记一次，后续覆盖尾时刻——工具循环多轮 reasoning
	// 合并为本轮总耗时，随 assistant 消息落库（reasoning_duration_ms）。
	reasoningStartedAt time.Time
	reasoningEndedAt   time.Time
}

// reasoningDurationMS 返回本轮 reasoning 首/尾 delta 的毫秒差；无 reasoning
// 返回 nil（与「有 reasoning 但不足 1ms」的 0 区分）。时钟回拨时夹到 0。
func (s *runState) reasoningDurationMS() *int64 {
	if s == nil || s.reasoningStartedAt.IsZero() || s.reasoningEndedAt.IsZero() {
		return nil
	}
	ms := s.reasoningEndedAt.Sub(s.reasoningStartedAt).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// Run 写 user 消息、执行工具循环、写 assistant 消息。
// 开始后所有失败都通过 emit(Event{Type:EventError}) 发送，并返回非 nil error。
func (r *Runner) Run(ctx context.Context, in RunInput, emit func(Event) error) error {
	if emit == nil {
		return runErr(codeInternal, "emit callback is required", false)
	}
	if r.st == nil {
		return r.emitOnly(ctx, emit, in.RequestID, codeInternal, "storage is not configured", false, true)
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		return r.emitOnly(ctx, emit, in.RequestID, codeValidationFailed, "conversation_id is required", false, true)
	}
	if strings.TrimSpace(in.Content) == "" {
		return r.emitOnly(ctx, emit, in.RequestID, codeValidationFailed, "content is required", false, true)
	}

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.StreamMaxDuration)
	defer cancel()

	conv, err := r.st.Conversations().Get(runCtx, in.ConversationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return r.emitOnly(runCtx, emit, in.RequestID, codeConversationNotFound, "conversation not found", false, true)
		}
		return r.emitOnly(runCtx, emit, in.RequestID, codeInternal, "failed to load conversation", false, true)
	}
	info, err := r.resolve(runCtx, conv, in.ProviderID, in.Model)
	if err != nil {
		var re *RunError
		if errors.As(err, &re) {
			return r.emitOnly(runCtx, emit, in.RequestID, re.Code, re.Message, re.Retryable, true)
		}
		return r.emitOnly(runCtx, emit, in.RequestID, codeInternal, "failed to resolve provider", false, true)
	}

	turnID := uuid.NewString()
	state := &runState{conv: conv, turnID: turnID, model: info.Model, requestID: in.RequestID}

	userMsg, err := r.createUserMessage(runCtx, conv, info, turnID, in.Content)
	if err != nil {
		return r.emitOnly(runCtx, emit, in.RequestID, codeInternal, "failed to store message", false, true)
	}
	_ = r.st.Conversations().Touch(runCtx, conv.ID, r.now())

	// 六期 BE-P（契约 5.8 第 9 条）：写 user 消息后解析一次有效门限
	//（会话覆盖 > 全局缺省 standard）并贯穿本轮全部工具判定（11 节第 18
	// 条：每 turn 至多一次 settings 读）。
	approvalMode, err := approvals.EffectiveMode(runCtx, r.st, conv)
	if err != nil {
		return r.abort(ctx, runCtx, emit, state, codeInternal, "failed to resolve approval mode", false, true)
	}

	messages, err := r.buildMessages(runCtx, conv.ID, userMsg, r.cfg.ContextMessages)
	if err != nil {
		return r.abort(ctx, runCtx, emit, state, codeInternal, "failed to load conversation history", false, true)
	}

	client, _, err := r.clientFor(runCtx, info.Provider.ID)
	if err != nil {
		return r.abort(ctx, runCtx, emit, state, codeProviderNotFound, "provider is not available", false, true)
	}

	finishReason := "stop"
	var emitErr error

loop:
	for iteration := 0; iteration < r.cfg.MaxIterations; iteration++ {
		callCtx, callCancel := context.WithTimeout(runCtx, r.cfg.LLMCallTimeout)
		var iterText strings.Builder
		var iterCalls []llm.ToolCall
		var iterFinish string
		var iterUsage *llm.Usage

		streamErr := client.Stream(callCtx, llm.Request{
			Model:       info.Model,
			Messages:    messages,
			Tools:       ToolDefinitions(),
			Temperature: 0.2,
			MaxTokens:   4096,
		}, func(ev llm.StreamEvent) error {
			switch ev.Type {
			case llm.EventToken:
				state.content.WriteString(ev.Delta)
				iterText.WriteString(ev.Delta)
				if err := emit(Event{Type: EventToken, Token: &TokenPayload{Delta: ev.Delta}}); err != nil {
					emitErr = err
					return err
				}
			case llm.EventReasoning:
				now := r.now()
				if state.reasoningStartedAt.IsZero() {
					state.reasoningStartedAt = now
				}
				state.reasoningEndedAt = now
				state.reasoning.WriteString(ev.Delta)
				if err := emit(Event{Type: EventReasoning, Token: &TokenPayload{Delta: ev.Delta}}); err != nil {
					emitErr = err
					return err
				}
			case llm.EventToolCall:
				if ev.ToolCall != nil {
					iterCalls = append(iterCalls, *ev.ToolCall)
				}
			case llm.EventDone:
				iterFinish = ev.FinishReason
				if ev.Usage != nil {
					u := *ev.Usage
					iterUsage = &u
				}
			}
			return nil
		})
		callCancel()

		if streamErr != nil {
			if emitErr != nil {
				return r.abort(ctx, runCtx, emit, state, codeClientDisconnected, "client disconnected", false, false)
			}
			code, msg, retryable := r.classifyLLMError(ctx, runCtx, streamErr)
			return r.abort(ctx, runCtx, emit, state, code, msg, retryable, true)
		}
		if iterUsage != nil {
			state.usage.PromptTokens += iterUsage.PromptTokens
			state.usage.CompletionTokens += iterUsage.CompletionTokens
			state.usage.TotalTokens += iterUsage.TotalTokens
		}

		if len(iterCalls) == 0 {
			if iterFinish == "length" && iterText.Len() == 0 {
				return r.abort(ctx, runCtx, emit, state, codeLLMTruncated, "model output was truncated", false, true)
			}
			if iterFinish == "length" {
				finishReason = "length"
			} else {
				finishReason = "stop"
			}
			break loop
		}

		if iteration == r.cfg.MaxIterations-1 {
			return r.abort(ctx, runCtx, emit, state, codeMaxIterations, "tool loop exceeded the maximum number of iterations", false, true)
		}

		for i := range iterCalls {
			if iterCalls[i].ID == "" {
				iterCalls[i].ID = "call_" + uuid.NewString()
			}
		}
		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   iterText.String(),
			ToolCalls: iterCalls,
		})
		for _, call := range iterCalls {
			outcome := r.executeToolCall(runCtx, conv, info, turnID, in, call, emit, approvalMode)
			if outcome.EmitErr != nil {
				return r.abort(ctx, runCtx, emit, state, codeClientDisconnected, "client disconnected", false, false)
			}
			if outcome.Fatal != nil {
				code := codeInternal
				if errors.Is(outcome.Fatal, context.Canceled) && ctx.Err() != nil {
					code = codeClientDisconnected
				}
				return r.abort(ctx, runCtx, emit, state, code, "tool execution failed", false, ctx.Err() == nil)
			}
			if outcome.AskUser {
				finishReason = "ask_user"
				state.content.WriteString(outcome.Question)
				if err := emit(Event{Type: EventToken, Token: &TokenPayload{Delta: outcome.Question}}); err != nil {
					return r.abort(ctx, runCtx, emit, state, codeClientDisconnected, "client disconnected", false, false)
				}
				break loop
			}
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    toolMessageContent(outcome, r.cfg.ToolResultBytes),
				ToolCallID: call.ID,
				Name:       call.Name,
			})
		}
	}

	assistant, err := r.st.Messages().Create(runCtx, store.CreateMessageParams{
		ConversationID:      conv.ID,
		TurnID:              turnID,
		Role:                store.RoleAssistant,
		Content:             state.content.String(),
		Reasoning:           state.reasoning.String(),
		ReasoningDurationMS: state.reasoningDurationMS(),
		Status:              store.MessageComplete,
		Model:               info.Model,
		PromptTokens:        state.usage.PromptTokens,
		CompletionTokens:    state.usage.CompletionTokens,
		CreatedAt:           r.now(),
	})
	if err != nil {
		return r.emitOnly(runCtx, emit, in.RequestID, codeInternal, "failed to store assistant message", false, true)
	}

	if err := emit(Event{Type: EventDone, Done: &DonePayload{
		TurnID:       turnID,
		MessageID:    assistant.ID,
		FinishReason: finishReason,
		Usage:        state.usage,
	}}); err != nil {
		return runErr(codeClientDisconnected, "client disconnected", false)
	}
	return nil
}

func (r *Runner) createUserMessage(ctx context.Context, conv store.Conversation, info RunInfo, turnID, content string) (store.Message, error) {
	var msg store.Message
	err := r.st.WithTx(ctx, func(tx store.Store) error {
		m, err := tx.Messages().Create(ctx, store.CreateMessageParams{
			ConversationID: conv.ID,
			TurnID:         turnID,
			Role:           store.RoleUser,
			Content:        content,
			Status:         store.MessageComplete,
			Model:          info.Model,
			CreatedAt:      r.now(),
		})
		if err != nil {
			return err
		}
		msg = m
		if conv.ProviderID == nil {
			if _, err := tx.Conversations().BindModel(ctx, conv.ID, info.Provider.ID, info.Model, r.now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return store.Message{}, err
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	return msg, nil
}

// buildMessages 加载最近消息并按 40 条 / 200KB 裁剪；system prompt 始终保留。
func (r *Runner) buildMessages(ctx context.Context, conversationID string, userMsg store.Message, limit int) ([]llm.Message, error) {
	msgs, _, err := r.st.Messages().ListPage(ctx, conversationID, 0, limit)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].ID != userMsg.ID {
		msgs = append(msgs, userMsg)
	}

	// 工具调用窗口必须与消息窗口对齐：若只取固定条数的最新 tool_calls，
	// 工具调用密集的长会话中，消息窗口内最旧 assistant 轮次的调用会落在
	// 窗口外，模型只能看到残缺的该轮工具历史。这里从新到旧分页拉取，
	// 直到覆盖消息窗口内最旧 assistant 轮次，或翻完全部 / 达到页数上限；
	// 翻页过程中只保留消息窗口内出现的轮次，控制内存占用。
	byTurn := map[string][]store.ToolCall{}
	oldestTurn := ""
	for _, m := range msgs {
		if m.Role == store.RoleAssistant {
			if oldestTurn == "" {
				oldestTurn = m.TurnID
			}
			byTurn[m.TurnID] = nil
		}
	}
	if tcs := r.st.ToolCalls(); tcs != nil && oldestTurn != "" {
		var collected []store.ToolCall
		sawOldest := false
		before := int64(0)
		for page := 0; page < contextToolCallMaxPages; page++ {
			calls, hasMore, err := tcs.ListPage(ctx, conversationID, before, contextToolCallPageSize)
			if err != nil {
				break // 查询失败时容忍降级：与旧实现一致，用已取到的数据。
			}
			for _, tc := range calls {
				if _, ok := byTurn[tc.TurnID]; ok {
					collected = append(collected, tc)
					if tc.TurnID == oldestTurn {
						sawOldest = true
					}
				}
			}
			if len(calls) == 0 || !hasMore {
				break
			}
			// 页内最旧一条已越过最旧 assistant 轮次：该轮次调用已完整覆盖。
			if sawOldest && calls[0].TurnID != oldestTurn {
				break
			}
			before = calls[0].Seq
		}
		sort.SliceStable(collected, func(i, j int) bool { return collected[i].Seq < collected[j].Seq })
		for _, tc := range collected {
			byTurn[tc.TurnID] = append(byTurn[tc.TurnID], tc)
		}
	}

	type group struct {
		messages []llm.Message
		bytes    int
	}
	groups := make([]group, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case store.RoleUser:
			groups = append(groups, group{
				messages: []llm.Message{{Role: llm.RoleUser, Content: m.Content}},
				bytes:    len(m.Content),
			})
		case store.RoleAssistant:
			calls := byTurn[m.TurnID]
			llmCalls := make([]llm.ToolCall, 0, len(calls))
			for _, tc := range calls {
				if tc.Name == "" {
					continue
				}
				llmCalls = append(llmCalls, llm.ToolCall{ID: tc.ProviderCallID, Name: tc.Name, Arguments: tc.Arguments})
			}
			if m.Content == "" && len(llmCalls) == 0 {
				continue
			}
			g := group{
				messages: []llm.Message{{Role: llm.RoleAssistant, Content: m.Content, ToolCalls: llmCalls}},
				bytes:    len(m.Content),
			}
			for _, tc := range calls {
				if tc.Name == "" {
					continue
				}
				content := "{}"
				if len(tc.Result) > 0 {
					content = string(tc.Result)
				} else if tc.ErrorCode != "" {
					content = string(errorResult(tc.ErrorCode, "tool failed"))
				}
				g.messages = append(g.messages, llm.Message{
					Role:       llm.RoleTool,
					Content:    content,
					ToolCallID: tc.ProviderCallID,
					Name:       tc.Name,
				})
				g.bytes += len(content) + len(tc.Arguments)
			}
			groups = append(groups, g)
		}
	}

	var selected []group
	totalBytes, totalMessages := 0, 0
	for i := len(groups) - 1; i >= 0; i-- {
		g := groups[i]
		if len(selected) > 0 {
			if totalBytes+g.bytes > r.cfg.ContextBytes || totalMessages+len(g.messages) > r.cfg.ContextMessages {
				break
			}
		}
		selected = append(selected, g)
		totalBytes += g.bytes
		totalMessages += len(g.messages)
	}

	out := make([]llm.Message, 0, totalMessages+1)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: SystemPrompt})
	for i := len(selected) - 1; i >= 0; i-- {
		out = append(out, selected[i].messages...)
	}
	return out, nil
}

func (r *Runner) clientFor(ctx context.Context, providerID string) (llm.Client, store.Provider, error) {
	if r.client != nil {
		return r.client, store.Provider{ID: providerID}, nil
	}
	if r.registry == nil {
		return nil, store.Provider{}, errors.New("agent: llm registry is not configured")
	}
	return r.registry.Client(ctx, providerID)
}

func (r *Runner) classifyLLMError(ctx, runCtx context.Context, err error) (code, message string, retryable bool) {
	if runCtx.Err() == context.DeadlineExceeded {
		return codeStreamTimeout, "stream exceeded maximum duration", false
	}
	if ctx.Err() != nil {
		return codeClientDisconnected, "client disconnected", false
	}
	switch {
	case errors.Is(err, llm.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return codeLLMTimeout, "language model call timed out", true
	case errors.Is(err, llm.ErrStream):
		return codeLLMStream, "invalid response from language model", false
	case errors.Is(err, llm.ErrInvalidConfig):
		return codeLLMUpstream, "language model is misconfigured", false
	default:
		return codeLLMUpstream, "language model request failed", false
	}
}

// isEmitFailure 保留给需要区分前端断开的调用方。
func isEmitFailure(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, errEmitFailed)
}

var errEmitFailed = errors.New("agent: emit failed")

// emitOnly 在没有写 user 消息时发送错误事件。
func (r *Runner) emitOnly(ctx context.Context, emit func(Event) error, requestID, code, message string, retryable, emitEvent bool) error {
	if emitEvent && ctx.Err() == nil {
		if err := emit(Event{Type: EventError, Error: &ErrorPayload{Code: code, Message: message, RequestID: requestID, Retryable: retryable}}); err != nil {
			return &RunError{Code: codeClientDisconnected, Message: "client disconnected", Err: err}
		}
	}
	return runErr(code, message, retryable)
}

// abort 持久化已收到的部分内容（error 状态），再发送 error 事件。
func (r *Runner) abort(ctx, runCtx context.Context, emit func(Event) error, state *runState, code, message string, retryable, emitEvent bool) error {
	if state != nil && r.st != nil {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := r.st.Messages().Create(pctx, store.CreateMessageParams{
			ConversationID:      state.conv.ID,
			TurnID:              state.turnID,
			Role:                store.RoleAssistant,
			Content:             state.content.String(),
			Reasoning:           state.reasoning.String(),
			ReasoningDurationMS: state.reasoningDurationMS(),
			Status:              store.MessageError,
			ErrorCode:           code,
			Model:               state.model,
			PromptTokens:        state.usage.PromptTokens,
			CompletionTokens:    state.usage.CompletionTokens,
			CreatedAt:           r.now(),
		}); err != nil && r.log != nil {
			r.log.Warn("agent_persist_error_failed", "conversation_id", state.conv.ID, "error", err)
		}
	}
	if emitEvent && runCtx.Err() == nil {
		if err := emit(Event{Type: EventError, Error: &ErrorPayload{
			Code: code, Message: message, RequestID: stateRequestID(state), Retryable: retryable,
		}}); err != nil {
			return &RunError{Code: codeClientDisconnected, Message: "client disconnected", Err: err}
		}
	}
	return runErr(code, message, retryable)
}

func stateRequestID(state *runState) string {
	if state == nil {
		return ""
	}
	return state.requestID
}

// ---------------------------------------------------------------------------
// 工具执行
// ---------------------------------------------------------------------------

type toolOutcome struct {
	Result   json.RawMessage
	Err      *ToolError
	Status   store.ToolCallStatus
	AskUser  bool
	Question string
	Fatal    error
	EmitErr  error
	// ExecStartedAt 非 0 值时覆盖 rec.StartedAt 作为 tool_result 耗时的计时
	// 起点：需审批的调用在人工审批等待期间命令从未执行，等待结束后重置
	// 起点，避免把审批等待（TTL 可达数十秒）误读为命令执行耗时。
	ExecStartedAt time.Time
}

// toolMessageContent 返回回填给模型的 tool 消息内容。
// 结果必须先按 ToolResultBytes 截断，否则单次 256KB 的工具输出会在
// 多轮循环中累积，超出模型上下文并放大 token 成本。
func toolMessageContent(out toolOutcome, maxBytes int) string {
	if out.Err != nil {
		return string(errorResult(out.Err.Code, out.Err.Message))
	}
	if len(out.Result) == 0 {
		return "{}"
	}
	return string(truncateResult(out.Result, maxBytes))
}

// approvalMode 是本轮 turn 的有效审批门限（六期 BE-P，Run 解析一次后
// 贯穿全部工具判定；空串按 standard 保守处理——NeedsApproval 的 default 分支）。
func (r *Runner) executeToolCall(ctx context.Context, conv store.Conversation, info RunInfo, turnID string, in RunInput, call llm.ToolCall, emit func(Event) error, approvalMode store.ApprovalMode) toolOutcome {
	args := call.Arguments
	if len(args) == 0 || !json.Valid(args) {
		args = json.RawMessage("{}")
	}

	riskLevel := string(risk.LevelSafe)
	known := knownTool(call.Name)
	var execArgs sshExecArgs
	// C1 四期：容器工具的 server_id（审计 resource 归属）。
	var containerServerID string
	argsOK := true
	switch call.Name {
	case ToolSSHExec:
		if err := decodeStrictArgs(args, &execArgs); err != nil {
			argsOK = false
			// 参数不合契约时不能把 risk_level 记成默认 safe：command 字段
			// 若仍可提取（常见：其余字段类型错/未知字段），按其真实命令分类；
			// 完全提取不到时才落列默认值 safe（此时该调用必然 failed，无命令
			// 可评估）。列有 CHECK 约束，只能存枚举值。此时无服务器上下文，
			// 按契约缺省 linux 分册分类。
			var loose struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(args, &loose) == nil && loose.Command != "" {
				riskLevel = string(risk.Classify(loose.Command, risk.OSLinux).Level)
			}
		} else {
			// 服务器上下文决定分册（契约 7.2）；服务器不存在时由 runSSHExec
			// 报 SERVER_NOT_FOUND，这里按缺省 linux 记录。
			if execArgs.ServerID != "" {
				if srv, err := r.st.Servers().Get(ctx, execArgs.ServerID); err == nil {
					riskLevel = string(netRiskClassify(execArgs.Command, srv).Level)
				}
			}
		}
	case ToolServerFacts:
		var a serverFactsArgs
		argsOK = decodeStrictArgs(args, &a) == nil
	case ToolDBQuery:
		var a dbQueryArgs
		argsOK = decodeStrictArgs(args, &a) == nil
	case ToolAskUser:
		var a askUserArgs
		argsOK = decodeStrictArgs(args, &a) == nil
	case ToolListServers, ToolListDatabases:
		argsOK = decodeStrictArgs(args, &struct{}{}) == nil
	case ToolContainerList, ToolImageList:
		var a containerServerArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		containerServerID = a.ServerID
	case ToolContainerInspect:
		var a containerIDArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		containerServerID = a.ServerID
	case ToolContainerLogs:
		var a containerLogsArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		containerServerID = a.ServerID
	case ToolContainerAction:
		var a containerActionArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		containerServerID = a.ServerID
		// 风险分级写死（契约 7.2）：start=safe；stop/restart=needs_approval
		//（CONTAINER_CHANGE）。动作集封闭为三个（forbidden 不存在于此工具）。
		switch a.Action {
		case "start":
			riskLevel = string(risk.LevelSafe)
		case "stop", "restart":
			riskLevel = string(risk.LevelNeedsApproval)
		}
	case ToolGetMetrics:
		var a getMetricsArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		containerServerID = a.ServerID // 复用审计归属通道（仅 server_id 提取）
	case ToolListAlerts:
		var a listAlertsArgs
		argsOK = decodeStrictArgs(args, &a) == nil
	case ToolAuditSearch:
		var a auditSearchArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		if a.ServerID != nil && *a.ServerID != "" {
			containerServerID = *a.ServerID
		}
	case ToolBatchExecute:
		var a batchExecuteArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		// 工具整体按 needs_approval 语义建模（契约 7.2：M3）——实际逐台
		// 分类在 batchx 内完成，safe+auto_exec 直跑、不因工具级标记强制审批。
		riskLevel = string(risk.LevelNeedsApproval)
	case ToolRunDiagnosis:
		var a runDiagnosisArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		containerServerID = a.ServerID
		// 恒 safe、无审批（契约 7.2：M2）——引擎内部 safe 闸是唯一执行闸。
	case ToolRunInspection:
		var a runInspectionArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		if len(a.ServerIDs) > 0 {
			containerServerID = a.ServerIDs[0]
		}
	case ToolGetInspectionReport:
		var a getInspectionReportArgs
		argsOK = decodeStrictArgs(args, &a) == nil
		if a.ReportID != nil {
			containerServerID = *a.ReportID
		}
	}

	providerCallID := call.ID
	if providerCallID == "" {
		providerCallID = "call_" + uuid.NewString()
	}

	// 六期打磨 sprint 1：命令执行类工具在创建行时落门限快照，刷新/重进
	// 会话后工具卡的「在 <mode> 门限下执行」徽标仍可见（此前 approval_mode
	// 只在 SSE tool_result 里存在，refetch 即丢失）。其余工具传空串（NULL）。
	recordMode := store.ApprovalMode("")
	if isCommandExecTool(call.Name) {
		recordMode = approvalMode
	}
	rec, err := r.st.ToolCalls().Create(ctx, store.CreateToolCallParams{
		ConversationID: conv.ID,
		TurnID:         turnID,
		ProviderCallID: providerCallID,
		Name:           call.Name,
		Arguments:      args,
		RiskLevel:      riskLevel,
		Status:         store.ToolStarted,
		StartedAt:      r.now(),
		ApprovalMode:   recordMode,
	})
	if err != nil {
		return toolOutcome{Fatal: err}
	}

	r.auditEvent(ctx, "tool.call", "model", conversationActor(conv.ID), "tool_call", rec.ID, &conv.ID, serverIDFromArgs(call.Name, execArgs, containerServerID), map[string]any{
		"name": call.Name, "risk_level": riskLevel, "provider_call_id": providerCallID,
	})

	if err := emit(Event{Type: EventToolCall, ToolCall: &ToolCallPayload{
		ToolCallID:     rec.ID,
		ProviderCallID: providerCallID,
		Name:           call.Name,
		Arguments:      args,
	}}); err != nil {
		return toolOutcome{EmitErr: err}
	}

	var out toolOutcome
	switch {
	case !known:
		out = toolOutcome{Err: &ToolError{Code: codeToolNotFound, Message: "unknown tool"}, Status: store.ToolFailed}
	case !argsOK:
		out = toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	case call.Name == ToolAskUser:
		out = r.runAskUser(ctx, args)
	case call.Name == ToolListServers:
		out = r.runListServers(ctx)
	case call.Name == ToolListDatabases:
		out = r.runListDatabases(ctx)
	case call.Name == ToolServerFacts:
		out = r.runServerFacts(ctx, rec, args)
	case call.Name == ToolSSHExec:
		out = r.runSSHExec(ctx, conv, rec, providerCallID, execArgs, emit, in, approvalMode)
	case call.Name == ToolDBQuery:
		out = r.runDBQuery(ctx, rec, args)
	case call.Name == ToolContainerList:
		out = r.runContainerList(ctx, rec, args)
	case call.Name == ToolContainerInspect:
		out = r.runContainerInspect(ctx, rec, args)
	case call.Name == ToolContainerAction:
		out = r.runContainerAction(ctx, conv, rec, providerCallID, args, emit, in, approvalMode)
	case call.Name == ToolContainerLogs:
		out = r.runContainerLogs(ctx, rec, args)
	case call.Name == ToolImageList:
		out = r.runImageList(ctx, rec, args)
	case call.Name == ToolGetMetrics:
		out = r.runGetMetrics(ctx, args)
	case call.Name == ToolListAlerts:
		out = r.runListAlerts(ctx, args)
	case call.Name == ToolAuditSearch:
		out = r.runAuditSearch(ctx, args)
	case call.Name == ToolBatchExecute:
		out = r.runBatchExecute(ctx, conv, rec, in, args)
	case call.Name == ToolRunDiagnosis:
		out = r.runRunDiagnosis(ctx, conv, args)
	case call.Name == ToolRunInspection:
		out = r.runRunInspection(ctx, args)
	case call.Name == ToolGetInspectionReport:
		out = r.runGetInspectionReport(ctx, args)
	}

	if out.EmitErr != nil {
		return out
	}
	if out.Fatal != nil {
		return out
	}

	status := out.Status
	if status == "" {
		if out.Err != nil {
			status = store.ToolFailed
		} else {
			status = store.ToolSucceeded
		}
	}
	errorCode := ""
	errorMessage := ""
	if out.Err != nil {
		errorCode = out.Err.Code
		errorMessage = truncateErrorMessage(out.Err.Message)
	}

	var result json.RawMessage
	if out.Err == nil {
		result = truncateResult(out.Result, r.cfg.ToolResultBytes)
	}
	if _, err := r.st.ToolCalls().Finish(ctx, store.FinishToolCallParams{
		ID:           rec.ID,
		Status:       status,
		Result:       result,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		FinishedAt:   r.now(),
	}); err != nil && r.log != nil {
		r.log.Warn("agent_finish_tool_call_failed", "tool_call_id", rec.ID, "error", err)
	}

	if out.Err != nil {
		r.auditEvent(ctx, "tool.error", "model", conversationActor(conv.ID), "tool_call", rec.ID, &conv.ID, nil, map[string]any{
			"name": call.Name, "code": out.Err.Code,
		})
	} else {
		r.auditEvent(ctx, "tool.result", "model", conversationActor(conv.ID), "tool_call", rec.ID, &conv.ID, nil, map[string]any{
			"name": call.Name, "status": string(status),
		})
	}

	// 审批等待段已由 runSSHExec 从计时中剔除（见 toolOutcome.ExecStartedAt）：
	// DurationMS 只代表真实执行耗时；denied/expired 等命令从未执行的终态
	// 不会把人工审批等待计入。
	durationStart := rec.StartedAt
	if !out.ExecStartedAt.IsZero() {
		durationStart = out.ExecStartedAt
	}
	// 六期 BE-P（契约 6.2）：命令执行类工具的 tool_result 携带判定时的
	// 有效门限；其余工具省略该键（approval_mode omitempty）。
	resultMode := ""
	if isCommandExecTool(call.Name) {
		resultMode = string(approvalMode)
	}
	if err := emit(Event{Type: EventToolResult, ToolResult: &ToolResultPayload{
		ToolCallID:   rec.ID,
		Name:         call.Name,
		OK:           out.Err == nil,
		Result:       result,
		Error:        out.Err,
		DurationMS:   r.now().Sub(durationStart).Milliseconds(),
		ApprovalMode: resultMode,
	}}); err != nil {
		out.EmitErr = err
	}
	return out
}

// isCommandExecTool 判断是否命令执行类工具（契约 6.2：tool_result 事件
// 携带 approval_mode 的范围；ReplayStep 的 execution 步仅前两者）。
func isCommandExecTool(name string) bool {
	switch name {
	case ToolSSHExec, ToolContainerAction, ToolBatchExecute:
		return true
	}
	return false
}

func (r *Runner) runAskUser(ctx context.Context, raw json.RawMessage) toolOutcome {
	var args askUserArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	q := strings.TrimSpace(args.Question)
	if q == "" || utf8.RuneCountInString(q) > 2000 {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "question must be 1..2000 characters"}, Status: store.ToolFailed}
	}
	res, _ := json.Marshal(map[string]any{"question": q, "status": "awaiting_user"})
	return toolOutcome{Result: res, Status: store.ToolAwaitingUser, AskUser: true, Question: q}
}

type serverInfo struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	DeviceKind       string   `json:"device_kind"` // N1：server | network_device
	OS               string   `json:"os"`
	NetdevBrand      string   `json:"netdev_brand"` // N1：仅 network_device 非空
	AccessProto      string   `json:"access_proto"` // N1：network_device: ssh|telnet
	Host             string   `json:"host"`
	Port             int      `json:"port"`
	Tags             []string `json:"tags"`
	AutoExecEnabled  bool     `json:"auto_exec_enabled"`
	ContainerRuntime string   `json:"container_runtime"` // C1 四期：''|none|docker|podman|containerd
}

func (r *Runner) runListServers(ctx context.Context) toolOutcome {
	servers, err := r.st.Servers().List(ctx)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "failed to list servers"}, Status: store.ToolFailed}
	}
	items := make([]serverInfo, 0, len(servers))
	for _, s := range servers {
		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}
		kind := s.DeviceKind
		if kind == "" {
			kind = store.DeviceKindServer
		}
		items = append(items, serverInfo{
			ID: s.ID, Name: s.Name, DeviceKind: string(kind), OS: string(s.OS),
			NetdevBrand: string(s.NetdevBrand), AccessProto: string(s.AccessProto),
			Host: s.Host, Port: s.Port,
			Tags: tags, AutoExecEnabled: s.AutoExecEnabled,
			ContainerRuntime: string(s.ContainerRuntime),
		})
	}
	res, _ := json.Marshal(map[string]any{"servers": items})
	return toolOutcome{Result: res, Status: store.ToolSucceeded}
}

type databaseInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Engine       string `json:"engine"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	DatabaseName string `json:"database_name"`
}

func (r *Runner) runListDatabases(ctx context.Context) toolOutcome {
	databases, err := r.st.Databases().List(ctx)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "failed to list databases"}, Status: store.ToolFailed}
	}
	items := make([]databaseInfo, 0, len(databases))
	for _, d := range databases {
		items = append(items, databaseInfo{
			ID: d.ID, Name: d.Name, Engine: string(d.Engine),
			Host: d.Host, Port: d.Port, DatabaseName: d.DatabaseName,
		})
	}
	res, _ := json.Marshal(map[string]any{"databases": items})
	return toolOutcome{Result: res, Status: store.ToolSucceeded}
}

func (r *Runner) runServerFacts(ctx context.Context, rec store.ToolCall, raw json.RawMessage) toolOutcome {
	var args serverFactsArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.ServerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id is required"}, Status: store.ToolFailed}
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}
	timeout := r.cfg.SSHDefaultTimeout
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 三期（N1，契约 7.5/2.8.5）：网络设备按 netdev_brand 使用固定只读
	// 命令模板，经 netdev 执行；输出解析进 facts、原文进 raw_output，
	// 解析失败的字段留空不整体报错（无审批，命中 <P>SAFE_FACTS）。
	if isNetworkDevice(srv) {
		if r.netcli == nil {
			return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "netdev executor is not configured"}, Status: store.ToolFailed}
		}
		target, err := r.netDevTarget(srv)
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
		}
		brand := netdev.BrandOf(srv.NetdevBrand)
		cmds := netdev.FactsCommands(brand)
		out := make([]string, 0, len(cmds))
		var durSum int64
		var lastErr error
		for _, cmd := range cmds {
			res, err := r.netcli.Execute(runCtx, target, netdev.RunRequest{
				Command: cmd, Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
			})
			if err != nil {
				lastErr = err
				break
			}
			durSum += res.DurationMS
			out = append(out, res.Combined)
		}
		if lastErr != nil {
			code, msg := classifyNetCLIError(lastErr)
			return toolOutcome{Err: &ToolError{Code: code, Message: msg}, Status: store.ToolFailed}
		}
		var ifOut string
		if len(out) > 1 {
			ifOut = out[1]
		}
		facts := netdev.ParseFacts(brand, string(srv.NetdevBrand), out[0], ifOut)
		resultJSON, _ := json.Marshal(map[string]any{
			"server_id":   srv.ID,
			"facts":       facts,
			"raw_output":  strings.Join(out, "\n"),
			"duration_ms": durSum,
		})
		r.auditEvent(ctx, "command.executed", "model", conversationActor(rec.ConversationID), "server", srv.ID, &rec.ConversationID, &srv.ID, map[string]any{
			"tool": "server_facts", "netdev_brand": string(srv.NetdevBrand), "duration_ms": durSum,
		})
		return toolOutcome{Result: resultJSON, Status: store.ToolSucceeded}
	}

	// 契约 7.5：server_facts 按 os 使用固定模板；通道按 2.7 路由
	//（WinRM 认证直接经 winrmx PowerShell 端点，SSH 通道由本层包装外壳）。
	type execResult struct {
		Combined   string
		Truncated  bool
		DurationMS int64
		ExitCode   int
		sshRes     *sshx.RunResult // 非 nil 表示 SSH 通道（主机密钥回写只在 SSH 通道有意义）
	}
	var (
		res     execResult
		execErr error
	)
	if srv.OS == store.ServerOSWindows {
		factsCmd := winFactsCommand()
		if winRMChannel(srv) {
			if r.winrm == nil {
				return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "winrm executor is not configured"}, Status: store.ToolFailed}
			}
			target, err := r.winRMTarget(srv)
			if err != nil {
				return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
			}
			out, err := r.winrm.Execute(runCtx, target, winrmx.RunRequest{
				Command: factsCmd, Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
			})
			if err == nil {
				res = execResult{Combined: out.Combined, Truncated: out.Truncated, DurationMS: out.DurationMS, ExitCode: out.ExitCode}
			}
			execErr = err
		} else {
			target, err := r.serverTarget(srv)
			if err != nil {
				return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
			}
			if r.ssh == nil {
				return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "ssh executor is not configured"}, Status: store.ToolFailed}
			}
			out, err := r.ssh.Execute(runCtx, target, sshx.RunRequest{
				Command: wrapPowerShellForSSH(factsCmd), Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
			})
			if err == nil {
				res = execResult{Combined: out.Combined, Truncated: out.Truncated, DurationMS: out.DurationMS, ExitCode: out.ExitCode, sshRes: &out}
			}
			execErr = err
		}
	} else {
		target, err := r.serverTarget(srv)
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
		}
		if r.ssh == nil {
			return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "ssh executor is not configured"}, Status: store.ToolFailed}
		}
		out, err := r.ssh.Execute(runCtx, target, sshx.RunRequest{
			Command: serverFactsCommand, Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
		})
		if err == nil {
			res = execResult{Combined: out.Combined, Truncated: out.Truncated, DurationMS: out.DurationMS, ExitCode: out.ExitCode, sshRes: &out}
		}
		execErr = err
	}
	if execErr != nil {
		code, msg := classifySSHError(runCtx, execErr)
		return toolOutcome{Err: &ToolError{Code: code, Message: msg}, Status: store.ToolFailed}
	}
	// 主机密钥只在 SSH 通道有意义（WinRM 无主机密钥语义，契约 5.2A）。
	if res.sshRes != nil {
		r.recordHostKey(ctx, srv, res.sshRes.FirstSeen, res.sshRes.Fingerprint, res.sshRes.HostKey)
	}
	var facts serverFacts
	if srv.OS == store.ServerOSWindows {
		facts = parseWinServerFacts(res.Combined)
	} else {
		facts = parseServerFacts(res.Combined)
		// 四期 C1（契约 7.5）：Linux facts 模板末尾追加了容器探测命令，
		// 输出裸词解析进 facts.container_runtime。
		facts.ContainerRuntime = parseContainerRuntimeTail(res.Combined)
	}
	// 四期 C1（契约 2.9 约定 1）：探测结果写回 servers.container_runtime
	//（探测缓存；写回失败只记日志，不影响 facts 返回）。走到这里的只剩
	// linux/windows 服务器（network_device 已在上方提前返回）。
	if rt := facts.ContainerRuntime; rt != "" && r.st != nil {
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if _, uerr := r.st.Servers().UpdateContainerRuntime(writeCtx, srv.ID, store.ContainerRuntime(rt), r.now()); uerr != nil && r.log != nil {
			r.log.Warn("agent_update_container_runtime_failed", "server_id", srv.ID, "error", uerr.Error())
		}
		writeCancel()
	}
	out, _ := json.Marshal(map[string]any{
		"server_id":  srv.ID,
		"facts":      facts,
		"raw_output": res.Combined,
	})
	r.auditEvent(ctx, "command.executed", "model", conversationActor(rec.ConversationID), "server", srv.ID, &rec.ConversationID, &srv.ID, map[string]any{
		"tool": "server_facts", "exit_code": res.ExitCode, "duration_ms": res.DurationMS,
	})
	return toolOutcome{Result: out, Status: store.ToolSucceeded}
}

func (r *Runner) runSSHExec(ctx context.Context, conv store.Conversation, rec store.ToolCall, providerCallID string, args sshExecArgs, emit func(Event) error, in RunInput, approvalMode store.ApprovalMode) (out toolOutcome) {
	command := args.Command
	if args.ServerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id is required"}, Status: store.ToolFailed}
	}
	if args.TimeoutSeconds != nil && (*args.TimeoutSeconds < 1 || *args.TimeoutSeconds > MaxSSHTimeoutS) {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "timeout_seconds must be between 1 and 300"}, Status: store.ToolFailed}
	}
	if command == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "command is required"}, Status: store.ToolFailed}
	}
	if len(command) > MaxCommandBytes || strings.ContainsRune(command, 0) {
		return toolOutcome{Err: &ToolError{Code: codeCommandInvalid, Message: "command is invalid or too large"}, Status: store.ToolFailed}
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}

	// 分类在包装前对模型给的原始命令进行（契约 7.2/2.8.7）：网络设备按
	// 厂商分册整体分类（不做任何包装，单条 CLI 行原样发送）。
	decision := netRiskClassify(command, srv)
	if decision.Level == risk.LevelForbidden {
		r.auditEvent(ctx, "command.forbidden", "model", conversationActor(conv.ID), "server", srv.ID, &conv.ID, &srv.ID, map[string]any{
			"tool_call_id": rec.ID, "rule_id": decision.RuleID,
		})
		return toolOutcome{
			Err:    &ToolError{Code: codeCommandForbidden, Message: "command is forbidden by the risk policy"},
			Status: store.ToolDenied,
		}
	}

	// 六期 BE-P（契约 2.11 约定 2）：审批判定收敛到统一策略函数——
	// 门限 × auto_exec × level × severity（severity 仅 needs_approval 参与，
	// risk.SeverityOf 为静态 map 查找）。替换一至五期的内联
	// 「auto_exec_enabled && LevelSafe」判定。
	needsApproval := approvals.NeedsApproval(approvals.PolicyInput{
		Mode:            approvalMode,
		AutoExecEnabled: srv.AutoExecEnabled,
		Level:           decision.Level,
		Severity:        risk.SeverityOf(decision.RuleID),
	})

	approvalID := ""
	// editedCommand 是 decide 批准时的用户改写命令（六期 BE-P，契约 2.11
	// 约定 3）：非空时逐字节执行它，不再回读模型原始参数。
	editedCommand := ""
	if needsApproval {
		if r.approvals == nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "approval service is not configured"}, Status: store.ToolFailed}
		}
		approval, err := r.approvals.Create(ctx, approvals.CreateRequest{
			ConversationID: &conv.ID,
			ToolCallID:     &rec.ID,
			ServerID:       &srv.ID,
			ServerName:     srv.Name,
			Command:        command,
			RiskLevel:      string(decision.Level),
			RuleID:         decision.RuleID,
			Reason:         decision.Reason,
			ApprovalMode:   string(approvalMode),
			RequestID:      in.RequestID,
		})
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "failed to create approval"}, Status: store.ToolFailed}
		}
		approvalID = approval.ID
		// 审批等待期间命令从未执行：Wait 返回后把 tool_result 的耗时计时
		// 起点重置为等待结束时刻（defer 补写到命名返回值，覆盖其后所有
		// return 路径），仅统计真实执行段，避免 45s 级审批 TTL 被误读为
		// 命令执行耗时。
		var execStart time.Time
		defer func() {
			if !execStart.IsZero() {
				out.ExecStartedAt = execStart
			}
		}()
		if _, err := r.st.ToolCalls().SetStatus(ctx, rec.ID, store.ToolAwaitingApproval, "", ""); err != nil && r.log != nil {
			r.log.Warn("agent_tool_status_failed", "tool_call_id", rec.ID, "error", err)
		}
		if err := emit(Event{Type: EventApprovalRequired, ApprovalRequired: &approval}); err != nil {
			// 流已断开：不能留下可被 HTTP 批准的 pending 审批，也不能把
			// tool_call 永久留在 awaiting_approval（六期打磨 sprint 3）。
			r.cancelApprovalAndConverge(ctx, rec.ID, approval.ID, in.RequestID)
			return toolOutcome{EmitErr: err}
		}

		decisionResult, err := r.approvals.Wait(ctx, approval.ID)
		if err != nil {
			// 等待异常终止同样未执行命令：取消审批 + 收敛 tool_call，再重置
			// 计时起点。
			r.cancelApprovalAndConverge(ctx, rec.ID, approval.ID, in.RequestID)
			execStart = r.now()
			if ctx.Err() != nil {
				return toolOutcome{Fatal: ctx.Err()}
			}
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "approval wait failed"}, Status: store.ToolFailed}
		}
		execStart = r.now()

		resolved := Event{Type: EventApprovalResolved, ApprovalResolved: &ApprovalResolvedPayload{
			ApprovalID:    approval.ID,
			Status:        decisionResult.Status,
			DecidedAt:     decisionResult.DecidedAt,
			EditedCommand: decisionResult.EditedCommand,
		}}
		if err := emit(resolved); err != nil {
			// 审批已决策但结果事件发不出去（客户端已断开）：命令不会执行，
			// tool_call 同样不能停在 awaiting_approval。
			r.convergeResolvedToolCall(ctx, rec.ID)
			return toolOutcome{EmitErr: err}
		}
		switch decisionResult.Status {
		case store.ApprovalApproved:
			// 继续执行；批准且用户改写时改执行改写后命令（契约 2.11 约定 3）。
			editedCommand = decisionResult.EditedCommand
		case store.ApprovalDenied:
			// 契约 9.7（六期打磨 sprint 6）：detail 补 command 与 risk_level
			// ——与 batchx 的拒绝行同口径（internal/batchx/execute.go）。
			// 此前只有 tool_call_id + approval_id，审计页「命令拒绝」行的
			// 命令摘要与结果列恒「—」，看不出拒的是哪条命令。
			r.auditEvent(ctx, "command.denied", "user", conversationActor(conv.ID), "server", srv.ID, &conv.ID, &srv.ID, map[string]any{
				"tool_call_id": rec.ID, "approval_id": approval.ID,
				"command": approval.Command, "risk_level": approval.RiskLevel,
			})
			return toolOutcome{
				Err:    &ToolError{Code: codeApprovalDenied, Message: "command was denied by the operator"},
				Status: store.ToolDenied,
			}
		default:
			return toolOutcome{
				Err:    &ToolError{Code: codeApprovalTimeout, Message: "approval expired before a decision"},
				Status: store.ToolExpired,
			}
		}
	}

	timeout := r.cfg.SSHDefaultTimeout
	if args.TimeoutSeconds != nil {
		seconds := *args.TimeoutSeconds
		if seconds < 1 {
			seconds = 1
		}
		if seconds > MaxSSHTimeoutS {
			seconds = MaxSSHTimeoutS
		}
		timeout = time.Duration(seconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 六期 BE-P：实际执行命令=改写后命令（decide 批准且用户改写时），
	// 否则模型原始命令。
	execCommand := command
	if editedCommand != "" {
		execCommand = editedCommand
	}

	// 契约 2.7 双轨路由：WinRM 认证走 winrmx（PowerShell 端点 + exit
	// $LASTEXITCODE）；SSH 通道保持登录 shell（Linux）或包装 PowerShell
	// 外壳（Windows OpenSSH）。审批流与审计不因通道变化。
	type runOutcome struct {
		exitCode             int
		combined             string
		truncated            bool
		durationMS           int64
		firstSeen            bool
		fingerprint, hostKey string
		cleartextWarning     string // telnet 网络设备：明文风险告警（契约 2.6 透传）
	}
	var res runOutcome
	var execErr error
	if isNetworkDevice(srv) {
		// 契约 2.8：网络设备命令不做任何包装，原样单条发送；审批流与
		// 审计不因通道变化（上方审批段与下方落库共用）。
		if r.netcli == nil {
			return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "netdev executor is not configured"}, Status: store.ToolFailed}
		}
		target, err := r.netDevTarget(srv)
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
		}
		out, err := r.netcli.Execute(runCtx, target, netdev.RunRequest{
			Command: execCommand, Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
		})
		if err == nil {
			res = runOutcome{exitCode: out.ExitCode, combined: out.Combined, truncated: out.Truncated, durationMS: out.DurationMS, cleartextWarning: out.CleartextWarning}
		}
		execErr = err
	} else if winRMChannel(srv) {
		if r.winrm == nil {
			return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "winrm executor is not configured"}, Status: store.ToolFailed}
		}
		target, err := r.winRMTarget(srv)
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
		}
		out, err := r.winrm.Execute(runCtx, target, winrmx.RunRequest{
			Command: execCommand, Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
		})
		if err == nil {
			res = runOutcome{exitCode: out.ExitCode, combined: out.Combined, truncated: out.Truncated, durationMS: out.DurationMS}
		}
		execErr = err
	} else {
		target, err := r.serverTarget(srv)
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}, Status: store.ToolFailed}
		}
		if r.ssh == nil {
			return toolOutcome{Err: &ToolError{Code: codeSSHExecFailed, Message: "ssh executor is not configured"}, Status: store.ToolFailed}
		}
		sshCommand := execCommand
		if srv.OS == store.ServerOSWindows {
			sshCommand = wrapPowerShellForSSH(execCommand)
		}
		out, err := r.ssh.Execute(runCtx, target, sshx.RunRequest{
			Command: sshCommand, Timeout: timeout, MaxOutputBytes: MaxOutputBytes,
		})
		if err == nil {
			res = runOutcome{
				exitCode: out.ExitCode, combined: out.Combined, truncated: out.Truncated,
				durationMS: out.DurationMS, firstSeen: out.FirstSeen,
				fingerprint: out.Fingerprint, hostKey: out.HostKey,
			}
		}
		execErr = err
	}
	if execErr != nil {
		if ctx.Err() != nil {
			return toolOutcome{Fatal: ctx.Err()}
		}
		code, msg := classifySSHError(runCtx, execErr)
		if isNetworkDevice(srv) {
			code, msg = classifyNetCLIError(execErr)
		}
		return toolOutcome{Err: &ToolError{Code: code, Message: msg}, Status: store.ToolFailed}
	}
	r.recordHostKey(ctx, srv, res.firstSeen, res.fingerprint, res.hostKey)

	var approvalPtr *string
	if approvalID != "" {
		approvalPtr = &approvalID
	}
	// 函数签名的命名返回值也叫 out，此处序列化结果改名避免遮蔽。
	// 六期 BE-P（契约 2.11 约定 3/4）：result 记录实际执行——command 为
	// 改写后命令（未改写=模型原始命令），回放的 execution 步据此重建。
	resultPayload := map[string]any{
		"server_id":   srv.ID,
		"command":     execCommand,
		"exit_code":   res.exitCode,
		"output":      res.combined,
		"truncated":   res.truncated,
		"duration_ms": res.durationMS,
		"risk_level":  string(decision.Level),
		"approval_id": approvalPtr,
	}
	if res.cleartextWarning != "" {
		// telnet 明文告警透传进工具结果（对话可见，契约 2.6）。
		resultPayload["cleartext_warning"] = res.cleartextWarning
	}
	resultJSON, _ := json.Marshal(resultPayload)
	// 契约 9.7：command.executed 携带实际执行的命令文本（免审批档下这是
	// 事后复原「跑了什么」的唯一途径）、approval_mode（直执行/审批执行均
	// 携带）；改写时另增 original_command（原命令）。
	executedDetail := map[string]any{
		"tool_call_id": rec.ID, "risk_level": string(decision.Level), "exit_code": res.exitCode,
		"approval_mode": string(approvalMode), "command": execCommand,
	}
	if editedCommand != "" {
		executedDetail["original_command"] = command
	}
	if res.cleartextWarning != "" {
		executedDetail["cleartext_warning"] = true
	}
	r.auditEvent(ctx, "command.executed", "model", conversationActor(conv.ID), "server", srv.ID, &conv.ID, &srv.ID, executedDetail)
	return toolOutcome{Result: resultJSON, Status: store.ToolSucceeded}
}

// convergedWriteTimeout 是 tool_call 收敛落库的写超时（同 approvals.Cancel
// 的 cancelWriteTimeout 口径）：请求 ctx 此时通常已取消，只给短暂机会。
const convergedWriteTimeout = 5 * time.Second

// convergeToolCallStatus 把审批等待中的 tool_call 从 awaiting_approval 收敛
// 为终态（六期打磨 sprint 3）。
//
// 背景：回答流断开（emit 失败）或 Wait 异常终止时，runner 会取消审批
// （approvals.Cancel → status=expired、note=stream_cancelled），但此前直接
// return EmitErr/Fatal——tool_call 永久停在 awaiting_approval：历史工具卡
// 常驻「待审批」，刷新后与同会话审批卡的「已取消」自相矛盾，回放同步错误。
//
// 只收敛仍在等待态的调用（先读一次再写：SetStatus 无条件覆盖，不能把其他
// 路径已写出的终态改成过期）。读失败或已终态直接跳过，绝不影响主流程。
// 落库用 WithoutCancel + 短超时（请求 ctx 此时多半已取消）。
func (r *Runner) convergeToolCallStatus(ctx context.Context, toolCallID, errorCode string) {
	if r.st == nil || toolCallID == "" {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), convergedWriteTimeout)
	defer cancel()
	cur, err := r.st.ToolCalls().ByID(writeCtx, toolCallID)
	if err == nil && cur.Status != store.ToolAwaitingApproval {
		return
	}
	if _, err := r.st.ToolCalls().SetStatus(writeCtx, toolCallID, store.ToolExpired, errorCode, ""); err != nil && r.log != nil {
		r.log.Warn("agent_converge_tool_call_failed", "tool_call_id", toolCallID, "error", err.Error())
	}
}

// cancelApprovalAndConverge 取消审批并把对应 tool_call 收敛为 expired。
// 「审批作废」与「工具状态收敛」是同一个语义动作，放在一处完成，避免
// 只做一半留下状态矛盾（六期打磨 sprint 3）。
func (r *Runner) cancelApprovalAndConverge(ctx context.Context, toolCallID, approvalID, requestID string) {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), convergedWriteTimeout)
	_, _ = r.approvals.Cancel(cancelCtx, approvalID, "stream_cancelled", requestID)
	cancel()
	r.convergeToolCallStatus(ctx, toolCallID, codeApprovalCancelled)
}

// convergeResolvedToolCall 用于「审批已决策、但 approval_resolved 事件发不
// 出去」：客户端已断开、命令不会执行，tool_call 收敛为 expired。审批本身
// 已正常决策，因此用 CLIENT_DISCONNECTED 而不是 APPROVAL_CANCELLED（与
// abort 的错误码同口径）。
func (r *Runner) convergeResolvedToolCall(ctx context.Context, toolCallID string) {
	r.convergeToolCallStatus(ctx, toolCallID, codeClientDisconnected)
}

func (r *Runner) runDBQuery(ctx context.Context, rec store.ToolCall, raw json.RawMessage) toolOutcome {
	var args dbQueryArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.DatabaseID == "" || args.SQL == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "database_id and sql are required"}, Status: store.ToolFailed}
	}
	if len(args.SQL) > MaxSQLBytes || strings.ContainsRune(args.SQL, 0) {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "sql is invalid or too large"}, Status: store.ToolFailed}
	}
	database, err := r.st.Databases().Get(ctx, args.DatabaseID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "DATABASE_NOT_FOUND", Message: "database not found"}, Status: store.ToolFailed}
	}
	engine, ok := sqlEngine(database.Engine)
	if !ok {
		return toolOutcome{Err: &ToolError{Code: codeSQLQueryFailed, Message: "unsupported database engine"}, Status: store.ToolFailed}
	}
	guard, err := sqlguard.Check(engine, args.SQL)
	if err != nil || !guard.Allowed {
		r.auditEvent(ctx, "database.query_rejected", "model", conversationActor(rec.ConversationID), "database", database.ID, &rec.ConversationID, nil, map[string]any{
			"tool_call_id": rec.ID, "rule_id": guard.RuleID,
		})
		return toolOutcome{
			Err:    &ToolError{Code: codeSQLNotReadOnly, Message: "only read-only queries are allowed"},
			Status: store.ToolFailed,
		}
	}
	if r.db == nil {
		return toolOutcome{Err: &ToolError{Code: codeSQLQueryFailed, Message: "database executor is not configured"}, Status: store.ToolFailed}
	}
	password, err := r.decrypt(database.PasswordEncrypted)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "database credentials are unavailable"}, Status: store.ToolFailed}
	}
	res, err := r.db.Query(ctx, database, password, dbproxy.QueryRequest{
		SQL: args.SQL, Timeout: 15 * time.Second, MaxRows: 1000, MaxBytes: 1048576,
	})
	if err != nil {
		code := codeSQLQueryFailed
		if errors.Is(err, dbproxy.ErrNotReadOnly) {
			code = codeSQLNotReadOnly
		} else if ctx.Err() != nil {
			code = codeSQLTimeout
		}
		return toolOutcome{Err: &ToolError{Code: code, Message: fmt.Sprintf("query failed: %v", err)}, Status: store.ToolFailed}
	}
	out, _ := json.Marshal(map[string]any{
		"database_id": database.ID,
		"columns":     res.Columns,
		"rows":        res.Rows,
		"row_count":   res.RowCount,
		"truncated":   res.Truncated,
		"duration_ms": res.DurationMS,
	})
	r.auditEvent(ctx, "database.query", "model", conversationActor(rec.ConversationID), "database", database.ID, &rec.ConversationID, nil, map[string]any{
		"tool_call_id": rec.ID, "row_count": res.RowCount,
	})
	return toolOutcome{Result: out, Status: store.ToolSucceeded}
}

// ---------------------------------------------------------------------------
// 凭据、主机密钥与审计辅助
// ---------------------------------------------------------------------------

// classifyNetCLIError 把 netdev 哨兵错误映射为运行错误码（契约 9.6）。
func classifyNetCLIError(err error) (code, message string) {
	switch {
	case errors.Is(err, netdev.ErrLoginFailed):
		return codeNetCLILoginFailed, "network device login failed"
	case errors.Is(err, netdev.ErrExecFailed):
		return codeNetCLIExecFailed, fmt.Sprintf("network device command failed: %v", err)
	case errors.Is(err, context.DeadlineExceeded):
		return codeSSHTimeout, "network device command timed out"
	default:
		return codeSSHExecFailed, fmt.Sprintf("network device command failed: %v", err)
	}
}

// netDevTarget 解密凭据并构造网络设备连接目标（契约 5.2B Target）。
func (r *Runner) netDevTarget(srv store.Server) (netdev.Target, error) {
	if srv.AuthMethod != store.AuthMethodPassword {
		return netdev.Target{}, errors.New("agent: network devices require password auth")
	}
	pw, err := r.decrypt(srv.PasswordEncrypted)
	if err != nil {
		return netdev.Target{}, err
	}
	enable, err := r.decrypt(srv.EnableSecretEncrypted)
	if err != nil {
		return netdev.Target{}, err
	}
	port := srv.Port
	if port == 0 {
		port = 22
	}
	return netdev.Target{
		ServerID:     srv.ID,
		Brand:        netdev.BrandOf(srv.NetdevBrand),
		AccessProto:  srv.AccessProto,
		Host:         srv.Host,
		Port:         port,
		Username:     srv.Username,
		Password:     pw,
		EnableSecret: enable,
		KnownHostKey: srv.HostKeyPublic,
	}, nil
}

// winRMTarget 解密凭据并构造 WinRM 连接目标（契约 5.2A：UseHTTPS 由
// password_basic 且端口 5986 推导，其余 false）。
func (r *Runner) winRMTarget(srv store.Server) (winrmx.Target, error) {
	if srv.AuthMethod != store.AuthMethodPasswordNTLM && srv.AuthMethod != store.AuthMethodPasswordBasic {
		return winrmx.Target{}, errors.New("agent: not a winrm auth method")
	}
	pw, err := r.decrypt(srv.PasswordEncrypted)
	if err != nil {
		return winrmx.Target{}, err
	}
	return winrmx.Target{
		ServerID: srv.ID,
		Host:     srv.Host,
		Port:     srv.WinRMPort,
		Username: srv.Username,
		Password: pw,
		UseHTTPS: srv.AuthMethod == store.AuthMethodPasswordBasic && srv.WinRMPort == 5986,
		AuthType: srv.AuthMethod,
	}, nil
}

func (r *Runner) serverTarget(srv store.Server) (sshx.Target, error) {
	target := sshx.Target{
		ServerID:     srv.ID,
		Host:         srv.Host,
		Port:         srv.Port,
		Username:     srv.Username,
		AuthMethod:   srv.AuthMethod,
		KnownHostKey: srv.HostKeyPublic,
	}
	switch srv.AuthMethod {
	case store.AuthMethodPassword:
		pw, err := r.decrypt(srv.PasswordEncrypted)
		if err != nil {
			return sshx.Target{}, err
		}
		target.Password = pw
	case store.AuthMethodPrivateKey:
		key, err := r.decrypt(srv.PrivateKeyEncrypted)
		if err != nil {
			return sshx.Target{}, err
		}
		passphrase, err := r.decrypt(srv.PassphraseEncrypted)
		if err != nil {
			return sshx.Target{}, err
		}
		target.PrivateKey = key
		target.Passphrase = passphrase
	default:
		return sshx.Target{}, errors.New("agent: unsupported auth method")
	}
	return target, nil
}

func (r *Runner) decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if r.cipher == nil {
		return "", errors.New("agent: cipher is not configured")
	}
	return r.cipher.Decrypt(ciphertext)
}

func (r *Runner) recordHostKey(ctx context.Context, srv store.Server, firstSeen bool, fingerprint, hostKey string) {
	if !firstSeen || hostKey == "" {
		return
	}
	if _, err := r.st.Servers().UpdateHostKey(ctx, srv.ID, fingerprint, hostKey, r.now()); err != nil && r.log != nil {
		r.log.Warn("agent_update_host_key_failed", "server_id", srv.ID, "error", err)
	}
	r.auditEvent(ctx, "server.host_key_trusted", "model", conversationActor(""), "server", srv.ID, nil, &srv.ID, map[string]any{
		"fingerprint": fingerprint,
	})
}

func classifySSHError(ctx context.Context, err error) (code, message string) {
	var hostKeyErr *sshx.HostKeyError
	switch {
	case errors.As(err, &hostKeyErr):
		return codeSSHHostKeyChanged, "server host key changed"
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() == context.DeadlineExceeded:
		return codeSSHTimeout, "ssh command timed out"
	default:
		return codeSSHExecFailed, fmt.Sprintf("ssh command failed: %v", err)
	}
}

func sqlEngine(engine store.DatabaseEngine) (sqlguard.Engine, bool) {
	switch engine {
	case store.EnginePostgres:
		return sqlguard.EnginePostgres, true
	case store.EngineMySQL:
		return sqlguard.EngineMySQL, true
	case store.EngineSQLite:
		return sqlguard.EngineSQLite, true
	default:
		return "", false
	}
}

func conversationActor(conversationID string) string {
	if conversationID == "" {
		return ""
	}
	return "conversation:" + conversationID
}

func serverIDFromArgs(tool string, args sshExecArgs, containerServerID string) *string {
	switch tool {
	case ToolSSHExec, ToolServerFacts:
		if args.ServerID != "" {
			id := args.ServerID
			return &id
		}
		return nil
	case ToolContainerList, ToolContainerInspect, ToolContainerAction, ToolContainerLogs, ToolImageList,
		ToolGetMetrics, ToolAuditSearch: // 五期 M1：get_metrics/audit_search 的 server_id 审计归属
		if containerServerID != "" {
			id := containerServerID
			return &id
		}
		return nil
	}
	return nil
}

// auditEnqueueTimeout 是审计入队允许的最长等待；正常情况下 events 缓冲
// (1024) 加后台批量消费会让 Enqueue 立即返回，这里只防 writer 停摆时永久阻塞。
const auditEnqueueTimeout = 5 * time.Second

func (r *Runner) auditEvent(ctx context.Context, action, actorType, actorID, resourceType, resourceID string, conversationID, serverID *string, detail map[string]any) {
	if r.audit == nil {
		return
	}
	e := audit.Event{
		ActorType:      actorType,
		ActorID:        actorID,
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		ConversationID: conversationID,
		ServerID:       serverID,
		Detail:         detail,
	}
	// 工具轮次的审计必须落库，即使 SSE 客户端已断开（runCtx 已取消）：
	// Enqueue 的 select 在「channel 可发送」与「ctx.Done」同时就绪时按 Go
	// 语义伪随机选择，直接传已取消的 ctx 会让事件约半数被静默丢弃。
	// 与 abort/approvals 落库一致，使用 WithoutCancel 加短超时。
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEnqueueTimeout)
	defer cancel()
	if err := r.audit.Enqueue(enqueueCtx, e); err != nil && r.log != nil {
		r.log.Warn("agent_audit_enqueue_failed", "action", action, "error", err)
	}
}
