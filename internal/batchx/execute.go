// execute.go 实现批量执行通道与审批回填（契约 5.18）：
//   - OnApprovalDecided：source=batch 审批 decide 后的执行回填入口
//     （approve：逐字节执行 approval.command → FinishResult(executed|failed)；
//     deny：FinishResult(denied)；expired 由过期扫描收敛）
//   - executeCommand：按契约 2.7 双轨路由（WinRM 走 PowerShell 端点、SSH
//     保持登录 shell/Windows 包装外壳、网络设备经注入的 ExecFunc）
//   - classifyFor：逐台 risk 分类（网络设备按厂商分册 ClassifyNet）
//   - expireLoop：每 30s 扫描 awaiting_approval 台，审批已终态时收敛 result
package batchx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"aiops/internal/audit"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// OnApprovalDecided 是 source=batch 审批 decide 后的执行回填入口
// （契约 5.18；decide handler 在 approvals.Decide 成功后同步调用）。
// 非 batch 审批直接返回 nil（防御；handler 只对 batch 审批调用）。
func (s *Service) OnApprovalDecided(ctx context.Context, approval store.Approval, approve bool) error {
	if approval.Source != "batch" {
		return nil
	}
	result, err := s.st.BatchJobs().ByApproval(ctx, approval.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	// result 已被取消/过期（batch Cancel 或过期扫描先到）：不再执行。
	if result.Status != store.BatchResultAwaitingApproval {
		return nil
	}
	if !approve {
		if _, err := s.st.BatchJobs().FinishResult(ctx, store.FinishBatchResultParams{
			ID: result.ID, Status: store.BatchResultDenied, FinishedAt: s.now(),
		}); err != nil {
			return err
		}
		sid := result.ServerID
		s.emitAudit(ctx, audit.Event{ActorType: "user", ActorID: approvalDecidedBy(approval)}, audit.Event{
			Action: actionCmdDenied, ResourceType: "server", ResourceID: result.ServerID,
			ServerID: &sid,
			// 契约 9.7（六期打磨 sprint 5）：拒绝行必须能在审计页复原
			//「拒的是哪条命令」——detail 带 command（审批创建时的原命令，
			// 拒绝不执行改写）。此前只有 approval_id + batch_job_id，
			// 审计页「命令摘要」恒「—」且展开无完整命令。
			Detail: map[string]any{
				"approval_id": approval.ID, "batch_job_id": result.BatchJobID,
				"command": approval.Command, "risk_level": approval.RiskLevel,
			},
		})
		_, err = s.RecalcJobStatus(ctx, result.BatchJobID)
		return err
	}

	// approve：逐字节执行改写后命令（契约 2.11 约定 3：decide 携带
	// edited_command 时经 Service.Decide 重分类并落库，执行取该值；为空
	// 回退原始 command——六期评审发现批量路径曾忽略用户改写）。
	execCommand := approval.Command
	if approval.EditedCommand != "" {
		execCommand = approval.EditedCommand
	}
	// 契约 9.7：批量审批复用 command.approved（detail 加 batch_job_id）。
	// 该行由本包写而非 approvals.Service——只有这里知道 batch_job_id；
	// approvals 对 source=batch 已跳过同名动作，一次批准只落一行。
	sid := result.ServerID
	approvedDetail := map[string]any{
		"approval_id": approval.ID, "batch_job_id": result.BatchJobID,
		"source": "batch", "risk_level": approval.RiskLevel,
		"approval_mode": string(approval.ApprovalMode), "command": approval.Command,
	}
	if approval.EditedCommand != "" {
		approvedDetail["edited_command"] = approval.EditedCommand
		approvedDetail["original_command"] = approval.Command
		approvedDetail["original_risk_level"] = approval.RiskLevel
		approvedDetail["edited_risk_level"] = approval.EditedRiskLevel
	}
	s.emitAudit(ctx, audit.Event{ActorType: "user", ActorID: approvalDecidedBy(approval)}, audit.Event{
		Action: actionCmdApproved, ResourceType: "server", ResourceID: sid,
		ServerID: &sid,
		Detail:   approvedDetail,
	})
	var out execOutcome
	if approval.ServerID == nil || *approval.ServerID == "" {
		out = execOutcome{ErrorCode: errCodeServerNotFound}
	} else if srv, err := s.st.Servers().Get(ctx, *approval.ServerID); err != nil {
		out = execOutcome{ErrorCode: errCodeServerNotFound}
	} else {
		out = s.executeCommand(srv, execCommand)
	}
	status := store.BatchResultExecuted
	if out.ErrorCode != "" {
		status = store.BatchResultFailed
	}
	// 命令已真实执行，结果必须落库：此前沿用 HTTP 请求 ctx，客户端在
	// 执行窗口断开时回填失败，30s 后被过期扫描误标 EXEC_INTERRUPTED
	//（状态与事实背离——五期评审发现）。改用不受请求取消影响的 ctx。
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer writeCancel()
	fr, err := s.st.BatchJobs().FinishResult(writeCtx, store.FinishBatchResultParams{
		ID: result.ID, Status: status, ExitCode: out.ExitCode, Output: out.Output,
		Truncated: out.Truncated, ErrorCode: out.ErrorCode, DurationMS: out.DurationMS,
		FinishedAt: s.now(),
	})
	if errors.Is(err, store.ErrNotFound) {
		// 状态守卫未命中：行已被 Cancel（cancelled）或过期扫描收敛——
		// 命令已执行的事实只进审计，不再覆盖终态（五期评审语义）。
		fr = store.BatchResult{}
	} else if err != nil {
		return err
	}
	_ = fr
	if status == store.BatchResultExecuted {
		sid := result.ServerID
		s.emitAudit(ctx, audit.Event{ActorType: "user", ActorID: approvalDecidedBy(approval)}, audit.Event{
			Action: actionCmdExecuted, ResourceType: "server", ResourceID: result.ServerID,
			ServerID: &sid,
			Detail: map[string]any{
				"approval_id": approval.ID, "batch_job_id": result.BatchJobID,
				"exit_code": derefInt(out.ExitCode), "risk_level": approval.RiskLevel,
				// 契约 9.7（六期打磨 sprint 3）：command.executed 携带实际
				// 执行命令文本（改写时为用户改写后的 execCommand）——审批
				// 执行行同样必须能在审计页复原「跑的是什么」。
				"command": execCommand,
			},
		})
	}
	_, err = s.RecalcJobStatus(writeCtx, result.BatchJobID)
	return err
}

func approvalDecidedBy(ap store.Approval) string {
	if ap.DecidedBy == nil {
		return ""
	}
	return *ap.DecidedBy
}

// execOutcome 是 executeCommand 的统一结果（ErrorCode 空=成功；status 由
// 调用方按 ErrorCode 派生 executed|failed）。
type execOutcome struct {
	ExitCode   *int
	Output     string
	Truncated  bool
	ErrorCode  string
	DurationMS int64
}

// executeCommand 按契约 2.7 双轨路由执行单台命令；执行段受 ExecTimeout
// （60s）硬限制，输出经通道层截断到 MaxOutput（256KB）。
func (s *Service) executeCommand(srv store.Server, command string) execOutcome {
	execCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ExecTimeout)
	defer cancel()

	start := s.now()
	if isNetworkDevice(srv) {
		if s.netExec == nil {
			return execOutcome{ErrorCode: errCodeNetCLIExecFailed, DurationMS: msSince(s, start)}
		}
		exec, err := s.netExec(srv)
		if err != nil {
			return execOutcome{ErrorCode: errCodeNetCLIExecFailed, DurationMS: msSince(s, start)}
		}
		res, err := exec(execCtx, command, s.cfg.ExecTimeout)
		if err != nil {
			return execOutcome{ErrorCode: netExecError(err), DurationMS: msSince(s, start)}
		}
		return execOutcome{
			ExitCode: &res.ExitCode, Output: res.Combined, Truncated: res.Truncated,
			DurationMS: res.DurationMS,
		}
	}
	if winRMChannel(srv) {
		if s.winrm == nil {
			return execOutcome{ErrorCode: errCodeSSHExecFailed, DurationMS: msSince(s, start)}
		}
		target, err := s.winRMTarget(srv)
		if err != nil {
			return execOutcome{ErrorCode: errCodeSSHExecFailed, DurationMS: msSince(s, start)}
		}
		res, err := s.winrm.Execute(execCtx, target, winrmx.RunRequest{
			Command: command, Timeout: s.cfg.ExecTimeout, MaxOutputBytes: s.cfg.MaxOutput,
		})
		if err != nil {
			return execOutcome{ErrorCode: channelError(execCtx, err), DurationMS: msSince(s, start)}
		}
		return execOutcome{
			ExitCode: &res.ExitCode, Output: res.Combined, Truncated: res.Truncated,
			DurationMS: res.DurationMS,
		}
	}
	if s.ssh == nil {
		return execOutcome{ErrorCode: errCodeSSHExecFailed, DurationMS: msSince(s, start)}
	}
	target, err := s.sshTarget(srv)
	if err != nil {
		return execOutcome{ErrorCode: errCodeSSHExecFailed, DurationMS: msSince(s, start)}
	}
	execCommand := command
	if srv.OS == store.ServerOSWindows {
		// SSH 通道执行 Windows 命令时包装 PowerShell 外壳（契约 2.7.2：
		// 包装发生在调用方；风险分类在包装前完成——Dispatch 已按原始命令分类）。
		execCommand = "powershell -NoProfile -NonInteractive -Command " + command
	}
	res, err := s.ssh.Execute(execCtx, target, sshx.RunRequest{
		Command: execCommand, Timeout: s.cfg.ExecTimeout, MaxOutputBytes: s.cfg.MaxOutput,
	})
	if err != nil {
		return execOutcome{ErrorCode: channelError(execCtx, err), DurationMS: msSince(s, start)}
	}
	return execOutcome{
		ExitCode: &res.ExitCode, Output: res.Combined, Truncated: res.Truncated,
		DurationMS: res.DurationMS,
	}
}

