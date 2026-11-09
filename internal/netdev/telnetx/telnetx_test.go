// telnetx_test.go 覆盖裸 TCP Telnet 客户端：IAC 协商应答与数据清洗、
// 提示符检测、分页自动续读（--More-- 与 ---- More ----）、超时、输出截断、
// 明文告警常量、连接拒绝与关闭幂等。
package telnetx

import (
	"bufio"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lineDevice 是逐行交互的最小 telnet 设备。
type lineDevice struct {
	ln        net.Listener
	received  chan string
	onConnect func(w *bufio.Writer)
	onLine    func(line string, w *bufio.Writer)
}

// newLineDevice 在启动 accept 循环前固定回调（避免与 handle goroutine 竞争）。
func newLineDevice(t *testing.T, onConnect func(w *bufio.Writer), onLine func(line string, w *bufio.Writer)) *lineDevice {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fd := &lineDevice{ln: ln, received: make(chan string, 64), onConnect: onConnect, onLine: onLine}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go fd.handle(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return fd
}

func (fd *lineDevice) port(t *testing.T) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(fd.ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return port
}

func (fd *lineDevice) handle(conn net.Conn) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	if fd.onConnect != nil {
		fd.onConnect(w)
		_ = w.Flush()
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		// 设备 telnet 层剥离客户端的协商应答字节（IAC DONT/WONT …）。
		line = stripIAC(strings.TrimRight(line, "\r\n"))
		if line == "" {
			continue
		}
		select {
		case fd.received <- line:
		default:
		}
		if fd.onLine != nil {
			fd.onLine(line, w)
			_ = w.Flush()
		}
	}
}

// stripIAC 从字符串中剥掉全部 IAC 命令序列（2 或 3 字节）。
func stripIAC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == iac {
			if i+1 < len(s) {
				switch s[i+1] {
				case do, dont, will, wont:
					i += 3
					continue
				default:
					i += 2
					continue
				}
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func mustReadLine(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return ""
	}
}

var promptRe = regexp.MustCompile(`[a-zA-Z0-9._<>#\[\]-]+[>#\]] ?$`)

// TestNegotiationAndLogin 验证 IAC 协商清洗（WILL/DO 应答、IAC IAC 数据、
// 子协商丢弃）与登录行交互顺序。
func TestNegotiationAndLogin(t *testing.T) {
	fd := newLineDevice(t,
		func(w *bufio.Writer) {
			// IAC WILL ECHO(1)、IAC DO SGA(3)：客户端应回 DONT 1 / WONT 3。
			_, _ = w.Write([]byte{iac, will, 1, iac, do, 3})
			_, _ = w.WriteString("Username: ")
		},
		func(line string, w *bufio.Writer) {
			switch line {
			case "admin":
				_, _ = w.WriteString("Password: ")
			case "secret":
				// IAC IAC → 数据 0xFF；子协商（TERMTYPE）负载必须丢弃。
				_, _ = w.Write([]byte{iac, iac})
				_, _ = w.Write([]byte{iac, sb, 24, 0, 'Q', iac, se})
				_, _ = w.WriteString("\r\nrouter> ")
			}
		})

	conn, err := Dial(t.Context(), "127.0.0.1", fd.port(t), Config{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Expect(regexp.MustCompile(`(?i)username:`), 2*time.Second); err != nil {
		t.Fatalf("expect username: %v", err)
	}
	if err := conn.SendLine("admin"); err != nil {
		t.Fatalf("send user: %v", err)
	}
	if _, err := conn.Expect(regexp.MustCompile(`(?i)password:`), 2*time.Second); err != nil {
		t.Fatalf("expect password: %v", err)
	}
	if err := conn.SendLine("secret"); err != nil {
		t.Fatalf("send pass: %v", err)
	}
	out, err := conn.Expect(promptRe, 2*time.Second)
	if err != nil {
		t.Fatalf("expect prompt: %v (out=%q)", err, out)
	}
	// IAC IAC 清洗为单个 0xFF 数据字节；子协商负载 Q 不得泄漏。
	if strings.IndexByte(out, 0xFF) < 0 {
		t.Errorf("IAC IAC should surface as data 0xFF, out=%q", out)
	}
	if strings.Contains(out, "Q") {
		t.Errorf("subnegotiation payload leaked: %q", out)
	}
	if got := mustReadLine(t, fd.received, "username"); got != "admin" {
		t.Errorf("server got username %q", got)
	}
	if got := mustReadLine(t, fd.received, "password"); got != "secret" {
		t.Errorf("server got password %q", got)
	}
}

// TestPagerAutoContinue 验证 --More-- / ---- More ---- 自动续读全链路：
// 设备停在分页提示，客户端自动发空格，设备继续输出余下行与提示符，
// 输出不含分页残留。
func TestPagerAutoContinue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		more   string
		prompt string
	}{
		{"cisco", "--More--", "router# "},
		{"huawei", "  ---- More ----", "<hw-sw> "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer ln.Close()
			go servePagedSession(ln, tc.more, tc.prompt)

			_, portStr, _ := net.SplitHostPort(ln.Addr().String())
			port, _ := strconv.Atoi(portStr)
			conn, err := Dial(t.Context(), "127.0.0.1", port, Config{})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			if _, err := conn.Expect(promptRe, 2*time.Second); err != nil {
				t.Fatalf("expect prompt: %v", err)
			}
			if err := conn.SendLine("show run"); err != nil {
				t.Fatalf("send: %v", err)
			}
			out, truncated, err := conn.ExpectPrompt(promptRe, DefaultMoreRe, " ", 3*time.Second, 0)
			if err != nil {
				t.Fatalf("expect after pager: %v (out=%q)", err, out)
			}
			if truncated {
				t.Errorf("unexpected truncation")
			}
			for _, want := range []string{"part-one", "part-two"} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q: %q", want, out)
				}
			}
			if DefaultMoreRe.MatchString(out) {
				t.Errorf("pager residue not consumed: %q", out)
			}
		})
	}
}

