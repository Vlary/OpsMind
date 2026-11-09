// Package telnetx 是极简 Telnet 客户端（裸 TCP 实现，N1）。
//
// 承载网络设备（契约 2.8/5.2B）的 Telnet 接入通道：
//   - 协商：RFC 854 IAC 命令解析与应答（DO→WONT、WILL→DONT，拒绝一切
//     本地/远端选项；子协商丢弃），数据流过滤为纯净输出；
//   - 提示符检测：带超时的正则 expect（Expect）；
//   - 分页自动续读：ExpectPrompt 在等待提示符期间检测 `--More--` 与厂商
//     等价物并自动发送续读按键（默认空格）；
//   - 超时：全部等待受 deadline 约束，超时返回 ErrTimeout；
//   - 输出截断：超过 maxBytes 后继续读取并丢弃（避免设备端管道写满阻塞），
//     标记 truncated；
//   - 明文告警：Telnet 明文传输登录与 enable 密码——CleartextWarning
//     常量由上层（创建确认、审计、结果透传）引用，见契约 2.6/2.8。
package telnetx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"time"
)

// CleartextWarning 是 Telnet 明文风险的固定告警文案（契约 2.6：telnet 明文
// 传输登录与 enable 密码；上层创建/更新须显式确认，结果与审计透传本告警）。
const CleartextWarning = "telnet transmits the login password and enable secret in cleartext; prefer ssh when the device supports it"

// ErrTimeout 表示等待提示符/数据超时。
var ErrTimeout = errors.New("telnetx: expect timeout")

// ErrClosed 表示连接已关闭。
var ErrClosed = errors.New("telnetx: connection closed")

// errReadTimeout 是管道读的内部超时哨兵（ExpectPrompt 转为 ErrTimeout）。
var errReadTimeout = errors.New("telnetx: read deadline exceeded")

// Config 是连接配置；零值字段取默认。
type Config struct {
	DialTimeout time.Duration // 建立 TCP 连接的超时；缺省 10s
}

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	return c
}

// stream 是后台 reader 到消费者的缓冲管道：写端无条件消费（协商清洗后的
// 数据绝不为等读者而阻塞），读端支持带超时的阻塞读。
type stream struct {
	mu  sync.Mutex
	buf []byte
	err error
	// truncated 在 buf 触达上限丢弃最旧数据后置位；消费端可据此提示输出不完整。
	truncated bool
	notify    chan struct{} // 容量 1 的唤醒信号
}

// streamMaxBuf 限制单个 telnet 会话流的未读缓冲上限：消费端长时间不读
// （如等待永不出现的提示符、对端洪水输出）时防止无界增长打爆进程内存，
// 超限丢最旧数据（生产者继续，截断语义与 expect 层 maxOut 一致）。
const streamMaxBuf = 4 << 20 // 4MiB

func newStream() *stream { return &stream{notify: make(chan struct{}, 1)} }