// channelError 把 SSH/WinRM 通道错误映射为错误码（与 agent 同口径；
// batchx 不 import agent，本包内实现）。
func channelError(ctx context.Context, err error) string {
	var hostKeyErr *sshx.HostKeyError
	switch {
	case errors.As(err, &hostKeyErr):
		return errCodeSSHHostKeyChanged
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() == context.DeadlineExceeded:
		return errCodeSSHTimeout
	default:
		return errCodeSSHExecFailed
	}
}

// netExecError 把注入通道的网络设备错误映射为错误码（batchx 不 import
// netdev，无法区分登录/执行失败，统一 NETCLI_EXEC_FAILED）。
func netExecError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return errCodeSSHTimeout
	}
	return errCodeNetCLIExecFailed
}

// classifyFor 逐台分类（契约 5.18：网络设备按 ClassifyNet 厂商分册，
// 其余按 os 分册）。
func classifyFor(srv store.Server, command string) risk.Decision {
	if isNetworkDevice(srv) {
		return risk.ClassifyNet(command, netBrandOf(srv.NetdevBrand))
	}
	return risk.Classify(command, riskOS(srv))
}

func riskOS(srv store.Server) risk.OS {
	switch srv.OS {
	case store.ServerOSWindows:
		return risk.OSWindows
	default:
		return risk.OSLinux
	}
}

