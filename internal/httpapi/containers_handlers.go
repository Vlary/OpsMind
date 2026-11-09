// containers_handlers.go 实现四期 C1 的容器 REST 五路由（契约 2.5/2.9）：
// 列表/详情/日志/镜像为实时 CLI 查询（容器数据不落库）；操作统一走
// console 审批（source=console，conversation/tool_call 为 NULL，命令固化
// 为按运行时拼好的逐字节命令），批准时由 decide handler 同步执行
// （approvals.go 的 C1 扩展）。containerd 只探测不管理；network_device
// 恒不支持。
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/containerx"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// 容器执行上限（契约 7.2：探测 15s；CLI 60s；输出 256KB——通道层截断）。
const (
	containerDetectTimeout = 15 * time.Second
	containerCLITimeout    = 60 * time.Second
	containerMaxOutput     = 262144
)

// containerActionRequest 是 POST /containers/:cid/action 请求体。
type containerActionRequest struct {
	Action string `json:"action"`
}

// RegisterContainerRoutes 注册四期 C1 容器路由（契约 2.5）。
func (d *Deps) RegisterContainerRoutes(api *gin.RouterGroup) {
	api.GET("/servers/:id/containers", d.handleListContainers)
	api.GET("/servers/:id/containers/:cid", d.handleInspectContainer)
	api.POST("/servers/:id/containers/:cid/action", d.handleContainerAction)
	api.GET("/servers/:id/containers/:cid/logs", d.handleContainerLogs)
	api.GET("/servers/:id/images", d.handleListImages)
}

// containerExecFunc 按契约 2.7 通道规则构造 ExecFunc（WinRM 走 PowerShell
// 端点；SSH 原样发送，Windows 由 agent 层同款外壳包装——REST 路径仅
// linux/windows 服务器可达，network_device 已在 runtime 解析处拒绝）。
func (d *Deps) containerExecFunc(srv store.Server) (containerx.ExecFunc, error) {
	if srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic {
		if d.WinRM == nil {
			return nil, errors.New("winrm executor unavailable")
		}
		target, err := d.winRMTarget(srv)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, command string, timeout time.Duration) (containerx.ExecResult, error) {
			out, err := d.WinRM.Execute(ctx, target, winrmx.RunRequest{
				Command: command, Timeout: timeout, MaxOutputBytes: containerMaxOutput,
			})
			if err != nil {
				return containerx.ExecResult{}, err
			}
			return containerx.ExecResult{
				ExitCode: out.ExitCode, Combined: out.Combined,
				Truncated: out.Truncated, DurationMS: out.DurationMS,
			}, nil
		}, nil
	}
	if d.SSH == nil {
		return nil, errors.New("ssh executor unavailable")
	}
	target, err := d.sshTarget(srv)
	if err != nil {
		return nil, err
	}
	isWinSSH := srv.OS == store.ServerOSWindows
	return func(ctx context.Context, command string, timeout time.Duration) (containerx.ExecResult, error) {
		execCommand := command
		if isWinSSH {
			execCommand = "powershell -NoProfile -NonInteractive -Command " + command
		}
		out, err := d.SSH.Execute(ctx, target, sshx.RunRequest{
			Command: execCommand, Timeout: timeout, MaxOutputBytes: containerMaxOutput,
		})
		if err != nil {
			return containerx.ExecResult{}, err
		}
		return containerx.ExecResult{
			ExitCode: out.ExitCode, Combined: out.Combined,
			Truncated: out.Truncated, DurationMS: out.DurationMS,
		}, nil
	}, nil
}

