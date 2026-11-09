// loop.go 实现诊断 mini-loop（契约 2.10 约定 3 / 5.16 / 11 节 #7）：
// 模型循环 ≤6 轮，每轮模型给出一条候选命令 → risk.Classify（按目标 os；
// network_device 走 ClassifyNet）→ 仅 Level==safe 执行（needs_approval/
// forbidden 一律不执行，拒绝原因回给模型继续推理——代码强制，不写在
// 提示词里）；模型返回报告 / 轮数耗尽 / 超时 / LLM 错误即终止。
package diagnosis

import (
	"aiops/internal/cryptox"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"aiops/internal/llm"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// 契约 11 节写死的循环上限（常量；改动先改契约）。
const (
	// MaxModelRounds 固定 6：模型循环轮数上限。
	MaxModelRounds = 6
	// CommandTimeout 固定 30s：单条采集命令。
	CommandTimeout = 30 * time.Second
	// MaxOutputBytes 固定 262144：单命令输出上限（通道层）。
	MaxOutputBytes int64 = 262144
	// llmCallTimeout 是单次模型调用超时（会话 300s 之内的每次调用预算）。
	llmCallTimeout = 120 * time.Second
	// llmTextLimit 是单次模型响应聚合文本的字节上限（解析器输入限制）。
	llmTextLimit = 1 << 20 // 1MB
	// probeCommandMaxBytes 是模型候选命令的输入上限（>16KB 直接拒绝）。
	probeCommandMaxBytes = 16384
)

// 循环错误码（写入 diagnosis_sessions.error_code / 审计）。
const (
	errCodeMaxRounds = "DIAGNOSIS_MAX_ROUNDS"
	errCodeTimeout   = "DIAGNOSIS_TIMEOUT"
)

var errMaxRounds = errors.New("diagnosis: model rounds exhausted")

// loopResponse 是模型每轮输出的协议 JSON。
type loopResponse struct {
	Action  string          `json:"action"`
	Command string          `json:"command"`
	Report  json.RawMessage `json:"report"`
}

// probeResult 是引擎回给模型的伪结果（JSON 字符串，作为下一轮 user 消息）。
type probeResult struct {
	Action     string `json:"action"`
	Command    string `json:"command"`
	RiskLevel  string `json:"risk_level"`
	Executed   bool   `json:"executed"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Output     string `json:"output,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Rejected   string `json:"rejected,omitempty"`
}

// runLoop 驱动一个会话的模型循环（client/provider 由引擎解析注入，测试
// 可注入 fake）。返回固化后的报告；错误时 err 携带 error_code（loopError）。
func (e *Engine) runLoop(ctx context.Context, client llm.Client, provider store.Provider,
	sess store.DiagnosisSession, srv store.Server) (DiagnosisReport, error) {

	now := e.clock()
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: diagnosisSystemPrompt(srv)},
		{Role: llm.RoleUser, Content: buildFirstUserMessage(ctx, e.st, sess, srv, now)},
	}

	var lastReportErr error
	reportRetried := false // 报告解析失败重试一次（契约 5.16）

	for round := 0; round < MaxModelRounds; round++ {
		if ctx.Err() != nil {
			return DiagnosisReport{}, &loopError{code: errCodeTimeout, err: ctx.Err()}
		}
		text, err := streamOnce(ctx, client, provider.DefaultModel, msgs)
		if err != nil {
			return DiagnosisReport{}, asLoopError(err)
		}
		resp, perr := parseLoopResponse(text)
		if perr != nil {
			// 非协议输出：把错误反馈给模型修正（各计一次重试窗口内反馈，
			// 不单独重试——轮数即硬上限）。
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: text},
				llm.Message{Role: llm.RoleUser, Content: `{"action":"error","message":"output is not valid protocol JSON: ` +
					strings.ReplaceAll(perr.Error(), `"`, `'`) + `"}`},
			)
			continue
		}
		switch resp.Action {
		case "probe":
			result := e.runProbe(ctx, sess, srv, resp.Command)
			raw, _ := json.Marshal(result)
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: text},
				llm.Message{Role: llm.RoleUser, Content: string(raw)},
			)
		case "report":
			report, rerr := ParseReport(resp.Report)
			if rerr == nil {
				os := risk.OSLinux
				if srv.OS == store.ServerOSWindows {
					os = risk.OSWindows
				}
				report.Normalize(os)
				return report, nil
			}
			lastReportErr = rerr
			if reportRetried {
				return DiagnosisReport{}, &loopError{code: "DIAGNOSIS_REPORT_INVALID", err: rerr}
			}
			reportRetried = true
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: text},
				llm.Message{Role: llm.RoleUser, Content: `{"action":"error","message":"report invalid: ` +
					strings.ReplaceAll(rerr.Error(), `"`, `'`) + `; output the corrected report JSON again"}`},
			)
		default:
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: text},
				llm.Message{Role: llm.RoleUser, Content: `{"action":"error","message":"action must be probe or report"}`},
			)
		}
	}
	if lastReportErr != nil {
		return DiagnosisReport{}, &loopError{code: "DIAGNOSIS_REPORT_INVALID", err: lastReportErr}
	}
	return DiagnosisReport{}, &loopError{code: errCodeMaxRounds, err: errMaxRounds}
}

