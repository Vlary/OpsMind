// Package sshx 是 SSH 连接池与命令执行器。
//
// 连接按 ServerID 分桶，桶内最多 4 条连接、空闲 60s 回收；全局最多 64 条。
// 主机密钥遵循 TOFU：首次连接接受并返回密钥，之后按 authorized_keys 行逐字节
// 比较，不一致返回 *HostKeyError。禁止使用 ssh.InsecureIgnoreHostKey。
package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/semaphore"

	"aiops/internal/store"
)

// 默认与上限（CONTRACT.md 5.2）。
const (
	defaultTimeout    = 60 * time.Second
	maxTimeout        = 300 * time.Second
	defaultMaxOutput  = int64(262144)
	maxMaxOutput      = int64(262144)
	defaultPortNumber = 22
)

// ErrNotImplemented 保留以兼容骨架阶段调用方；新代码不应依赖它。
var ErrNotImplemented = errors.New("sshx: not implemented")

// ErrCredentialMissing 表示目标缺少所需的凭据。
var ErrCredentialMissing = errors.New("sshx: credential missing")

// ErrPoolClosed 表示连接池已关闭。
var ErrPoolClosed = errors.New("sshx: pool closed")

// Target 是一次 SSH 连接目标。
type Target struct {
	ServerID     string
	Host         string
	Port         int
	Username     string
	AuthMethod   store.AuthMethod
	Password     string
	PrivateKey   string
	Passphrase   string
	KnownHostKey string // 已信任的 authorized_keys 行；空表示 TOFU
}

// TestResult 是连接测试结果。
type TestResult struct {
	LatencyMS     int64  `json:"latency_ms"`
	ServerVersion string `json:"server_version"`
	Fingerprint   string `json:"fingerprint"`
	HostKey       string `json:"-"`
	FirstSeen     bool   `json:"first_seen"`
}

// RunRequest 是执行请求。
type RunRequest struct {
	Command        string
	Timeout        time.Duration
	MaxOutputBytes int64
}

// RunResult 是执行结果。
type RunResult struct {
	ExitCode    int    `json:"exit_code"`
	Combined    string `json:"output"`
	Truncated   bool   `json:"truncated"`
	DurationMS  int64  `json:"duration_ms"`
	Fingerprint string `json:"-"`
	HostKey     string `json:"-"`
	FirstSeen   bool   `json:"-"`
}

// HostKeyError 表示已信任主机密钥与远端不一致。
type HostKeyError struct {
	ServerID string
	Expected string
	Actual   string
}

func (e *HostKeyError) Error() string {
	return "sshx: host key changed for server " + e.ServerID
}

// Executor 是 SSH 执行器接口。
type Executor interface {
	Test(ctx context.Context, target Target) (TestResult, error)
	Execute(ctx context.Context, target Target, req RunRequest) (RunResult, error)
	Invalidate(serverID string)
	Close() error
}

// Config 是连接池配置。
type Config struct {
	MaxPerServer     int
	IdleTimeout      time.Duration
	MaxConnections   int
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
}

// Pool 实现 Executor。
type Pool struct {
	cfg    Config
	log    *slog.Logger
	global *semaphore.Weighted

	mu      sync.Mutex
	buckets map[string]*bucket
	closed  bool

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// New 构造连接池并应用契约默认值。
func New(cfg Config, log *slog.Logger) *Pool {
	if cfg.MaxPerServer <= 0 {
		cfg.MaxPerServer = 4
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 64
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 15 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pool{
		cfg:     cfg,
		log:     log,
		global:  semaphore.NewWeighted(int64(cfg.MaxConnections)),
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	p.wg.Add(1)
	go p.reapIdle()
	return p
}

// Test 只做 TCP/SSH 握手，不执行命令。
func (p *Pool) Test(ctx context.Context, target Target) (TestResult, error) {
	start := time.Now()
	rc, verifier, err := p.dial(ctx, target)
	if err != nil {
		return TestResult{}, err
	}
	defer p.closeRaw(rc)

	return TestResult{
		LatencyMS:     time.Since(start).Milliseconds(),
		ServerVersion: string(rc.client.ServerVersion()),
		Fingerprint:   verifier.fingerprint(),
		HostKey:       verifier.actualLine(),
		FirstSeen:     verifier.wasFirstSeen(),
	}, nil
}

// Execute 在持久的 SSH 会话中执行一条非交互命令。
func (p *Pool) Execute(ctx context.Context, target Target, req RunRequest) (RunResult, error) {
	if req.Command == "" {
		return RunResult{}, errors.New("sshx: empty command")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	maxOut := req.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutput
	}
	if maxOut > maxMaxOutput {
		maxOut = maxMaxOutput
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	b, conn, err := p.acquire(ctx, target)
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{
		Fingerprint: conn.fingerprint,
		HostKey:     conn.hostKey,
		FirstSeen:   conn.firstSeen,
	}

	session, err := conn.client.NewSession()
	if err != nil {
		p.discard(b, conn)
		return RunResult{}, fmt.Errorf("sshx: open session: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		p.discard(b, conn)
		return RunResult{}, fmt.Errorf("sshx: stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		p.discard(b, conn)
		return RunResult{}, fmt.Errorf("sshx: stderr pipe: %w", err)
	}

	writer := newCappedWriter(maxOut)
	if err := session.Start(req.Command); err != nil {
		_ = session.Close()
		p.discard(b, conn)
		return RunResult{}, fmt.Errorf("sshx: start command: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(writer, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(writer, stderr)
	}()

	done := make(chan error, 1)
	go func() {
		err := session.Wait()
		wg.Wait()
		done <- err
	}()

	select {
	case waitErr := <-done:
		result.Combined = writer.String()
		result.Truncated = writer.Truncated()
		result.DurationMS = time.Since(start).Milliseconds()
		if waitErr == nil {
			result.ExitCode = 0
			p.release(b, conn)
			return result, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitStatus()
			p.release(b, conn)
			return result, nil
		}
		p.discard(b, conn)
		return result, fmt.Errorf("sshx: wait: %w", waitErr)

	case <-ctx.Done():
		// 超时或取消：关闭会话与连接，远端命令随连接断开而终止。
		_ = session.Close()
		_ = conn.client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		p.discard(b, conn)
		result.Combined = writer.String()
		result.Truncated = writer.Truncated()
		result.DurationMS = time.Since(start).Milliseconds()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("sshx: command timed out: %w", context.DeadlineExceeded)
		}
		return result, ctx.Err()
	}
}

// Invalidate 关闭并移除该服务器的连接桶。
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	b := p.buckets[serverID]
	delete(p.buckets, serverID)
	p.mu.Unlock()
	if b != nil {
		b.closeAll()
	}
}

// Close 关闭所有连接并停止空闲回收。
func (p *Pool) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()

	p.mu.Lock()
	p.closed = true
	buckets := p.buckets
	p.buckets = make(map[string]*bucket)
	p.mu.Unlock()

	for _, b := range buckets {
		b.closeAll()
	}
	return nil
}

var _ Executor = (*Pool)(nil)
