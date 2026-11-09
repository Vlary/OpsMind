// containers_test.go 验证四期 C1 的容器 REST 五路由与 console 审批闭环
// （契约 G29）：POST action 创建 source=console 审批（conversation/tool_call
// NULL、command 固化逐字节）且不执行；decide approve 同步逐字节执行、响应
// execution、审计 container.action；deny 不执行；过期 409；chat 审批 decide
// execution=null；unsupported runtime 400；CID 不合法 400 不进执行通道。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/sshx"
	"aiops/internal/store"

	"github.com/google/uuid"
)

func seedContainerServerForAPI(t *testing.T, env *testEnv, runtime store.ContainerRuntime, kind store.DeviceKind) string {
	t.Helper()
	id := uuid.NewString()
	env.st.mu.Lock()
	env.st.servers[id] = store.Server{
		ID: id, Name: "srv-c", DeviceKind: kind, OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root", AuthMethod: store.AuthMethodPassword,
		ContainerRuntime: runtime, Tags: []string{},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	env.st.mu.Unlock()
	return id
}

func dockerRouteExec(out string, code int) func(context.Context, sshx.Target, sshx.RunRequest) (sshx.RunResult, error) {
	return func(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
		_ = req
		return sshx.RunResult{ExitCode: code, Combined: out, DurationMS: 12}, nil
	}
}

// decodeBodyMap 解 JSON 响应为 map（action/decide 包装断言用）。
func decodeBodyMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// TestConsoleApprovalClosedLoop：POST action → console 审批（不执行）→
// decide approve → 逐字节执行 + execution + container.action 审计。
// approvalOf 解包 decide 响应的 ApprovalDecisionResult 包装（契约 8.9/5.5）。
func approvalOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	ap, _ := body["approval"].(map[string]any)
	if ap == nil {
		t.Fatalf("response missing approval wrapper: %v", body)
	}
	return ap
}

