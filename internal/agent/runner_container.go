// runner_container.go 实现四期 C1 的五个容器 AI 工具（契约 7.1/7.2/5.2C）：
// container_list/container_inspect/container_logs/image_list 为固定只读
// 模板恒 safe、无审批（同 server_facts 模式）；container_action 的
// start=safe、stop/restart=needs_approval（CONTAINER_CHANGE），
// auto_exec_enabled 语义与 ssh_exec 完全一致，审批命令即执行命令（逐字节）。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/containerx"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// errExecNotConfigured 表示对应执行通道未注入。
var errExecNotConfigured = errors.New("agent: executor is not configured")

// containerActionExecTimeout 是改写命令经通道逐字节执行的预算（六期
// BE-P；与 containerx 内部 cliTimeout=60s 同口径）。
const containerActionExecTimeout = 60 * time.Second

// sshRunRequest / winRMRunRequest 构造通道层执行请求（输出上限 256KB，
// 契约 7.2；logs 输出截断由通道层完成，Truncated 透传）。
func sshRunRequest(command string, timeout time.Duration) sshx.RunRequest {
	return sshx.RunRequest{Command: command, Timeout: timeout, MaxOutputBytes: MaxOutputBytes}
}

func winRMRunRequest(command string, timeout time.Duration) winrmx.RunRequest {
	return winrmx.RunRequest{Command: command, Timeout: timeout, MaxOutputBytes: MaxOutputBytes}
}

// containerExecFunc 按契约 2.7 通道规则构造 containerx 的 ExecFunc：
// WinRM 认证走 winrmx PowerShell 端点（Windows 探测模板语句块由其内部
// 包装执行）；SSH 通道 Linux 原样发送、Windows 包装 PowerShell 外壳。
func (r *Runner) containerExecFunc(srv store.Server) (containerx.ExecFunc, error) {
	if winRMChannel(srv) {
		if r.winrm == nil {
			return nil, errExecNotConfigured
		}
		target, err := r.winRMTarget(srv)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, command string, timeout time.Duration) (containerx.ExecResult, error) {
			out, err := r.winrm.Execute(ctx, target, winRMRunRequest(command, timeout))
			if err != nil {
				return containerx.ExecResult{}, err
			}
			return containerx.ExecResult{
				ExitCode: out.ExitCode, Combined: out.Combined,
				Truncated: out.Truncated, DurationMS: out.DurationMS,
			}, nil
		}, nil
	}
	if r.ssh == nil {
		return nil, errExecNotConfigured
	}
	target, err := r.serverTarget(srv)
	if err != nil {
		return nil, err
	}
	isWinSSH := srv.OS == store.ServerOSWindows
	return func(ctx context.Context, command string, timeout time.Duration) (containerx.ExecResult, error) {
		execCommand := command
		if isWinSSH {
			execCommand = wrapPowerShellForSSH(command)
		}
		out, err := r.ssh.Execute(ctx, target, sshRunRequest(execCommand, timeout))
		if err != nil {
			return containerx.ExecResult{}, err
		}
		return containerx.ExecResult{
			ExitCode: out.ExitCode, Combined: out.Combined,
			Truncated: out.Truncated, DurationMS: out.DurationMS,
		}, nil
	}, nil
}

// containerRuntimeFor 解析服务器容器运行时（契约 2.9 约定 2）：
// network_device 恒不支持；” 时先同步探测一次（15s）并写回，
// 探测失败按 none 处理；非 docker/podman 一律 CONTAINER_UNSUPPORTED。
func (r *Runner) containerRuntimeFor(ctx context.Context, srv store.Server) (containerx.Runtime, containerx.ExecFunc, *ToolError) {
	if isNetworkDevice(srv) {
		return "", nil, &ToolError{Code: codeContainerUnsupported, Message: "server has no supported container management plane"}
	}
	exec, err := r.containerExecFunc(srv)
	if err != nil {
		return "", nil, &ToolError{Code: codeInternal, Message: "server credentials are unavailable"}
	}
	rt := containerx.Runtime(srv.ContainerRuntime)
	if rt == containerx.RuntimeNone {
		detectCtx, cancel := context.WithTimeout(ctx, ContainerDetectLimit)
		defer cancel()
		detected, derr := containerx.Detect(detectCtx, exec, string(srv.OS))
		if derr != nil {
			// 通道错误：本次请求按 none 处理并照常 CONTAINER_UNSUPPORTED
			//（契约 2.9 约定 2，失败关闭），但不持久化——一次 15s 超时/
			// 拨号失败曾把服务器钉死为 none（此后仅对 '' 重探，只有
			// POST /test 可救），保持 '' 下次请求重探（评审发现）。
			detected = containerx.RuntimeAbsent
			rt = detected
			if r.log != nil {
				r.log.Warn("agent_container_detect_channel_error", "server_id", srv.ID, "error", derr.Error())
			}
		} else {
			rt = detected
			if r.st != nil {
				writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				if _, uerr := r.st.Servers().UpdateContainerRuntime(writeCtx, srv.ID, store.ContainerRuntime(detected), r.now()); uerr != nil && r.log != nil {
					r.log.Warn("agent_update_container_runtime_failed", "server_id", srv.ID, "error", uerr.Error())
				}
				writeCancel()
			}
		}
	}
	if rt != containerx.RuntimeDocker && rt != containerx.RuntimePodman {
		return "", nil, &ToolError{Code: codeContainerUnsupported, Message: "server has no supported container management plane"}
	}
	return rt, exec, nil
}

