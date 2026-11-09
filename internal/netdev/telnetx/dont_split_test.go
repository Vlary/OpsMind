package telnetx

import "testing"

// drainedNonBlocking 读出 stream 缓冲中的全部字节（测试辅助，不阻塞）。
func drainedNonBlocking(t *testing.T, s *stream) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]byte(nil), s.buf...)
	s.buf = nil
	return out
}

// DONT/WONT 跨 TCP 块时选项字节不得混入数据流（评审实证回归：
// 第一块 [O,K,IAC,DONT] + 第二块 [0x18,'X']，修复前 0x18 被当数据）。
func TestFilterDontWontSplitAcrossChunks(t *testing.T) {
	c := &Conn{st: newStream()} // dont/wont 无需应答，不触碰 c.conn
	inSub := false

	pending := c.filter([]byte{'O', 'K', iac, dont}, &inSub, nil)
	if got := string(drainedNonBlocking(t, c.st)); got != "OK" {
		t.Fatalf("first chunk output = %q, want OK", got)
	}
	if len(pending) != 2 || pending[0] != iac || pending[1] != dont {
		t.Fatalf("pending = %v, want [IAC DONT]", pending)
	}

	pending = c.filter([]byte{0x18, 'X'}, &inSub, pending)
	if got := string(drainedNonBlocking(t, c.st)); got != "X" {
		t.Fatalf("second chunk output = %q, want X（0x18 是 DONT 选项字节，必须被吞掉）", got)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after second chunk = %v, want empty", pending)
	}
}

// 同一 WONT：暂存后与后续选项字节合并解析，选项字节不进数据流。
func TestFilterWontSplitAcrossChunks(t *testing.T) {
	c := &Conn{st: newStream()}
	inSub := false
	pending := c.filter([]byte{'A', iac, wont}, &inSub, nil)
	if got := string(drainedNonBlocking(t, c.st)); got != "A" {
		t.Fatalf("first chunk output = %q, want A", got)
	}
	pending = c.filter([]byte{0x27, 'B'}, &inSub, pending)
	if got := string(drainedNonBlocking(t, c.st)); got != "B" {
		t.Fatalf("second chunk output = %q, want B", got)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want empty", pending)
	}
}