func TestConsoleApprovalClosedLoop(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = dockerRouteExec("9f2c\n", 0)

	// 1) 创建 console 审批（不直接执行）：200 + {approval} 包装（契约
	// ContainerActionResponse；曾返回 201 扁平致前端 data.approval 崩溃）。
	rec := env.do(http.MethodPost, "/api/servers/"+id+"/containers/9f2ca3c1/action", `{"action":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var actionBody struct {
		Approval store.Approval `json:"approval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &actionBody); err != nil {
		t.Fatal(err)
	}
	ap := actionBody.Approval
	if ap.Source != "console" || ap.Status != store.ApprovalPending {
		t.Fatalf("approval = %+v, want source=console pending", ap)
	}
	if ap.ConversationID != nil || ap.ToolCallID != nil {
		t.Fatalf("console approval context must be NULL: %+v", ap)
	}
	if ap.Command != "docker stop 9f2ca3c1" {
		t.Fatalf("command = %q, want fixed byte-exact command", ap.Command)
	}
	if ap.RuleID != "CONTAINER_CHANGE" || ap.RiskLevel != "needs_approval" {
		t.Fatalf("rule = %q/%q", ap.RuleID, ap.RiskLevel)
	}
	if len(env.sshExec.calls) != 0 {
		t.Fatalf("action endpoint must not execute, calls = %+v", env.sshExec.calls)
	}

	// 2) 批准：decide 同步逐字节执行。
	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 契约 8.9/5.5：decide 响应为 ApprovalDecisionResult{approval, execution}。
	var decided struct {
		Approval struct {
			Status string `json:"status"`
			Source string `json:"source"`
		} `json:"approval"`
		Execution *struct {
			ExitCode   int    `json:"exit_code"`
			Output     string `json:"output"`
			Truncated  bool   `json:"truncated"`
			DurationMS int64  `json:"duration_ms"`
			ErrorCode  string `json:"error_code"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decided); err != nil {
		t.Fatal(err)
	}
	if decided.Approval.Status != string(store.ApprovalApproved) || decided.Approval.Source != "console" {
		t.Fatalf("decided = %+v", decided)
	}
	if decided.Execution == nil || decided.Execution.ExitCode != 0 || decided.Execution.Output != "9f2c\n" {
		t.Fatalf("execution = %+v", decided.Execution)
	}
	// 逐字节执行（审批命令与执行命令一致）。
	if len(env.sshExec.calls) != 1 || env.sshExec.calls[0].Command != "docker stop 9f2ca3c1" {
		t.Fatalf("exec calls = %+v", env.sshExec.calls)
	}

	// 3) container.action 审计（source=console）。
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	if err := env.auditW.Flush(flushCtx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}
	found := false
	env.st.mu.Lock()
	for _, l := range env.st.auditLogs {
		if l.Action != "container.action" {
			continue
		}
		detail := string(l.Detail)
		if strings.Contains(detail, `"runtime":"docker"`) &&
			strings.Contains(detail, `"container_id":"9f2ca3c1"`) &&
			strings.Contains(detail, `"action":"stop"`) &&
			strings.Contains(detail, `"exit_code":0`) &&
			strings.Contains(detail, `"source":"console"`) {
			found = true
		}
	}
	env.st.mu.Unlock()
	if !found {
		t.Fatal("container.action audit missing or detail mismatch")
	}
}

// TestConsoleApprovalDeny：deny 不执行、execution=null。
func TestConsoleApprovalDeny(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = dockerRouteExec("x", 0)

	rec := env.do(http.MethodPost, "/api/servers/"+id+"/containers/pg/action", `{"action":"restart"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action = %d %s", rec.Code, rec.Body.String())
	}
	var ap store.Approval
	if raw, err := json.Marshal(approvalOf(t, decodeBodyMap(t, rec))); err == nil {
		_ = json.Unmarshal(raw, &ap)
	}

	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"deny"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if ap := approvalOf(t, body); ap["status"] != string(store.ApprovalDenied) {
		t.Fatalf("status = %v", ap["status"])
	}
	if body["execution"] != nil {
		t.Fatalf("deny must not execute, execution = %v", body["execution"])
	}
	if len(env.sshExec.calls) != 0 {
		t.Fatalf("denied action must not execute")
	}
}

// TestConsoleApprovalExpired：过期审批 decide → 409。
func TestConsoleApprovalExpired(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	rec := env.do(http.MethodPost, "/api/servers/"+id+"/containers/pg/action", `{"action":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action = %d", rec.Code)
	}
	var ap store.Approval
	if raw, err := json.Marshal(approvalOf(t, decodeBodyMap(t, rec))); err == nil {
		_ = json.Unmarshal(raw, &ap)
	}
	// 手工置为 expired（既有 sweeper 语义）。
	env.st.mu.Lock()
	e := env.st.approvals[ap.ID]
	e.Status = store.ApprovalExpired
	env.st.approvals[ap.ID] = e
	env.st.mu.Unlock()

	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired decide = %d, want 409", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "APPROVAL_NOT_PENDING" {
		t.Fatalf("code = %s", code)
	}
	if len(env.sshExec.calls) != 0 {
		t.Fatalf("expired approval must not execute")
	}
}

// TestConsoleApprovalExecFailureNotRolledBack：执行失败不回滚审批状态，
// 失败信息只进 execution 与审计 detail。
func TestConsoleApprovalExecFailureNotRolledBack(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = dockerRouteExec("cannot stop: no such container", 125)

	rec := env.do(http.MethodPost, "/api/servers/"+id+"/containers/pg/action", `{"action":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action = %d", rec.Code)
	}
	var ap store.Approval
	if raw, err := json.Marshal(approvalOf(t, decodeBodyMap(t, rec))); err == nil {
		_ = json.Unmarshal(raw, &ap)
	}

	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body.String())
	}
	var decided map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decided)
	decidedApproval := approvalOf(t, decided)
	if decidedApproval["status"] != string(store.ApprovalApproved) {
		t.Fatalf("approval status must stay approved, got %v", decidedApproval["status"])
	}
	exec, _ := decided["execution"].(map[string]any)
	if exec == nil || exec["exit_code"].(float64) != 125 || exec["error_code"] != "CONTAINER_CLI_FAILED" {
		t.Fatalf("execution = %v", decided["execution"])
	}
}

