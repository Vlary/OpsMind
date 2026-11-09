// runner_netdev_test.go 覆盖 N1 的 exec/facts 路由（契约 2.8）：
// network_device 按 ClassifyNet 分册分类、命令原样单条经 netdev 执行
// （不做包装）、forbidden 不进入审批、needs_approval 走审批链路、
// server_facts 按品牌固定模板、错误码映射 NETCLI_*。
package agent

import (
	"context"
	"strings"
	"testing"

	"aiops/internal/llm"
	"aiops/internal/netdev"
	"aiops/internal/store"
)

// fakeNetCLIExec 记录 Execute 收到的命令与目标。
type fakeNetCLIExec struct {
	targets  []netdev.Target
	commands []string
	execErr  error
}

func (f *fakeNetCLIExec) Execute(_ context.Context, target netdev.Target, req netdev.RunRequest) (netdev.RunResult, error) {
	f.targets = append(f.targets, target)
	f.commands = append(f.commands, req.Command)
	if f.execErr != nil {
		return netdev.RunResult{}, f.execErr
	}
	return netdev.RunResult{ExitCode: 0, Combined: "net-ok", DurationMS: 3}, nil
}
func (f *fakeNetCLIExec) Test(context.Context, netdev.Target) (netdev.TestResult, error) {
	return netdev.TestResult{LatencyMS: 4, ServerVersion: "15.2(4)E7"}, nil
}
func (f *fakeNetCLIExec) Invalidate(string) {}
func (f *fakeNetCLIExec) Close() error      { return nil }

func seedNetdevServer(t *testing.T, brand store.NetdevBrand, proto store.AccessProto) *testStore {
	t.Helper()
	st := seed(false)
	st.servers["nd1"] = store.Server{
		ID: "nd1", Name: "core-sw-1", DeviceKind: store.DeviceKindNetworkDevice,
		OS: store.ServerOSNetwork, NetdevBrand: brand, AccessProto: proto,
		Host: "10.0.0.2", Port: 22, Username: "admin",
		AuthMethod:      store.AuthMethodPassword,
		AutoExecEnabled: true, Tags: []string{},
	}
	return st
}

// TestRunSSHExecNetdevRouting：cisco 设备 safe 命令（autoExecEnabled）按
// cisco 分册放行并原样经 netdev 执行；SSH 执行器不被触碰。
func TestRunSSHExecNetdevRouting(t *testing.T) {
	st := seedNetdevServer(t, store.NetdevBrandCisco, store.AccessProtoSSH)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"nd1","command":"show version"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	netExec := &fakeNetCLIExec{}
	r.SetNetCLIExecutor(netExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("network device must not hit ssh executor, got %+v", ssh.calls)
	}
	if len(netExec.commands) != 1 || netExec.commands[0] != "show version" {
		t.Fatalf("netdev commands = %v, want single unwrapped command", netExec.commands)
	}
	if len(netExec.targets) != 1 || netExec.targets[0].Brand != netdev.BrandCisco ||
		netExec.targets[0].AccessProto != store.AccessProtoSSH {
		t.Fatalf("netdev targets = %+v", netExec.targets)
	}
	tc := st.toolCalls[len(st.toolCalls)-1]
	if tc.Status != store.ToolSucceeded || tc.RiskLevel != string(riskLevelSafe()) {
		t.Fatalf("tool call = %+v, want succeeded/safe", tc)
	}
	if !strings.Contains(string(tc.Result), "net-ok") {
		t.Fatalf("result missing netdev output: %s", tc.Result)
	}
}