// classifyContainerExecError 把 containerx 错误映射为工具错误码（契约 9.6）。
func classifyContainerExecError(err error) *ToolError {
	var cliErr *containerx.CLIError
	switch {
	case errors.Is(err, containerx.ErrContainerNotFound):
		return &ToolError{Code: codeContainerNotFound, Message: "container not found"}
	case errors.Is(err, containerx.ErrInvalidContainerID):
		return &ToolError{Code: codeToolArgsInvalid, Message: "invalid container id"}
	case errors.Is(err, containerx.ErrUnsupportedRuntime):
		return &ToolError{Code: codeContainerUnsupported, Message: "server has no supported container management plane"}
	case errors.As(err, &cliErr):
		return &ToolError{Code: codeContainerCLIFailed, Message: "container CLI command failed"}
	case errors.Is(err, context.DeadlineExceeded):
		return &ToolError{Code: codeSSHTimeout, Message: "container command timed out"}
	default:
		return &ToolError{Code: codeContainerCLIFailed, Message: "container command failed"}
	}
}

func (r *Runner) runContainerList(ctx context.Context, rec store.ToolCall, raw json.RawMessage) toolOutcome {
	var args containerServerArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.ServerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id is required"}, Status: store.ToolFailed}
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}
	rt, exec, terr := r.containerRuntimeFor(ctx, srv)
	if terr != nil {
		return toolOutcome{Err: terr, Status: store.ToolFailed}
	}
	containers, err := containerx.ListContainers(ctx, rt, exec)
	if err != nil {
		return toolOutcome{Err: classifyContainerExecError(err), Status: store.ToolFailed}
	}
	out, _ := json.Marshal(map[string]any{
		"server_id":  srv.ID,
		"runtime":    string(rt),
		"containers": containers,
	})
	// 只读容器操作不审计（契约 9.7）；tool.call/tool.result 已有。
	return toolOutcome{Result: out, Status: store.ToolSucceeded}
}

func (r *Runner) runContainerInspect(ctx context.Context, rec store.ToolCall, raw json.RawMessage) toolOutcome {
	var args containerIDArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.ServerID == "" || args.ContainerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id and container_id are required"}, Status: store.ToolFailed}
	}
	if err := containerx.ValidateContainerID(args.ContainerID); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid container id"}, Status: store.ToolFailed}
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}
	rt, exec, terr := r.containerRuntimeFor(ctx, srv)
	if terr != nil {
		return toolOutcome{Err: terr, Status: store.ToolFailed}
	}
	detail, err := containerx.InspectContainer(ctx, rt, args.ContainerID, exec)
	if err != nil {
		return toolOutcome{Err: classifyContainerExecError(err), Status: store.ToolFailed}
	}
	out, _ := json.Marshal(map[string]any{
		"server_id":    srv.ID,
		"container_id": args.ContainerID,
		"container":    detail,
	})
	return toolOutcome{Result: out, Status: store.ToolSucceeded}
}

func (r *Runner) runContainerLogs(ctx context.Context, rec store.ToolCall, raw json.RawMessage) toolOutcome {
	var args containerLogsArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.ServerID == "" || args.ContainerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id and container_id are required"}, Status: store.ToolFailed}
	}
	if err := containerx.ValidateContainerID(args.ContainerID); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid container id"}, Status: store.ToolFailed}
	}
	tail := ContainerLogsDefaultTail
	if args.Tail != nil {
		if *args.Tail < 1 || *args.Tail > ContainerLogsMaxTail {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "tail must be between 1 and 1000"}, Status: store.ToolFailed}
		}
		tail = *args.Tail
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}
	rt, exec, terr := r.containerRuntimeFor(ctx, srv)
	if terr != nil {
		return toolOutcome{Err: terr, Status: store.ToolFailed}
	}
	output, truncated, err := containerx.ContainerLogs(ctx, rt, args.ContainerID, tail, exec)
	if err != nil {
		return toolOutcome{Err: classifyContainerExecError(err), Status: store.ToolFailed}
	}
	out, _ := json.Marshal(map[string]any{
		"server_id":    srv.ID,
		"container_id": args.ContainerID,
		"tail":         tail,
		"output":       output,
		"truncated":    truncated,
	})
	return toolOutcome{Result: out, Status: store.ToolSucceeded}
}

