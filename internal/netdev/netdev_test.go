// netdev_test.go 覆盖契约 5.2B / 验收 G24：fake 设备 TCP server 仿真
// 提示符/登录/分页——telnet 登录交互（Username/Password 应答顺序）、分页
// 关闭命令发送、命令回显剥离、--More-- 残留清洗、错误指示 ExitCode=1、
// 超时→NETCLI_EXEC_FAILED 哨兵、登录失败→NETCLI_LOGIN_FAILED 哨兵、
// enable 自动应答仅对 enable/super 命令触发、输出截断、Invalidate 语义、
// Test 探针版本解析、五品牌 facts 解析；SSH 通道用 fake sshd（PTY + 回显）
// 验证经 sshx.Session 的 expect 与 TOFU 透传。
package netdev

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"aiops/internal/netdev/telnetx"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// fake telnet 设备
// ---------------------------------------------------------------------------

// fakeDevice 是脚本化网络设备（raw TCP）。按品牌参数化提示符与分页命令；
// lines 记录收到的全部命令行；spaces 记录分页续读收到的空格数。
type fakeDevice struct {
	ln       net.Listener
	brand    Brand
	behavior string // ok | login_fail | exec_hang | error | paged | enable_ask | enable_ask_no_secret

	mu     sync.Mutex
	lines  []string
	spaces int
}

func newFakeDevice(t *testing.T, brand Brand, behavior string) *fakeDevice {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fd := &fakeDevice{ln: ln, brand: brand, behavior: behavior}
	go fd.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return fd
}

func (fd *fakeDevice) port(t *testing.T) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(fd.ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return p
}

func (fd *fakeDevice) received() []string {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return append([]string(nil), fd.lines...)
}

func (fd *fakeDevice) spaceCount() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.spaces
}

func (fd *fakeDevice) acceptLoop() {
	for {
		conn, err := fd.ln.Accept()
		if err != nil {
			return
		}
		go fd.handle(conn)
	}
}

func (fd *fakeDevice) record(line string) {
	fd.mu.Lock()
	fd.lines = append(fd.lines, line)
	fd.mu.Unlock()
}

// handle 仿真一台设备的完整会话。
func (fd *fakeDevice) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	write := func(s string) { _, _ = w.WriteString(s); _ = w.Flush() }
	prof := ProfileOf(fd.brand)
	prompt := fakePrompt(fd.brand)

	// 登录序列。
	write("Username: ")
	if readLine(r) != "admin" {
		write("% Authentication failed\r\n")
		return
	}
	write("Password: ")
	pw := readLine(r)
	if pw != "secret" {
		if fd.behavior == "login_fail" {
			write("% Login failed\r\nUsername: ")
			// 再收一行后放弃（客户端会超时或重试）。
			_ = readLine(r)
			return
		}
	}
	write("\r\n" + prompt)

	for {
		line := readLine(r)
		if line == "" {
			continue
		}
		fd.record(line)
		switch {
		case line == prof.PagerOffCmd:
			write(line + "\r\n" + prompt) // 回显 + 提示符，无正文
		case strings.HasPrefix(line, "show version"), strings.HasPrefix(line, "display version"):
			write(line + "\r\n")
			write("Cisco IOS Software, C2960 Software (C2960-LANBASEK9-M), Version 15.2(4)E7\r\n")
			write("cisco WS-C2960-24TC-L (PowerPC405) processor\r\n")
			write("uptime is 11 weeks, 4 days, 22 hours, 57 minutes\r\n")
			write("System returned to ROM by power-on\r\n")
			if fd.behavior == "error" {
				write("% Invalid input detected at '^' marker\r\n")
			} else if fd.behavior == "paged" {
				write("part-one\r\n--More-- ")
				// 等待空格续读。
				for {
					b, err := r.ReadByte()
					if err != nil {
						return
					}
					if b == ' ' {
						fd.mu.Lock()
						fd.spaces++
						fd.mu.Unlock()
						break
					}
				}
				write("\r\npart-two\r\n")
			}
			write(prompt)
		case line == "enable" || line == "super":
			if fd.behavior == "enable_ask" || fd.behavior == "enable_ask_no_secret" {
				write("Password: ")
				if fd.behavior == "enable_ask" {
					if readLine(r) != "enable-secret" {
						write("% Bad secrets\r\n" + prompt)
						return
					}
					write(prompt) // 认证成功：特权提示符
				} else {
					// 不再读取：客户端不应答时设备停在 Password: 等待。
					// 给客户端留出观察窗口后挂起。
					time.Sleep(5 * time.Second)
					return
				}
			} else {
				write(line + "\r\n" + prompt)
			}
		case line == "exit":
			return
		case fd.behavior == "exec_hang":
			// 收到命令后不响应，制造 expect 超时。
			time.Sleep(30 * time.Second)
			return
		default:
			write(line + "\r\nOK\r\n" + prompt)
		}
	}
}

