// runner_phase6sprint3_test.go 覆盖六期打磨 sprint 3 的审批取消态收敛
// （契约 6.3）：回答流断开 / Wait 异常终止时，除了取消审批（expired +
// note=stream_cancelled），关联 tool_call 也必须从 awaiting_approval 收敛为
// expired 并落 error_code=APPROVAL_CANCELLED。
//
// 走查实测的缺陷：发一轮 needs_approval 命令 → 审批卡出现 → 离开会话再回来，
// 审批为 expired+stream_cancelled（对话卡显示「已取消」），但
// GET /api/conversations/:id/tool-calls 仍是 awaiting_approval——历史工具卡
// 常驻「待审批」，回放同步显示待审批。全仓此前只有 batchx 有过期收敛循环。
package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"aiops/internal/approvals"
	"aiops/internal/llm"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// blockingApprovals 的 Wait 阻塞到 ctx 取消（模拟「审批卡出现后用户停止
// 生成 / 客户端断开」——Wait 返回 ctx.Err，runner 走 Fatal 分支）。
type blockingApprovals struct {
	*fakeApprovals
	once        sync.Once
	waitEntered sync.WaitGroup
}

func (b *blockingApprovals) Wait(ctx context.Context, _ string) (approvals.Decision, error) {
	b.once.Do(b.waitEntered.Done)
	<-ctx.Done()
	return approvals.Decision{}, ctx.Err()
}

// awaitingToolCall 返回 store 里第一条仍处于 awaiting_approval 的工具调用。
func awaitingToolCalls(st *testStore) []store.ToolCall {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]store.ToolCall, 0, 1)
	for _, tc := range st.toolCalls {
		if tc.Status == store.ToolAwaitingApproval {
			out = append(out, tc)
		}
	}
	return out
}

// TestRunStreamEmitFailureConvergesToolCall 回答流在 approval_required 事件
// 处断开：审批被取消（既有行为），tool_call 必须收敛为 expired +
// APPROVAL_CANCELLED（本次修复）。
func TestRunStreamEmitFailureConvergesToolCall(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		if ev.Type == EventApprovalRequired {
			return errors.New("client gone")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected emit failure")
	}
	// 既有行为不变：审批仍被取消。
	if len(ap.cancelled) != 1 || ap.cancelled[0] != "ap-1:stream_cancelled" {
		t.Fatalf("approval cancel = %v, want ap-1:stream_cancelled", ap.cancelled)
	}
	// 修复点：tool_call 不得停在 awaiting_approval。
	if left := awaitingToolCalls(st); len(left) != 0 {
		t.Fatalf("tool calls still awaiting_approval: %+v", left)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.toolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", st.toolCalls)
	}
	tc := st.toolCalls[0]
	if tc.Status != store.ToolExpired {
		t.Fatalf("tool call status = %q, want expired", tc.Status)
	}
	if tc.ErrorCode != "APPROVAL_CANCELLED" {
		t.Fatalf("tool call error_code = %q, want APPROVAL_CANCELLED", tc.ErrorCode)
	}
}