func netBrandOf(b store.NetdevBrand) risk.NetBrand {
	switch b {
	case store.NetdevBrandCisco:
		return risk.NetBrandCisco
	case store.NetdevBrandHuawei:
		return risk.NetBrandHuawei
	case store.NetdevBrandH3C:
		return risk.NetBrandH3C
	case store.NetdevBrandJunos:
		return risk.NetBrandJunos
	default:
		return risk.NetBrandGeneric
	}
}

func isNetworkDevice(srv store.Server) bool {
	return srv.DeviceKind == store.DeviceKindNetworkDevice
}

// winRMChannel 判断服务器是否走 WinRM 通道（契约 2.7：仅 Windows +
// WinRM 认证；与 agent 同款判定，batchx 不 import agent）。
func winRMChannel(srv store.Server) bool {
	return srv.OS == store.ServerOSWindows &&
		(srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic)
}

// sshTarget 解密凭据并构造 SSH 连接目标（契约 2.7；与 agent/httpapi 同款）。
func (s *Service) sshTarget(srv store.Server) (sshx.Target, error) {
	target := sshx.Target{
		ServerID:     srv.ID,
		Host:         srv.Host,
		Port:         srv.Port,
		Username:     srv.Username,
		AuthMethod:   srv.AuthMethod,
		KnownHostKey: srv.HostKeyPublic,
	}
	switch srv.AuthMethod {
	case store.AuthMethodPassword:
		pw, err := s.decrypt(srv.PasswordEncrypted)
		if err != nil {
			return sshx.Target{}, err
		}
		target.Password = pw
	case store.AuthMethodPrivateKey:
		key, err := s.decrypt(srv.PrivateKeyEncrypted)
		if err != nil {
			return sshx.Target{}, err
		}
		passphrase, err := s.decrypt(srv.PassphraseEncrypted)
		if err != nil {
			return sshx.Target{}, err
		}
		target.PrivateKey = key
		target.Passphrase = passphrase
	default:
		return sshx.Target{}, errors.New("batchx: unsupported auth method")
	}
	return target, nil
}