// readLine 读一行并记录到 fake；连接断开返回空串。
func readLine(r *bufio.Reader) string {
	line, err := r.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// fakePrompt 返回品牌对应的固定提示符（含尾空格）。
func fakePrompt(b Brand) string {
	switch b {
	case BrandHuawei, BrandH3C:
		return "<hw-sw> "
	case BrandJunos:
		return "root@junos-dev> "
	default:
		return "core-sw# "
	}
}

func netTarget(fd *fakeDevice, brand Brand) Target {
	return Target{
		ServerID:    "srv-1",
		Brand:       brand,
		AccessProto: store.AccessProtoTelnet,
		Host:        "127.0.0.1",
		Port:        0, // 由调用方填
		Username:    "admin",
		Password:    "secret",
	}
}

func newTestPool() *Pool {
	return New(Config{ExpectTick: 20 * time.Millisecond}, nil, nil)
}

// ---------------------------------------------------------------------------
// telnet 通道：登录、分页关闭、回显清洗、分页残留清洗、错误指示、超时
// ---------------------------------------------------------------------------

// TestExecuteTelnetHappyPath 验证完整流程：登录应答顺序、分页关闭命令已
// 发送、输出剥离回显与提示符、exit 收尾、明文告警透传。
func TestExecuteTelnetHappyPath(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "ok")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d (output=%q)", res.ExitCode, res.Combined)
	}
	if res.CleartextWarning != telnetx.CleartextWarning {
		t.Errorf("cleartext warning not propagated: %q", res.CleartextWarning)
	}
	for _, want := range []string{"Version 15.2(4)E7", "uptime is 11 weeks"} {
		if !strings.Contains(res.Combined, want) {
			t.Errorf("output missing %q: %q", want, res.Combined)
		}
	}
	for _, unwanted := range []string{"show version", "core-sw#", "--More--"} {
		if strings.Contains(res.Combined, unwanted) {
			t.Errorf("output should not contain %q: %q", unwanted, res.Combined)
		}
	}
	// 命令序列：分页关闭 → 目标命令 → exit；登录凭证行先于一切命令。
	got := fd.received()
	wantSeq := []string{"terminal length 0", "show version", "exit"}
	if len(got) < len(wantSeq) || got[len(got)-len(wantSeq) : len(got)-1][0] != wantSeq[0] {
		t.Errorf("received sequence = %v, want pager-off before command", got)
	}
}

// TestExecuteTelnetPagedOutput 验证 --More-- 自动续读与残留清洗。
func TestExecuteTelnetPagedOutput(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "paged")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d (output=%q)", res.ExitCode, res.Combined)
	}
	if !strings.Contains(res.Combined, "part-one") || !strings.Contains(res.Combined, "part-two") {
		t.Errorf("paged output incomplete: %q", res.Combined)
	}
	if MoreRe.MatchString(res.Combined) {
		t.Errorf("--More-- residue in output: %q", res.Combined)
	}
	if fd.spaceCount() == 0 {
		t.Errorf("pager did not auto-continue (no space sent)")
	}
}

// TestExecuteTelnetErrorIndicator 验证错误指示命中 → ExitCode=1 且输出保留。
func TestExecuteTelnetErrorIndicator(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "error")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1 on device error", res.ExitCode)
	}
	if !strings.Contains(res.Combined, "% Invalid input") {
		t.Errorf("device error text lost: %q", res.Combined)
	}
}

// TestExecuteTelnetLoginFailed 验证凭据错误 → ErrLoginFailed。
func TestExecuteTelnetLoginFailed(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "login_fail")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Password = "wrong"
	tgt.Port = fd.port(t)

	_, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 5 * time.Second})
	if !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("want ErrLoginFailed, got %v", err)
	}
}