func (s *stream) write(b []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, b...)
	if len(s.buf) > streamMaxBuf {
		s.buf = s.buf[len(s.buf)-streamMaxBuf:]
		s.truncated = true
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *stream) closeWithError(err error) {
	s.mu.Lock()
	if s.err == nil {
		if err == nil {
			err = ErrClosed
		}
		s.err = err
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// read 读取数据；timeout > 0 时超时返回 errReadTimeout（已读数据保留）。
func (s *stream) read(out []byte, timeout time.Duration) (int, error) {
	for {
		s.mu.Lock()
		if len(s.buf) > 0 {
			n := copy(out, s.buf)
			s.buf = s.buf[n:]
			s.mu.Unlock()
			return n, nil
		}
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if timeout <= 0 {
			<-s.notify
			continue
		}
		select {
		case <-s.notify:
		case <-time.After(timeout):
			return 0, errReadTimeout
		}
	}
}

// Conn 是一条已协商的 Telnet 连接；Read 返回 IAC 清洗后的数据流。
type Conn struct {
	conn net.Conn
	cfg  Config

	st *stream

	closeOnce sync.Once
}

// Dial 建立 TCP 连接并启动协商 reader。
func Dial(ctx context.Context, host string, port int, cfg Config) (*Conn, error) {
	cfg = cfg.withDefaults()
	if host == "" || port <= 0 {
		return nil, fmt.Errorf("telnetx: host and port are required")
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	dialer := net.Dialer{Timeout: cfg.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("telnetx: dial %s: %w", addr, err)
	}
	c := &Conn{conn: conn, cfg: cfg, st: newStream()}
	go c.readLoop()
	return c, nil
}

// Telnet 协议常量（RFC 854）。
const (
	iac  = 255 // Interpret As Command
	dont = 254
	do   = 253
	wont = 252
	will = 251
	sb   = 250 // Subnegotiation Begin
	se   = 240 // Subnegotiation End
)

// readLoop 持续读取并解析 IAC 命令：数据进管道，协商按拒绝策略应答。
func (c *Conn) readLoop() {
	raw := make([]byte, 4096)
	var pending []byte // 跨块的半个 IAC 序列暂存
	var inSub bool
	for {
		n, err := c.conn.Read(raw)
		if n > 0 {
			pending = c.filter(raw[:n], &inSub, pending)
		}
		if err != nil {
			c.st.closeWithError(err)
			return
		}
	}
}

// filter 解析一块原始字节：清洗 IAC 命令并按需回写协商应答。
func (c *Conn) filter(chunk []byte, inSub *bool, pending []byte) []byte {
	var out []byte
	buf := append(pending, chunk...)
	pending = pending[:0]
	i := 0
	reply := func(b ...byte) { _, _ = c.conn.Write(b) }
	// parseLoop 标签：块尾遇到不完整 IAC 序列时必须结束本轮解析（残片留给
	// 下一块）。switch 内的裸 break 只退出 switch、i 不前进，会造成死循环
	// 且 pending 无界增长（实测测试进程膨胀 >20GB 触发全局 OOM，两次）。
parseLoop:
	for i < len(buf) {
		b := buf[i]
		if b != iac {
			if !*inSub {
				out = append(out, b)
			}
			i++
			continue
		}
		if i+1 >= len(buf) { // 块尾半个 IAC：留给下一块
			pending = append(pending, iac)
			break
		}
		cmd := buf[i+1]
		switch cmd {
		case iac: // IAC IAC → 数据 0xFF
			if !*inSub {
				out = append(out, iac)
			}
			i += 2
		case do:
			if i+2 >= len(buf) {
				pending = append(pending, iac, do)
				break parseLoop
			}
			reply(iac, wont, buf[i+2]) // 拒绝一切本地选项
			i += 3
		case will:
			if i+2 >= len(buf) {
				pending = append(pending, iac, will)
				break parseLoop
			}
			reply(iac, dont, buf[i+2]) // 拒绝一切远端选项
			i += 3
		case dont, wont:
			// 跨块处理与 do/will 同构：块尾只剩 [IAC, DONT] 时暂存等待
			// 下一块的选项字节，否则该字节会被当作数据写进输出流
			// （评审实证：[O,K,IAC,DONT]+[0x18,'X'] 输出混入 0x18）。
			if i+2 >= len(buf) {
				pending = append(pending, iac, cmd)
				break parseLoop
			}
			i += 3 // 无需应答
		case sb:
			*inSub = true
			i += 2
		default: // NOP/AYT/DM 等：无业务语义；子协商结束符复位
			if *inSub && cmd == se {
				*inSub = false
			}
			i += 2
		}
	}
	if len(out) > 0 {
		c.st.write(out)
	}
	return pending
}

// Read 读取 IAC 清洗后的数据（阻塞，无超时——expect 语义用 Expect*）。
func (c *Conn) Read(p []byte) (int, error) { return c.st.read(p, 0) }

// Write 发送原始数据。
func (c *Conn) Write(p []byte) (int, error) { return c.conn.Write(p) }

// Close 关闭连接；幂等。
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.conn.Close()
		c.st.closeWithError(ErrClosed)
	})
	return err
}

// SendLine 发送一行（CRLF 行尾，NVT 语义）。
func (c *Conn) SendLine(line string) error {
	_, err := c.conn.Write(append([]byte(line), '\r', '\n'))
	return err
}

