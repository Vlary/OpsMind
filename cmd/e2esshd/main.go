// e2e 假 SSH 服务器（p2 sprint 5，终端真链路 E2E 用）。
//
// 背景：既有终端 e2e 全部 mock（term-recordings.spec.ts 头注释自认后端
// 集成后应去 mock），preview 代理对 /api/term 也一直 ws:false——PTY 尺寸
// 同步缺陷（挂载即上报的 resize 在 CONNECTING 期被静默丢弃，服务端永远
// 停在创建时的 120×40）因此从未被 e2e 暴露（走查实测零 resize 帧）。
//
// 本程序提供一台真实可拨的 SSH 服务器供后端 sshx 建立 PTY：
//   - SSH 端口（默认 127.0.0.1:18083）：密码认证（e2e 固定密码
//     e2e-ssh-pass，接受任意用户名），接受 pty-req + shell + window-change，
//     shell 打开即写一段 banner（浏览器 xterm 可见，证明 PTY 链路建立）；
//   - 控制端口（默认 127.0.0.1:18084）：GET /status 返回 PTY 初始尺寸、
//     历次 window-change 与 shell 计数（e2e 据此断言浏览器 → WS → 后端
//     → PTY 的尺寸同步）；POST /reset 清零记录（用例间隔离）。
//
// 主机密钥为进程内生成的 ed25519（每次启动随机）：后端 sshx 是 TOFU
// 首连自动信任，无需预置指纹。不模拟真实 shell 行为（无回显/命令执行），
// 只记录窗口尺寸与写 banner——终端会话数据面由真浏览器 xterm 承担。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"golang.org/x/crypto/ssh"
)

// e2eSSHPassword 是假 sshd 接受的固定密码（e2e 建服务器时写入）。
const e2eSSHPassword = "e2e-ssh-pass"

// shellBanner 是 shell 打开后写给 PTY 的可见输出（浏览器 xterm 中出现
// 即证明 WS→PTY 双向链路已建立）。
const shellBanner = "fake-sshd ready (e2e terminal link)\r\n$ "

// windowSize 是一次 PTY 窗口尺寸事件。
type windowSize struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

// recorder 聚合全部会话的尺寸事件（e2e 只建单会话，聚合即该会话）。
type recorder struct {
	mu       sync.Mutex
	initial  *windowSize
	changes  []windowSize
	shells   int
	inputs   int
	inputLen int
	// execs 记录 exec 请求收到的命令（六期打磨 sprint 3：批量/命令执行
	// E2E 需要一条真实的「命令送达并返回输出」链路，此前只实现 shell）。
	execs []string
}

func (r *recorder) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{
		"initial":       r.initial,
		"changes":       r.changes,
		"shells":        r.shells,
		"inputs":        r.inputs,
		"input_bytes":   r.inputLen,
		"final":         r.finalLocked(),
		"window_change": len(r.changes),
		"execs":         append([]string{}, r.execs...),
	}
}

// finalLocked 返回 PTY 的最终尺寸（初始 + 最后一次 window-change）。
func (r *recorder) finalLocked() *windowSize {
	if r.initial == nil {
		return nil
	}
	final := *r.initial
	if n := len(r.changes); n > 0 {
		final = r.changes[n-1]
	}
	return &final
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initial = nil
	r.changes = nil
	r.shells = 0
	r.inputs = 0
	r.inputLen = 0
	r.execs = nil
}

func (r *recorder) recordInitial(cols, rows uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.initial == nil {
		r.initial = &windowSize{Cols: cols, Rows: rows}
	}
}

func (r *recorder) recordChange(cols, rows uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, windowSize{Cols: cols, Rows: rows})
}

func (r *recorder) recordShell() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shells++
}

func (r *recorder) recordInput(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs++
	r.inputLen += n
}

// recordExec 记录 exec 请求的命令（命令执行链路经此观测）。
func (r *recorder) recordExec(command string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, command)
}