// TestRunApprovalWaitFailureConvergesToolCall Wait 异常终止（非 ctx 取消）：
// 审批取消 + tool_call 必须离开 awaiting_approval。这条路径最终由
// executeToolCall 的 Finish 落 failed/INTERNAL（工具确实失败了，不是过期），
// 关键断言是「不残留待审批」。
func TestRunApprovalWaitFailureConvergesToolCall(t *testing.T) {
	st := seed(false)
	ap := &errApprovals{waitErr: errors.New("waiter stopped")}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
		{events: finalEvents("approval wait broke", llm.Usage{})},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, ap, client)

	events, err := runCollect(r, runInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := lastToolResult(events)
	if res == nil || res.Error == nil || res.Error.Code != codeInternal {
		t.Fatalf("result = %+v", res)
	}
	if len(ap.cancelled) != 1 {
		t.Fatalf("cancelled = %v", ap.cancelled)
	}
	if left := awaitingToolCalls(st); len(left) != 0 {
		t.Fatalf("tool calls still awaiting_approval: %+v", left)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.toolCalls[0].Status != store.ToolFailed {
		t.Fatalf("tool call = %+v, want failed（等待异常，非过期）", st.toolCalls[0])
	}
}

// TestRunCanceledContextConvergesToolCall ctx 取消（用户停止生成 / 客户端断开
// 导致 Wait 返回 ctx.Err）→ toolOutcome.Fatal，executeToolCall 提前 return
// 不走 Finish：这是「永久停在 awaiting_approval」的主路径，必须收敛为
// expired/APPROVAL_CANCELLED。
func TestRunCanceledContextConvergesToolCall(t *testing.T) {
	st := seed(false)
	ap := &blockingApprovals{fakeApprovals: &fakeApprovals{}}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
	}}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 等审批卡出现（Wait 进入阻塞）后取消本轮。
	ap.waitEntered.Add(1)
	go func() {
		ap.waitEntered.Wait()
		cancel()
	}()
	err := r.Run(ctx, runInput(), func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected run error after cancel")
	}
	if len(ap.cancelled) != 1 {
		t.Fatalf("cancelled = %v, want 1", ap.cancelled)
	}
	if left := awaitingToolCalls(st); len(left) != 0 {
		t.Fatalf("tool calls still awaiting_approval: %+v", left)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tc := st.toolCalls[0]
	if tc.Status != store.ToolExpired || tc.ErrorCode != "APPROVAL_CANCELLED" {
		t.Fatalf("tool call = %+v, want expired/APPROVAL_CANCELLED", tc)
	}
}

// TestRunApprovalResolvedEmitFailureConvergesToolCall 审批已决策但
// approval_resolved 发不出去：审批本身没有被取消（不能写
// APPROVAL_CANCELLED），tool_call 仍须离开 awaiting_approval，落
// CLIENT_DISCONNECTED（与 abort 的错误码同口径）。
func TestRunApprovalResolvedEmitFailureConvergesToolCall(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		if ev.Type == EventApprovalResolved {
			return errors.New("client gone")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected emit failure")
	}
	// 审批已决策：这条路径不应再 Cancel 一次。
	if len(ap.cancelled) != 0 {
		t.Fatalf("cancelled = %v, want none（审批已决策）", ap.cancelled)
	}
	if left := awaitingToolCalls(st); len(left) != 0 {
		t.Fatalf("tool calls still awaiting_approval: %+v", left)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tc := st.toolCalls[0]
	if tc.Status != store.ToolExpired {
		t.Fatalf("tool call status = %q, want expired", tc.Status)
	}
	if tc.ErrorCode != codeClientDisconnected {
		t.Fatalf("tool call error_code = %q, want CLIENT_DISCONNECTED", tc.ErrorCode)
	}
}

// ctxCancelSSH 的 Execute 阻塞到 ctx 取消后返回 ctx.Err——模拟「审批已
// 批准、命令执行中客户端关闭页面」：执行通道随请求 ctx 取消而失败。
type ctxCancelSSH struct {
	sshx.Executor
	entered chan struct{}
	once    sync.Once
}

func (c *ctxCancelSSH) Execute(ctx context.Context, _ sshx.Target, _ sshx.RunRequest) (sshx.RunResult, error) {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	return sshx.RunResult{}, ctx.Err()
}

// TestRunApprovedExecCancelConvergesToolCall 第 6 轮走查 medium 回归：
// 审批者批准的同时发起人关闭/刷新页面，命令尚未执行完就被 ctx 取消，
// runSSHExec 返回 Fatal、executeToolCall 跳过 Finish——tool_call 永久停在
// awaiting_approval，而审批已是 approved 并从审批队列消失，发起人无法再
// 推进只能重发。断流必须把仍处于等待态的行收敛为 expired；审批已决策
// （approved）时错误码是 CLIENT_DISCONNECTED，且不得再 Cancel 审批。
func TestRunApprovedExecCancelConvergesToolCall(t *testing.T) {
	st := seed(false)
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
	}}
	ssh := &ctxCancelSSH{entered: make(chan struct{})}
	r := newRunner(st, ssh, nil, ap, client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ssh.entered
		cancel()
	}()
	if err := r.Run(ctx, runInput(), func(Event) error { return nil }); err == nil {
		t.Fatal("expected run error after cancel")
	}
	// 审批已 approved（已从审批队列消失）：收敛不得把它改成 expired。
	if len(ap.cancelled) != 0 {
		t.Fatalf("cancelled = %v, want none（审批已批准）", ap.cancelled)
	}
	if left := awaitingToolCalls(st); len(left) != 0 {
		t.Fatalf("tool calls still awaiting_approval: %+v", left)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.toolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", st.toolCalls)
	}
	tc := st.toolCalls[0]
	if tc.Status != store.ToolExpired || tc.ErrorCode != codeClientDisconnected {
		t.Fatalf("tool call = %+v, want expired/CLIENT_DISCONNECTED", tc)
	}
}

