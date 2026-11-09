// runner_win_test.go 覆盖 W1 的 exec 路由（契约 2.7 双轨）：WinRM 认证走
// winrmx、Windows SSH 通道包装 PowerShell 外壳、风险分册按 server.os、
// Windows server_facts 模板。
package agent

import (
	"context"
	"strings"
	"testing"

	"aiops/internal/llm"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// fakeWinRMExec 记录 Execute 收到的命令。
type fakeWinRMExec struct {
	winrmx.Executor
	targets  []winrmx.Target
	commands []string
}

func (f *fakeWinRMExec) Execute(_ context.Context, target winrmx.Target, req winrmx.RunRequest) (winrmx.RunResult, error) {
	f.targets = append(f.targets, target)
	f.commands = append(f.commands, req.Command)
	return winrmx.RunResult{ExitCode: 0, Combined: "winrm-ok", DurationMS: 2}, nil
}
func (f *fakeWinRMExec) Test(context.Context, winrmx.Target) (winrmx.TestResult, error) {
	return winrmx.TestResult{LatencyMS: 5, ServerVersion: "5.1.19041.1"}, nil
}
func (f *fakeWinRMExec) Invalidate(string) {}
func (f *fakeWinRMExec) Close() error      { return nil }

func seedWindowsServer(t *testing.T, auth store.AuthMethod) *testStore {
	t.Helper()
	st := seed(false)
	st.servers["w1"] = store.Server{
		ID: "w1", Name: "win-1", OS: store.ServerOSWindows, Host: "10.0.0.9",
		Port: 22, WinRMPort: 5986, Username: "Administrator", AuthMethod: auth,
		AutoExecEnabled: true, Tags: []string{},
	}
	return st
}

// TestRunSSHExecWindowsViaWinRM：WinRM 认证的 Windows 服务器走 winrmx，
// 命令原文传递（winrmx 自己包 PowerShell 端点），SSH 执行器不被触碰。
func TestRunSSHExecWindowsViaWinRM(t *testing.T) {
	st := seedWindowsServer(t, store.AuthMethodPasswordNTLM)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"w1","command":"Get-Service Spooler"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	winrmExec := &fakeWinRMExec{}
	r.SetWinRMExecutor(winrmExec)

	events, err := collectRun(t, r)
	_ = events
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("winrm-auth server must not hit ssh executor, got %+v", ssh.calls)
	}
	if len(winrmExec.commands) != 1 || winrmExec.commands[0] != "Get-Service Spooler" {
		t.Fatalf("winrm commands = %v", winrmExec.commands)
	}
	if len(winrmExec.targets) != 1 || winrmExec.targets[0].Port != 5986 ||
		winrmExec.targets[0].AuthType != store.AuthMethodPasswordNTLM {
		t.Fatalf("winrm targets = %+v", winrmExec.targets)
	}
	// Windows 分册 safe + autoExecEnabled=true → 直接执行，无审批。
	if tc := st.toolCalls[len(st.toolCalls)-1]; tc.Status != store.ToolSucceeded {
		t.Fatalf("tool call = %+v, want succeeded", tc)
	}
	if !strings.Contains(string(st.toolCalls[len(st.toolCalls)-1].Result), "winrm-ok") {
		t.Fatalf("result missing winrm output: %s", st.toolCalls[len(st.toolCalls)-1].Result)
	}
}

// TestRunSSHExecWindowsSSHPowerShellWrap：Windows + password（SSH 通道）时
// 命令包装 PowerShell 外壳后交给 sshx；风险分类仍基于原始命令。
func TestRunSSHExecWindowsSSHPowerShellWrap(t *testing.T) {
	st := seedWindowsServer(t, store.AuthMethodPassword)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"w1","command":"Get-Process"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	r.SetWinRMExecutor(&fakeWinRMExec{})

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 1 {
		t.Fatalf("ssh calls = %d", len(ssh.calls))
	}
	cmd := ssh.calls[0].Command
	if !strings.HasPrefix(cmd, "powershell -NoProfile -NonInteractive -Command Get-Process") {
		t.Fatalf("command = %q, want powershell wrapper", cmd)
	}
}

// TestRunSSHExecWindowsNeedsApproval：Windows 分册 needs_approval 命令走审批
// （autoExecEnabled=true 也不免审）。
func TestRunSSHExecWindowsNeedsApproval(t *testing.T) {
	st := seedWindowsServer(t, store.AuthMethodPasswordNTLM)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"w1","command":"Stop-Service Spooler"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	ap := &fakeApprovals{waitStatus: store.ApprovalDenied}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})
	winrmExec := &fakeWinRMExec{}
	r.SetWinRMExecutor(winrmExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(winrmExec.commands) != 0 {
		t.Fatalf("denied command must not execute: %v", winrmExec.commands)
	}
	if len(ap.created) != 1 || ap.created[0].RiskLevel != string(storeRiskNeedsApproval()) {
		t.Fatalf("approvals = %+v", ap.created)
	}
}