// TestChatApprovalDecideHasNoExecution：chat 审批 decide 响应 execution=null
// 且行为与既有一致。
func TestChatApprovalDecideHasNoExecution(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	convID := "11111111-1111-1111-1111-111111111111"
	toolID := "22222222-2222-2222-2222-222222222222"
	env.st.mu.Lock()
	env.st.approvals["ap-chat"] = store.Approval{
		ID: "ap-chat", ConversationID: &convID, ToolCallID: &toolID, Source: "chat",
		ServerID: &id, Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		Status: store.ApprovalPending, ExpiresAt: time.Now().Add(time.Hour),
	}
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/approvals/ap-chat/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if approvalOf(t, body)["status"] != string(store.ApprovalApproved) || body["execution"] != nil {
		t.Fatalf("chat decide = %v (execution must be null)", body)
	}
	// chat 审批不经容器执行通道。
	if len(env.sshExec.calls) != 0 {
		t.Fatalf("chat approval must not execute via container path")
	}
}

// TestApprovalListExcludesConsoleWhenFiltered：按 conversation 过滤不含
// console 行（契约 3.4）。
func TestApprovalListExcludesConsoleWhenFiltered(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	rec := env.do(http.MethodPost, "/api/servers/"+id+"/containers/pg/action", `{"action":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action = %d", rec.Code)
	}
	convID := "11111111-1111-1111-1111-111111111111"
	rec = env.do(http.MethodGet, "/api/approvals?conversation_id="+convID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "console") {
		t.Fatalf("console approval leaked into conversation filter: %s", rec.Body.String())
	}
	// 不过滤时可见。
	rec = env.do(http.MethodGet, "/api/approvals", "")
	if !strings.Contains(rec.Body.String(), `"console"`) {
		t.Fatalf("console approval missing from unfiltered list: %s", rec.Body.String())
	}
}

// TestContainerRoutesUnsupported：network_device 与 none 一律 400
// CONTAINER_UNSUPPORTED；CID 不合法 400 不进执行通道。
func TestContainerRoutesUnsupported(t *testing.T) {
	env := newTestEnv(t)
	nd := seedContainerServerForAPI(t, env, store.ContainerRuntimeNone, store.DeviceKindNetworkDevice)
	for _, path := range []string{
		"/api/servers/" + nd + "/containers",
		"/api/servers/" + nd + "/containers/pg",
		"/api/servers/" + nd + "/containers/pg/logs",
		"/api/servers/" + nd + "/images",
	} {
		rec := env.do(http.MethodGet, path, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", path, rec.Code)
		}
		if code, _ := errorBody(t, rec); code != "CONTAINER_UNSUPPORTED" {
			t.Fatalf("%s code = %s", path, code)
		}
	}
	rec := env.do(http.MethodPost, "/api/servers/"+nd+"/containers/pg/action", `{"action":"stop"}`)
	if code, _ := errorBody(t, rec); code != "CONTAINER_UNSUPPORTED" {
		t.Fatalf("action on network device = %s", code)
	}

	none := seedContainerServerForAPI(t, env, store.ContainerRuntimeAbsent, store.DeviceKindServer)
	rec = env.do(http.MethodGet, "/api/servers/"+none+"/containers", "")
	if code, _ := errorBody(t, rec); code != "CONTAINER_UNSUPPORTED" {
		t.Fatalf("none runtime = %s", code)
	}

	// 不合法 CID：不进入执行通道。
	docker := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = dockerRouteExec("{}", 0)
	rec = env.do(http.MethodGet, "/api/servers/"+docker+"/containers/evil;rm", "")
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("invalid cid = %s", code)
	}
	if len(env.sshExec.calls) != 0 {
		t.Fatalf("invalid cid must not reach exec channel")
	}
}

// TestContainerRoutesReadOnly：列表/详情/日志/镜像经 CLI 查询返回。
func TestContainerRoutesReadOnly(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
		switch {
		case req.Command == "docker ps -a --format json":
			return sshx.RunResult{ExitCode: 0, Combined: `{"ID":"9f2c","Names":"postgres","Image":"postgres:17","State":"running","Status":"Up","Ports":"","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}`}, nil
		case req.Command == "docker inspect 9f2c":
			return sshx.RunResult{ExitCode: 0, Combined: `[{"Name":"/pg","Created":"2026-10-26T15:36:05Z","Config":{"Image":"pg"},"State":{"Status":"running","ExitCode":0,"Pid":1},"Mounts":[]}]`}, nil
		case strings.Contains(req.Command, "docker logs --tail") && strings.HasSuffix(req.Command, "9f2c"):
			return sshx.RunResult{ExitCode: 0, Combined: "line"}, nil
		case req.Command == "docker image ls --format json":
			return sshx.RunResult{ExitCode: 0, Combined: `{"Repository":"nginx","Tag":"1.27","Size":"10.5MB","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}`}, nil
		}
		return sshx.RunResult{ExitCode: 1, Combined: "unexpected " + req.Command}, nil
	}

	rec := env.do(http.MethodGet, "/api/servers/"+id+"/containers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runtime":"docker"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	// 契约 ContainerListResponse {runtime, items}：键名必须是 items
	//（曾用 containers 键致前端 data.items 恒 undefined 表格恒空）。
	var listBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if items, ok := listBody["items"].([]any); !ok || len(items) == 0 {
		t.Fatalf("list items = %v, want non-empty array", listBody["items"])
	}
	if _, exists := listBody["containers"]; exists {
		t.Fatal("list response must not use legacy containers key")
	}
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers/9f2c", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pid":1`) {
		t.Fatalf("inspect = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers/9f2c/logs?tail=50", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tail":50`) {
		t.Fatalf("logs = %d %s", rec.Code, rec.Body.String())
	}
	// tail 越界。
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers/9f2c/logs?tail=1001", "")
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("tail out of range = %s", code)
	}
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/images", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"size_bytes":10500000`) {
		t.Fatalf("images = %d %s", rec.Code, rec.Body.String())
	}
}

