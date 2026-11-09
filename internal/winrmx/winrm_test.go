// winrm_test.go 用本地 fake WinRM SOAP 端点（httptest）验证执行器行为：
// Basic/NTLM 参数、PowerShell 包装与 "; exit $LASTEXITCODE"、退出码透传、
// 256KB 截断、UTF-8 清洗、超时与 Invalidate（CONTRACT.md G17 的 winrmx 部分）。
//
// NTLM 的完整 HTTP 协商（401 challenge + 消息封包）由 masterzen/winrm 内部
// 的 NTLM transport 负责，此处以参数构造断言覆盖（makeParameters），端到端
// SOAP 流程经 Basic 通道验证——两条通道共用同一 SOAP/命令管线。
package winrmx

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/masterzen/winrm"

	"aiops/internal/store"
)

// fakeWinRM 是最小 WinRM SOAP 端点：按 a:Action 分发，可配置输出与退出码。
type fakeWinRM struct {
	mu sync.Mutex
	// 每个请求的 Authorization 头。
	authHeaders []string
	// Command 请求解码出的 PowerShell 脚本（UTF-16LE 解码后）。
	scripts []string
	// commands 记录原始命令行（powershell ... -EncodedCommand <b64>）。
	commands []string

	stdout   string // stdout 流内容（原样，测试自行放非法 UTF-8）
	stderr   string
	exitCode int
	// running 模式：Receive 永远返回 Running（不 Done），用于超时测试。
	running bool
}

var actionRe = regexp.MustCompile(`<a:Action[^>]*>([^<]+)</a:Action>`)
var commandRe = regexp.MustCompile(`<rsp:Command><!\[CDATA\[(.*?)\]\]></rsp:Command>`)

const (
	soapCT = "application/soap+xml;charset=UTF-8"

	shellID   = "67A74734-DD32-4F10-89DE-49A060483810"
	commandID = "1A6DEE6B-EC68-4DD6-87E9-030C0048ECC4"

	createShellResponse = `<s:Envelope xml:lang="en-US" xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:x="http://schemas.xmlsoap.org/ws/2004/09/transfer" xmlns:w="http://schemas.dmtf.org/wbem/wsman/1/wsman.xsd" xmlns:rsp="http://schemas.microsoft.com/wbem/wsman/1/windows/shell"><s:Header><a:Action>http://schemas.xmlsoap.org/ws/2004/09/transfer/CreateResponse</a:Action><a:MessageID>uuid:195078CF-804B-41F7-A246-9CB3C1A41A9A</a:MessageID><a:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:To><a:RelatesTo>uuid:D00059E8-57D6-4035-AD8D-3EDC495DA163</a:RelatesTo></s:Header><s:Body><x:ResourceCreated><a:Address>http://127.0.0.1:5985/wsman</a:Address><a:ReferenceParameters><w:ResourceURI>http://schemas.microsoft.com/wbem/wsman/1/windows/shell/cmd</w:ResourceURI><w:SelectorSet><w:Selector Name="ShellId">` + shellID + `</w:Selector></w:SelectorSet></a:ReferenceParameters></x:ResourceCreated></s:Body></s:Envelope>`

	executeCommandResponse = `<s:Envelope xml:lang="en-US" xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:rsp="http://schemas.microsoft.com/wbem/wsman/1/windows/shell"><s:Header><a:Action>http://schemas.microsoft.com/wbem/wsman/1/windows/shell/CommandResponse</a:Action><a:MessageID>uuid:D9E108AA-E32B-45E3-8601-E9C70999D3BA</a:MessageID><a:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:To><a:RelatesTo>uuid:F530804C-6D02-4FA9-AE78-1997750594BA</a:RelatesTo></s:Header><s:Body><rsp:CommandResponse><rsp:CommandId>` + commandID + `</rsp:CommandId></rsp:CommandResponse></s:Body></s:Envelope>`
)