// TestRunSSHExecWindowsForbidden：Windows 分册 forbidden 命令直接拒绝。
func TestRunSSHExecWindowsForbidden(t *testing.T) {
	st := seedWindowsServer(t, store.AuthMethodPasswordNTLM)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolSSHExec, `{"server_id":"w1","command":"powershell -enc AAAA"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	winrmExec := &fakeWinRMExec{}
	r.SetWinRMExecutor(winrmExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(winrmExec.commands) != 0 {
		t.Fatalf("forbidden command must not execute: %v", winrmExec.commands)
	}
	if tc := st.toolCalls[len(st.toolCalls)-1]; tc.Status != store.ToolDenied ||
		tc.RiskLevel != "forbidden" {
		t.Fatalf("tool call = %+v", tc)
	}
}

// TestRunServerFactsWindows：Windows 的 server_facts 走 WinRM 通道且发送
// 固定模板内部脚本（winrmx 自行包 PowerShell 端点）。
func TestRunServerFactsWindows(t *testing.T) {
	st := seedWindowsServer(t, store.AuthMethodPasswordNTLM)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"w1"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	winrmExec := &fakeWinRMExec{}
	r.SetWinRMExecutor(winrmExec)

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("winrm-auth facts must not hit ssh executor")
	}
	if len(winrmExec.commands) != 1 {
		t.Fatalf("winrm commands = %d", len(winrmExec.commands))
	}
	if !strings.Contains(winrmExec.commands[0], "ConvertTo-Json -Compress -Depth 3") {
		t.Fatalf("facts template missing: %q", winrmExec.commands[0])
	}
	if strings.HasPrefix(winrmExec.commands[0], "powershell ") {
		t.Fatalf("winrm channel must send the inner script, not a wrapped one: %q", winrmExec.commands[0])
	}
}

// TestRunServerFactsWindowsSSHChannel：Windows + password 走 SSH 通道时模板
// 由 agent 包装 PowerShell 外壳。
func TestRunServerFactsWindowsSSHChannel(t *testing.T) {
	st := seedWindowsServer(t, store.AuthMethodPassword)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolServerFacts, `{"server_id":"w1"}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	r.SetWinRMExecutor(&fakeWinRMExec{})

	if _, err := collectRun(t, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ssh.calls) != 1 {
		t.Fatalf("ssh calls = %d", len(ssh.calls))
	}
	if !strings.HasPrefix(ssh.calls[0].Command, "powershell -NoProfile -NonInteractive -Command ") {
		t.Fatalf("facts via ssh must be wrapped: %q", ssh.calls[0].Command)
	}
	if !strings.Contains(ssh.calls[0].Command, "ConvertTo-Json") {
		t.Fatalf("facts template missing: %q", ssh.calls[0].Command)
	}
}

// TestParseWinServerFacts 覆盖 Windows facts JSON 解析（含 BOM 剥离与
// load_* null 容错）。
func TestParseWinServerFacts(t *testing.T) {
	out := "\uFEFF{\"hostname\":\"WIN-1\",\"os\":\"Windows Server 2022\",\"kernel\":\"\",\"arch\":\"AMD64\"," +
		"\"uptime_seconds\":1000,\"cpu_count\":8,\"load_1\":null,\"load_5\":null,\"load_15\":null," +
		"\"memory_total_bytes\":1000,\"memory_available_bytes\":500," +
		"\"disks\":[{\"mount\":\"C:\",\"filesystem\":\"OS\",\"size_bytes\":10,\"used_bytes\":4,\"available_bytes\":6}]," +
		"\"ip_addresses\":[\"10.0.0.9\"]}"
	facts := parseWinServerFacts(out)
	if facts.Hostname != "WIN-1" || facts.OS != "Windows Server 2022" || facts.Arch != "AMD64" {
		t.Fatalf("facts = %+v", facts)
	}
	if facts.Kernel != "" || facts.Load1 != 0 {
		t.Fatalf("kernel/load null handling: %+v", facts)
	}
	if len(facts.Disks) != 1 || facts.Disks[0].Mount != "C:" || facts.Disks[0].AvailableBytes != 6 {
		t.Fatalf("disks = %+v", facts.Disks)
	}
	if len(facts.IPAddresses) != 1 || facts.IPAddresses[0] != "10.0.0.9" {
		t.Fatalf("ips = %v", facts.IPAddresses)
	}

	// 破损输出：零值 + 非nil 切片。
	empty := parseWinServerFacts("not json")
	if empty.Disks == nil || empty.IPAddresses == nil {
		t.Fatalf("empty facts slices must be non-nil: %+v", empty)
	}
}

// --- 小工具 -----------------------------------------------------------------

func storeRiskNeedsApproval() string { return "needs_approval" }

func collectRun(t *testing.T, r *Runner) ([]Event, error) {
	t.Helper()
	var events []Event
	err := r.Run(context.Background(), runInput(), func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}
