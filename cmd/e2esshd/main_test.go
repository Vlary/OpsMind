package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func ed25519Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// startTestServer 在进程内拉起假 sshd（随机端口），返回 ssh/http 地址与
// 停止函数。fixture 自身的行为（pty 尺寸记录、window-change、banner）
// 由本测试锁定——e2e 真链路断言依赖这些语义。
func startTestServer(t *testing.T) (sshAddr, httpAddr string, stop func()) {
	t.Helper()
	rec := &recorder{}

	_, priv, err := ed25519Generate()
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-fakesshd-test",
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) == e2eSSHPassword {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("auth rejected")
		},
	}
	cfg.AddHostKey(signer)

	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ssh: %v", err)
	}
	go serveSSH(sshLn, cfg, rec)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", statusHandler(rec))
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen http: %v", err)
	}
	go func() { _ = (&http.Server{Handler: mux}).Serve(httpLn) }()

	return sshLn.Addr().String(), httpLn.Addr().String(), func() {
		_ = sshLn.Close()
		_ = httpLn.Close()
	}
}

// dialPty 按后端 sshx 的顺序建立 PTY 会话（RequestPty → 可选
// WindowChange → Shell），返回会话与 stdout 管道。
func dialPty(t *testing.T, addr, user string, cols, rows int, resize *struct{ Cols, Rows int }) (*ssh.Session, chan string) {
	t.Helper()
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(e2eSSHPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if resize != nil {
		if err := session.WindowChange(resize.Rows, resize.Cols); err != nil {
			t.Fatalf("window change: %v", err)
		}
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	output := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := stdout.Read(buf)
		output <- string(buf[:n])
	}()
	return session, output
}

func httpStatus(t *testing.T, addr string) map[string]any {
	t.Helper()
	return httpStatusFor(t, addr, "")
}

func httpStatusFor(t *testing.T, addr, user string) map[string]any {
	t.Helper()
	url := "http://" + addr + "/status"
	if user != "" {
		url += "?user=" + user
	}
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	defer response.Body.Close()
	var snapshot map[string]any
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return snapshot
}

func TestFakeSshdRecordsPtySizeAndWindowChange(t *testing.T) {
	sshAddr, httpAddr, stop := startTestServer(t)
	defer stop()

	session, output := dialPty(t, sshAddr, "e2e", 96, 25, &struct{ Cols, Rows int }{80, 20})
	defer session.Close()

	// banner 可读（PTY 输出链路建立）。
	select {
	case banner := <-output:
		if !contains(banner, "fake-sshd ready") {
			t.Fatalf("banner = %q", banner)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no banner within 3s")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := httpStatus(t, httpAddr)
		if snapshot["initial"] == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		initial, _ := snapshot["initial"].(map[string]any)
		if initial["cols"] != float64(96) || initial["rows"] != float64(25) {
			t.Fatalf("initial = %v, want 96x25", initial)
		}
		changes, _ := snapshot["changes"].([]any)
		if len(changes) != 1 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		change, _ := changes[0].(map[string]any)
		if change["cols"] != float64(80) || change["rows"] != float64(20) {
			t.Fatalf("change = %v, want 80x20", change)
		}
		final, _ := snapshot["final"].(map[string]any)
		if final["cols"] != float64(80) || final["rows"] != float64(20) {
			t.Fatalf("final = %v, want 80x20", final)
		}
		if snapshot["shells"] != float64(1) {
			t.Fatalf("shells = %v", snapshot["shells"])
		}
		return
	}
	t.Fatal("status never reflected pty records within 3s")
}

// TestFakeSshdPerUserIsolation 锁定按 SSH 用户名分桶：并行 e2e 中另一条
// 终端会话的 window-change 不得污染本用例的 initial/changes/final。
func TestFakeSshdPerUserIsolation(t *testing.T) {
	sshAddr, httpAddr, stop := startTestServer(t)
	defer stop()

	a, _ := dialPty(t, sshAddr, "user-a", 100, 30, &struct{ Cols, Rows int }{80, 20})
	defer a.Close()
	b, _ := dialPty(t, sshAddr, "user-b", 120, 40, nil)
	defer b.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sa := httpStatusFor(t, httpAddr, "user-a")
		sb := httpStatusFor(t, httpAddr, "user-b")
		ia, _ := sa["initial"].(map[string]any)
		ib, _ := sb["initial"].(map[string]any)
		if ia == nil || ib == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		changesA, _ := sa["changes"].([]any)
		if len(changesA) != 1 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		fa, _ := sa["final"].(map[string]any)
		fb, _ := sb["final"].(map[string]any)
		changesB, _ := sb["changes"].([]any)
		if ia["cols"] != float64(100) || ia["rows"] != float64(30) ||
			fa["cols"] != float64(80) || fa["rows"] != float64(20) {
			t.Fatalf("user-a initial/final = %v/%v, want 100x30/80x20", ia, fa)
		}
		if ib["cols"] != float64(120) || ib["rows"] != float64(40) ||
			fb["cols"] != float64(120) || fb["rows"] != float64(40) {
			t.Fatalf("user-b initial/final = %v/%v, want 120x40（不被 user-a 污染）", ib, fb)
		}
		if len(changesB) != 0 {
			t.Fatalf("user-b changes = %v, want empty", changesB)
		}
		return
	}
	t.Fatal("per-user pty records not available within 3s")
}

func TestFakeSshdRejectsWrongPassword(t *testing.T) {
	sshAddr, _, stop := startTestServer(t)
	defer stop()

	config := &ssh.ClientConfig{
		User:            "e2e",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	if client, err := ssh.Dial("tcp", sshAddr, config); err == nil {
		_ = client.Close()
		t.Fatal("dial with wrong password should fail")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestFakeSshdExecRecordsCommand 六期打磨 sprint 3：命令执行链路走 exec
// 请求（sshx.Execute → session.Start），夹具必须记录收到的命令文本（e2e
// 据此断言「改写后的命令确实送达通道」），但刻意不实现执行——本夹具只
// 服务终端 PTY（文件头语义），且监控采集器也经 sshx 打同一端口。
func TestFakeSshdExecRecordsCommand(t *testing.T) {
	sshAddr, httpAddr, stop := startTestServer(t)
	defer stop()

	config := &ssh.ClientConfig{
		User:            "e2e",
		Auth:            []ssh.AuthMethod{ssh.Password(e2eSSHPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", sshAddr, config)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	// exec 不被接受（会话请求被拒绝）——夹具不执行命令。
	_ = session.Start("whoami")
	_ = session.Close()

	// 控制端：命令仍被记录（e2e 可断言「改写后命令送达」）。
	snapshot := httpStatus(t, httpAddr)
	execs, _ := snapshot["execs"].([]any)
	if len(execs) != 1 || execs[0] != "whoami" {
		t.Fatalf("execs = %v, want [whoami]", snapshot["execs"])
	}
}