// runProbe 分类并（仅 safe 时）执行单条候选命令，返回伪结果。
func (e *Engine) runProbe(ctx context.Context, sess store.DiagnosisSession, srv store.Server, command string) probeResult {
	command = strings.TrimSpace(command)
	if command == "" || len(command) > probeCommandMaxBytes {
		return probeResult{Action: "probe_result", Command: command,
			RiskLevel: string(risk.LevelNeedsApproval), Rejected: "command is empty or exceeds 16KB"}
	}

	decision := classifyForServer(command, srv)
	if decision.Level != risk.LevelSafe {
		return probeResult{Action: "probe_result", Command: command,
			RiskLevel: string(decision.Level),
			Rejected:  "rejected by diagnosis engine: risk_level=" + string(decision.Level) + " (only safe read-only commands are executed); rewrite as an equivalent read-only form"}
	}

	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		// 诊断引擎只有 ssh/winrm 通道（契约 5.16 构造签名）；网络设备
		// CLI 通道不在此装配——如实告知模型，不臆造执行结果。
		return probeResult{Action: "probe_result", Command: command,
			RiskLevel: string(decision.Level), Executed: false,
			Rejected: "no execution channel for network devices in diagnosis engine"}
	}

	execCtx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	started := e.clock()
	combined, exitCode, truncated, err := e.execCommand(execCtx, srv, command)
	dur := e.clock().Sub(started).Milliseconds()

	e.auditCommand(sess, srv, command, exitCode, dur, err)

	result := probeResult{Action: "probe_result", Command: command,
		RiskLevel: string(decision.Level), Executed: err == nil,
		Truncated: truncated, DurationMS: dur}
	if err != nil {
		result.Rejected = "execution failed: " + err.Error()
		return result
	}
	result.ExitCode = exitCode
	result.Output = truncateBytes(combined, int(MaxOutputBytes))
	return result
}

// classifyForServer 按目标分册分类（契约 5.16：network_device 走 ClassifyNet）。
func classifyForServer(command string, srv store.Server) risk.Decision {
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		brand := risk.NetBrandGeneric
		switch srv.NetdevBrand {
		case store.NetdevBrandCisco:
			brand = risk.NetBrandCisco
		case store.NetdevBrandHuawei:
			brand = risk.NetBrandHuawei
		case store.NetdevBrandH3C:
			brand = risk.NetBrandH3C
		case store.NetdevBrandJunos:
			brand = risk.NetBrandJunos
		}
		return risk.ClassifyNet(command, brand)
	}
	if srv.OS == store.ServerOSWindows {
		return risk.Classify(command, risk.OSWindows)
	}
	return risk.Classify(command, risk.OSLinux)
}

// streamOnce 聚合一次流式调用的完整文本（无工具定义——诊断协议走纯文本
// JSON；聚合文本受 llmTextLimit 约束，超限即错误终止）。
func streamOnce(ctx context.Context, client llm.Client, model string, msgs []llm.Message) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	var b strings.Builder
	err := client.Stream(callCtx, llm.Request{Model: model, Messages: msgs}, func(ev llm.StreamEvent) error {
		switch ev.Type {
		case llm.EventToken:
			b.WriteString(ev.Delta)
			if b.Len() > llmTextLimit {
				return errors.New("diagnosis: model output exceeds 1MB")
			}
		case llm.EventToolCall:
			return errors.New("diagnosis: unexpected tool call in diagnosis loop")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

// parseLoopResponse 解析模型轮输出：先整串严格解析；失败时提取首个平衡
// 大括号段再试（模型常带 markdown 围栏）。输入超限已由 streamOnce 保证。
func parseLoopResponse(text string) (loopResponse, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return loopResponse{}, errors.New("empty output")
	}
	if r, err := decodeLoopJSON([]byte(text)); err == nil {
		return r, nil
	}
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return loopResponse{}, errors.New("no JSON object found")
	}
	depth := 0
	inStr, escape := false, false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inStr {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return decodeLoopJSON([]byte(text[start : i+1]))
			}
		}
	}
	return loopResponse{}, errors.New("unbalanced JSON object")
}

func decodeLoopJSON(raw []byte) (loopResponse, error) {
	var r loopResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return loopResponse{}, err
	}
	return r, nil
}

// loopError 携带 error_code 的循环错误。
type loopError struct {
	code string
	err  error
}

func (le *loopError) Error() string { return le.code + ": " + le.err.Error() }
func (le *loopError) Unwrap() error { return le.err }

