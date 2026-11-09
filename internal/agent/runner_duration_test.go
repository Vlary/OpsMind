package agent

import (
	"context"
	"testing"
	"time"

	"aiops/internal/llm"
	"aiops/internal/store"
)

// 六期打磨 sprint 2（契约 8.6 第 13 条）：reasoning 首/尾 delta 的服务端
// 时间差随 assistant 消息落库，供历史消息回看显示「深度思考 · 耗时 Ns」。
func TestRunPersistsReasoningDuration(t *testing.T) {
	st := seed(false)
	client := &fakeLLM{steps: []scriptStep{
		{events: []llm.StreamEvent{
			{Type: llm.EventReasoning, Delta: "first thought"},
			{Type: llm.EventReasoning, Delta: "second thought"},
			{Type: llm.EventToken, Delta: "answer"},
			{Type: llm.EventDone, FinishReason: "stop", Usage: &llm.Usage{}},
		}},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})

	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	calls := 0
	r.clock = func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * 100 * time.Millisecond)
	}

	if err := r.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	var assistant *store.Message
	for i := range st.messages {
		if st.messages[i].Role == store.RoleAssistant {
			assistant = &st.messages[i]
		}
	}
	if assistant == nil {
		t.Fatal("assistant message not persisted")
	}
	if assistant.Reasoning != "first thoughtsecond thought" {
		t.Fatalf("reasoning = %q", assistant.Reasoning)
	}
	if assistant.ReasoningDurationMS == nil {
		t.Fatal("reasoning_duration_ms is nil, want measured duration")
	}
	if got := *assistant.ReasoningDurationMS; got != 100 {
		t.Fatalf("reasoning_duration_ms = %d, want 100", got)
	}

	// 无 reasoning 的轮次不写计时（NULL 与「<1ms」的 0 区分）。
	st2 := seed(false)
	client2 := &fakeLLM{steps: []scriptStep{{events: finalEvents("plain answer", llm.Usage{})}}}
	r2, _ := newRunnerForTest(t, st2, client2, nil, RunnerConfig{})
	if err := r2.Run(context.Background(), runInput(), func(Event) error { return nil }); err != nil {
		t.Fatalf("Run without reasoning: %v", err)
	}
	st2.mu.Lock()
	defer st2.mu.Unlock()
	for _, m := range st2.messages {
		if m.Role == store.RoleAssistant && m.ReasoningDurationMS != nil {
			t.Fatalf("reasoning_duration_ms = %d, want nil", *m.ReasoningDurationMS)
		}
	}
}

func TestRunStateReasoningDuration(t *testing.T) {
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		state *runState
		want  *int64
	}{
		{name: "nil state", state: nil},
		{name: "no reasoning", state: &runState{}},
		{
			name:  "only first delta",
			state: &runState{reasoningStartedAt: start},
		},
		{
			name:  "clock skew clamps to zero",
			state: &runState{reasoningStartedAt: start, reasoningEndedAt: start.Add(-time.Second)},
			want:  int64Ptr(0),
		},
		{
			name:  "seconds",
			state: &runState{reasoningStartedAt: start, reasoningEndedAt: start.Add(2500 * time.Millisecond)},
			want:  int64Ptr(2500),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.state.reasoningDurationMS()
			if tc.want == nil {
				if got != nil {
					t.Fatalf("reasoningDurationMS() = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("reasoningDurationMS() = %v, want %d", got, *tc.want)
			}
		})
	}
}

func int64Ptr(v int64) *int64 { return &v }