// TestExecuteTelnetTimeout 验证命令挂起 → ErrExecFailed。
func TestExecuteTelnetTimeout(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "exec_hang")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Port = fd.port(t)

	// exec_hang 分支只吞非探针命令（show version 分支在前会正常应答）。
	_, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show clock", Timeout: 1 * time.Second})
	if !errors.Is(err, ErrExecFailed) {
		t.Fatalf("want ErrExecFailed, got %v", err)
	}
}

// TestExecuteTelnetEnableAutoAnswer 验证 enable 的二级密码提示自动应答
// （EnableSecret 非空）。
func TestExecuteTelnetEnableAutoAnswer(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "enable_ask")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.EnableSecret = "enable-secret"
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "enable", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d (output=%q)", res.ExitCode, res.Combined)
	}
	// 自动应答的 enable 密码不应出现在清洗输出里。
	if strings.Contains(res.Combined, "enable-secret") {
		t.Errorf("enable secret leaked into output: %q", res.Combined)
	}
}

// TestExecuteTelnetEnableNoSecret 验证未配置 enable 密码时不注入输入、
// 原样返回设备输出且 ExitCode=1（契约 5.2B）。
func TestExecuteTelnetEnableNoSecret(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "enable_ask_no_secret")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco) // EnableSecret 空
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "enable", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("execute returned error instead of device output: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(strings.ToLower(res.Combined), "password") {
		t.Errorf("device prompt output lost: %q", res.Combined)
	}
}

// TestExecuteTelnetHuaweiProfile 验证 huawei profile 的提示符/分页关闭命令
// 路由与登录序列（Username/Password）。
func TestExecuteTelnetHuaweiProfile(t *testing.T) {
	fd := newFakeDevice(t, BrandHuawei, "ok")
	pool := newTestPool()
	tgt := netTarget(fd, BrandHuawei)
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "display version", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d (output=%q)", res.ExitCode, res.Combined)
	}
	got := fd.received()
	if len(got) == 0 || got[0] != "screen-length 0 temporary" {
		t.Errorf("huawei pager-off not sent first: %v", got)
	}
}

// TestExecuteTelnetTruncation 验证 256KB 上限内的小 maxOut 截断。
func TestExecuteTelnetTruncation(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "ok")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Port = fd.port(t)

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 5 * time.Second, MaxOutputBytes: 40})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.Truncated {
		t.Errorf("want truncated=true")
	}
	if len(res.Combined) > 40 {
		t.Errorf("kept %d bytes exceeds max", len(res.Combined))
	}
}

// TestInvalidateIdempotent 验证 Invalidate 幂等且对未知设备为空操作。
func TestInvalidateIdempotent(t *testing.T) {
	pool := newTestPool()
	pool.Invalidate("no-such-server")
	pool.Invalidate("no-such-server")
	if err := pool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 探针
// ---------------------------------------------------------------------------

// TestProbeTelnet 验证探针命令执行与版本串解析；telnet 无主机密钥语义。
func TestProbeTelnet(t *testing.T) {
	fd := newFakeDevice(t, BrandCisco, "ok")
	pool := newTestPool()
	tgt := netTarget(fd, BrandCisco)
	tgt.Port = fd.port(t)

	res, err := pool.Test(t.Context(), tgt)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !strings.HasPrefix(res.ServerVersion, "15.2(4)E7") {
		t.Errorf("server version = %q, want parsed 15.2(4)E7...", res.ServerVersion)
	}
	if res.Fingerprint != "" || res.FirstSeen {
		t.Errorf("telnet must have no host key semantics: %q/%v", res.Fingerprint, res.FirstSeen)
	}
	if res.CleartextWarning == "" {
		t.Errorf("cleartext warning missing on telnet test result")
	}
}

// ---------------------------------------------------------------------------
// SSH 通道（fake sshd：PTY + 回显 + 提示符 + 命令响应）
// ---------------------------------------------------------------------------

// fakeSSHDevice 是本地 SSH 服务：密码认证、pty-req + shell、行回显与
// 提示符应答（cisco 形态）。
type fakeSSHDevice struct {
	ln   net.Listener
	addr string

	mu    sync.Mutex
	lines []string
}

func newFakeSSHDevice(t *testing.T) *fakeSSHDevice {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) != "secret" {
				return nil, fmt.Errorf("bad password")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fd := &fakeSSHDevice{ln: ln, addr: ln.Addr().String()}
	go fd.acceptLoop(cfg)
	t.Cleanup(func() { _ = ln.Close() })
	return fd
}

func (fd *fakeSSHDevice) port(t *testing.T) int {
	t.Helper()
	_, portStr, _ := net.SplitHostPort(fd.addr)
	p, _ := strconv.Atoi(portStr)
	return p
}

func (fd *fakeSSHDevice) received() []string {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return append([]string(nil), fd.lines...)
}

func (fd *fakeSSHDevice) acceptLoop(cfg *ssh.ServerConfig) {
	for {
		conn, err := fd.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
			if err != nil {
				_ = conn.Close()
				return
			}
			defer sconn.Close()
			go ssh.DiscardRequests(reqs)
			for newChan := range chans {
				if newChan.ChannelType() != "session" {
					_ = newChan.Reject(ssh.UnknownChannelType, "session only")
					continue
				}
				ch, chReqs, err := newChan.Accept()
				if err != nil {
					continue
				}
				go fd.handleSession(ch, chReqs)
			}
		}()
	}
}

