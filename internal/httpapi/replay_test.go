// replay_test.go 覆盖六期 BE-P 会话执行回放（契约 2.11 约定 4）：
// 排序稳定性（timestamp+kind 权重+来源键、execution=finished_at ?? started_at）、
// approval 步 command=edited ?? original、ACL（会话不存在 404）、
// 空会话（steps=[] truncated=false）。另覆盖 settings 端点与
// PATCH approval_mode / effective_approval_mode 装配。
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

func putSetting(t *testing.T, env *testEnv, key, value string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	env.st.settings[key] = store.Setting{Key: key, Value: raw, UpdatedAt: time.Now()}
	env.st.mu.Unlock()
}

// seedReplayConversation 构造一个带 user/assistant(reasoning)/tool_call/
// execution/approval(edited) 的会话，返回会话 ID。
func seedReplayConversation(t *testing.T, env *testEnv) string {
	t.Helper()
	conv := store.Conversation{ID: "conv-replay", Title: "回放"}
	env.st.mu.Lock()
	env.st.conversations[conv.ID] = conv
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	// user 消息（seq 1）。
	env.st.messages = append(env.st.messages, store.Message{
		ID: "msg-1", Seq: 1, ConversationID: conv.ID, TurnID: "turn-1",
		Role: store.RoleUser, Content: "看一下磁盘", Status: store.MessageComplete, CreatedAt: base,
	})
	// 工具调用（seq 2）：ssh_exec 成功，result 含 command/exit_code/output/duration。
	fin := base.Add(2 * time.Second)
	env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
		ID: "tc-1", Seq: 2, ConversationID: conv.ID, TurnID: "turn-1",
		ProviderCallID: "call_1", Name: "ssh_exec",
		Arguments: json.RawMessage(`{"server_id":"srv-1","command":"df -h"}`),
		Result:    json.RawMessage(`{"server_id":"srv-1","command":"df -h","exit_code":0,"output":"Filesystem","truncated":false,"duration_ms":120,"risk_level":"safe","approval_id":null}`),
		RiskLevel: "safe", Status: store.ToolSucceeded, StartedAt: base.Add(time.Second), FinishedAt: &fin,
	})
	// assistant 消息（seq 3）：content + reasoning + 服务端计时（六期打磨
	// sprint 1：reasoning_duration_ms 必须进回放载荷）。
	reasoningMS := int64(1000)
	env.st.messages = append(env.st.messages, store.Message{
		ID: "msg-2", Seq: 3, ConversationID: conv.ID, TurnID: "turn-1",
		Role: store.RoleAssistant, Content: "磁盘充足", Reasoning: "先查 df",
		ReasoningDurationMS: &reasoningMS,
		Status:              store.MessageComplete, CreatedAt: base.Add(3 * time.Second),
	})
	// 改写过的审批。
	convID := conv.ID
	tcID := "tc-1"
	apID := "ap-1"
	env.st.approvals[apID] = store.Approval{
		ID: apID, ConversationID: &convID, ToolCallID: &tcID, Source: "chat",
		ServerID: strptr("srv-1"), ServerName: "web-1",
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "SERVICE_CHANGE", Reason: "service change", Status: store.ApprovalApproved,
		ApprovalMode:    store.ApprovalModeStandard,
		RequestedAt:     base.Add(1500 * time.Millisecond),
		DecidedAt:       strptime(base.Add(1600 * time.Millisecond)),
		EditedCommand:   "systemctl restart nginx --no-block",
		EditedRiskLevel: "needs_approval",
		EditedRuleID:    "SERVICE_CHANGE",
	}
	env.st.mu.Unlock()
	return conv.ID
}

func strptr(s string) *string { return &s }

func strptime(t time.Time) *time.Time { return &t }