func receiveResponse(stdout, stderr string, exitCode int, done bool) string {
	state := "http://schemas.microsoft.com/wbem/wsman/1/windows/shell/CommandState/Running"
	closing := ""
	if done {
		state = "http://schemas.microsoft.com/wbem/wsman/1/windows/shell/CommandState/Done"
		closing = fmt.Sprintf("<rsp:ExitCode>%d</rsp:ExitCode>", exitCode)
	}
	streams := ""
	if stdout != "" {
		streams += fmt.Sprintf(`<rsp:Stream Name="stdout" CommandId="%s">%s</rsp:Stream>`, commandID, stdout)
	}
	if stderr != "" {
		streams += fmt.Sprintf(`<rsp:Stream Name="stderr" CommandId="%s">%s</rsp:Stream>`, commandID, stderr)
	}
	return fmt.Sprintf(`<s:Envelope xml:lang="en-US" xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:rsp="http://schemas.microsoft.com/wbem/wsman/1/windows/shell"><s:Header><a:Action>http://schemas.microsoft.com/wbem/wsman/1/windows/shell/ReceiveResponse</a:Action><a:MessageID>uuid:AAD46BD4-6315-4C3C-93D4-94A55773287D</a:MessageID><a:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:To><a:RelatesTo>uuid:18A52A06-9027-41DC-8850-3F244595AF62</a:RelatesTo></s:Header><s:Body><rsp:ReceiveResponse>%s<rsp:CommandState CommandId="%s" State="%s">%s</rsp:CommandState></rsp:ReceiveResponse></s:Body></s:Envelope>`,
		streams, commandID, state, closing)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// decodeEncodedCommand 从 `powershell ... -EncodedCommand <b64>` 还原
// UTF-16LE 脚本的 ASCII 视图（高字节丢弃，足够断言脚本内容）。
func decodeEncodedCommand(cmdline string) string {
	idx := strings.LastIndex(cmdline, "-EncodedCommand ")
	if idx < 0 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cmdline[idx+len("-EncodedCommand "):]))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for i := 0; i+1 < len(raw); i += 2 {
		sb.WriteByte(raw[i])
	}
	return sb.String()
}