// containerRuntimeFor 解析运行时：network_device → 400 CONTAINER_UNSUPPORTED；
// ” 先探测一次并写回（失败按 none）；非 docker/podman → 400
// CONTAINER_UNSUPPORTED（detail 携带现值，契约 2.9 约定 2）。
func (d *Deps) containerRuntimeFor(c *gin.Context, srv store.Server) (containerx.Runtime, containerx.ExecFunc, bool) {
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		// 契约 2.9 约定 1/S21①：detail 携带现值 runtime（network_device 恒空串）。
		writeAPIErrorDetail(c, http.StatusBadRequest, apierr.ContainerUnsupported,
			"network device has no container management plane", map[string]any{"runtime": ""})
		return "", nil, false
	}
	exec, err := d.containerExecFunc(srv)
	if err != nil {
		d.logError(c, "build container exec target failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to prepare credentials")
		return "", nil, false
	}
	rt := containerx.Runtime(srv.ContainerRuntime)
	if rt == containerx.RuntimeNone {
		detectCtx, cancel := context.WithTimeout(c.Request.Context(), containerDetectTimeout)
		defer cancel()
		detected, derr := containerx.Detect(detectCtx, exec, string(srv.OS))
		if derr != nil {
			// 通道错误：本次按 none 返回 400（失败关闭）但不持久化——
			// 探测确认（derr==nil，含确认 none）才写回，避免一次瞬态
			// 失败把服务器钉死为 CONTAINER_UNSUPPORTED（评审发现）。
			detected = containerx.RuntimeAbsent
			rt = detected
		} else {
			rt = detected
			if d.Store != nil {
				_, _ = d.Store.Servers().UpdateContainerRuntime(c.Request.Context(), srv.ID, store.ContainerRuntime(detected), d.now())
			}
		}
	}
	if rt != containerx.RuntimeDocker && rt != containerx.RuntimePodman {
		// 契约 2.9 约定 1：detail 携带现值 runtime（''/none/containerd 等），
		// 前端据此增强文案（评审发现后端从不填 details，前端分支成死代码）。
		writeAPIErrorDetail(c, http.StatusBadRequest, apierr.ContainerUnsupported,
			"server has no supported container runtime (container_runtime="+string(rt)+")",
			map[string]any{"runtime": string(rt)})
		return "", nil, false
	}
	return rt, exec, true
}

// writeContainerError 把 containerx 错误映射为统一错误响应（契约 9.6）。
// p4 sprint 6：SSH/WinRM 通道层错误（拨号/握手/认证/会话失败——命令未
// 送达远端）单独映射 502 SSH_EXEC_FAILED，不再落 default 的
// CONTAINER_CLI_FAILED（其文案断言「daemon 未运行」，把「服务器 SSH 不可
// 达」的根因报成容器问题，误导排障——走查 D 实测：库值 docker + 指向
// 127.0.0.1:1 的服务器，横幅误导）。口径与 agent 容器工具路径一致
// （runner.go classifySSHError 同样把通道错误归 SSH_EXEC_FAILED）。
func (d *Deps) writeContainerError(c *gin.Context, err error) {
	var cliErr *containerx.CLIError
	switch {
	case errors.Is(err, containerx.ErrContainerNotFound):
		writeAPIError(c, http.StatusNotFound, apierr.ContainerNotFound, "container not found")
	case errors.Is(err, containerx.ErrInvalidContainerID):
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid container id")
	case errors.Is(err, containerx.ErrUnsupportedRuntime):
		writeAPIError(c, http.StatusBadRequest, apierr.ContainerUnsupported, "server has no supported container runtime")
	case errors.As(err, &cliErr):
		writeAPIError(c, http.StatusBadGateway, apierr.ContainerCLIFailed, "container CLI command failed")
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(c, http.StatusGatewayTimeout, apierr.SSHTimeout, "container command timed out")
	case isContainerChannelError(err):
		// 通道错误在 DeadlineExceeded 之后：超时形态（sshx/winrmx 超时
		// 错误同样带包前缀）优先归 504 SSH_TIMEOUT，保持既有映射不变。
		d.logError(c, "container exec channel failed", err)
		writeAPIError(c, http.StatusBadGateway, apierr.SSHExecFailed, "server channel failed before container CLI ran")
	default:
		d.logError(c, "container CLI failed", err)
		writeAPIError(c, http.StatusBadGateway, apierr.ContainerCLIFailed, "container CLI command failed")
	}
}

// isContainerChannelError 判定错误是否为 SSH/WinRM 通道层失败（拨号、
// 握手、认证、会话建立、连接池——命令未送达远端或通道中断）。判据：
// sshx 与 winrmx 的 Executor 错误恒以 "sshx: " / "winrmx: " 前缀包装
// （internal/sshx executor.go/pool.go 的 dial/handshake/open session/
// start command/wait 与 internal/winrmx winrm.go/pool.go 的 run
// command/acquire 全路径枚举核实），沿 errors.Unwrap 链检查前缀即可
// 完整覆盖，不依赖文本猜测网络层细节；CLIError（exit≠0，"containerx: "
// 前缀）与解析超限错误（"containerx: output exceeds …"）不含该前缀，
// 不会误入本分支。
func isContainerChannelError(err error) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		msg := cur.Error()
		if strings.HasPrefix(msg, "sshx: ") || strings.HasPrefix(msg, "winrmx: ") {
			return true
		}
	}
	return false
}

func (d *Deps) containerServer(c *gin.Context) (store.Server, bool) {
	if !d.requireStore(c) {
		return store.Server{}, false
	}
	srv, err := d.Store.Servers().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return store.Server{}, false
	}
	return srv, true
}