// racingApprovals 模拟「审批者 decide 与发起人断流同时发生」：Wait 因 ctx
// 取消返回，Cancel 发现审批已被另一请求决策（store.ErrConflict），审批已是
// approved/denied 并从审批队列消失。
type racingApprovals struct {
	*fakeApprovals
	waitEntered sync.WaitGroup
}

func (f *racingApprovals) Wait(ctx context.Context, _ string) (approvals.Decision, error) {
	f.waitEntered.Done()
	<-ctx.Done()
	return approvals.Decision{}, ctx.Err()
}

func (f *racingApprovals) Cancel(context.Context, string, string, string) (store.Approval, error) {
	return store.Approval{}, store.ErrConflict
}

// TestRunWaitCancelRacingDecisionUsesClientDisconnected 竞态回归：审批者在
// 断流同一瞬间完成 decide 时 Cancel 会失败（ErrConflict）。审批已不在队列，
// 不能按「流取消连带作废」写 APPROVAL_CANCELLED；命令未执行，tool_call 按
// 断流口径收敛为 expired/CLIENT_DISCONNECTED。
func TestRunWaitCancelRacingDecisionUsesClientDisconnected(t *testing.T) {
	st := seed(false)
	ap := &racingApprovals{fakeApprovals: &fakeApprovals{}}
	ap.waitEntered.Add(1)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
	}}
	r := newRunner(st, &hostKeySSH{}, nil, ap, client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ap.waitEntered.Wait()
		cancel()
	}()
	if err := r.Run(ctx, runInput(), func(Event) error { return nil }); err == nil {
		t.Fatal("expected run error after cancel")
	}
	if left := awaitingToolCalls(st); len(left) != 0 {
		t.Fatalf("tool calls still awaiting_approval: %+v", left)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tc := st.toolCalls[0]
	if tc.Status != store.ToolExpired || tc.ErrorCode != codeClientDisconnected {
		t.Fatalf("tool call = %+v, want expired/CLIENT_DISCONNECTED", tc)
	}
}

// TestRunApprovalExpiredKeepsTimeoutCode 回归护栏：正常审批过期（TTL 到）
// 仍走 APPROVAL_TIMEOUT，不被新收敛逻辑改写。
func TestRunApprovalExpiredKeepsTimeoutCode(t *testing.T) {
	st := seed(false)
	ap := &fakeApprovals{waitStatus: store.ApprovalExpired}
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"s1","command":"systemctl restart nginx"}`)},
		{events: finalEvents("approval expired", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})

	if _, err := runCollect(r, runInput()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tc := st.toolCalls[0]
	if tc.Status != store.ToolExpired || tc.ErrorCode != codeApprovalTimeout {
		t.Fatalf("tool call = %+v, want expired/APPROVAL_TIMEOUT", tc)
	}
}
