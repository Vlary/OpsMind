// W3：交互终端 PTY 会话（CONTRACT.md 5.2）。
//
// Session 在独立的 SSH 连接上请求 pty-req + shell（不设 exec）；连接不进入
// 连接池的空闲列表，由调用方（termx）Close。全局并发计入 Pool 的信号量。
// 本文件只新增能力，不修改 Executor 既有方法的行为。
package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// PTY 终端类型与窗口范围（契约 5.2/5.13）。
const (
	ptyTerm    = "xterm-256color"
	minPTYCols = 10
	maxPTYCols = 500
	minPTYRows = 4
	maxPTYRows = 200
)

// ErrInvalidPTYWindow 表示请求的 PTY 窗口尺寸越界。
var ErrInvalidPTYWindow = errors.New("sshx: invalid pty window")

// SessionRequest 是交互 PTY 请求参数。
type SessionRequest struct {
	Cols int    // 10..500
	Rows int    // 4..200
	Term string // 调用方恒传 "xterm-256color"
}

// PTYSession 是一个交互终端会话；stdout 与 stderr 合并为单 reader。
type PTYSession interface {
	Write(p []byte) (int, error) // 用户输入 → PTY stdin
	Read(p []byte) (int, error)  // PTY 输出（stdout+stderr 合并；单 reader）
	Resize(cols, rows int) error // ssh-pty window-change；失败仅记日志不终止会话
	Close() error                // 关闭 channel 与底层连接；幂等
	Wait() error                 // 远端退出后返回（Close 触发返回 nil）
}

// PTYOpener 是需要 PTY 能力的调用方（termx）使用的窄接口。
//
// Executor 接口按契约 5.2 定义含 Session；在接口文件更新前，*Pool 已实现
// 本接口，调用方以类型断言取得。
type PTYOpener interface {
	Session(ctx context.Context, target Target, req SessionRequest) (PTYSession, error)
}

// Session 打开交互 PTY：独立拨号（计入全局信号量），请求 pty-req + shell。
func (p *Pool) Session(ctx context.Context, target Target, req SessionRequest) (PTYSession, error) {
	if req.Cols < minPTYCols || req.Cols > maxPTYCols || req.Rows < minPTYRows || req.Rows > maxPTYRows {
		return nil, fmt.Errorf("%w: cols=%d rows=%d", ErrInvalidPTYWindow, req.Cols, req.Rows)
	}
	term := req.Term
	if term == "" {
		term = ptyTerm
	}

	// PTY 是有状态的长会话：不复用池化连接，独立拨号；rawConn 持有全局
	// 信号量配额，Close 时释放，空闲回收（reapIdle）触不到它。
	rc, _, err := p.dial(ctx, target)
	if err != nil {
		return nil, err
	}

	sess, err := rc.client.NewSession()
	if err != nil {
		rc.close()
		return nil, fmt.Errorf("sshx: open pty session: %w", err)
	}

	// stdin 必须在 Shell 之前取出（x/crypto/ssh：started 后 StdinPipe 报错）。
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		rc.close()
		return nil, fmt.Errorf("sshx: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		rc.close()
		return nil, fmt.Errorf("sshx: stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		rc.close()
		return nil, fmt.Errorf("sshx: stderr pipe: %w", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:   1,
		ssh.ICANON: 1,
		ssh.ISIG:   1,
	}
	if err := sess.RequestPty(term, req.Rows, req.Cols, modes); err != nil {
		_ = sess.Close()
		rc.close()
		return nil, fmt.Errorf("sshx: request pty: %w", err)
	}

	// stdout+stderr 合并为单 reader（契约 5.2：PTYSession.Read 单 reader）。
	pr, pw := io.Pipe()
	var mergeWG sync.WaitGroup
	mergeWG.Add(2)
	go func() {
		defer mergeWG.Done()
		_, _ = io.Copy(pw, stdout)
	}()
	go func() {
		defer mergeWG.Done()
		_, _ = io.Copy(pw, stderr)
	}()

	if err := sess.Shell(); err != nil {
		_ = pw.Close()
		_ = sess.Close()
		rc.close()
		return nil, fmt.Errorf("sshx: start shell: %w", err)
	}

	ps := &ptySession{
		raw:      rc,
		sess:     sess,
		stdin:    stdin,
		pr:       pr,
		pw:       pw,
		mergeWG:  &mergeWG,
		waitDone: make(chan struct{}),
	}
	go ps.waitLoop()
	return ps, nil
}

// ptySession 实现 PTYSession。
type ptySession struct {
	raw      *rawConn
	sess     *ssh.Session
	stdin    io.WriteCloser
	pr       *io.PipeReader
	pw       *io.PipeWriter
	mergeWG  *sync.WaitGroup
	waitDone chan struct{}

	closeOnce sync.Once
}

// Read 读取 PTY 输出（stdout+stderr 合并）。
func (s *ptySession) Read(p []byte) (int, error) {
	return s.pr.Read(p)
}

// Write 把用户输入写入 PTY stdin。
func (s *ptySession) Write(p []byte) (int, error) {
	return s.stdin.Write(p)
}

// Resize 发送 window-change 请求。
func (s *ptySession) Resize(cols, rows int) error {
	if cols < minPTYCols || cols > maxPTYCols || rows < minPTYRows || rows > maxPTYRows {
		return fmt.Errorf("%w: cols=%d rows=%d", ErrInvalidPTYWindow, cols, rows)
	}
	// x/crypto/ssh 的参数顺序是 (height, width)。
	return s.sess.WindowChange(rows, cols)
}

// Wait 阻塞直到远端 shell 退出或会话被 Close。
func (s *ptySession) Wait() error {
	<-s.waitDone
	return nil
}

// waitLoop 等待 shell 退出并解除 Read 阻塞，随后关闭底层连接。
func (s *ptySession) waitLoop() {
	_ = s.sess.Wait()
	// shell 退出：关闭合并管道写端（Read 返回 EOF），唤醒所有等待者。
	_ = s.pw.Close()
	s.mergeWG.Wait()
	s.raw.close()
	close(s.waitDone)
}

// Close 关闭 PTY 与底层连接；幂等。
func (s *ptySession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.sess.Close()
		// 关闭管道写端解除 Read 阻塞；waitLoop 随 sess.Wait 返回后收尾
		// （close(waitDone)），这里最多等它 3 秒。
		_ = s.pw.Close()
		s.raw.close()
		select {
		case <-s.waitDone:
		case <-time.After(3 * time.Second):
			// waitLoop 异常卡死时也不阻塞调用方（治理收尾有总时限）。
		}
	})
	return err
}

var _ PTYSession = (*ptySession)(nil)
var _ PTYOpener = (*Pool)(nil)