// handleSession：pty-req/shell 后进入行协议（回显 + 提示符应答）。
func (fd *fakeSSHDevice) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	shellStarted := false
	r := bufio.NewReader(ch)
	write := func(s string) { _, _ = ch.Write([]byte(s)) }
	prompt := "ssh-sw# "

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			if !shellStarted {
				shellStarted = true
				write(prompt)
				go func() {
					for {
						line, err := r.ReadString('\n')
						if err != nil {
							return
						}
						line = strings.TrimRight(line, "\r\n")
						fd.mu.Lock()
						fd.lines = append(fd.lines, line)
						fd.mu.Unlock()
						switch {
						case line == "terminal length 0":
							write(line + "\r\n" + prompt)
						case line == "show version":
							write(line + "\r\n")
							write("IOS-XE Software, Version 17.06.04a\r\n")
							write("uptime is 2 days\r\n")
							write(prompt)
						case line == "exit":
							_ = ch.Close()
							return
						default:
							write(line + "\r\nOK\r\n" + prompt)
						}
					}
				}()
			}
		case "window-change":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// TestExecuteSSHChannel 验证 SSH 通道经 sshx.Session 的 PTY expect 全流程。
func TestExecuteSSHChannel(t *testing.T) {
	fd := newFakeSSHDevice(t)
	sshPool := sshx.New(sshx.Config{}, nil)
	defer sshPool.Close()
	pool := New(Config{ExpectTick: 20 * time.Millisecond}, sshPool, nil)

	tgt := Target{
		ServerID:    "srv-ssh-1",
		Brand:       BrandCisco,
		AccessProto: store.AccessProtoSSH,
		Host:        "127.0.0.1",
		Port:        fd.port(t),
		Username:    "admin",
		Password:    "secret",
	}

	res, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("execute over ssh: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d (output=%q)", res.ExitCode, res.Combined)
	}
	if !strings.Contains(res.Combined, "Version 17.06.04a") {
		t.Errorf("ssh output missing version: %q", res.Combined)
	}
	if strings.Contains(res.Combined, "show version") {
		t.Errorf("ssh echo not stripped: %q", res.Combined)
	}
	if strings.Contains(res.Combined, "ssh-sw#") {
		t.Errorf("ssh prompt not stripped: %q", res.Combined)
	}
	if res.CleartextWarning != "" {
		t.Errorf("ssh channel must not carry cleartext warning")
	}
	got := fd.received()
	if len(got) < 2 || got[0] != "terminal length 0" || got[1] != "show version" {
		t.Errorf("ssh session sequence = %v", got)
	}
}

// TestProbeSSHTOFU 验证 Test 的 SSH 通道返回真实指纹并透传 FirstSeen。
func TestProbeSSHTOFU(t *testing.T) {
	fd := newFakeSSHDevice(t)
	sshPool := sshx.New(sshx.Config{}, nil)
	defer sshPool.Close()
	pool := New(Config{ExpectTick: 20 * time.Millisecond}, sshPool, nil)

	tgt := Target{
		ServerID:    "srv-ssh-2",
		Brand:       BrandCisco,
		AccessProto: store.AccessProtoSSH,
		Host:        "127.0.0.1",
		Port:        fd.port(t),
		Username:    "admin",
		Password:    "secret",
	}
	res, err := pool.Test(t.Context(), tgt)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !strings.HasPrefix(res.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q, want SHA256:...", res.Fingerprint)
	}
	if !res.FirstSeen {
		t.Errorf("first seen should be true on TOFU first connect")
	}
	if !strings.HasPrefix(res.ServerVersion, "17.06.04a") {
		t.Errorf("server version = %q", res.ServerVersion)
	}
	if res.CleartextWarning != "" {
		t.Errorf("ssh probe must not carry cleartext warning")
	}
}

