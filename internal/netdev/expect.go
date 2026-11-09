// expect.go 是提示符驱动的 expect 执行引擎（契约 2.8 约定 3 / 5.2B 行为）：
// 打开通道 →（仅 telnet）登录提示交互 → 等提示符 → 发分页关闭命令并丢弃
// 其输出 → 发命令 → 等提示符（轮询、总预算 Timeout）→ 清洗输出（剥首行
// 命令回显、剥尾提示符行、剥 --More-- 分页残留）→ 发 exit 关闭。
package netdev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"aiops/internal/store"
)

// ErrLoginFailed 是登录阶段失败（凭据错误/提示符未出现）。
var ErrLoginFailed = errors.New("netdev: device login failed")

// ErrExecFailed 是执行阶段失败（expect 超时/通道断开）。
var ErrExecFailed = errors.New("netdev: device command execution failed")

// expectSession 驱动一条已打开的通道完成一次登录 + 单命令执行。
type expectSession struct {
	pool   *Pool
	prof   *Profile
	ch     Channel
	telnet bool

	deadline time.Time // 总预算（req.Timeout）
	maxOut   int64

	// pump goroutine 把通道数据送入 dataCh；错误送入 errCh 后退出；
	// done 在会话收尾时关闭，解除 pump 在通道满时的发送阻塞。
	dataCh chan []byte
	errCh  chan error
	done   chan struct{}

	kept      []byte // 头部保留输出（maxOut 截断）
	tail      []byte // 尾部窗口（提示符/分页/登录提示判定，不受截断影响）
	truncated bool
}

// startExpect 构造并启动一个 expect 会话（读取泵随会话启动）。
func (p *Pool) startExpect(ctx context.Context, t Target, prof *Profile, ch Channel, budget time.Duration, maxOut int64) *expectSession {
	s := &expectSession{
		pool:     p,
		prof:     prof,
		ch:       ch,
		telnet:   t.AccessProto == store.AccessProtoTelnet,
		deadline: time.Now().Add(budget),
		maxOut:   maxOut,
		dataCh:   make(chan []byte, 64),
		errCh:    make(chan error, 1),
		done:     make(chan struct{}),
	}
	go s.pump()
	return s
}

func (s *expectSession) pump() {
	buf := make([]byte, 8192)
	for {
		n, err := s.ch.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case s.dataCh <- chunk:
			case <-s.done:
				return
			}
		}
		if err != nil {
			s.errCh <- err
			return
		}
	}
}

// sendLine 发送一行（CRLF 行尾）。
func (s *expectSession) sendLine(line string) error {
	_, err := s.ch.Write([]byte(line + "\r\n"))
	return err
}

// feed 把一块数据并入 kept/tail（截断只影响 kept 头部，判定不受影响）。
func (s *expectSession) feed(chunk []byte) {
	if s.maxOut <= 0 || int64(len(s.kept)) < s.maxOut {
		room := s.maxOut - int64(len(s.kept))
		if s.maxOut > 0 && int64(len(chunk)) > room {
			s.kept = append(s.kept, chunk[:room]...)
			s.truncated = true
		} else {
			s.kept = append(s.kept, chunk...)
		}
	} else {
		s.truncated = true // 已满：继续读取并丢弃
	}
	s.tail = append(s.tail, chunk...)
	if int64(len(s.tail)) > tailWindow {
		s.tail = s.tail[int64(len(s.tail))-tailWindow:]
	}
}

// wait 读取直到 cond 返回 true（cond 作用在当前 tail 上）；超时/通道断开
// 返回错误（含已收集内容供调用方判断阶段归属）。
func (s *expectSession) wait(cond func(tail []byte) bool) error {
	for {
		if cond(s.tail) {
			return nil
		}
		remain := time.Until(s.deadline)
		if remain <= 0 {
			return fmt.Errorf("expect timeout after total budget")
		}
		select {
		case chunk := <-s.dataCh:
			s.feed(chunk)
		case err := <-s.errCh:
			return fmt.Errorf("channel closed: %w", err)
		case <-time.After(remain):
			return fmt.Errorf("expect timeout after total budget")
		}
	}
}