// TestContainerNotFoundMaps404：inspect/logs 指向不存在容器 → 404
// CONTAINER_NOT_FOUND。
func TestContainerNotFoundMaps404(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, _ sshx.RunRequest) (sshx.RunResult, error) {
		return sshx.RunResult{ExitCode: 1, Combined: "Error: No such object: missing"}, nil
	}
	rec := env.do(http.MethodGet, "/api/servers/"+id+"/containers/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inspect missing = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "CONTAINER_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers/missing/logs", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "CONTAINER_NOT_FOUND") {
		t.Fatalf("logs missing = %d %s", rec.Code, rec.Body.String())
	}
}

// TestContainerRouteChannelErrorMapsSSHExecFailed（p4 sprint 6，走查 D）：
// 库值 runtime=docker 且 SSH 不可达（拨号立拒）时，列表不得把通道层根因
// 报成 CONTAINER_CLI_FAILED（前端文案「daemon 未运行」误导排障）——通道
// 错误映射 502 SSH_EXEC_FAILED（与 agent 容器工具路径 classifySSHError
// 同口径）；CLI exit≠0 仍 CONTAINER_CLI_FAILED；超时仍 504 SSH_TIMEOUT
// （DeadlineExceeded 分支先于通道前缀判定）；decide 批准后的通道失败
// execution.error_code 同口径。
func TestContainerRouteChannelErrorMapsSSHExecFailed(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)

	// 1) 通道层拨号失败（sshx 前缀 + net 级原因）→ 502 SSH_EXEC_FAILED。
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, _ sshx.RunRequest) (sshx.RunResult, error) {
		return sshx.RunResult{}, fmt.Errorf("sshx: dial 127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused")
	}
	rec := env.do(http.MethodGet, "/api/servers/"+id+"/containers", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("channel error status = %d, want 502", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "SSH_EXEC_FAILED" {
		t.Fatalf("channel error code = %s, want SSH_EXEC_FAILED", code)
	}
	// 镜像列表同口径（同一 writeContainerError）。
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/images", "")
	if code, _ := errorBody(t, rec); code != "SSH_EXEC_FAILED" {
		t.Fatalf("images channel error code = %s, want SSH_EXEC_FAILED", code)
	}

	// 2) WinRM 通道错误（winrmx 前缀）同口径。
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, _ sshx.RunRequest) (sshx.RunResult, error) {
		return sshx.RunResult{}, fmt.Errorf("winrmx: run command: unknown error 401")
	}
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers", "")
	if code, _ := errorBody(t, rec); code != "SSH_EXEC_FAILED" {
		t.Fatalf("winrm channel error code = %s, want SSH_EXEC_FAILED", code)
	}

	// 3) 超时（sshx 前缀 + DeadlineExceeded）优先归 504 SSH_TIMEOUT。
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, _ sshx.RunRequest) (sshx.RunResult, error) {
		return sshx.RunResult{}, fmt.Errorf("sshx: command timed out: %w", context.DeadlineExceeded)
	}
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers", "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want 504", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "SSH_TIMEOUT" {
		t.Fatalf("timeout code = %s, want SSH_TIMEOUT", code)
	}

	// 4) 回归：CLI exit≠0（命令已执行）仍 CONTAINER_CLI_FAILED。
	env.sshExec.execFn = dockerRouteExec("Cannot connect to the Docker daemon", 1)
	rec = env.do(http.MethodGet, "/api/servers/"+id+"/containers", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("cli failure status = %d, want 502", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "CONTAINER_CLI_FAILED" {
		t.Fatalf("cli failure code = %s, want CONTAINER_CLI_FAILED", code)
	}

	// 5) console 审批 decide：批准后通道失败 → execution.error_code 同口径。
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, _ sshx.RunRequest) (sshx.RunResult, error) {
		return sshx.RunResult{}, fmt.Errorf("sshx: handshake: ssh: unable to authenticate")
	}
	rec = env.do(http.MethodPost, "/api/servers/"+id+"/containers/pg/action", `{"action":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action = %d %s", rec.Code, rec.Body.String())
	}
	var ap store.Approval
	if raw, err := json.Marshal(approvalOf(t, decodeBodyMap(t, rec))); err == nil {
		_ = json.Unmarshal(raw, &ap)
	}
	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body.String())
	}
	decided := decodeBodyMap(t, rec)
	execRes, _ := decided["execution"].(map[string]any)
	if execRes == nil || execRes["error_code"] != "SSH_EXEC_FAILED" {
		t.Fatalf("execution = %v, want error_code SSH_EXEC_FAILED", decided["execution"])
	}
}

// TestContainerDetectOnEmptyRuntime：” 时先探测写回；探测失败按 none →
// 400（detail 携带现值）。
func TestContainerDetectOnEmptyRuntime(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeNone, store.DeviceKindServer)
	env.sshExec.execFn = func(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
		if strings.Contains(req.Command, "command -v docker") {
			return sshx.RunResult{ExitCode: 0, Combined: "podman\n"}, nil
		}
		return sshx.RunResult{ExitCode: 0, Combined: `[{"Id":"a","Names":["x"],"Image":"i","State":"running","Status":"Up","IsInfra":false}]`}, nil
	}
	rec := env.do(http.MethodGet, "/api/servers/"+id+"/containers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runtime":"podman"`) {
		t.Fatalf("list after detect = %d %s", rec.Code, rec.Body.String())
	}
	env.st.mu.Lock()
	got := env.st.servers[id].ContainerRuntime
	env.st.mu.Unlock()
	if got != store.ContainerRuntimePodman {
		t.Fatalf("container_runtime = %q, want podman（写回）", got)
	}
}