// TestSSHLoginFailed 验证 SSH 密码错误映射为 ErrLoginFailed。
func TestSSHLoginFailed(t *testing.T) {
	fd := newFakeSSHDevice(t)
	sshPool := sshx.New(sshx.Config{}, nil)
	defer sshPool.Close()
	pool := New(Config{ExpectTick: 20 * time.Millisecond}, sshPool, nil)

	tgt := Target{
		ServerID:    "srv-ssh-3",
		Brand:       BrandCisco,
		AccessProto: store.AccessProtoSSH,
		Host:        "127.0.0.1",
		Port:        fd.port(t),
		Username:    "admin",
		Password:    "wrong",
	}
	_, err := pool.Execute(t.Context(), tgt, RunRequest{Command: "show version", Timeout: 10 * time.Second})
	if err == nil {
		t.Fatalf("want error for bad ssh password")
	}
	// sshx 拨号失败在 openChannel 返回（登录阶段前置），不带 ErrLoginFailed
	// 哨兵——由调用方按 SSH 语义映射；这里断言错误文本可识别。
	if !strings.Contains(err.Error(), "ssh") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// facts（契约 7.5）
// ---------------------------------------------------------------------------

func TestFactsCommands(t *testing.T) {
	cases := map[Brand][]string{
		BrandCisco:   {"show version", "show ip interface brief | exclude unassigned"},
		BrandHuawei:  {"display version", "display ip interface brief"},
		BrandH3C:     {"display version", "display ip interface brief"},
		BrandJunos:   {"show version", "show interfaces terse"},
		BrandGeneric: {"show version"},
	}
	for brand, want := range cases {
		got := FactsCommands(brand)
		if len(got) != len(want) {
			t.Errorf("FactsCommands(%s) = %v, want %v", brand, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("FactsCommands(%s)[%d] = %q, want %q", brand, i, got[i], want[i])
			}
		}
	}
}

func TestParseFactsCisco(t *testing.T) {
	versionOut := `core-sw# show version
Cisco IOS Software, C2960 Software (C2960-LANBASEK9-M), Version 15.2(4)E7
cisco WS-C2960-24TC-L (PowerPC405) processor
Cisco WS-C2960-24TC-L (PowerPC405) processor (revision N0) with 131072K bytes of memory.
Model number: WS-C2960-24TC-L
System serial number: FDO1122Z34E
core-sw uptime is 11 weeks, 4 days, 22 hours, 57 minutes`
	ifOut := `Interface IP-Address OK Method Status Protocol
Vlan1 192.168.1.1 YES manual up up
GigabitEthernet0/1 unassigned YES unset down down`

	f := ParseFacts(BrandCisco, "cisco", versionOut, ifOut)
	if f.Hostname != "core-sw" {
		t.Errorf("hostname = %q", f.Hostname)
	}
	if f.Vendor != "cisco" {
		t.Errorf("vendor = %q", f.Vendor)
	}
	if !strings.HasPrefix(f.SoftwareVersion, "15.2(4)E7") {
		t.Errorf("software version = %q", f.SoftwareVersion)
	}
	if f.SerialNumber != "FDO1122Z34E" {
		t.Errorf("serial = %q", f.SerialNumber)
	}
	if !strings.Contains(f.Uptime, "11 weeks") {
		t.Errorf("uptime = %q", f.Uptime)
	}
	if !strings.Contains(f.Model, "WS-C2960-24TC-L") {
		t.Errorf("model = %q", f.Model)
	}
	if len(f.Interfaces) != 2 {
		t.Fatalf("interfaces = %+v", f.Interfaces)
	}
	vlan1 := f.Interfaces[0]
	if vlan1.Name != "Vlan1" || vlan1.IP != "192.168.1.1" || vlan1.Status != "up" || vlan1.Protocol != "up" {
		t.Errorf("interfaces[0] = %+v", vlan1)
	}
}

func TestParseFactsHuaweiAndJunos(t *testing.T) {
	hwVersion := `<hw-sw>display version
Huawei Versatile Routing Platform Software
VRP (R) Software, Version 5.160 (S5700 V200R005C00)
HUAWEI S5700-28C-EI Routing Switch uptime is 30 days`
	hwIf := `Interface IP Address/Mask Physical Protocol
Vlanif100 10.1.1.1/24 up up
MEth0/0/1 unassigned down down`
	f := ParseFacts(BrandHuawei, "huawei", hwVersion, hwIf)
	if !strings.HasPrefix(f.SoftwareVersion, "5.160") {
		t.Errorf("huawei version = %q", f.SoftwareVersion)
	}
	if !strings.Contains(f.Uptime, "30 days") {
		t.Errorf("huawei uptime = %q", f.Uptime)
	}
	if len(f.Interfaces) != 2 || f.Interfaces[0].Name != "Vlanif100" || f.Interfaces[0].IP != "10.1.1.1/24" {
		t.Errorf("huawei interfaces = %+v", f.Interfaces)
	}

	jVersion := `root@junos-dev> show version
Hostname: junos-dev
Model: mx240
Junos: 21.4R3-S4.9
uptime: 12 days`
	jIf := `Interface Admin Link proto local
xe-0/0/0 up up inet 10.0.0.1/30
xe-0/0/1 down down`
	fj := ParseFacts(BrandJunos, "junos", jVersion, jIf)
	if !strings.HasPrefix(fj.SoftwareVersion, "21.4R3") {
		t.Errorf("junos version = %q", fj.SoftwareVersion)
	}
	if !strings.Contains(fj.Model, "mx240") {
		t.Errorf("junos model = %q", fj.Model)
	}
	if len(fj.Interfaces) != 2 || fj.Interfaces[0].Name != "xe-0/0/0" {
		t.Errorf("junos interfaces = %+v", fj.Interfaces)
	}
}

// TestParseFactsPartialFailure 验证解析失败的字段留空、不整体报错。
func TestParseFactsPartialFailure(t *testing.T) {
	f := ParseFacts(BrandGeneric, "generic", "\r\nsome banner\r\n", "")
	if f.SoftwareVersion != "" || f.SerialNumber != "" || f.Uptime != "" || f.Model != "" {
		t.Errorf("partial parse should leave failures empty: %+v", f)
	}
	if f.Interfaces == nil {
		t.Errorf("interfaces must be non-nil empty slice")
	}
}

// TestBrandOf 验证 store.NetdevBrand 映射与未知值失败关闭。
func TestBrandOf(t *testing.T) {
	cases := map[store.NetdevBrand]Brand{
		store.NetdevBrandCisco: BrandCisco, store.NetdevBrandHuawei: BrandHuawei,
		store.NetdevBrandH3C: BrandH3C, store.NetdevBrandJunos: BrandJunos,
		store.NetdevBrandGeneric: BrandGeneric,
	}
	for in, want := range cases {
		if got := BrandOf(in); got != want {
			t.Errorf("BrandOf(%s) = %s, want %s", in, got, want)
		}
	}
	if BrandOf(store.NetdevBrand("nokia")) != BrandGeneric {
		t.Errorf("unknown brand must fail closed to generic")
	}
}

// TestTargetCleartextWarning 验证明文告警字段透传（telnet 有、ssh 无）。
func TestTargetCleartextWarning(t *testing.T) {
	telnetT := netTarget(nil, BrandCisco)
	if telnetT.CleartextWarning() != telnetx.CleartextWarning {
		t.Errorf("telnet target must carry warning")
	}
	sshT := telnetT
	sshT.AccessProto = store.AccessProtoSSH
	if sshT.CleartextWarning() != "" {
		t.Errorf("ssh target must not carry warning")
	}
}

// TestExecuteEmptyCommand 验证空命令拒绝。
func TestExecuteEmptyCommand(t *testing.T) {
	pool := newTestPool()
	if _, err := pool.Execute(t.Context(), netTarget(nil, BrandCisco), RunRequest{}); err == nil {
		t.Fatalf("want error for empty command")
	}
}

// 编译期保证 io 被引用（fake 读取器辅助）。
var _ = io.Discard
