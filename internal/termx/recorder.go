// Package termx 实现堡垒机式终端会话治理：SSH PTY ↔ WebSocket 桥、
// asciinema v2 录制、空闲/时长治理与审计接入（CONTRACT.md 5.13/6.4）。
package termx

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 录制常量（契约 5.13）：
//   - 首行 header {"version":2,"width":cols,"height":rows}
//   - 事件仅 o（输出）与 r（resize，data="COLSxROWS"）
//   - 时间为自会话开始的 float 秒（UnixNano 差值）
//   - 文件 <RecordingsDir>/<session_id>.cast，0600
//   - 每 ≤FramesPerTick 事件 flush 一次；Close 时 sync
const (
	defaultFramesPerTick = 64
	recorderFilePerm     = 0o600
	recorderDirPerm      = 0o750
	castFileSuffix       = ".cast"
)

// castHeader 是 .cast 首行（asciinema v2）。
type castHeader struct {
	Version int `json:"version"`
	Width   int `json:"width"`
	Height  int `json:"height"`
}

// castEvent 是一行 NDJSON 事件：[elapsedSeconds, "o"|"r", data]。
type castEvent [3]any

// Recorder 把终端输出流写成 asciinema v2 NDJSON。
//
// 写入失败不中断会话：首次失败后进入降级模式（丢弃后续事件），
// 由调用方据 Failed() 判定 has_recording=false。
type Recorder struct {
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	startNS int64
	pending int // 自上次 flush 的事件数
	frames  int // 每 tick 的事件上限
	bytes   int64
	failed  bool
	closed  bool
}

// NewRecorder 创建 <dir>/<sessionID>.cast 并写 header。
//
// sessionID 必须是会话 UUID（文件名即 UUID，不可预测，无路径拼接）；
// 由调用方（Manager）保证，本函数不再拼接任何用户输入。
func NewRecorder(dir, sessionID string, cols, rows int, now time.Time) (*Recorder, error) {
	if err := os.MkdirAll(dir, recorderDirPerm); err != nil {
		return nil, fmt.Errorf("termx: create recordings dir: %w", err)
	}
	path := filepath.Join(dir, sessionID+castFileSuffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, recorderFilePerm)
	if err != nil {
		return nil, fmt.Errorf("termx: open recording: %w", err)
	}
	r := &Recorder{
		f:       f,
		w:       bufio.NewWriterSize(f, 16*1024),
		startNS: now.UnixNano(),
		frames:  defaultFramesPerTick,
	}
	header, err := json.Marshal(castHeader{Version: 2, Width: cols, Height: rows})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := r.writeLine(header); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// elapsedSeconds 返回相对会话开始的 float 秒（UnixNano 差值）。
func (r *Recorder) elapsedSeconds(now time.Time) float64 {
	return float64(now.UnixNano()-r.startNS) / float64(time.Second)
}

// writeLine 写一行 JSON 并累计字节数（含换行）。
func (r *Recorder) writeLine(line []byte) error {
	n, err := r.w.Write(line)
	r.bytes += int64(n)
	if err != nil {
		return err
	}
	n, err = r.w.WriteString("\n")
	r.bytes += int64(n)
	return err
}

// Output 记录一个 o 事件（PTY 输出原始字节）。
func (r *Recorder) Output(now time.Time, data []byte) {
	if len(data) == 0 {
		return
	}
	r.record(now, "o", string(data))
}

// Resize 记录一个 r 事件（data="COLSxROWS"）。
func (r *Recorder) Resize(now time.Time, cols, rows int) {
	r.record(now, "r", fmt.Sprintf("%dx%d", cols, rows))
}

// record 序列化并写一行事件；失败进入降级模式。
func (r *Recorder) record(now time.Time, kind, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed || r.closed {
		return
	}
	elapsed := r.elapsedSeconds(now)
	line, err := json.Marshal(castEvent{elapsed, kind, data})
	if err != nil { // 不应发生（string/float 组合）
		r.failed = true
		return
	}
	if err := r.writeLine(line); err != nil {
		r.failed = true
		return
	}
	r.pending++
}

// Flush 把缓冲写盘；每 ≤frames 个事件调用一次（由治理循环每秒驱动）。
func (r *Recorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed || r.closed || r.pending == 0 {
		return
	}
	if err := r.w.Flush(); err != nil {
		r.failed = true
		return
	}
	r.pending = 0
}

// ShouldFlush 报告是否达到写盘 tick 的事件阈值。
func (r *Recorder) ShouldFlush() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending >= r.frames
}

// Close 封盘：flush + sync + close，返回累计写入字节数。
// 失败时返回已累计字节与错误（调用方据此置 has_recording=false）。
func (r *Recorder) Close() (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.bytes, nil
	}
	r.closed = true
	if r.failed {
		_ = r.f.Close()
		return 0, fmt.Errorf("termx: recording degraded")
	}
	flushErr := r.w.Flush()
	syncErr := r.f.Sync()
	closeErr := r.f.Close()
	if flushErr != nil || syncErr != nil || closeErr != nil {
		return 0, fmt.Errorf("termx: close recording: flush=%v sync=%v close=%v",
			flushErr, syncErr, closeErr)
	}
	return r.bytes, nil
}

// Bytes 返回累计写入字节数（含未 flush 的缓冲）。
func (r *Recorder) Bytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Failed 报告录制是否已降级（写失败，不再产生有效文件）。
func (r *Recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}