func (d *Deps) handleListContainers(c *gin.Context) {
	srv, ok := d.containerServer(c)
	if !ok {
		return
	}
	rt, exec, ok := d.containerRuntimeFor(c, srv)
	if !ok {
		return
	}
	containers, err := containerx.ListContainers(c.Request.Context(), rt, exec)
	if err != nil {
		d.writeContainerError(c, err)
		return
	}
	// 契约 ContainerListResponse：{runtime, items}——前端读 data.items，
	// 后端曾用 containers 键致表格恒空（评审 verified）。
	c.JSON(http.StatusOK, gin.H{"server_id": srv.ID, "runtime": string(rt), "items": containers})
}

func (d *Deps) handleInspectContainer(c *gin.Context) {
	srv, ok := d.containerServer(c)
	if !ok {
		return
	}
	cid := c.Param("cid")
	if err := containerx.ValidateContainerID(cid); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid container id")
		return
	}
	rt, exec, ok := d.containerRuntimeFor(c, srv)
	if !ok {
		return
	}
	detail, err := containerx.InspectContainer(c.Request.Context(), rt, cid, exec)
	if err != nil {
		d.writeContainerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"server_id": srv.ID, "container_id": cid, "container": detail})
}

func (d *Deps) handleContainerLogs(c *gin.Context) {
	srv, ok := d.containerServer(c)
	if !ok {
		return
	}
	cid := c.Param("cid")
	if err := containerx.ValidateContainerID(cid); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid container id")
		return
	}
	tail := containerx.DefaultTail
	if raw := c.Query("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > containerx.MaxTail {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "tail must be an integer between 1 and 1000")
			return
		}
		tail = n
	}
	rt, exec, ok := d.containerRuntimeFor(c, srv)
	if !ok {
		return
	}
	output, truncated, err := containerx.ContainerLogs(c.Request.Context(), rt, cid, tail, exec)
	if err != nil {
		d.writeContainerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"server_id": srv.ID, "container_id": cid, "tail": tail,
		"output": output, "truncated": truncated,
	})
}

func (d *Deps) handleListImages(c *gin.Context) {
	srv, ok := d.containerServer(c)
	if !ok {
		return
	}
	rt, exec, ok := d.containerRuntimeFor(c, srv)
	if !ok {
		return
	}
	images, err := containerx.ListImages(c.Request.Context(), rt, exec)
	if err != nil {
		d.writeContainerError(c, err)
		return
	}
	// 契约 ImageListResponse：{runtime, items}（同上）。
	c.JSON(http.StatusOK, gin.H{"server_id": srv.ID, "runtime": string(rt), "items": images})
}