// drainPager 在等待主条件期间自动续读分页提示（--More-- 厂商等价物）。
// 返回的 cond 包装主条件：分页提示命中时发送空格续读并剥残留。
func (s *expectSession) withPager(main func(tail []byte) bool) func(tail []byte) bool {
	return func(tail []byte) bool {
		if main(tail) {
			return true
		}
		if MoreRe.Match(tail) {
			_, _ = s.ch.Write([]byte(" "))
			if loc := MoreRe.FindIndex(tail); loc != nil {
				s.tail = append(s.tail[:loc[0]], s.tail[loc[1]:]...)
			}
			if loc := MoreRe.FindIndex(s.kept); loc != nil {
				s.kept = append(s.kept[:loc[0]], s.kept[loc[1]:]...)
			}
		}
		return false
	}
}

// matchTail 报告 tail 的最后一行是否匹配 re。
func matchTail(re *regexp.Regexp, tail []byte) bool {
	if len(tail) == 0 {
		return false
	}
	idx := bytes.LastIndexByte(tail, '\n')
	last := bytes.TrimRight(tail[idx+1:], "\r")
	if len(last) == 0 {
		return false
	}
	return re.Match(last)
}

// login 完成登录交互（仅 telnet；SSH 通道认证由 sshx 完成）并等待提示符。
func (s *expectSession) login(t Target) error {
	if !s.telnet {
		// SSH：直接等提示符（个别设备 SSH 后仍在 CLI 层要密码——按契约
		// 5.2B v1 不应答、按登录失败处理）。
		if err := s.wait(s.withPager(func(tail []byte) bool {
			return matchTail(s.prof.PromptRe, tail)
		})); err != nil {
			return fmt.Errorf("%w: %v", ErrLoginFailed, err)
		}
		return nil
	}
	// telnet：等 Username:/login:（个别设备直接要密码，二者先到先答）。
	if err := s.wait(func(tail []byte) bool {
		return matchTail(s.prof.LoginUserRe, tail) || matchTail(s.prof.LoginPassRe, tail)
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrLoginFailed, err)
	}
	if matchTail(s.prof.LoginUserRe, s.tail) {
		if err := s.sendLine(t.Username); err != nil {
			return fmt.Errorf("%w: %v", ErrLoginFailed, err)
		}
		// 清缓冲：登录提示是「等新提示出现」语义，残留的 Username: 不应
		// 干扰下一个等待。
		s.reset()
		if err := s.wait(func(tail []byte) bool {
			return matchTail(s.prof.LoginPassRe, tail)
		}); err != nil {
			return fmt.Errorf("%w: %v", ErrLoginFailed, err)
		}
	}
	if err := s.sendLine(t.Password); err != nil {
		return fmt.Errorf("%w: %v", ErrLoginFailed, err)
	}
	// 清缓冲：残留的 Password: 会使「凭据被拒」检测立即误触发——凭据
	// 被拒的形态是设备**重新**发出登录提示（契约 5.2B：登录阶段失败）。
	s.reset()
	// 等提示符；凭据被拒时设备会重发登录提示或断开——均按登录失败。
	if err := s.wait(s.withPager(func(tail []byte) bool {
		return matchTail(s.prof.PromptRe, tail) ||
			matchTail(s.prof.LoginUserRe, tail) || matchTail(s.prof.LoginPassRe, tail)
	})); err != nil {
		return fmt.Errorf("%w: %v", ErrLoginFailed, err)
	}
	if matchTail(s.prof.PromptRe, s.tail) {
		return nil
	}
	return fmt.Errorf("%w: credentials rejected (login prompt re-appeared)", ErrLoginFailed)
}