// asLoopError 把 LLM/ctx 错误映射为带码错误。
func asLoopError(err error) error {
	if err == nil {
		return nil
	}
	var le *loopError
	if errors.As(err, &le) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &loopError{code: errCodeTimeout, err: err}
	}
	return &loopError{code: "LLM_UPSTREAM_ERROR", err: err}
}

// ---------------------------------------------------------------------------
// 执行通道（契约 2.7 双轨路由；同 monitor collector 的模式——diagnosis 不
// import monitor，各持一份）
// ---------------------------------------------------------------------------

func (e *Engine) execCommand(ctx context.Context, srv store.Server, command string) (string, int, bool, error) {
	if srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic {
		if e.winrm == nil {
			return "", 0, false, errors.New("winrm executor not configured")
		}
		target, err := e.winrmTarget(srv)
		if err != nil {
			return "", 0, false, err
		}
		out, err := e.winrm.Execute(ctx, target, winrmx.RunRequest{
			Command: command, Timeout: CommandTimeout, MaxOutputBytes: MaxOutputBytes,
		})
		if err != nil {
			return "", 0, false, err
		}
		return out.Combined, out.ExitCode, out.Truncated, nil
	}
	if e.ssh == nil {
		return "", 0, false, errors.New("ssh executor not configured")
	}
	target, err := e.sshTarget(srv)
	if err != nil {
		return "", 0, false, err
	}
	execCommand := command
	if srv.OS == store.ServerOSWindows {
		execCommand = "powershell -NoProfile -NonInteractive -Command " + command
	}
	out, err := e.ssh.Execute(ctx, target, sshx.RunRequest{
		Command: execCommand, Timeout: CommandTimeout, MaxOutputBytes: MaxOutputBytes,
	})
	if err != nil {
		return "", 0, false, err
	}
	return out.Combined, out.ExitCode, out.Truncated, nil
}

func (e *Engine) winrmTarget(srv store.Server) (winrmx.Target, error) {
	if srv.AuthMethod != store.AuthMethodPasswordNTLM && srv.AuthMethod != store.AuthMethodPasswordBasic {
		return winrmx.Target{}, errors.New("diagnosis: not a winrm auth method")
	}
	pw, err := decrypt(e.cipher, srv.PasswordEncrypted)
	if err != nil {
		return winrmx.Target{}, errors.New("diagnosis: decrypt password failed")
	}
	return winrmx.Target{
		ServerID: srv.ID, Host: srv.Host, Port: srv.WinRMPort, Username: srv.Username,
		Password: pw, UseHTTPS: srv.AuthMethod == store.AuthMethodPasswordBasic && srv.WinRMPort == 5986,
		AuthType: srv.AuthMethod,
	}, nil
}

func (e *Engine) sshTarget(srv store.Server) (sshx.Target, error) {
	target := sshx.Target{
		ServerID: srv.ID, Host: srv.Host, Port: srv.Port, Username: srv.Username,
		AuthMethod: srv.AuthMethod, KnownHostKey: srv.HostKeyPublic,
	}
	switch srv.AuthMethod {
	case store.AuthMethodPassword:
		pw, err := decrypt(e.cipher, srv.PasswordEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("diagnosis: decrypt password failed")
		}
		target.Password = pw
	case store.AuthMethodPrivateKey:
		key, err := decrypt(e.cipher, srv.PrivateKeyEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("diagnosis: decrypt private key failed")
		}
		passphrase, err := decrypt(e.cipher, srv.PassphraseEncrypted)
		if err != nil {
			return sshx.Target{}, errors.New("diagnosis: decrypt passphrase failed")
		}
		target.PrivateKey = key
		target.Passphrase = passphrase
	default:
		return sshx.Target{}, errors.New("diagnosis: unsupported auth method")
	}
	return target, nil
}

// decrypt 带 nil 防护的解密：cipher 未注入时返回错误而非 panic
// （五期评审：agent/batchx 的同名辅助有防护，monitor/diagnosis/inspection
// 曾直接调用，nil 接收者会在采集/诊断/巡检 goroutine 内 panic 杀死进程）。
func decrypt(cipher *cryptox.Cipher, ciphertext string) (string, error) {
	// 空串短路（六期打磨 sprint 4，走查 high）：passphrase 是私钥认证的
	// 可选字段（openapi 4.3：私钥认证必须提供 private_key，passphrase
	// 可选），未填时库里存空串。cryptox.Decrypt("") 返回
	// ErrInvalidCiphertext，此前把「没有口令」误判成解密失败——私钥认证
	// 服务器的 AI 诊断整条路径失败。agent/batchx 的同名辅助一直有该
	// 短路，此处对齐。
	if ciphertext == "" {
		return "", nil
	}
	if cipher == nil {
		return "", errors.New("diagnosis: cipher unavailable")
	}
	return cipher.Decrypt(ciphertext)
}
