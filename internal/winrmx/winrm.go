// Package winrmx 是 WinRM 命令执行通道（二期 W1，CONTRACT.md 5.2A）。
//
// 仅支持密码认证：password_ntlm 走 NTLM（Negotiate，HTTP 下自带签名），
// password_basic 走 Basic（明文，仅建议 HTTPS 5986；契约不强制，风险由 UI 提示）。
// 不 import sshx：TestResult/RunRequest/RunResult 在本包独立定义，agent 按
// 路由矩阵（契约 2.7）择一持有，避免包环。
//
// Execute 以 PowerShell 端点运行命令并附加 "; exit $LASTEXITCODE" 取退出码；
// 两条通道（SSH/WinRM）对同一 Windows 命令产生一致的执行语义。
package winrmx

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/masterzen/winrm"
	"golang.org/x/text/encoding/unicode"

	"aiops/internal/store"
)

// 默认与上限（CONTRACT.md 5.2A）。
const (
	defaultTimeout    = 60 * time.Second
	maxTimeout        = 300 * time.Second
	defaultMaxOutput  = int64(262144)
	maxMaxOutput      = int64(262144)
	defaultWinRMPort  = 5985
	httpsWinRMPort    = 5986
	defaultRequestTTL = 55 * time.Second // 服务端 MaxTimeoutms=60000 硬上限内留余量
)

// ErrCredentialMissing 表示目标缺少所需的凭据。
var ErrCredentialMissing = errors.New("winrmx: credential missing")

// ErrPoolClosed 表示连接池已关闭。
var ErrPoolClosed = errors.New("winrmx: pool closed")

// ErrUnsupportedAuth 表示认证方式不属于 WinRM 通道（password_ntlm/password_basic）。
var ErrUnsupportedAuth = errors.New("winrmx: unsupported auth method for the winrm channel")

// Target 是一次 WinRM 连接目标（CONTRACT.md 5.2A）。
type Target struct {
	ServerID string
	Host     string
	Port     int // winrm_port；5985(HTTP)/5986(HTTPS)
	Username string
	Password string
	UseHTTPS bool             // password_basic 且 port=5986 时由调用方置 true；其余 false
	AuthType store.AuthMethod // 仅 password_ntlm / password_basic
}

// TestResult 是连接测试结果。WinRM 无主机密钥语义：Fingerprint 恒空串、
// FirstSeen 恒 false（契约 5.2A），调用方不得因此触发 UpdateHostKey。
type TestResult struct {
	LatencyMS     int64  `json:"latency_ms"`
	ServerVersion string `json:"server_version"`
	Fingerprint   string `json:"fingerprint"`
	FirstSeen     bool   `json:"first_seen"`
}

// RunRequest 是执行请求。
type RunRequest struct {
	Command        string
	Timeout        time.Duration // 0 => 60s；最大 300s（且单请求 ≤ RequestTimeout 分片）
	MaxOutputBytes int64         // 0 => 262144；最大 262144
}

// RunResult 是执行结果。stdout/stderr 合并输出，超过上限标记 Truncated。
type RunResult struct {
	ExitCode   int    `json:"exit_code"`
	Combined   string `json:"output"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

// Executor 是 WinRM 执行器接口。方法集与 sshx.Executor 同形（少 Session：
// WinRM 无 PTY，终端会话对 WinRM 认证的服务器返回 TERM_UNSUPPORTED）。
type Executor interface {
	Test(ctx context.Context, target Target) (TestResult, error)
	Execute(ctx context.Context, target Target, req RunRequest) (RunResult, error)
	Invalidate(serverID string)
	Close() error
}

// probeCommand 是连接测试探针（契约 5.2A）：单条 PowerShell 命令返回系统信息
// ——第一行计算机名（Get-WmiObject Win32_ComputerSystem），第二行 PowerShell
// 版本（$PSVersionTable.PSVersion），后者作为 server_version 返回。
const probeCommand = `(Get-WmiObject Win32_ComputerSystem | Select-Object -ExpandProperty Name); $PSVersionTable.PSVersion.ToString()`

// encodePowerShell 构造 PowerShell 端点命令。
//
// 契约模板为 `powershell -NoProfile -NonInteractive -Command <command>; exit $LASTEXITCODE`；
// WinRM 的 Command 载荷由远端 cmd.exe 解释，<command> 内的引号会破坏 -Command
// 参数边界（且 $LASTEXITCODE 会被外层 cmd 抢先展开成字面文本）。因此按库验证过
// 的做法把脚本整体编码为 UTF-16LE Base64 经 -EncodedCommand 传递，语义与契约
// 模板一致：PowerShell 端点 + NoProfile + NonInteractive + exit $LASTEXITCODE。
func encodePowerShell(script string) string {
	encoded, err := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).
		NewEncoder().Bytes([]byte(script))
	if err != nil {
		// UTF-16 编码对任意 UTF-8 输入都不会失败；防御分支兜底为原样
		// 单字节编码，保证函数总有可用载荷。
		encoded = []byte(script)
	}
	return "powershell -NoProfile -NonInteractive -EncodedCommand " +
		base64.StdEncoding.EncodeToString(encoded)
}

// sanitizeUTF8 把远端输出清洗为合法 UTF-8：PowerShell 控制台输出可能混入
// OEM 代码页字节，直接入库会产生非法 UTF-8 字符串。
func sanitizeUTF8(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}

// runScript 在一个 WinRM shell 内执行 PowerShell 脚本并收集合并输出。
func runScript(ctx context.Context, client *winrm.Client, script string, timeout time.Duration, maxOut int64) (RunResult, error) {
	psCmd := encodePowerShell(script)
	start := time.Now()
	stdout := newCappedWriter(maxOut)
	stderr := newCappedWriter(maxOut)

	// ctx 超时由调用方约束（context.WithTimeout）；masterzen/winrm 的
	// RunWithContext 在 ctx 取消时终止接收循环并返回错误（超时时保留已读
	// 输出，与 sshx 行为一致）。
	exitCode, err := client.RunWithContext(ctx, psCmd, stdout, stderr)
	if err != nil {
		combined := sanitizeUTF8(stdout.String() + stderr.String())
		truncated := stdout.Truncated() || stderr.Truncated()
		if maxOut > 0 && int64(len(combined)) > maxOut {
			combined = combined[:maxOut]
			truncated = true
		}
		partial := RunResult{
			ExitCode:   exitCode,
			Combined:   combined,
			Truncated:  truncated,
			DurationMS: time.Since(start).Milliseconds(),
		}
		if ctx.Err() != nil {
			return partial, fmt.Errorf("winrmx: command timed out: %w", ctx.Err())
		}
		return partial, fmt.Errorf("winrmx: run command: %w", err)
	}

	combined := sanitizeUTF8(stdout.String() + stderr.String())
	truncated := stdout.Truncated() || stderr.Truncated()
	if maxOut > 0 && int64(len(combined)) > maxOut {
		combined = combined[:maxOut]
		truncated = true
	}
	return RunResult{
		ExitCode:   exitCode,
		Combined:   sanitizeUTF8(combined),
		Truncated:  truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}