func TestConversationReplayOrdering(t *testing.T) {
	env := newTestEnv(t)
	convID := seedReplayConversation(t, env)

	rec := env.do(http.MethodGet, "/api/conversations/"+convID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ConversationID string       `json:"conversation_id"`
		Steps          []replayStep `json:"steps"`
		Truncated      bool         `json:"truncated"`
	}
	decodeBody(t, rec, &resp)
	if resp.ConversationID != convID {
		t.Fatalf("conversation_id = %s", resp.ConversationID)
	}
	if resp.Truncated {
		t.Fatal("truncated should be false")
	}
	wantKinds := []string{
		"user_message", // ts=10:00:00
		"tool_call",    // ts=10:00:01
		"approval",     // ts=10:00:01.5
		"execution",    // ts=finished_at=10:00:03
		"reasoning",    // ts=10:00:03（assistant 消息；同 ts 下 reasoning(4) < assistant_text(5)）
		"assistant_text",
	}
	if len(resp.Steps) != len(wantKinds) {
		t.Fatalf("steps = %d, want %d: %+v", len(resp.Steps), len(wantKinds), resp.Steps)
	}
	for i, want := range wantKinds {
		if resp.Steps[i].Kind != want {
			t.Errorf("step[%d].kind = %s, want %s", i, resp.Steps[i].Kind, want)
		}
		if resp.Steps[i].Seq != int64(i+1) {
			t.Errorf("step[%d].seq = %d, want %d", i, resp.Steps[i].Seq, i+1)
		}
	}
	// execution 步从 result 提取。
	execStep := resp.Steps[3]
	if execStep.ToolCallID == nil || *execStep.ToolCallID != "tc-1" {
		t.Fatalf("execution tool_call_id = %v", execStep.ToolCallID)
	}
	if execStep.ExitCode == nil || *execStep.ExitCode != 0 {
		t.Fatalf("execution exit_code = %v", execStep.ExitCode)
	}
	if execStep.DurationMS == nil || *execStep.DurationMS != 120 {
		t.Fatalf("execution duration_ms = %v", execStep.DurationMS)
	}
	if execStep.Command == nil || *execStep.Command != "df -h" {
		t.Fatalf("execution command = %v", execStep.Command)
	}
	// approval 步：command=edited ?? original；original 仅改写时非 null。
	apStep := resp.Steps[2]
	if apStep.Command == nil || *apStep.Command != "systemctl restart nginx --no-block" {
		t.Fatalf("approval command = %v, want edited", apStep.Command)
	}
	if apStep.OriginalCommand == nil || *apStep.OriginalCommand != "systemctl restart nginx" {
		t.Fatalf("approval original_command = %v", apStep.OriginalCommand)
	}
	if apStep.EditedRiskLevel == nil || *apStep.EditedRiskLevel != "needs_approval" {
		t.Fatalf("approval edited_risk_level = %v", apStep.EditedRiskLevel)
	}
	// reasoning 步带服务端计时（六期打磨 sprint 1：此前恒 null，回放摘要
	// 永远显示「深度思考 · 耗时 —」）。
	reasoningStep := resp.Steps[4]
	if reasoningStep.Kind != "reasoning" {
		t.Fatalf("step[4].kind = %s, want reasoning", reasoningStep.Kind)
	}
	if reasoningStep.ReasoningDurationMS == nil || *reasoningStep.ReasoningDurationMS != 1000 {
		t.Fatalf("reasoning reasoning_duration_ms = %v, want 1000", reasoningStep.ReasoningDurationMS)
	}
	// 非 reasoning 步为 null（字段全集写死，未用字段为 null）。
	if resp.Steps[5].ReasoningDurationMS != nil {
		t.Fatalf("assistant_text reasoning_duration_ms = %v, want null",
			*resp.Steps[5].ReasoningDurationMS)
	}
}

func TestConversationReplayACLAndEmpty(t *testing.T) {
	env := newTestEnv(t)
	// ACL：不存在的会话 404（登录态仍要求会话存在）。
	rec := env.do(http.MethodGet, "/api/conversations/missing/replay", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing conversation status = %d", rec.Code)
	}
	// 空会话：steps=[] 且 truncated=false。
	conv := store.Conversation{ID: "conv-empty", Title: "空"}
	env.st.mu.Lock()
	env.st.conversations[conv.ID] = conv
	env.st.mu.Unlock()
	rec = env.do(http.MethodGet, "/api/conversations/conv-empty/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty conversation status = %d", rec.Code)
	}
	if body := rec.Body.String(); !json.Valid([]byte(body)) {
		t.Fatalf("invalid json: %s", body)
	}
	var resp struct {
		Steps     []replayStep `json:"steps"`
		Truncated bool         `json:"truncated"`
	}
	decodeBody(t, rec, &resp)
	if resp.Steps == nil || len(resp.Steps) != 0 {
		t.Fatalf("steps = %+v, want []", resp.Steps)
	}
	if resp.Truncated {
		t.Fatal("truncated should be false for empty conversation")
	}
}