func (r *Runner) runImageList(ctx context.Context, rec store.ToolCall, raw json.RawMessage) toolOutcome {
	var args containerServerArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.ServerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id is required"}, Status: store.ToolFailed}
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}
	rt, exec, terr := r.containerRuntimeFor(ctx, srv)
	if terr != nil {
		return toolOutcome{Err: terr, Status: store.ToolFailed}
	}
	images, err := containerx.ListImages(ctx, rt, exec)
	if err != nil {
		return toolOutcome{Err: classifyContainerExecError(err), Status: store.ToolFailed}
	}
	out, _ := json.Marshal(map[string]any{
		"server_id": srv.ID,
		"runtime":   string(rt),
		"images":    images,
	})
	return toolOutcome{Result: out, Status: store.ToolSucceeded}
}

// runContainerAction 对容器执行 start/stop/restart（契约 7.2）。
// start=safe；stop/restart=needs_approval（CONTAINER_CHANGE，severity=high，
// 7.3E）；审批判定走统一门限 approvals.NeedsApproval（六期 BE-P，契约
// 2.11 约定 2——替换内联「needs_approval || !auto_exec」）。审批命令即
// 逐字节命令（如 `docker stop 9f2c...`）；decide 批准且用户改写时执行
// 改写后命令（经通道逐字节执行，契约 2.11 约定 3）。
func (r *Runner) runContainerAction(ctx context.Context, conv store.Conversation, rec store.ToolCall, providerCallID string, raw json.RawMessage, emit func(Event) error, in RunInput, approvalMode store.ApprovalMode) (out toolOutcome) {
	var args containerActionArgs
	if err := decodeStrictArgs(raw, &args); err != nil || args.ServerID == "" || args.ContainerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id and container_id are required"}, Status: store.ToolFailed}
	}
	var action containerx.Action
	switch args.Action {
	case "start", "stop", "restart":
		action = containerx.Action(args.Action)
	default:
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "action must be start, stop or restart"}, Status: store.ToolFailed}
	}
	if err := containerx.ValidateContainerID(args.ContainerID); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid container id"}, Status: store.ToolFailed}
	}
	srv, err := r.st.Servers().Get(ctx, args.ServerID)
	if err != nil {
		return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
	}
	rt, exec, terr := r.containerRuntimeFor(ctx, srv)
	if terr != nil {
		return toolOutcome{Err: terr, Status: store.ToolFailed}
	}

	// 分级写死（契约 7.2）；动作集封闭，forbidden 不存在于此工具。
	// CONTAINER_CHANGE 归 high（7.3E：容器变更面）。
	riskLevel := containerRiskLevel(action)
	needsApproval := approvals.NeedsApproval(approvals.PolicyInput{
		Mode:            approvalMode,
		AutoExecEnabled: srv.AutoExecEnabled,
		Level:           risk.Level(riskLevel),
		Severity:        risk.SeverityOf(ruleContainerChange),
	})

	approvalID := ""
	editedCommand := ""
	if needsApproval {
		if r.approvals == nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "approval service is not configured"}, Status: store.ToolFailed}
		}
		command := containerx.ActionCommand(rt, args.ContainerID, action)
		approval, err := r.approvals.Create(ctx, approvals.CreateRequest{
			ConversationID: &conv.ID,
			ToolCallID:     &rec.ID,
			ServerID:       &srv.ID,
			ServerName:     srv.Name,
			Command:        command,
			RiskLevel:      riskLevel,
			RuleID:         ruleContainerChange,
			Reason:         "container state change",
			ApprovalMode:   string(approvalMode),
			RequestID:      in.RequestID,
		})
		if err != nil {
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "failed to create approval"}, Status: store.ToolFailed}
		}
		approvalID = approval.ID
		var execStart time.Time
		defer func() {
			if !execStart.IsZero() {
				out.ExecStartedAt = execStart
			}
		}()
		if _, err := r.st.ToolCalls().SetStatus(ctx, rec.ID, store.ToolAwaitingApproval, "", ""); err != nil && r.log != nil {
			r.log.Warn("agent_tool_status_failed", "tool_call_id", rec.ID, "error", err.Error())
		}
		if err := emit(Event{Type: EventApprovalRequired, ApprovalRequired: &approval}); err != nil {
			// 流已断开：取消审批并收敛 tool_call（六期打磨 sprint 3）。
			r.cancelApprovalAndConverge(ctx, rec.ID, approval.ID, in.RequestID)
			return toolOutcome{EmitErr: err}
		}
		decisionResult, err := r.approvals.Wait(ctx, approval.ID)
		if err != nil {
			r.cancelApprovalAndConverge(ctx, rec.ID, approval.ID, in.RequestID)
			execStart = r.now()
			if ctx.Err() != nil {
				return toolOutcome{Fatal: ctx.Err()}
			}
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "approval wait failed"}, Status: store.ToolFailed}
		}
		execStart = r.now()
		if err := emit(Event{Type: EventApprovalResolved, ApprovalResolved: &ApprovalResolvedPayload{
			ApprovalID:    approval.ID,
			Status:        decisionResult.Status,
			DecidedAt:     decisionResult.DecidedAt,
			EditedCommand: decisionResult.EditedCommand,
		}}); err != nil {
			r.convergeResolvedToolCall(ctx, rec.ID)
			return toolOutcome{EmitErr: err}
		}
		switch decisionResult.Status {
		case store.ApprovalApproved:
			// 继续执行；批准且用户改写时改执行改写后命令（契约 2.11 约定 3）。
			editedCommand = decisionResult.EditedCommand
		case store.ApprovalDenied:
			return toolOutcome{
				Err:    &ToolError{Code: codeApprovalDenied, Message: "action was denied by the operator"},
				Status: store.ToolDenied,
			}
		default:
			return toolOutcome{
				Err:    &ToolError{Code: codeApprovalTimeout, Message: "approval expired before a decision"},
				Status: store.ToolExpired,
			}
		}
	}

	// 执行：未改写走结构化 containerx（逐字节同审批命令）；改写时经通道
	// 逐字节执行改写后命令（decide 已重新 Classify 拒绝 forbidden；超时
	// 同 containerx 内部 cliTimeout=60s）。
	var res containerx.ExecResult
	if editedCommand != "" {
		var execErr error
		res, execErr = exec(ctx, editedCommand, containerActionExecTimeout)
		if execErr != nil {
			terr := classifyContainerExecError(execErr)
			// 执行失败也落 container.action 审计（契约 9.7：含失败）。
			r.auditContainerAction(ctx, conv.ID, srv.ID, string(rt), args.ContainerID, string(action), res.ExitCode, "chat", terr.Code)
			return toolOutcome{Err: terr, Status: store.ToolFailed}
		}
	} else {
		var err error
		res, err = containerx.ContainerAction(ctx, rt, args.ContainerID, action, exec)
		if err != nil {
			terr := classifyContainerExecError(err)
			// 执行失败也落 container.action 审计（契约 9.7：含失败）。
			r.auditContainerAction(ctx, conv.ID, srv.ID, string(rt), args.ContainerID, string(action), res.ExitCode, "chat", terr.Code)
			return toolOutcome{Err: terr, Status: store.ToolFailed}
		}
	}
	r.auditContainerAction(ctx, conv.ID, srv.ID, string(rt), args.ContainerID, string(action), res.ExitCode, "chat", "")

	// 实际执行命令（六期 BE-P：result 的 command 为改写后命令，回放据此重建）。
	executedCommand := containerx.ActionCommand(rt, args.ContainerID, action)
	if editedCommand != "" {
		executedCommand = editedCommand
	}
	var approvalPtr *string
	if approvalID != "" {
		approvalPtr = &approvalID
	}
	outJSON, _ := json.Marshal(map[string]any{
		"server_id":    srv.ID,
		"container_id": args.ContainerID,
		"action":       string(action),
		"command":      executedCommand,
		"exit_code":    res.ExitCode,
		"output":       res.Combined,
		"truncated":    res.Truncated,
		"duration_ms":  res.DurationMS,
		"risk_level":   riskLevel,
		"approval_id":  approvalPtr,
	})
	return toolOutcome{Result: outJSON, Status: store.ToolSucceeded}
}

// auditContainerAction 落 container.action 审计（契约 9.7：resource_type=
// server、detail={runtime, container_id, action, exit_code, source,
// error_code?}；不含容器日志内容）。
func (r *Runner) auditContainerAction(ctx context.Context, conversationID, serverID, runtime, containerID, action string, exitCode int, source, errorCode string) {
	detail := map[string]any{
		"runtime":      runtime,
		"container_id": containerID,
		"action":       action,
		"exit_code":    exitCode,
		"source":       source,
	}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	r.auditEvent(ctx, "container.action", "model", conversationActor(conversationID), "server", serverID, &conversationID, &serverID, detail)
}

// containerRiskLevel 返回容器动作的风险级别（契约 7.2：start=safe；
// stop/restart=needs_approval；动作集封闭，forbidden 不存在于此工具）。
func containerRiskLevel(action containerx.Action) string {
	if action == containerx.ActionStart {
		return "safe"
	}
	return "needs_approval"
}