func (f *fakeWinRM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	f.mu.Unlock()

	m := actionRe.FindSubmatch(body)
	if m == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	action := string(m[1])
	switch action {
	case "http://schemas.xmlsoap.org/ws/2004/09/transfer/Create":
		w.Header().Set("Content-Type", soapCT)
		_, _ = w.Write([]byte(createShellResponse))
	case "http://schemas.microsoft.com/wbem/wsman/1/windows/shell/Command":
		cm := commandRe.FindSubmatch(body)
		if cm != nil {
			f.mu.Lock()
			f.commands = append(f.commands, string(cm[1]))
			f.scripts = append(f.scripts, decodeEncodedCommand(string(cm[1])))
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", soapCT)
		_, _ = w.Write([]byte(executeCommandResponse))
	case "http://schemas.microsoft.com/wbem/wsman/1/windows/shell/Receive":
		f.mu.Lock()
		out, errB, code, running := f.stdout, f.stderr, f.exitCode, f.running
		f.mu.Unlock()
		w.Header().Set("Content-Type", soapCT)
		if running {
			_, _ = w.Write([]byte(receiveResponse("", "", 0, false)))
			return
		}
		_, _ = w.Write([]byte(receiveResponse(out, errB, code, true)))
	case "http://schemas.xmlsoap.org/ws/2004/09/transfer/Delete",
		"http://schemas.microsoft.com/wbem/wsman/1/windows/shell/Signal":
		w.Header().Set("Content-Type", soapCT)
		_, _ = w.Write([]byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"/>`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeWinRM) recorded() (auths []string, commands []string, scripts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out1 := append([]string(nil), f.authHeaders...)
	out2 := append([]string(nil), f.commands...)
	out3 := append([]string(nil), f.scripts...)
	return out1, out2, out3
}

// startFakeWinRM 启动 fake SOAP 端点并返回目标（host/port 从 URL 提取）。
func startFakeWinRM(t *testing.T, fake *fakeWinRM) (*httptest.Server, Target) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	idx := strings.LastIndex(host, ":")
	target := Target{
		ServerID: "srv-test",
		Host:     host[:idx],
		Port:     atoiOr(host[idx+1:], 5985),
		Username: "Administrator",
		Password: "s3cret!",
		AuthType: store.AuthMethodPasswordBasic,
	}
	return srv, target
}

func atoiOr(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}

func newTestPool() *Pool {
	return New(Config{IdleTimeout: time.Hour}, nil)
}

// --- 参数构造（NTLM/Basic） -------------------------------------------------

// TestMakeParametersNTLMVsBasic 验证认证参数路由：NTLM 装配 ClientNTLM
// transport，Basic 走默认 transport（库对每个请求 SetBasicAuth）。
func TestMakeParametersNTLMVsBasic(t *testing.T) {
	p := newTestPool()
	ntlmParams := p.makeParameters(store.AuthMethodPasswordNTLM)
	if ntlmParams.TransportDecorator == nil {
		t.Fatal("password_ntlm must install the NTLM transport decorator")
	}
	if tr := ntlmParams.TransportDecorator(); tr == nil {
		t.Fatal("NTLM transport decorator must produce a transporter")
	}
	basicParams := p.makeParameters(store.AuthMethodPasswordBasic)
	if basicParams.TransportDecorator != nil {
		t.Fatal("password_basic must use the default (basic-auth) transport")
	}
	if p.makeParameters(store.AuthMethodPasswordNTLM).Timeout != "PT55S" {
		t.Fatalf("OperationTimeout must default to PT55S, got %q", p.makeParameters(store.AuthMethodPasswordNTLM).Timeout)
	}
}

// TestBuildClientUnsupportedAuth 验证非 WinRM 认证方式被拒绝。
func TestBuildClientUnsupportedAuth(t *testing.T) {
	p := newTestPool()
	for _, am := range []store.AuthMethod{store.AuthMethodPassword, store.AuthMethodPrivateKey} {
		if _, err := p.buildClient(Target{AuthType: am, Host: "h", Username: "u"}); err == nil {
			t.Fatalf("auth %s must be rejected", am)
		}
	}
	if _, err := p.buildClient(Target{AuthType: store.AuthMethodPasswordNTLM}); err == nil {
		t.Fatal("missing host/username must be rejected")
	}
}

// --- 端到端（Basic 通道） ---------------------------------------------------

// TestExecuteBasicAuthAndOutput 验证 Basic 凭据随每个 SOAP 请求发送，
// stdout/stderr 合并返回、退出码透传。
func TestExecuteBasicAuthAndOutput(t *testing.T) {
	fake := &fakeWinRM{
		stdout:   b64("WIN-ABC-123\n"),
		stderr:   b64("warn: something\n"),
		exitCode: 0,
	}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	res, err := p.Execute(context.Background(), target, RunRequest{Command: "Get-Service Spooler"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Combined, "WIN-ABC-123") || !strings.Contains(res.Combined, "warn: something") {
		t.Fatalf("combined output missing streams: %q", res.Combined)
	}
	if res.Truncated {
		t.Fatal("must not be truncated")
	}

	auths, _, _ := fake.recorded()
	if len(auths) == 0 {
		t.Fatal("no requests recorded")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("Administrator:s3cret!"))
	for i, a := range auths {
		if a != want {
			t.Fatalf("request %d authorization = %q, want %q", i, a, want)
		}
	}
}

// TestExecutePowershellWrapperAndLASTEXITCODE 验证命令以 PowerShell 端点
// 包装执行且脚本带 "; exit $LASTEXITCODE"（契约 5.2A）。
func TestExecutePowershellWrapperAndLASTEXITCODE(t *testing.T) {
	fake := &fakeWinRM{exitCode: 42}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	res, err := p.Execute(context.Background(), target, RunRequest{Command: "ping -n 1 127.0.0.1"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("exit code = %d, want 42 (fake-provided)", res.ExitCode)
	}
	_, commands, scripts := fake.recorded()
	if len(commands) == 0 {
		t.Fatal("no command recorded")
	}
	if !strings.HasPrefix(commands[0], "powershell -NoProfile -NonInteractive -EncodedCommand ") {
		t.Fatalf("command not wrapped as PowerShell endpoint: %q", commands[0])
	}
	if !strings.HasSuffix(scripts[0], "ping -n 1 127.0.0.1; exit $LASTEXITCODE") {
		t.Fatalf("script must append '; exit $LASTEXITCODE', got %q", scripts[0])
	}
}

// TestExecuteTruncation 验证 stdout/stderr 合并后的 256KB 截断。
func TestExecuteTruncation(t *testing.T) {
	big := strings.Repeat("A", 200*1024)
	fake := &fakeWinRM{stdout: b64(big), stderr: b64(big), exitCode: 0}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	res, err := p.Execute(context.Background(), target, RunRequest{Command: "Get-ChildItem C:\\logs"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.Truncated {
		t.Fatal("output must be marked truncated")
	}
	if int64(len(res.Combined)) > 262144 {
		t.Fatalf("combined output = %d bytes, want <= 262144", len(res.Combined))
	}
}

// TestExecuteUTF8Sanitize 验证非法 UTF-8 输出被替换为 U+FFFD 而非原样入库。
func TestExecuteUTF8Sanitize(t *testing.T) {
	fake := &fakeWinRM{stdout: b64("ok\n\xff\xfe\n"), exitCode: 0}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	res, err := p.Execute(context.Background(), target, RunRequest{Command: "Get-Date"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !bytes.Contains([]byte(res.Combined), []byte("ok")) {
		t.Fatalf("valid part lost: %q", res.Combined)
	}
	if strings.Contains(res.Combined, "\xff") {
		t.Fatalf("invalid UTF-8 leaked: %q", res.Combined)
	}
	if !strings.Contains(res.Combined, "\uFFFD") {
		t.Fatalf("replacement char missing: %q", res.Combined)
	}
}

// TestExecuteTimeout 验证命令超时返回错误（Receive 永远 Running）。
func TestExecuteTimeout(t *testing.T) {
	fake := &fakeWinRM{running: true}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	start := time.Now()
	_, err := p.Execute(context.Background(), target, RunRequest{
		Command: "Start-Sleep -Seconds 600",
		Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("timeout must produce an error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error must mention timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

// TestExecuteEmptyCommand 验证空命令被拒绝。
func TestExecuteEmptyCommand(t *testing.T) {
	p := newTestPool()
	defer p.Close()
	if _, err := p.Execute(context.Background(), Target{}, RunRequest{}); err == nil {
		t.Fatal("empty command must fail")
	}
}

// TestProbeCommandShape 验证连接测试探针输出两行（计算机名 + PS 版本）。
func TestProbeCommandShape(t *testing.T) {
	fake := &fakeWinRM{stdout: b64("WIN-ABC-123\n5.1.19041.5007\n"), exitCode: 0}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	res, err := p.Test(context.Background(), target)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if res.ServerVersion != "5.1.19041.5007" {
		t.Fatalf("server_version = %q, want 5.1.19041.5007", res.ServerVersion)
	}
	if res.Fingerprint != "" || res.FirstSeen {
		t.Fatal("winrm has no host-key semantics: fingerprint must be empty, first_seen false")
	}
	if res.LatencyMS < 0 {
		t.Fatalf("negative latency: %d", res.LatencyMS)
	}
	_, _, scripts := fake.recorded()
	if len(scripts) == 0 || !strings.Contains(scripts[0], "Win32_ComputerSystem") {
		t.Fatalf("probe must query Win32_ComputerSystem, got %q", scripts)
	}
}

// TestInvalidate 验证按 ServerID 失效执行桶后可重建并继续执行。
func TestInvalidate(t *testing.T) {
	fake := &fakeWinRM{stdout: b64("ok"), exitCode: 0}
	_, target := startFakeWinRM(t, fake)
	p := newTestPool()
	defer p.Close()

	if _, err := p.Execute(context.Background(), target, RunRequest{Command: "Get-Date"}); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	p.mu.Lock()
	_, hasBucket := p.buckets[target.ServerID]
	p.mu.Unlock()
	if !hasBucket {
		t.Fatal("bucket must exist after execute")
	}

	p.Invalidate(target.ServerID)
	p.mu.Lock()
	_, hasBucket = p.buckets[target.ServerID]
	p.mu.Unlock()
	if hasBucket {
		t.Fatal("bucket must be removed by Invalidate")
	}

	if _, err := p.Execute(context.Background(), target, RunRequest{Command: "Get-Date"}); err != nil {
		t.Fatalf("execute after invalidate: %v", err)
	}
}

// TestTargetKeyContainsChannelFields 验证桶键包含通道要素：任一变化即重建。
func TestTargetKeyContainsChannelFields(t *testing.T) {
	base := Target{Host: "h", Port: 5985, Username: "u", AuthType: store.AuthMethodPasswordNTLM}
	if targetKey(base) == targetKey(Target{Host: "h2", Port: 5985, Username: "u", AuthType: store.AuthMethodPasswordNTLM}) {
		t.Fatal("host change must alter key")
	}
	if targetKey(base) == targetKey(Target{Host: "h", Port: 5986, Username: "u", AuthType: store.AuthMethodPasswordNTLM}) {
		t.Fatal("port change must alter key")
	}
	if targetKey(base) == targetKey(Target{Host: "h", Port: 5985, Username: "u2", AuthType: store.AuthMethodPasswordNTLM}) {
		t.Fatal("username change must alter key")
	}
	if targetKey(base) == targetKey(Target{Host: "h", Port: 5985, Username: "u", AuthType: store.AuthMethodPasswordBasic}) {
		t.Fatal("auth type change must alter key")
	}
	if targetKey(Target{Port: 0, Host: "h", Username: "u", AuthType: store.AuthMethodPasswordNTLM}) != targetKey(Target{Port: 5985, Host: "h", Username: "u", AuthType: store.AuthMethodPasswordNTLM}) {
		t.Fatal("port 0 must default to 5985 in key")
	}
}

// TestFormatISODuration 验证 OperationTimeout 编码。
func TestFormatISODuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{55 * time.Second, "PT55S"},
		{60 * time.Second, "PT60S"},
		{0, "PT1S"},
		{-time.Second, "PT1S"},
	} {
		if got := formatISODuration(tc.d); got != tc.want {
			t.Fatalf("formatISODuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestEncodePowerShell 验证脚本编码为 UTF-16LE Base64 的 EncodedCommand。
func TestEncodePowerShell(t *testing.T) {
	got := encodePowerShell("Get-Date")
	if !strings.HasPrefix(got, "powershell -NoProfile -NonInteractive -EncodedCommand ") {
		t.Fatalf("unexpected prefix: %q", got)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "powershell -NoProfile -NonInteractive -EncodedCommand "))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	// "Get-Date" 的 UTF-16LE。
	want := []byte{'G', 0, 'e', 0, 't', 0, '-', 0, 'D', 0, 'a', 0, 't', 0, 'e', 0}
	if !bytes.Equal(raw, want) {
		t.Fatalf("decoded = %v, want %v", raw, want)
	}
}

// TestPoolClosed 验证关闭后执行被拒绝。
func TestPoolClosed(t *testing.T) {
	p := newTestPool()
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := p.Execute(context.Background(), Target{ServerID: "x", Host: "h", Username: "u", AuthType: store.AuthMethodPasswordNTLM}, RunRequest{Command: "Get-Date"}); err == nil {
		t.Fatal("execute on closed pool must fail")
	}
}

// 接口与第三方类型编译期断言。
var (
	_ Executor          = (*Pool)(nil)
	_ winrm.Transporter = (*winrm.ClientNTLM)(nil)
	_ http.Handler      = (*fakeWinRM)(nil)
)