// disablePager 发送分页关闭命令并丢弃其输出（不支持的设备按普通输出处理，
// 失败不中断——契约 5.2B）。
func (s *expectSession) disablePager() {
	cmd := s.prof.PagerOffCmd
	if cmd == "" {
		return
	}
	_ = s.sendLine(cmd)
	_ = s.wait(func(tail []byte) bool {
		return matchTail(s.prof.PromptRe, tail)
	})
	s.reset()
}

// runCommand 发送单条命令并等待提示符，返回命令输出段（含清洗前原始内容
// 已在 kept/tail）。enable/super 命令触发的 Password: 提示按契约 5.2B：
// EnableSecret 非空自动应答一次；为空则原样返回并 ExitCode=1。
func (s *expectSession) runCommand(t Target, command string) (needExitCode1 bool, err error) {
	s.reset()
	if err := s.sendLine(command); err != nil {
		return false, fmt.Errorf("%w: %v", ErrExecFailed, err)
	}
	enable := s.prof.isEnableCommand(command)
	answered := false
	if err := s.wait(s.withPager(func(tail []byte) bool {
		if enable && !answered && t.EnableSecret != "" && matchTail(s.prof.LoginPassRe, tail) {
			// enable 的二级密码提示：自动应答一次（仅 enable/super 场景）。
			answered = true
			_ = s.sendLine(t.EnableSecret)
			return false
		}
		if enable && t.EnableSecret == "" && matchTail(s.prof.LoginPassRe, tail) {
			// 未配置 enable 密码：不注入任何输入，原样返回（ExitCode=1）。
			needExitCode1 = true
			return true
		}
		return matchTail(s.prof.PromptRe, tail)
	})); err != nil {
		return needExitCode1, fmt.Errorf("%w: %v", ErrExecFailed, err)
	}
	return needExitCode1, nil
}

// reset 清空收集缓冲（阶段之间不串数据）。
func (s *expectSession) reset() {
	s.kept = s.kept[:0]
	s.tail = s.tail[:0]
	s.truncated = false
}

// close 发送 exit 并关闭通道（尽力而为，错误不影响结果）；done 解除
// pump 的发送阻塞。
func (s *expectSession) close() {
	defer close(s.done)
	_ = s.sendLine("exit")
	// 给设备一个处理 exit 的宽限再关连接。
	deadline := time.Now().Add(closeGrace)
	for time.Now().Before(deadline) {
		select {
		case <-s.dataCh:
		case <-s.errCh:
			_ = s.ch.Close()
			return
		case <-time.After(50 * time.Millisecond):
			_ = s.ch.Close()
			return
		}
	}
	_ = s.ch.Close()
}

// cleanOutput 清洗命令输出（契约 5.2B）：剥首行命令回显（输出首行逐前缀
// 比对命令行，忽略大小写与两端空白；不匹配则保留）、剥尾提示符行、剥
// --More-- / ---- More ---- 分页残留。
func (s *expectSession) cleanOutput(command string) string {
	out := string(s.kept)

	// 剥分页残留（telnetx 层已剥过，SSH 通道在此兜底）。
	if loc := MoreRe.FindStringIndex(out); loc != nil {
		out = out[:loc[0]] + out[loc[1]:]
	}

	lines := strings.Split(out, "\n")
	// 剥尾提示符行：末段匹配 PromptRe 即剥。
	if n := len(lines); n > 0 && s.prof.PromptRe.MatchString(strings.TrimRight(lines[n-1], "\r")) {
		lines = lines[:n-1]
	}
	// 剥首行命令回显（忽略大小写与两端空白的前缀比对）。
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		cmd := strings.TrimSpace(command)
		if cmd != "" && strings.HasPrefix(strings.ToLower(first), strings.ToLower(cmd)) {
			lines = lines[1:]
		}
	}
	return strings.Trim(strings.Join(lines, "\n"), "\r\n")
}

// hitError 报告清洗后输出是否命中设备错误指示。
func (s *expectSession) hitError(out string) bool {
	return s.prof.ErrorRe != nil && s.prof.ErrorRe.MatchString(out)
}