// servePagedSession 服务一次会话：提示符 → show run → 分页两段（空格续读）。
func servePagedSession(ln net.Listener, more, prompt string) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(prompt))
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		line := strings.TrimRight(string(buf[:n]), "\r\n")
		if line == "" || line == " " {
			continue
		}
		switch {
		case strings.HasPrefix(line, "show"):
			_, _ = conn.Write([]byte("line show\r\npart-one\r\n" + more + " "))
			// 等空格续读后发余下内容与提示符。
			for {
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				if strings.ContainsAny(string(buf[:n]), " \r") {
					_, _ = conn.Write([]byte("\r\npart-two\r\n" + prompt))
					return
				}
			}
		case line == "exit":
			return
		}
	}
}

// TestExpectTimeout 验证超时返回 ErrTimeout 并附带已读内容。
func TestExpectTimeout(t *testing.T) {
	fd := newLineDevice(t, func(w *bufio.Writer) {
		_, _ = w.WriteString("hello\r\n") // 永远不来提示符
	}, nil)
	conn, err := Dial(t.Context(), "127.0.0.1", fd.port(t), Config{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	out, err := conn.Expect(promptRe, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "expect timeout") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("partial output lost: %q", out)
	}
}

// TestOutputTruncation 验证 maxBytes 截断：输出截短且 truncated=true。
func TestOutputTruncation(t *testing.T) {
	fd := newLineDevice(t, func(w *bufio.Writer) {
		_, _ = w.WriteString(strings.Repeat("a", 4096) + "\r\nrouter> ")
	}, nil)
	conn, err := Dial(t.Context(), "127.0.0.1", fd.port(t), Config{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	out, truncated, err := conn.ExpectPrompt(promptRe, nil, "", 2*time.Second, 100)
	if err != nil {
		t.Fatalf("expect: %v", err)
	}
	if !truncated {
		t.Errorf("want truncated=true")
	}
	if len(out) > 100 {
		t.Errorf("kept output %d bytes exceeds max 100", len(out))
	}
}

// TestCleartextWarning 验证明文告警常量可被上层引用（透传锚点）。
func TestCleartextWarning(t *testing.T) {
	if !strings.Contains(CleartextWarning, "cleartext") {
		t.Errorf("CleartextWarning must mention cleartext risk: %q", CleartextWarning)
	}
}

// TestDialRefused 验证不可达端口返回 dial 错误。
func TestDialRefused(t *testing.T) {
	if _, err := Dial(t.Context(), "127.0.0.1", 1, Config{DialTimeout: time.Second}); err == nil {
		t.Fatalf("want dial error for closed port")
	}
}

// TestCloseIdempotent 验证 Close 幂等且解除阻塞读。
func TestCloseIdempotent(t *testing.T) {
	fd := newLineDevice(t, func(w *bufio.Writer) { _, _ = w.WriteString("x") }, nil)
	conn, err := Dial(t.Context(), "127.0.0.1", fd.port(t), Config{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second close should be idempotent, got %v", err)
	}
	buf := make([]byte, 8)
	if _, err := conn.Read(buf); err == nil {
		t.Errorf("read after close should fail")
	}
}