func main() {
	sshAddr := flag.String("ssh", "127.0.0.1:18083", "SSH 监听地址")
	httpAddr := flag.String("http", "127.0.0.1:18084", "控制 HTTP 监听地址")
	flag.Parse()

	rec := &recorder{}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("fakesshd: generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		log.Fatalf("fakesshd: host key signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-fakesshd",
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) == e2eSSHPassword {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("fakesshd: auth rejected for %q", meta.User())
		},
	}
	cfg.AddHostKey(signer)

	listener, err := net.Listen("tcp", *sshAddr)
	if err != nil {
		log.Fatalf("fakesshd: listen ssh %s: %v", *sshAddr, err)
	}
	go serveSSH(listener, cfg, rec)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rec.snapshot())
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) {
		rec.reset()
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("fakesshd: ssh=%s http=%s", *sshAddr, *httpAddr)
	log.Fatal(http.ListenAndServe(*httpAddr, mux))
}

// serveSSH 接受 SSH 连接并只服务 "session" 通道。
func serveSSH(listener net.Listener, cfg *ssh.ServerConfig, rec *recorder) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			_, chans, reqs, err := ssh.NewServerConn(c, cfg)
			if err != nil {
				_ = c.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			for newChannel := range chans {
				if newChannel.ChannelType() != "session" {
					_ = newChannel.Reject(ssh.UnknownChannelType, "session channels only")
					continue
				}
				channel, requests, err := newChannel.Accept()
				if err != nil {
					continue
				}
				go handleSession(channel, requests, rec)
			}
		}(conn)
	}
}

// handleSession 处理一条会话通道的请求：pty-req 记录初始尺寸，shell 写
// banner，window-change 记录新尺寸；stdin 持续排空（termx 会转发 input
// 帧，不排空会因窗口写满阻塞对端）。
func handleSession(channel ssh.Channel, requests <-chan *ssh.Request, rec *recorder) {
	defer channel.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := channel.Read(buf)
			if n > 0 {
				rec.recordInput(n)
			}
			if err != nil {
				return
			}
		}
	}()
	for req := range requests {
		switch req.Type {
		case "pty-req":
			_, cols, rows := parsePtyReq(req.Payload)
			rec.recordInitial(cols, rows)
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell":
			rec.recordShell()
			_, _ = channel.Write([]byte(shellBanner))
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "exec":
			// 六期打磨 sprint 3：记录 exec 请求的命令（命令执行链路
			// sshx.Execute → session.Start 走这里），但**不实现执行**——
			// 本夹具的语义契约是「只服务终端 PTY」（见文件头），且监控采集器
			// 也经 sshx 对同一端口执行采集模板：一旦 exec 成功返回，此前
			// 依赖「采集失败」的既有 e2e 断言（如 p5round6 双机概要）会被
			// 未预期的可采集服务器污染。记录命令即可支撑「改写后的命令确实
			// 送达通道」的断言；执行结果仍按通道失败处理。
			command := parseExecPayload(req.Payload)
			rec.recordExec(command)
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		case "window-change":
			cols, rows := parseWindowChange(req.Payload)
			rec.recordChange(cols, rows)
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

// parsePtyReq 解析 pty-req 载荷（RFC 4254 6.2；x/crypto ptyRequestMsg：
// string Term, uint32 Columns, uint32 Rows, uint32 Width, uint32 Height,
// string Modelist）。
func parsePtyReq(payload []byte) (term string, cols, rows uint32) {
	var msg struct {
		Term     string
		Columns  uint32
		Rows     uint32
		Width    uint32
		Height   uint32
		Modelist string
	}
	if err := ssh.Unmarshal(payload, &msg); err != nil {
		return "", 0, 0
	}
	return msg.Term, msg.Columns, msg.Rows
}

// parseExecPayload 解析 exec 载荷（RFC 4254 6.5：string command）。
func parseExecPayload(payload []byte) string {
	var msg struct{ Command string }
	if err := ssh.Unmarshal(payload, &msg); err != nil {
		return ""
	}
	return msg.Command
}

// parseWindowChange 解析 window-change 载荷（RFC 4254 6.7；x/crypto
// ptyWindowChangeMsg：uint32 Columns, uint32 Rows, uint32 Width, uint32
// Height）。
func parseWindowChange(payload []byte) (cols, rows uint32) {
	var msg struct {
		Columns uint32
		Rows    uint32
		Width   uint32
		Height  uint32
	}
	if err := ssh.Unmarshal(payload, &msg); err != nil {
		return 0, 0
	}
	return msg.Columns, msg.Rows
}