// TestContainerUnsupportedDetailRuntime 契约 2.9 约定 1/S21①：
// CONTAINER_UNSUPPORTED 错误体的 details 携带现值 runtime（前端
// containerFailures.ts 依赖该值增强文案，曾为死代码——评审发现）。
func TestContainerUnsupportedDetailRuntime(t *testing.T) {
	env := newTestEnv(t)
	// network_device：detail.runtime 恒空串。
	netdev := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindNetworkDevice)
	rec := env.do(http.MethodGet, "/api/servers/"+netdev+"/containers", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("netdev containers = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "CONTAINER_UNSUPPORTED" {
		t.Fatalf("code = %s", body.Error.Code)
	}
	if v, ok := body.Error.Details["runtime"]; !ok || v != "" {
		t.Fatalf("netdev details.runtime = %v(ok=%v), want empty string", v, ok)
	}

	// server + runtime=none：details.runtime 为现值。
	srv := seedContainerServerForAPI(t, env, store.ContainerRuntimeNone, store.DeviceKindServer)
	rec = env.do(http.MethodGet, "/api/servers/"+srv+"/containers", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("none containers = %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if v, ok := body.Error.Details["runtime"]; !ok || v != "none" {
		t.Fatalf("none details.runtime = %v(ok=%v), want none", v, ok)
	}
}

// TestNonContainerConsoleApprovalAuditsCommandExecuted 六期评审：诊断 fix
// 等非容器命令经 console 审批执行后不得落 runtime/container_id/action 全
// 空的 container.action——应落 command.executed（带完整命令与门限快照）。
func TestNonContainerConsoleApprovalAuditsCommandExecuted(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	env.sshExec.execFn = func(context.Context, sshx.Target, sshx.RunRequest) (sshx.RunResult, error) {
		return sshx.RunResult{ExitCode: 0, Combined: "ok"}, nil
	}

	// 手工插入 source=console、command 为非容器形态的 pending 审批。
	apID := "ap-noncontainer"
	env.st.mu.Lock()
	srvID := id
	env.st.approvals[apID] = store.Approval{
		ID: apID, Source: "console", ServerID: &srvID,
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "SERVICE_CHANGE", Status: store.ApprovalPending,
		ExpiresAt: time.Now().Add(time.Hour), ApprovalMode: store.ApprovalModeNone,
	}
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/approvals/"+apID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body.String())
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	if err := env.auditW.Flush(flushCtx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var gotCommandExecuted, gotContainerActionWithEmptyRuntime bool
	var detailText string
	for _, l := range env.st.auditLogs {
		detail := string(l.Detail)
		if l.Action == "command.executed" && strings.Contains(detail, "systemctl restart nginx") {
			gotCommandExecuted = true
			detailText = detail
		}
		if l.Action == "container.action" && strings.Contains(detail, `"runtime":""`) {
			gotContainerActionWithEmptyRuntime = true
		}
	}
	if !gotCommandExecuted {
		t.Fatal("non-container console approval must audit command.executed with full command")
	}
	if !strings.Contains(detailText, "approval_mode") {
		t.Fatalf("command.executed detail missing approval_mode: %s", detailText)
	}
	if gotContainerActionWithEmptyRuntime {
		t.Fatal("non-container command must not audit container.action with empty runtime")
	}
}

// TestContainerDirectExecutionAuditsApprovalMode 六期打磨 sprint 6（走查 low）：
// 门限判定直执行分支的 container.action 审计必须携带 approval_mode 门限
// 快照（契约 9.7 / 5261 行「直执行同样携带门限快照」）——否则免审批/仅高危
// 档直执行后，该审计行无法自证是哪档门限放行（审批分支早已带该字段）。
func TestContainerDirectExecutionAuditsApprovalMode(t *testing.T) {
	env := newTestEnv(t)
	id := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	// auto_exec=true + 全局 none 档：stop（needs_approval）直执行、不建审批。
	env.st.mu.Lock()
	srv := env.st.servers[id]
	srv.AutoExecEnabled = true
	env.st.servers[id] = srv
	env.st.settings["approval.mode"] = store.Setting{Key: "approval.mode", Value: json.RawMessage(`"none"`)}
	env.st.mu.Unlock()
	env.sshExec.execFn = dockerRouteExec("", 0)

	rec := env.do(http.MethodPost, "/api/servers/"+id+"/containers/c1d2e3f4a5b6/action", `{"action":"restart"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("action = %d %s", rec.Code, rec.Body.String())
	}
	body := decodeBodyMap(t, rec)
	if body["approval"] != nil {
		t.Fatalf("none 档直执行必须返回 approval=null，got %v", body["approval"])
	}
	if body["execution"] == nil {
		t.Fatalf("none 档直执行必须返回 execution: %v", body)
	}
	if len(env.sshExec.calls) != 1 || env.sshExec.calls[0].Command != "docker restart c1d2e3f4a5b6" {
		t.Fatalf("exec calls = %+v", env.sshExec.calls)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	if err := env.auditW.Flush(flushCtx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var found bool
	for _, l := range env.st.auditLogs {
		if l.Action != "container.action" {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal(l.Detail, &detail); err != nil {
			t.Fatalf("detail unmarshal: %v", err)
		}
		if detail["source"] != "console" || detail["action"] != "restart" {
			continue
		}
		if detail["approval_mode"] != "none" {
			t.Fatalf("container.action detail.approval_mode = %v，want none（直执行也须带门限快照）", detail["approval_mode"])
		}
		found = true
	}
	if !found {
		t.Fatalf("container.action audit missing: %+v", env.st.auditLogs)
	}
}