// TestConversationReplayTruncation：步数超 20000 丢弃最旧并置 truncated
// （用 buildReplaySteps 直接构造，避免 SQL 灌 2 万行）。
func TestConversationReplayTruncation(t *testing.T) {
	const n = replayMaxSteps + 5
	msgs := make([]store.Message, 0, n)
	base := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		msgs = append(msgs, store.Message{
			ID: "m", Seq: int64(i + 1), Role: store.RoleUser, Content: "x",
			Status: store.MessageComplete, CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	steps := buildReplaySteps(msgs, nil, nil, nil, nil)
	if len(steps) != n {
		t.Fatalf("expanded steps = %d, want %d", len(steps), n)
	}
	// 复制 handler 的排序+截断逻辑验证上限（handler 内联，此处同口径断言）。
	if len(steps) > replayMaxSteps {
		steps = steps[len(steps)-replayMaxSteps:]
	}
	if len(steps) != replayMaxSteps {
		t.Fatalf("truncated steps = %d, want %d", len(steps), replayMaxSteps)
	}
	if steps[0].Seq != 0 { // 排序键未重编前保持原值
		t.Fatalf("unexpected seq %d", steps[0].Seq)
	}
}

// TestConversationReplayToolCallFailureFields 覆盖六期打磨 sprint 2 的三处
// 回放载荷缺口（走查 medium）：
//  1. tool_call 步必须带 error_code（此前恒 null，失败调用看不出原因）；
//  2. tool_call 步必须带 approval_mode（免审批档直执行的会话据此显示
//     「高风险 · 免审批执行」而不是「需审批」）；
//  3. 失败且**没有 result** 的命令调用也要生成 execution 步（status=failed
//     + error_code + exit_code=-1 + output 空串）；denied/expired 等「命令
//     从未执行」的情形仍然不产生 execution 步。
func TestConversationReplayToolCallFailureFields(t *testing.T) {
	env := newTestEnv(t)
	conv := store.Conversation{ID: "conv-fail", Title: "失败回放"}
	env.st.mu.Lock()
	env.st.conversations[conv.ID] = conv
	base := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	fin := base.Add(2 * time.Second)
	// SSH 通道失败：runner 在写 result 之前早退，tool_calls.result 为空。
	env.st.toolCalls = append(env.st.toolCalls,
		store.ToolCall{
			ID: "tc-fail", Seq: 1, ConversationID: conv.ID, TurnID: "turn-1",
			ProviderCallID: "call_fail", Name: "ssh_exec",
			Arguments:    json.RawMessage(`{"server_id":"srv-1","command":"uptime"}`),
			RiskLevel:    "needs_approval",
			Status:       store.ToolFailed,
			ErrorCode:    "SSH_EXEC_FAILED",
			ApprovalMode: string(store.ApprovalModeNone),
			StartedAt:    base, FinishedAt: &fin,
		},
		// 被拒绝的调用：never executed，result 空且状态 denied。
		store.ToolCall{
			ID: "tc-denied", Seq: 2, ConversationID: conv.ID, TurnID: "turn-1",
			ProviderCallID: "call_denied", Name: "ssh_exec",
			Arguments: json.RawMessage(`{"server_id":"srv-1","command":"reboot"}`),
			RiskLevel: "needs_approval", Status: store.ToolDenied,
			ErrorCode: "APPROVAL_DENIED", StartedAt: base.Add(time.Second),
		},
	)
	env.st.mu.Unlock()

	rec := env.do(http.MethodGet, "/api/conversations/"+conv.ID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Steps []replayStep `json:"steps"`
	}
	decodeBody(t, rec, &resp)
	if len(resp.Steps) != 3 {
		t.Fatalf("steps = %d, want 3: %+v", len(resp.Steps), resp.Steps)
	}
	// 按 id/kind 取步（排序由 timestamp+kind 权重决定：tc-fail(base) <
	// tc-denied(base+1s) < execution(base+2s)，断言不依赖具体下标）。
	find := func(kind, toolCallID string) *replayStep {
		for i := range resp.Steps {
			s := resp.Steps[i]
			if s.Kind == kind && s.ToolCallID != nil && *s.ToolCallID == toolCallID {
				return &resp.Steps[i]
			}
		}
		return nil
	}

	tcStep := find("tool_call", "tc-fail")
	if tcStep == nil {
		t.Fatalf("tool_call step for tc-fail missing: %+v", resp.Steps)
	}
	if tcStep.ErrorCode == nil || *tcStep.ErrorCode != "SSH_EXEC_FAILED" {
		t.Fatalf("tool_call error_code = %v, want SSH_EXEC_FAILED", tcStep.ErrorCode)
	}
	if tcStep.ApprovalMode == nil || *tcStep.ApprovalMode != "none" {
		t.Fatalf("tool_call approval_mode = %v, want none", tcStep.ApprovalMode)
	}

	// 失败调用也产生 execution 步（无 result）：failed + error_code +
	// exit_code=-1 + output 空串。
	execStep := find("execution", "tc-fail")
	if execStep == nil {
		t.Fatalf("execution step for failed tc-fail missing: %+v", resp.Steps)
	}
	if execStep.Status == nil || *execStep.Status != "failed" {
		t.Fatalf("execution status = %v, want failed", execStep.Status)
	}
	if execStep.ErrorCode == nil || *execStep.ErrorCode != "SSH_EXEC_FAILED" {
		t.Fatalf("execution error_code = %v, want SSH_EXEC_FAILED", execStep.ErrorCode)
	}
	if execStep.ExitCode == nil || *execStep.ExitCode != -1 {
		t.Fatalf("execution exit_code = %v, want -1", execStep.ExitCode)
	}
	if execStep.Output == nil || *execStep.Output != "" {
		t.Fatalf("execution output = %v, want empty string", execStep.Output)
	}

	// 被拒绝的调用：只有 tool_call 步，无 execution 步（命令从未执行）。
	denied := find("tool_call", "tc-denied")
	if denied == nil {
		t.Fatalf("tool_call step for tc-denied missing: %+v", resp.Steps)
	}
	if denied.ErrorCode == nil || *denied.ErrorCode != "APPROVAL_DENIED" {
		t.Fatalf("denied tool_call error_code = %v", denied.ErrorCode)
	}
	// denied 没有 approval_mode 快照 → null（不渲染门限徽标）。
	if denied.ApprovalMode != nil {
		t.Fatalf("denied approval_mode = %v, want null", *denied.ApprovalMode)
	}
	if find("execution", "tc-denied") != nil {
		t.Fatal("denied call must not produce an execution step")
	}
}

// TestReplayUserMessageStripsContextPrefix：回放 user_message 必须与聊天气泡
// 同口径剥离 ChatPage 的内部上下文前缀（六期打磨 sprint 2 走查：此前回放
// 原样展示 `<!--opsmind-context … -->`）。
func TestReplayUserMessageStripsContextPrefix(t *testing.T) {
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	msgs := []store.Message{
		{
			ID: "m1", Seq: 1, Role: store.RoleUser,
			Content: "<!--opsmind-context\nservers: walk-ssh (id=bf590bd6-0000)\n-->\n__walk_uptime__",
			Status:  store.MessageComplete, CreatedAt: base,
		},
		{
			// 正文本身以 `<!--` 开头但不是上下文标记：不得误剥。
			ID: "m2", Seq: 2, Role: store.RoleUser,
			Content: "<!-- 这是普通 HTML 注释 --> 正文",
			Status:  store.MessageComplete, CreatedAt: base.Add(time.Second),
		},
		{
			// 无前缀原样返回。
			ID: "m3", Seq: 3, Role: store.RoleUser,
			Content: "普通消息", Status: store.MessageComplete, CreatedAt: base.Add(2 * time.Second),
		},
	}
	steps := buildReplaySteps(msgs, nil, nil, nil, nil)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	if got := *steps[0].Content; got != "__walk_uptime__" {
		t.Fatalf("content = %q, want %q", got, "__walk_uptime__")
	}
	if got := *steps[1].Content; got != "<!-- 这是普通 HTML 注释 --> 正文" {
		t.Fatalf("plain comment content = %q", got)
	}
	if got := *steps[2].Content; got != "普通消息" {
		t.Fatalf("plain content = %q", got)
	}
}

func TestSettingsEndpoints(t *testing.T) {
	env := newTestEnv(t)
	// 键缺失兜底 standard。
	rec := env.do(http.MethodGet, "/api/settings", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approval_mode":"standard"`) {
		t.Fatalf("GET default = %d %s", rec.Code, rec.Body.String())
	}
	// PUT 非法值 400。
	rec = env.do(http.MethodPut, "/api/settings", `{"approval_mode":"fast"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid = %d", rec.Code)
	}
	// PUT 合法值 200 且生效。
	rec = env.do(http.MethodPut, "/api/settings", `{"approval_mode":"high"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approval_mode":"high"`) {
		t.Fatalf("PUT high = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/settings", "")
	if !strings.Contains(rec.Body.String(), `"approval_mode":"high"`) {
		t.Fatalf("GET after PUT = %s", rec.Body.String())
	}
	// 非法 JSON 值兜底 standard。
	putSetting(t, env, "approval.mode", "bogus-not-json-string")
	rec = env.do(http.MethodGet, "/api/settings", "")
	if !strings.Contains(rec.Body.String(), `"approval_mode":"standard"`) {
		t.Fatalf("GET malformed = %s", rec.Body.String())
	}
	// 未登录 401。
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	raw := httptest.NewRecorder()
	env.router.ServeHTTP(raw, req)
	if raw.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d", raw.Code)
	}
}

func TestConversationApprovalModeOverride(t *testing.T) {
	env := newTestEnv(t)
	putSetting(t, env, "approval.mode", "standard")
	conv := store.Conversation{ID: "conv-mode", Title: "门限"}
	env.st.mu.Lock()
	env.st.conversations[conv.ID] = conv
	env.st.mu.Unlock()

	// 设置覆盖：effective=high。
	rec := env.do(http.MethodPatch, "/api/conversations/conv-mode", `{"approval_mode":"high"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH set = %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ApprovalMode          *string `json:"approval_mode"`
		EffectiveApprovalMode string  `json:"effective_approval_mode"`
	}
	decodeBody(t, rec, &got)
	if got.ApprovalMode == nil || *got.ApprovalMode != "high" {
		t.Fatalf("approval_mode = %v", got.ApprovalMode)
	}
	if got.EffectiveApprovalMode != "high" {
		t.Fatalf("effective = %s", got.EffectiveApprovalMode)
	}
	// GET 同样装配。
	rec = env.do(http.MethodGet, "/api/conversations/conv-mode", "")
	decodeBody(t, rec, &got)
	if got.EffectiveApprovalMode != "high" {
		t.Fatalf("GET effective = %s", got.EffectiveApprovalMode)
	}
	// null 清除覆盖：effective 回到全局 standard、approval_mode=null。
	rec = env.do(http.MethodPatch, "/api/conversations/conv-mode", `{"approval_mode":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH null = %d %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &got)
	if got.ApprovalMode != nil {
		t.Fatalf("approval_mode after null = %v, want null", got.ApprovalMode)
	}
	if got.EffectiveApprovalMode != "standard" {
		t.Fatalf("effective after null = %s", got.EffectiveApprovalMode)
	}
	// 非法枚举 400。
	rec = env.do(http.MethodPatch, "/api/conversations/conv-mode", `{"approval_mode":"fast"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH invalid = %d", rec.Code)
	}
	// 空请求体 400（title 与 approval_mode 至少一项）。
	rec = env.do(http.MethodPatch, "/api/conversations/conv-mode", `{"title":null}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH empty = %d", rec.Code)
	}
	// 不存在的会话 404。
	rec = env.do(http.MethodPatch, "/api/conversations/missing", `{"approval_mode":"all"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing = %d", rec.Code)
	}
}

// TestReplayApplyTruncationReasons 覆盖六期打磨 sprint 4 走查 low：
// 两种截断来源此前共用一个 truncated 标志，前端横幅硬编码「超出 20000 步
// 上限」——审批满 200 条时用户看到 200 步却被告知 20000 上限。现在原因
// 随响应下发（steps / approvals / 空串）。
func TestReplayApplyTruncationReasons(t *testing.T) {
	step := func(seq int64) replayStep {
		return replayStep{Seq: seq, Kind: "user_message", kindWeight: replayKindUserMessage}
	}

	// 未截断：原样返回、false、空原因。
	steps := []replayStep{step(1), step(2)}
	got, truncated, reason := replayApplyTruncation(steps, true)
	if len(got) != 2 || truncated || reason != "" {
		t.Fatalf("no truncation: len=%d truncated=%v reason=%q", len(got), truncated, reason)
	}

	// 审批满页：truncated=true、reason=approvals、步骤数不变。
	got, truncated, reason = replayApplyTruncation(steps, false)
	if len(got) != 2 || !truncated || reason != replayTruncatedApprovals {
		t.Fatalf("approvals truncation: len=%d truncated=%v reason=%q", len(got), truncated, reason)
	}

	// 步数上限：丢弃最旧、保留最近 replayMaxSteps 条，reason=steps。
	long := make([]replayStep, 0, replayMaxSteps+3)
	for i := 0; i < replayMaxSteps+3; i++ {
		long = append(long, step(int64(i+1)))
	}
	got, truncated, reason = replayApplyTruncation(long, true)
	if len(got) != replayMaxSteps || !truncated || reason != replayTruncatedSteps {
		t.Fatalf("steps truncation: len=%d truncated=%v reason=%q", len(got), truncated, reason)
	}
	if got[0].Seq != 4 {
		t.Fatalf("oldest steps must be dropped, first seq = %d", got[0].Seq)
	}

	// 两者同时命中：以步数为准（步骤确实被丢弃，口径应为 20000）。
	_, truncated, reason = replayApplyTruncation(long, false)
	if !truncated || reason != replayTruncatedSteps {
		t.Fatalf("both truncations: truncated=%v reason=%q", truncated, reason)
	}
}

// TestConversationReplayTruncatedReasonApprovals 走真实 handler：单状态
// 审批满 200 条时响应 truncated=true 且 truncated_reason=approvals，
// 步骤数远小于 20000（正是走查里自相矛盾的场景）。
func TestConversationReplayTruncatedReasonApprovals(t *testing.T) {
	env := newTestEnv(t)
	conv := store.Conversation{ID: "conv-ap-full", Title: "审批满页"}
	env.st.mu.Lock()
	env.st.conversations[conv.ID] = conv
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	convID := conv.ID
	for i := 0; i < replayPageSize+5; i++ {
		env.st.approvals[fmt.Sprintf("ap-%03d", i)] = store.Approval{
			ID: fmt.Sprintf("ap-%03d", i), ConversationID: &convID, Source: "chat",
			ServerID: strptr("srv-1"), ServerName: "web-1", Command: "uptime",
			RiskLevel: "needs_approval", RuleID: "SAFE", Reason: "r",
			Status: store.ApprovalPending, ApprovalMode: store.ApprovalModeStandard,
			// requested_at 递减：模拟 List 的 ORDER BY requested_at DESC。
			RequestedAt: base.Add(-time.Duration(i) * time.Second),
		}
	}
	env.st.mu.Unlock()

	rec := env.do(http.MethodGet, "/api/conversations/"+conv.ID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Steps           []replayStep `json:"steps"`
		Truncated       bool         `json:"truncated"`
		TruncatedReason string       `json:"truncated_reason"`
	}
	decodeBody(t, rec, &resp)
	if !resp.Truncated {
		t.Fatal("truncated must be true when a status page is full")
	}
	if resp.TruncatedReason != replayTruncatedApprovals {
		t.Fatalf("truncated_reason = %q, want %q", resp.TruncatedReason, replayTruncatedApprovals)
	}
	if len(resp.Steps) >= replayMaxSteps {
		t.Fatalf("steps = %d, must be far below %d (the contradiction under test)", len(resp.Steps), replayMaxSteps)
	}
}
