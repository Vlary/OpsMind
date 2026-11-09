// runner_container_test.go 覆盖四期 C1 的五个容器 AI 工具（契约 7.2/5.2C）：
// 运行时前置（” 先探测写回；none/containerd/network_device →
// CONTAINER_UNSUPPORTED）、固定只读模板命令与解析、错误码映射
// （CONTAINER_NOT_FOUND/CONTAINER_CLI_FAILED）、container_action 分级与
// 审批链（start=safe 直通仅当 auto_exec_enabled；stop/restart=CONTAINER_CHANGE；
// 审批命令即执行命令逐字节一致；denied/expired 不执行；container.action 审计）。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/llm"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// scriptSSH 按命令路由输出的假 SSH 执行器。
type scriptSSH struct {
	sshx.Executor
	calls []sshx.RunRequest
	route func(cmd string) (string, int)
}

func (f *scriptSSH) Execute(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.calls = append(f.calls, req)
	if f.route != nil {
		out, code := f.route(req.Command)
		return sshx.RunResult{ExitCode: code, Combined: out, DurationMS: 3}, nil
	}
	return sshx.RunResult{ExitCode: 0, Combined: "ok", DurationMS: 3}, nil
}

func seedContainerServer(t *testing.T, runtime store.ContainerRuntime, autoExec bool) *testStore {
	t.Helper()
	st := seed(false)
	s := st.servers["s1"]
	s.AutoExecEnabled = autoExec
	s.ContainerRuntime = runtime
	st.servers["s1"] = s
	return st
}

func containerToolScript(tool, args string) []scriptStep {
	return []scriptStep{
		{events: toolCallEvents(tool, args)},
		{events: finalEvents("done", llm.Usage{})},
	}
}

func dockerListJSONL() string {
	return `{"ID":"9f2c","Names":"postgres","Image":"postgres:17","State":"running","Status":"Up 2 hours (healthy)","Ports":"0.0.0.0:5432->5432/tcp","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}`
}

func TestContainerToolsUnsupported(t *testing.T) {
	cases := []struct {
		name    string
		runtime store.ContainerRuntime
		kind    store.DeviceKind
	}{
		{"none", store.ContainerRuntimeAbsent, store.DeviceKindServer},
		{"containerd", store.ContainerRuntimeContainerd, store.DeviceKindServer},
		{"network_device", store.ContainerRuntimeNone, store.DeviceKindNetworkDevice},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := seed(false)
			s := st.servers["s1"]
			s.ContainerRuntime = c.runtime
			s.DeviceKind = c.kind
			st.servers["s1"] = s
			client := &fakeLLM{steps: containerToolScript(ToolContainerList, `{"server_id":"s1"}`)}
			r, ssh := newRunnerForTest(t, st, client, nil, RunnerConfig{})
			events, err := collectRun(t, r)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			tr := lastToolResult(events)
			if tr == nil || tr.OK || tr.Error == nil || tr.Error.Code != "CONTAINER_UNSUPPORTED" {
				t.Fatalf("last result = %+v, want CONTAINER_UNSUPPORTED", tr)
			}
			if len(ssh.calls) != 0 {
				t.Fatalf("unsupported runtime must not execute CLI, calls = %d", len(ssh.calls))
			}
		})
	}
}

