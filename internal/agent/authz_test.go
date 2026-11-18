package agent

import (
	"context"
	"testing"

	"aiops/internal/llm"
	"aiops/internal/store"
)

// TestToolDefinitionsFilteredByRole 断言发给模型的工具列表按权限裁剪。
func TestToolDefinitionsFilteredByRole(t *testing.T) {
	admin := ToolDefinitionsForRole(store.RoleAdmin, nil)
	if len(admin) != len(ToolDefinitions()) {
		t.Fatalf("admin tools = %d, want all %d", len(admin), len(ToolDefinitions()))
	}

	auditor := FilterToolNames(store.RoleAuditor, nil)
	if len(auditor) != 1 || auditor[0] != ToolAuditSearch {
		t.Fatalf("auditor tools = %v, want [audit_search]", auditor)
	}

	// 审批者只有 assets:read：可见两个列目录工具，不能执行任何东西。
	approverTools := FilterToolNames(store.RoleApprover, nil)
	if len(approverTools) != 2 {
		t.Fatalf("approver tools = %v, want 2 discovery tools", approverTools)
	}
	for _, name := range approverTools {
		if name != ToolListServers && name != ToolListDatabases {
			t.Fatalf("approver unexpectedly sees %q", name)
		}
	}

	// 未知角色 fail-closed。
	if got := ToolDefinitionsForRole(store.Role("root"), nil); len(got) != 0 {
		t.Fatalf("unknown role tools = %d, want 0", len(got))
	}
}

// TestToolPermissionMapComplete 断言 18 个工具全部有权限映射（12.4 第 3 条）。
func TestToolPermissionMapComplete(t *testing.T) {
	for _, name := range ToolNames() {
		if _, ok := ToolPermission(name); !ok {
			t.Errorf("tool %q has no permission mapping", name)
		}
	}
	if _, ok := ToolPermission("drop_database"); ok {
		t.Fatal("unknown tool must not have a mapping")
	}
}

// TestRunDeniesToolWithoutPermission 断言执行层二次校验：即使模型硬调，
// 无权工具也被拒绝，并按要求落 tool_calls + tool.error。
func TestRunDeniesToolWithoutPermission(t *testing.T) {
	st := seed(true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"srv-1","command":"echo hi"}`)},
		{events: finalEvents("denied", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})

	in := runInput()
	in.Role = store.RoleApprover // 无 execute:write
	in.Permissions = nil

	var events []Event
	if err := r.Run(context.Background(), in, func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var result *ToolResultPayload
	for i := range events {
		if events[i].Type == EventToolResult {
			result = events[i].ToolResult
		}
	}
	if result == nil || result.OK || result.Error == nil || result.Error.Code != "TOOL_FORBIDDEN" {
		t.Fatalf("tool result = %+v, want TOOL_FORBIDDEN", result)
	}
	if len(st.toolCalls) != 1 || st.toolCalls[0].ErrorCode != "TOOL_FORBIDDEN" {
		t.Fatalf("tool calls = %+v, want TOOL_FORBIDDEN row", st.toolCalls)
	}
}

type fakeUserState struct {
	role    store.Role
	enabled bool
	err     error
}

func (f fakeUserState) UserAuthState(context.Context, string) (store.Role, bool, error) {
	return f.role, f.enabled, f.err
}

// TestUserStateRecheckDeniesAfterDemotion 断言每个 tool call 前复读用户：
// 运行中被降权/停用后不再执行（契约 12.4 第 3 条）。
func TestUserStateRecheckDeniesAfterDemotion(t *testing.T) {
	for _, tc := range []struct {
		name string
		look fakeUserState
	}{
		{"demoted", fakeUserState{role: store.RoleOperator, enabled: true}}, // operator 无 audit
		{"disabled", fakeUserState{role: store.RoleAdmin, enabled: false}},
		{"lookup error", fakeUserState{err: context.DeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := seed(true)
			client := &fakeLLM{steps: []scriptStep{
				{events: toolCallEvents(ToolAuditSearch, `{}`)},
				{events: finalEvents("denied", llm.Usage{})},
			}}
			r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
			r.SetUserAuthStateLookup(tc.look)

			var events []Event
			if err := r.Run(context.Background(), runInput(), func(ev Event) error {
				events = append(events, ev)
				return nil
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			var result *ToolResultPayload
			for i := range events {
				if events[i].Type == EventToolResult {
					result = events[i].ToolResult
				}
			}
			if result == nil || result.OK || result.Error == nil || result.Error.Code != "TOOL_FORBIDDEN" {
				t.Fatalf("tool result = %+v, want TOOL_FORBIDDEN", result)
			}
		})
	}
}