// winRMTarget 解密凭据并构造 WinRM 连接目标（契约 5.2A）。
func (s *Service) winRMTarget(srv store.Server) (winrmx.Target, error) {
	if srv.AuthMethod != store.AuthMethodPasswordNTLM && srv.AuthMethod != store.AuthMethodPasswordBasic {
		return winrmx.Target{}, errors.New("batchx: not a winrm auth method")
	}
	pw, err := s.decrypt(srv.PasswordEncrypted)
	if err != nil {
		return winrmx.Target{}, err
	}
	return winrmx.Target{
		ServerID: srv.ID,
		Host:     srv.Host,
		Port:     srv.WinRMPort,
		Username: srv.Username,
		Password: pw,
		UseHTTPS: srv.AuthMethod == store.AuthMethodPasswordBasic && srv.WinRMPort == 5986,
		AuthType: srv.AuthMethod,
	}, nil
}

func (s *Service) decrypt(enc string) (string, error) {
	if s.cipher == nil {
		return "", errors.New("batchx: cipher unavailable")
	}
	if enc == "" {
		return "", nil
	}
	return s.cipher.Decrypt(enc)
}

func msSince(s *Service, start time.Time) int64 {
	return int64(s.now().Sub(start).Milliseconds())
}

// expireLoop 每 30s 收敛 awaiting_approval 台（契约 5.18：审批超时由
// approvals sweeper 置 expired，本扫描把对应 result 同步置 expired 并
// 重算 job；decide 执行中断丢失的台兜底标记，保证 job 终态收敛）。
func (s *Service) expireLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(expireScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			if err := s.expireScanOnce(ctx); err != nil {
				s.log.Warn("batch_expire_scan_failed", "error", err.Error())
			}
			cancel()
		}
	}
}

// expireScanOnce 单轮扫描：最近 expireScanMaxJobs 个 job 中 status 非终态
// 的 job，其 awaiting_approval 台审批已终态时收敛 result（上限
// expireScanMaxResult 条，防 OOM）。
func (s *Service) expireScanOnce(ctx context.Context) error {
	if s.ap == nil {
		return nil
	}
	jobs, err := s.st.BatchJobs().List(ctx, expireScanMaxJobs)
	if err != nil {
		return err
	}
	checked := 0
	recalc := make(map[string]struct{})
	for _, job := range jobs {
		if isBatchTerminal(job.Status) {
			continue
		}
		_, results, err := s.st.BatchJobs().Get(ctx, job.ID)
		if err != nil {
			continue
		}
		for _, r := range results {
			if r.Status != store.BatchResultAwaitingApproval || r.ApprovalID == nil {
				continue
			}
			checked++
			if checked > expireScanMaxResult {
				return fmt.Errorf("batchx: expire scan result budget exhausted")
			}
			ap, err := s.ap.Get(ctx, *r.ApprovalID)
			if err != nil {
				continue
			}
			var status store.BatchResultStatus
			switch ap.Status {
			case store.ApprovalExpired:
				status = store.BatchResultExpired
			case store.ApprovalDenied:
				// decide handler 调用 OnApprovalDecided 失败/进程重启丢失的兜底。
				status = store.BatchResultDenied
			case store.ApprovalApproved:
				// 批准后执行未落库（进程崩溃等）：审批已消费，不可静默重放——
				// 标记失败让 job 收敛（契约 2.10 约定 5）。
				status = store.BatchResultFailed
			default:
				continue
			}
			errorCode := ""
			if status == store.BatchResultFailed {
				errorCode = errCodeExecInterrupted
			}
			if _, err := s.st.BatchJobs().FinishResult(ctx, store.FinishBatchResultParams{
				ID: r.ID, Status: status, ErrorCode: errorCode, FinishedAt: s.now(),
			}); err == nil {
				recalc[r.BatchJobID] = struct{}{}
			}
		}
	}
	for jobID := range recalc {
		if _, err := s.RecalcJobStatus(ctx, jobID); err != nil {
			s.log.Warn("batch_expire_recalc_failed", "job_id", jobID, "error", err.Error())
		}
	}
	return nil
}