// ---------------------------------------------------------------------------
// expect：提示符检测 / 分页自动续读 / 超时 / 截断
// ---------------------------------------------------------------------------

// DefaultMoreRe 匹配主流厂商的分页提示：Cisco `--More--`、
// Huawei/H3C `---- More ----`（契约 5.2B 清洗行）。
var DefaultMoreRe = regexp.MustCompile(`-{2,4}\s*More\s*-{2,4}`)

// Expect 读取直到 promptRe 命中，返回命中前的全部输出；超时返回
// ErrTimeout（附带已读内容）。
func (c *Conn) Expect(promptRe *regexp.Regexp, timeout time.Duration) (string, error) {
	out, _, err := c.ExpectPrompt(promptRe, nil, "", timeout, 0)
	return out, err
}

// ExpectPrompt 读取直到 promptRe 命中；期间检测到分页提示（moreRe 非空时）
// 自动发送 moreKey（缺省空格）续读；输出超过 maxBytes（>0 时）后继续读取
// 并丢弃，返回 truncated=true（避免设备端管道写满阻塞）。
//
// 提示符与分页判定在独立的尾部窗口上进行（保留最近 tailWindow 字节）：
// 头部截断不能吞掉提示符，否则 expect 永不终止。
// 提示符按「最后一行匹配」判定：正则应锚定行尾（如 `…[>#]\s?$`）；
// 以换行结尾的中间行不会误判。
func (c *Conn) ExpectPrompt(promptRe, moreRe *regexp.Regexp, moreKey string, timeout time.Duration, maxBytes int64) (output string, truncated bool, err error) {
	if moreKey == "" {
		moreKey = " "
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)

	var kept []byte
	var tail []byte
	chunk := make([]byte, 8192)

	for {
		if tailMatch(promptRe, tail) {
			return string(kept), truncated, nil
		}
		// 分页自动续读：分页提示出现且提示符未到 → 发送续读按键，
		// 并从 kept/tail 中剥掉分页残留（契约 5.2B 清洗行）。
		if moreRe != nil && moreRe.Match(tail) {
			if _, werr := c.conn.Write([]byte(moreKey)); werr != nil {
				return string(kept), truncated, werr
			}
			if loc := moreRe.FindIndex(tail); loc != nil {
				tail = append(tail[:loc[0]], tail[loc[1]:]...)
			}
			if loc := moreRe.FindIndex(kept); loc != nil {
				kept = append(kept[:loc[0]], kept[loc[1]:]...)
			}
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return string(kept), truncated, fmt.Errorf("%w after %s", ErrTimeout, timeout)
		}
		n, rerr := c.st.read(chunk, remain)
		if n > 0 {
			data := chunk[:n]
			if maxBytes <= 0 || int64(len(kept)) < maxBytes {
				room := maxBytes - int64(len(kept))
				if maxBytes > 0 && int64(n) > room {
					kept = append(kept, data[:room]...)
					truncated = true
				} else {
					kept = append(kept, data...)
				}
			} else {
				truncated = true // 已满：头部继续读并丢弃
			}
			// 尾部窗口滚动追加（提示符/分页判定不受头部截断影响）。
			tail = append(tail, data...)
			if int64(len(tail)) > tailWindow {
				tail = tail[int64(len(tail))-tailWindow:]
			}
			continue
		}
		if rerr != nil {
			if errors.Is(rerr, errReadTimeout) {
				return string(kept), truncated, fmt.Errorf("%w after %s", ErrTimeout, timeout)
			}
			return string(kept), truncated, rerr
		}
	}
}

// tailWindow 是提示符/分页判定的尾部窗口大小（提示符行最长不超过此值）。
const tailWindow = 1024

// tailMatch 报告 buf 的最后一行（最后一个 \n 之后）是否匹配 re；
// 以换行结尾的缓冲（提示符尚未到达）不匹配。
func tailMatch(re *regexp.Regexp, buf []byte) bool {
	if len(buf) == 0 {
		return false
	}
	idx := bytes.LastIndexByte(buf, '\n')
	last := bytes.TrimRight(buf[idx+1:], "\r")
	if len(last) == 0 {
		return false
	}
	return re.Match(last)
}