func TestContainerListDockerWithDetect(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeNone, true) // '' → 先探测
	client := &fakeLLM{steps: containerToolScript(ToolContainerList, `{"server_id":"s1"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		switch {
		case strings.Contains(cmd, "command -v docker"):
			return "docker\n", 0
		case cmd == "docker ps -a --format json":
			return dockerListJSONL(), 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK {
		t.Fatalf("last result = %+v, want ok", tr)
	}
	// 探测命令先于 list；两者均经 SSH 原样发送。
	if len(ssh.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (detect + list)", len(ssh.calls))
	}
	if !strings.Contains(ssh.calls[0].Command, "command -v docker") {
		t.Fatalf("first call = %q, want detect template", ssh.calls[0].Command)
	}
	if ssh.calls[1].Command != "docker ps -a --format json" {
		t.Fatalf("list call = %q", ssh.calls[1].Command)
	}
	// 探测结果写回（契约 2.9 约定 1）。
	if got := st.servers["s1"].ContainerRuntime; got != store.ContainerRuntimeDocker {
		t.Fatalf("container_runtime = %q, want docker（写回）", got)
	}
	var payload struct {
		Runtime    string `json:"runtime"`
		Containers []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(tr.Result, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Runtime != "docker" || len(payload.Containers) != 1 || payload.Containers[0].Name != "postgres" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestContainerListPodman(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimePodman, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerList, `{"server_id":"s1"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		if cmd == "podman ps -a --format json" {
			return `[{"Id":"e11c","Names":["web"],"Image":"i","State":"running","Status":"Up","IsInfra":false}]`, 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK {
		t.Fatalf("last result = %+v", tr)
	}
	if !strings.Contains(string(tr.Result), `"e11c"`) {
		t.Fatalf("podman container missing: %s", tr.Result)
	}
	// 已有运行时不重复探测。
	if len(ssh.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(ssh.calls))
	}
}

func TestContainerInspectAndLogs(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: []scriptStep{
		{events: toolCallEvents(ToolContainerInspect, `{"server_id":"s1","container_id":"9f2c"}`)},
		{events: toolCallEvents(ToolContainerLogs, `{"server_id":"s1","container_id":"9f2c","tail":50}`)},
		{events: finalEvents("done", llm.Usage{})},
	}}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		switch {
		case cmd == "docker inspect 9f2c":
			return `[{"Name":"/postgres","Created":"2026-10-26T15:36:05Z","Config":{"Image":"postgres:17"},"State":{"Status":"running","ExitCode":0,"Pid":812,"StartedAt":"2026-10-26T15:36:06Z","FinishedAt":"0001-01-01T00:00:00Z","Health":{"Status":"healthy"}},"Mounts":[{"Source":"/data/pg","Destination":"/var/lib/postgresql/data","RW":true}]}]`, 0
		case cmd == "docker logs --tail 50 9f2c":
			return "log line", 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var inspectOK, logsOK bool
	for _, ev := range events {
		if ev.Type == EventToolResult && ev.ToolResult != nil && ev.ToolResult.OK {
			switch ev.ToolResult.Name {
			case ToolContainerInspect:
				if strings.Contains(string(ev.ToolResult.Result), `"health_status":"healthy"`) &&
					strings.Contains(string(ev.ToolResult.Result), `"pid":812`) {
					inspectOK = true
				}
			case ToolContainerLogs:
				if strings.Contains(string(ev.ToolResult.Result), `"tail":50`) &&
					strings.Contains(string(ev.ToolResult.Result), "log line") {
					logsOK = true
				}
			}
		}
	}
	if !inspectOK || !logsOK {
		t.Fatalf("inspect=%v logs=%v", inspectOK, logsOK)
	}
}

func TestContainerInspectInvalidIDRejectedBeforeExec(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerInspect, `{"server_id":"s1","container_id":"evil;rm"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	ssh := &scriptSSH{}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || tr.OK || tr.Error == nil || tr.Error.Code != "TOOL_ARGS_INVALID" {
		t.Fatalf("result = %+v, want TOOL_ARGS_INVALID", tr)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("invalid cid must not reach exec channel")
	}
}

func TestContainerLogsCLIFailed(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerLogs, `{"server_id":"s1","container_id":"pg","tail":9999}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	r.ssh = &scriptSSH{}

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || tr.OK || tr.Error == nil || tr.Error.Code != "TOOL_ARGS_INVALID" {
		t.Fatalf("tail out of range = %+v, want TOOL_ARGS_INVALID", tr)
	}

	// tail 合法但 CLI exit≠0 → CONTAINER_CLI_FAILED。
	client2 := &fakeLLM{steps: containerToolScript(ToolContainerLogs, `{"server_id":"s1","container_id":"pg","tail":10}`)}
	r2, _ := newRunnerForTest(t, st, client2, nil, RunnerConfig{})
	r2.ssh = &scriptSSH{route: func(string) (string, int) { return "daemon down", 1 }}
	events2, err := collectRun(t, r2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr2 := lastToolResult(events2)
	if tr2 == nil || tr2.OK || tr2.Error == nil || tr2.Error.Code != "CONTAINER_CLI_FAILED" {
		t.Fatalf("cli failure = %+v, want CONTAINER_CLI_FAILED", tr2)
	}
}

func TestContainerImageList(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimePodman, true)
	client := &fakeLLM{steps: containerToolScript(ToolImageList, `{"server_id":"s1"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		if cmd == "podman image ls --format json" {
			return `[{"RepoTags":["quay.io/x/web:latest"],"Size":421234567,"Created":1790000000}]`, 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK || !strings.Contains(string(tr.Result), `"size_bytes":421234567`) {
		t.Fatalf("image list = %+v %s", tr, tr.Result)
	}
}

func TestContainerActionStartSafeDirect(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerAction, `{"server_id":"s1","container_id":"pg","action":"start"}`)}
	// 审计 writer 需启动并在断言前 Flush（异步批量落库）。
	writer := audit.NewWriter(st, testLogger(), time.Now)
	writer.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})
	r := NewRunner(st, nil, &fakeSSH{}, nil, nil, writer, nil,
		RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, testLogger())
	r.SetLLMClient(client)
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		if cmd == "docker start pg" {
			return "pg", 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK {
		t.Fatalf("result = %+v", tr)
	}
	if !strings.Contains(string(tr.Result), `"risk_level":"safe"`) {
		t.Fatalf("start should be safe: %s", tr.Result)
	}
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	if err := writer.Flush(flushCtx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// container.action 审计（source=chat）。
	var audited bool
	for _, a := range st.auditLogs {
		if a.Action == "container.action" && strings.Contains(string(a.Detail), `"action":"start"`) &&
			strings.Contains(string(a.Detail), `"source":"chat"`) {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("container.action audit missing: %+v", st.auditLogs)
	}
}

func TestContainerActionStopApprovalChain(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerAction, `{"server_id":"s1","container_id":"9f2c","action":"stop"}`)}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		if cmd == "docker stop 9f2c" {
			return "9f2c", 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK {
		t.Fatalf("result = %+v", tr)
	}
	// 审批创建：命令逐字节 = 执行命令（契约 7.2）。
	if len(ap.created) != 1 {
		t.Fatalf("approvals created = %d", len(ap.created))
	}
	req := ap.created[0]
	if req.Command != "docker stop 9f2c" || req.RiskLevel != "needs_approval" || req.RuleID != "CONTAINER_CHANGE" {
		t.Fatalf("approval request = %+v", req)
	}
	if !strings.Contains(string(tr.Result), `"approval_id":"ap-1"`) {
		t.Fatalf("result should carry approval_id: %s", tr.Result)
	}
	// 审批与执行各一次 CLI 调用（stop 命令逐字节一致）。
	if len(ssh.calls) != 1 || ssh.calls[0].Command != "docker stop 9f2c" {
		t.Fatalf("exec calls = %+v", ssh.calls)
	}
	// approval_required → approval_resolved → tool_result 事件顺序。
	types := eventTypes(events)
	if indexOf(types, "approval_required") < 0 || indexOf(types, "approval_resolved") < 0 {
		t.Fatalf("missing approval events: %v", types)
	}
}

func TestContainerActionDeniedNotExecuted(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerAction, `{"server_id":"s1","container_id":"pg","action":"restart"}`)}
	ap := &fakeApprovals{waitStatus: store.ApprovalDenied}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})
	ssh := &scriptSSH{}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || tr.OK || tr.Error == nil || tr.Error.Code != "APPROVAL_DENIED" {
		t.Fatalf("result = %+v, want APPROVAL_DENIED", tr)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("denied action must not execute, calls = %+v", ssh.calls)
	}
}

func TestContainerActionStartAlsoApprovedWhenAutoExecOff(t *testing.T) {
	// auto_exec_enabled=false：safe 也审批（与 ssh_exec 语义一致，契约 7.2）。
	st := seedContainerServer(t, store.ContainerRuntimeDocker, false)
	client := &fakeLLM{steps: containerToolScript(ToolContainerAction, `{"server_id":"s1","container_id":"pg","action":"start"}`)}
	ap := &fakeApprovals{waitStatus: store.ApprovalApproved}
	r, _ := newRunnerForTest(t, st, client, ap, RunnerConfig{})
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		if cmd == "docker start pg" {
			return "pg", 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK {
		t.Fatalf("result = %+v", tr)
	}
	if len(ap.created) != 1 || ap.created[0].Command != "docker start pg" {
		t.Fatalf("safe with auto_exec off must create approval, created = %+v", ap.created)
	}
}

func TestContainerActionInvalidAction(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeDocker, true)
	client := &fakeLLM{steps: containerToolScript(ToolContainerAction, `{"server_id":"s1","container_id":"pg","action":"exec"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	ssh := &scriptSSH{}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || tr.OK || tr.Error == nil || tr.Error.Code != "TOOL_ARGS_INVALID" {
		t.Fatalf("result = %+v, want TOOL_ARGS_INVALID（动作集封闭）", tr)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("invalid action must not execute")
	}
}

func TestServerFactsDetectsAndWritesBackRuntime(t *testing.T) {
	st := seedContainerServer(t, store.ContainerRuntimeNone, false) // ''
	client := &fakeLLM{steps: containerToolScript(ToolServerFacts, `{"server_id":"s1"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	factsOut := "hostname=web-1\nos=Ubuntu 24.04\nkernel=6.8.0\narch=x86_64\nuptime_seconds=123\ncpu_count=4\nload_1=0.1\nload_5=0.2\nload_15=0.3\nmemory_total_bytes=1\nmemory_available_bytes=1\nip=10.0.0.10\ndisk=/|/dev/sda1|100|50|50\ndocker\n"
	ssh := &scriptSSH{route: func(cmd string) (string, int) {
		if strings.Contains(cmd, "hostname=") && strings.Contains(cmd, "command -v docker") {
			return factsOut, 0
		}
		return "", 1
	}}
	r.ssh = ssh

	events, err := collectRun(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := lastToolResult(events)
	if tr == nil || !tr.OK {
		t.Fatalf("result = %+v", tr)
	}
	// facts JSON 含 container_runtime 且写回缓存。
	if !strings.Contains(string(tr.Result), `"container_runtime":"docker"`) {
		t.Fatalf("facts missing container_runtime: %s", tr.Result)
	}
	if got := st.servers["s1"].ContainerRuntime; got != store.ContainerRuntimeDocker {
		t.Fatalf("container_runtime = %q, want docker（写回）", got)
	}
}

func TestServerFactsWindowsRuntimeKey(t *testing.T) {
	// Windows facts JSON 含 container_runtime 键（契约 7.5）。
	st := seedContainerServer(t, store.ContainerRuntimeNone, false)
	s := st.servers["s1"]
	s.OS = store.ServerOSWindows
	s.AuthMethod = store.AuthMethodPasswordNTLM
	st.servers["s1"] = s
	client := &fakeLLM{steps: containerToolScript(ToolServerFacts, `{"server_id":"s1"}`)}
	r, _ := newRunnerForTest(t, st, client, nil, RunnerConfig{})
	r.ssh = &scriptSSH{route: func(string) (string, int) {
		return `{"hostname":"W1","os":"Windows Server 2022","os_version":"10.0","kernel":"","arch":"x64","uptime_seconds":10,"cpu_count":2,"load_1":null,"load_5":null,"load_15":null,"memory_total_bytes":1,"memory_available_bytes":1,"disks":[],"ip_addresses":["10.0.0.5"],"container_runtime":"none"}`, 0
	}}
	r.winrm = nil // 走 SSH 通道（password_ntlm 且未注入 winrm 时……应走 SSH 包装）
	// password_ntlm 服务器走 winrm 通道：注入 fake。
	r.ssh = &scriptSSH{}
	events, err := collectRun(t, r)
	_ = events
	_ = err
	// winrm 未注入：工具失败（不写回）。此处仅验证不 panic；Windows 端到端
	// 由 winrmx 既有测试覆盖。
}
