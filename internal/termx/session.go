// W3：终端会话的 WS 帧类型、PTY↔WS 桥与输入摘要（CONTRACT.md 5.13/6.4）。
package termx

import (
	"encoding/base64"
	"strings"
	"sync"
	"unicode/utf8"
)

// WS 帧类型常量（OpenAPI TermFrame*）。
const (
	frameTypeInput   = "input"
	frameTypeResize  = "resize"
	frameTypeOutput  = "output"
	frameTypeResized = "resized"
	frameTypeError   = "error"
	frameTypeClosed  = "closed"
)

// PTY 窗口范围（契约 5.13：resize 越界 400 帧不断开）。
const (
	minCols = 10
	maxCols = 500
	minRows = 4
	maxRows = 200

	// 单条 input 解码后 >8KB 断开（error 帧 COMMAND_TOO_LARGE 后
	// closed reason=error）。8KB 原始字节的 base64 上限为 10924。
	maxInputBytes = 8 * 1024
)

// 会话结束原因（契约 4.13 ended_reason 枚举）。
const (
	reasonClientClosed   = "client_closed"
	reasonIdleTimeout    = "idle_timeout"
	reasonMaxDuration    = "max_duration"
	reasonServerShutdown = "server_shutdown"
	reasonError          = "error"
)

// Frame 是服务端→客户端的 WS 帧（单行 JSON 文本帧）。
type Frame struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// clientFrame 是客户端→服务端帧的解码目标；字段缺失以零值呈现。
type clientFrame struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// outputFrame 构造 PTY 输出帧。
func outputFrame(data []byte) Frame {
	return Frame{Type: frameTypeOutput, Data: base64.StdEncoding.EncodeToString(data)}
}

// resizedFrame 构造 resize 确认帧。
func resizedFrame(cols, rows int) Frame {
	return Frame{Type: frameTypeResized, Cols: cols, Rows: rows}
}

// errorFrame 构造错误帧。
func errorFrame(code, message string) Frame {
	return Frame{Type: frameTypeError, Code: code, Message: message}
}

// closedFrame 构造关闭帧（最后一帧）。
func closedFrame(reason string) Frame {
	return Frame{Type: frameTypeClosed, Reason: reason}
}

// validWindowSize 校验 PTY 窗口范围。
func validWindowSize(cols, rows int) bool {
	return cols >= minCols && cols <= maxCols && rows >= minRows && rows <= maxRows
}

// inputSummary 累积终端输入行摘要（契约 5.13）：
//   - WS input 帧的原始字节按 \r/\n 切分为输入行（PTY 回显不去重）；
//     未以 \r/\n 结尾的尾部片段跨帧缓冲在 pending，与下一帧数据拼接后
//     再切行——浏览器 xterm 每次按键发一帧，无跨帧缓冲会把一条命令
//     拆成单字符行
//   - 每行截 512 字节、最多 1000 行，超限置 truncated
//   - 控制序列按不可打印字符替换为 \ufffd，原文不落审计
type inputSummary struct {
	mu        sync.Mutex
	lines     []string
	truncated bool
	pending   []byte
}

const (
	summaryMaxLineBytes = 512
	summaryMaxLines     = 1000
	// pending 缓冲上限（同单帧 input 上限）：超长无换行输入（如粘贴
	// 单行超长文本）达到上限即切分 flush 为一行，防止缓冲随输入无界增长。
	summaryMaxPendingBytes = maxInputBytes
)

// appendLine 追加一条已终结的输入行；行数已满时置 truncated 并返回 false。
func (s *inputSummary) appendLine(line []byte) bool {
	if len(s.lines) >= summaryMaxLines {
		s.truncated = true
		return false
	}
	s.lines = append(s.lines, sanitizeInputLine(line))
	return true
}

// add 追加一段原始输入字节：先与上一帧遗留的 pending 拼接再切行，
// 未以 \r/\n 结尾的尾部片段留在 pending 等待下一帧或 snapshot() 呈现。
func (s *inputSummary) add(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := make([]byte, 0, len(s.pending)+len(data))
	buf = append(buf, s.pending...)
	buf = append(buf, data...)

	parts := splitInputLines(buf)
	s.pending = s.pending[:0]
	if n := len(buf); n > 0 && buf[n-1] != '\r' && buf[n-1] != '\n' && len(parts) > 0 {
		s.pending = append(s.pending, parts[len(parts)-1]...)
		parts = parts[:len(parts)-1]
	}
	for _, line := range parts {
		if !s.appendLine(line) {
			// 摘要已封顶：不再缓存尾部片段（有界内存，语义无损——
			// 满 1000 行后本就不再展示后续输入）。
			s.pending = s.pending[:0]
			return
		}
	}
	if len(s.pending) >= summaryMaxPendingBytes {
		s.appendLine(s.pending) // 行满时仅置 truncated
		s.pending = s.pending[:0]
	}
}

// splitInputLines 按字节序把原始输入切成行（保留 \r 前缀语义：仅以
// \r 与 \n 为界，连续分隔符产生空行并被丢弃）。
func splitInputLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\r' || b == '\n' {
			if i > start {
				out = append(out, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

// sanitizeInputLine 截断到 512 字节并把控制字符/控制序列替换为 \ufffd；
// 非法 UTF-8 序列同样替换（原文绝不落审计）。
//
// 控制序列（契约 5.13）指 ANSI 转义序列：ESC [ 参数... 终止字节（CSI）
// 与 ESC + 单字符；整段替换为一个 \ufffd 概要标记。
func sanitizeInputLine(line []byte) string {
	if len(line) > summaryMaxLineBytes {
		line = line[:summaryMaxLineBytes]
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		c := line[i]
		if c == 0x1b { // ESC：吞掉一个转义序列。
			i += escSeqLen(line[i:])
			b.WriteRune('\ufffd')
			continue
		}
		r, size := utf8.DecodeRune(line[i:])
		if r == utf8.RuneError && size <= 1 {
			// 非法 UTF-8 字节：替换并前进 1 字节。
			b.WriteRune('\ufffd')
			i++
			continue
		}
		if r < 0x20 || r == 0x7f {
			// 控制字符概要展示。
			b.WriteRune('\ufffd')
			i += size
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

// escSeqLen 返回从 ESC 开始的转义序列长度（至少 2）：
// CSI（ESC [ ... 0x40..0x7E 终止）按参数计数；其余按 ESC + 1 字符。
func escSeqLen(seq []byte) int {
	if len(seq) < 2 {
		return len(seq)
	}
	if seq[1] != '[' { // 非 CSI：ESC + 单字符。
		return 2
	}
	for j := 2; j < len(seq); j++ {
		if seq[j] >= 0x40 && seq[j] <= 0x7e {
			return j + 1
		}
	}
	return len(seq) // 未终结的 CSI：剩余全部。
}

// snapshot 返回（lines, truncated, lineCount）；未终结的 pending 作为
// 最后一行一并呈现（不修改内部状态，快照幂等）。detail 装配用。
func (s *inputSummary) snapshot() (lines []string, truncated bool, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines), len(s.lines)+1)
	copy(out, s.lines)
	if len(s.pending) > 0 {
		if len(out) < summaryMaxLines {
			out = append(out, sanitizeInputLine(s.pending))
		} else {
			truncated = true // 行已满，pending 无法呈现
		}
	}
	return out, truncated || s.truncated, len(out)
}