// handleContainerAction 按统一审批门限分流（六期 BE-P，契约 2.11 约定 2
// 第 ④ 条）：needsApproval=true → 创建 source=console 审批返回
// {approval, execution:null}（废弃四期「控制台操作全部人工二段确认」的
// 恒审批裁决）；false → 同步执行返回 {approval:null, execution}。
// command 固化为按运行时拼好的逐字节命令；console 批量用全局门限。
func (d *Deps) handleContainerAction(c *gin.Context) {
	srv, ok := d.containerServer(c)
	if !ok {
		return
	}
	if d.Approvals == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "approval service unavailable")
		return
	}
	var req containerActionRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	var action containerx.Action
	switch req.Action {
	case "start", "stop", "restart":
		action = containerx.Action(req.Action)
	default:
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "action must be start, stop or restart")
		return
	}
	cid := c.Param("cid")
	if err := containerx.ValidateContainerID(cid); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid container id")
		return
	}
	rt, exec, ok := d.containerRuntimeFor(c, srv)
	if !ok {
		return
	}
	// 分级写死（契约 7.2）：start=safe；stop/restart=needs_approval
	//（CONTAINER_CHANGE，severity=high，7.3E）。console 用全局门限。
	riskLevel := "safe"
	if action != containerx.ActionStart {
		riskLevel = "needs_approval"
	}
	mode, err := approvals.GlobalMode(c.Request.Context(), d.Store)
	if err != nil {
		d.logError(c, "resolve approval mode failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
		return
	}
	if approvals.NeedsApproval(approvals.PolicyInput{
		Mode:            mode,
		AutoExecEnabled: srv.AutoExecEnabled,
		Level:           risk.Level(riskLevel),
		Severity:        risk.SeverityOf("CONTAINER_CHANGE"),
	}) {
		command := containerx.ActionCommand(rt, cid, action)
		ap, err := d.Approvals.Create(c.Request.Context(), approvals.CreateRequest{
			Source:       "console",
			ServerID:     &srv.ID,
			ServerName:   srv.Name,
			Command:      command,
			RiskLevel:    riskLevel,
			RuleID:       "CONTAINER_CHANGE",
			Reason:       "container state change",
			ApprovalMode: string(mode),
			RequestID:    c.GetString("request_id"),
		})
		if err != nil {
			if errors.Is(err, store.ErrInvalid) {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid approval request")
				return
			}
			d.logError(c, "create console approval failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
			return
		}
		// 契约 ContainerActionResponse（六期 wrapper）：需审批时
		// {approval, execution:null}。
		c.JSON(http.StatusOK, gin.H{"approval": ap, "execution": nil})
		return
	}

	// 门限判定直执行（none/high+auto_exec 等）：同步执行并返回
	// {approval:null, execution}；审计 container.action（source=console）。
	res, execErr := containerx.ContainerAction(c.Request.Context(), rt, cid, action, exec)
	execution := map[string]any{}
	// 契约 9.7 / 5261 行：直执行同样携带门限快照（approval_mode）——免审批/
	// 仅高危档直执行后，该审计行要能自证是哪档门限放行；与
	// executeConsoleApproval 的审批分支同字段（六期打磨 sprint 6）。
	detail := map[string]any{
		"source":        "console",
		"approval_mode": string(mode),
	}
	if execErr != nil {
		// 错误码口径与 executeConsoleApproval 一致（超时 504 SSH_TIMEOUT、
		// 通道失败 SSH_EXEC_FAILED、其余容器 CLI 失败）。
		code := apierr.ContainerCLIFailed
		if errors.Is(execErr, context.DeadlineExceeded) {
			code = apierr.SSHTimeout
		} else if isContainerChannelError(execErr) {
			code = apierr.SSHExecFailed
		}
		execution = map[string]any{"exit_code": -1, "output": "", "truncated": false, "error_code": code}
		detail["exit_code"] = -1
		detail["error_code"] = code
	} else {
		execution = map[string]any{
			"exit_code":   res.ExitCode,
			"output":      res.Combined,
			"truncated":   res.Truncated,
			"duration_ms": res.DurationMS,
		}
		detail["exit_code"] = res.ExitCode
		if res.ExitCode != 0 {
			execution["error_code"] = apierr.ContainerCLIFailed
			detail["error_code"] = apierr.ContainerCLIFailed
		}
	}
	detail["runtime"] = string(rt)
	detail["container_id"] = cid
	detail["action"] = string(action)
	serverID := srv.ID
	d.auditEnqueue(c, audit.Event{
		ActorType:    "user",
		ActorID:      c.GetString("user_id"),
		Action:       "container.action",
		ResourceType: "server",
		ResourceID:   serverID,
		ServerID:     &serverID,
		Detail:       detail,
	})
	c.JSON(http.StatusOK, gin.H{"approval": nil, "execution": execution})
}

// executeConsoleCommand 在服务器上同步逐字节执行命令（六期 BE-P：诊断
// fix 门限直执行分支复用的执行核心；失败口径与 executeConsoleApproval
// 一致）。返回值直接作为响应的 execution 字段，不写审计（调用方按语义
// 落 container.action 等）。
func (d *Deps) executeConsoleCommand(c *gin.Context, srv store.Server, command string) map[string]any {
	execution := map[string]any{
		"exit_code": -1,
		"output":    "",
		"truncated": false,
	}
	exec, err := d.containerExecFunc(srv)
	if err != nil {
		execution["error_code"] = apierr.Internal
		return execution
	}
	res, err := exec(c.Request.Context(), command, containerCLITimeout)
	if err != nil {
		code := apierr.ContainerCLIFailed
		if errors.Is(err, context.DeadlineExceeded) {
			code = apierr.SSHTimeout
		} else if isContainerChannelError(err) {
			code = apierr.SSHExecFailed
		}
		execution["error_code"] = code
		return execution
	}
	execution["exit_code"] = res.ExitCode
	execution["output"] = res.Combined
	execution["truncated"] = res.Truncated
	execution["duration_ms"] = res.DurationMS
	if res.ExitCode != 0 {
		execution["error_code"] = apierr.ContainerCLIFailed
	}
	return execution
}

// executeConsoleApproval 在 decide 批准 console 审批后同步逐字节执行
// 实际命令（六期 BE-P，契约 2.11 约定 3：edited_command 非空时的改写后
// 命令，否则 approval.command；契约 5.5：执行不发生在 Service.Decide——
// 在 decide handler；失败不回滚审批状态，失败信息只进 execution 与审计）。
// 返回值直接作为响应的 execution 字段。
func (d *Deps) executeConsoleApproval(c *gin.Context, ap store.Approval) map[string]any {
	detail := map[string]any{
		"exit_code": -1,
		"source":    "console",
		// 契约 9.7：command.executed/container.action 均携带门限快照
		//（审批落库时冻结的有效门限）。
		"approval_mode": string(ap.ApprovalMode),
	}
	execution := map[string]any{
		"exit_code": -1,
		"output":    "",
		"truncated": false,
	}
	fail := func(code string) map[string]any {
		execution["error_code"] = code
		detail["error_code"] = code
		d.auditConsoleContainerAction(c, ap, detail)
		return execution
	}
	if ap.ServerID == nil || *ap.ServerID == "" {
		return fail(apierr.Internal)
	}
	srv, err := d.Store.Servers().Get(c.Request.Context(), *ap.ServerID)
	if err != nil {
		return fail(apierr.ServerNotFound)
	}
	exec, err := d.containerExecFunc(srv)
	if err != nil {
		return fail(apierr.Internal)
	}
	// 实际执行命令=改写后命令（edited ?? original，契约 3.4 六期约束）。
	command := ap.Command
	if ap.EditedCommand != "" {
		command = ap.EditedCommand
	}
	res, err := exec(c.Request.Context(), command, containerCLITimeout)
	if err != nil {
		code := apierr.ContainerCLIFailed
		if errors.Is(err, context.DeadlineExceeded) {
			code = apierr.SSHTimeout
		} else if isContainerChannelError(err) {
			// p4 sprint 6：通道层失败（拨号/认证等）与 list 路径同口径归
			// SSH_EXEC_FAILED，不误导为容器 CLI 失败（writeContainerError）。
			code = apierr.SSHExecFailed
		}
		execution["output"] = ""
		return fail(code)
	}
	execution["exit_code"] = res.ExitCode
	execution["output"] = res.Combined
	execution["truncated"] = res.Truncated
	execution["duration_ms"] = res.DurationMS
	detail["exit_code"] = res.ExitCode
	if res.ExitCode != 0 {
		execution["error_code"] = apierr.ContainerCLIFailed
		detail["error_code"] = apierr.ContainerCLIFailed
	}
	d.auditConsoleContainerAction(c, ap, detail)
	return execution
}

// auditConsoleContainerAction 落 container.action 审计（契约 9.7：
// detail={runtime, container_id, action, exit_code, source:console, error_code?}；
// runtime/container_id/action 从实际执行命令解析（"docker stop 9f2c"——
// 六期起为 edited ?? original，与执行同源）。
func (d *Deps) auditConsoleContainerAction(c *gin.Context, ap store.Approval, detail map[string]any) {
	command := ap.Command
	if ap.EditedCommand != "" {
		command = ap.EditedCommand
	}
	runtime, cid, action := parseContainerApprovalCommand(command)
	serverID := ""
	if ap.ServerID != nil {
		serverID = *ap.ServerID
	}
	// 诊断 fix 等非容器命令复用 console 审批路径时，命令不是「cli 动作
	// cid」形态——落 container.action 会产生 runtime/container_id/action
	// 全空的错挂审计（六期评审发现）；改落 command.executed（带完整命令
	// 与门限快照），container.action 专用于容器命令。
	if runtime == "" {
		detail["command"] = command
		d.auditEvent(c, "command.executed", "server", serverID, detail)
		return
	}
	detail["runtime"] = runtime
	detail["container_id"] = cid
	detail["action"] = action
	d.auditEvent(c, "container.action", "server", serverID, detail)
}

// parseContainerApprovalCommand 解析固化的容器命令（containerx.ActionCommand
// 产物："<cli> <action> <cid>"）；非预期形态返回空串（不猜测）。
func parseContainerApprovalCommand(command string) (runtime, cid, action string) {
	parts := splitFields(command)
	if len(parts) != 3 {
		return "", "", ""
	}
	switch parts[0] {
	case "docker", "podman":
		runtime = parts[0]
	default:
		return "", "", ""
	}
	switch parts[1] {
	case "start", "stop", "restart":
		action = parts[1]
	default:
		return "", "", ""
	}
	return runtime, parts[2], action
}

// splitFields 按空白拆分（输入是内部构造的命令，无引号复杂性）。
func splitFields(s string) []string {
	out := make([]string, 0, 3)
	start := -1
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' || s[i] == '\t' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	return out
}