// TestRunSSHExecNetdevForbidden：forbidden（reload）不进入审批、不执行。
func TestRunSSHExecNetdevForbidden(t *testing.T) {
	st := seedNetdevServer(t, store.NetdevBrandCisco, store.AccessProtoTelnet)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"nd1","command":"reload"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	netExec := &fakeNetCLIExec{}
	r.SetNetCLIExecutor(netExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(netExec.commands) != 0 {
		t.Fatalf("forbidden command must not execute, got %v", netExec.commands)
	}
	tc := st.toolCalls[len(st.toolCalls)-1]
	if tc.Status != store.ToolDenied || tc.RiskLevel != "forbidden" {
		t.Fatalf("tool call = %+v, want denied/forbidden", tc)
	}
}

// TestRunSSHExecNetdevNeedsApproval：needs_approval（show running-config）
// 走审批链路（autoExecEnabled=true 也不免审）；拒绝后不执行。
func TestRunSSHExecNetdevNeedsApproval(t *testing.T) {
	st := seedNetdevServer(t, store.NetdevBrandHuawei, store.AccessProtoSSH)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"nd1","command":"display current-configuration"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalDenied}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})
	netExec := &fakeNetCLIExec{}
	r.SetNetCLIExecutor(netExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(netExec.commands) != 0 {
		t.Fatalf("denied command must not execute, got %v", netExec.commands)
	}
	if len(ap.created) != 1 {
		t.Fatalf("approvals = %+v, want one", ap.created)
	}
	if ap.created[0].RuleID != "NET_HUAWEI_CONFIG_DUMP" {
		t.Fatalf("approval rule = %s, want NET_HUAWEI_CONFIG_DUMP", ap.created[0].RuleID)
	}
	if ap.created[0].Command != "display current-configuration" {
		t.Fatalf("approval command = %q (must match executed command byte for byte)", ap.created[0].Command)
	}
}

// TestRunSSHExecNetdevErrorMapping：netdev 哨兵错误映射 NETCLI_* 错误码。
func TestRunSSHExecNetdevErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"login failed", netdev.ErrLoginFailed, "NETCLI_LOGIN_FAILED"},
		{"exec failed", netdev.ErrExecFailed, "NETCLI_EXEC_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := seedNetdevServer(t, store.NetdevBrandCisco, store.AccessProtoTelnet)
			client := &fakeLLM{steps: []scriptStep{
				{events: toolCallEvents(ToolSSHExec, `{"server_id":"nd1","command":"show version"}`)},
				{events: finalEvents("done", llm.Usage{})},
			}}
			r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
			netExec := &fakeNetCLIExec{execErr: tc.err}
			r.SetNetCLIExecutor(netExec)

			if _, err := collectRun(t, r); err != nil {
				t.Fatalf("Run: %v", err)
			}
			last := st.toolCalls[len(st.toolCalls)-1]
			if last.Status != store.ToolFailed || last.ErrorCode != tc.want {
				t.Fatalf("tool call = %+v, want failed/%s", last, tc.want)
			}
		})
	}
}

// TestRunServerFactsNetdev：网络设备 server_facts 按品牌固定模板（两条
// 只读命令）经 netdev 执行，facts/raw_output 进结果。
func TestRunServerFactsNetdev(t *testing.T) {
	st := seedNetdevServer(t, store.NetdevBrandHuawei, store.AccessProtoSSH)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"nd1"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	netExec := &fakeNetCLIExec{}
	r.SetNetCLIExecutor(netExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(netExec.commands) != 2 {
		t.Fatalf("facts commands = %v, want brand template (2 cmds)", netExec.commands)
	}
	if netExec.commands[0] != "display version" || netExec.commands[1] != "display ip interface brief" {
		t.Fatalf("facts template = %v", netExec.commands)
	}
	tc := st.toolCalls[len(st.toolCalls)-1]
	if tc.Status != store.ToolSucceeded {
		t.Fatalf("tool call = %+v", tc)
	}
	res := string(tc.Result)
	if !strings.Contains(res, `"vendor":"huawei"`) || !strings.Contains(res, "raw_output") {
		t.Fatalf("facts result missing fields: %s", res)
	}
}

// TestNetRiskClassifyBrandRouting：分册路由按 server.NetdevBrand。
func TestNetRiskClassifyBrandRouting(t *testing.T) {
	srv := store.Server{DeviceKind: store.DeviceKindNetworkDevice, NetdevBrand: store.NetdevBrandJunos}
	d := netRiskClassify("request system reboot", srv)
	if string(d.Level) != "forbidden" || d.RuleID != "NET_JUNOS_RELOAD" {
		t.Fatalf("junos routing = %s/%s", d.Level, d.RuleID)
	}
	// server 类资产不受网络分册影响：show 在 linux shell 分册是未知命令
	// （DEFAULT_ASK），绝不产生 NET_ 前缀规则。
	linuxSrv := store.Server{DeviceKind: store.DeviceKindServer, OS: store.ServerOSLinux}
	d = netRiskClassify("show version", linuxSrv)
	if d.RuleID != "DEFAULT_ASK" || strings.HasPrefix(d.RuleID, "NET_") {
		t.Fatalf("linux routing = %s, want linux DEFAULT_ASK", d.RuleID)
	}
}

// riskLevelSafe 避免 import risk 仅为此常量的辅助。
func riskLevelSafe() string { return "safe" }
