// pool.go 是网络设备执行器连接治理（契约 5.2B）：
// 每次 Execute 使用独立连接（网络设备 CLI 无多路复用语义），连接按
// ServerID 计入每服务器并发（MaxPerServer=2）与全局信号量（MaxConnections=32）；
// Invalidate/Close 语义同 sshx（对 server 类设备为幂等空操作）。
package netdev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"aiops/internal/sshx"
	"aiops/internal/store"
)

// telnetAccess 是 telnet 接入协议常量别名（Test 分支判定用）。
const telnetAccess = store.AccessProtoTelnet

// 默认与上限（契约 5.2B）。
const (
	defaultTimeout   = 60 * time.Second
	maxTimeout       = 300 * time.Second
	defaultMaxOutput = int64(262144)
	maxMaxOutput     = int64(262144)
	// tailWindow 是 expect 判定的尾部窗口大小（提示符/分页/登录提示）。
	tailWindow = 1024
	// testProbeTimeout 是 Test 探针执行的预算（登录+探针，独立于 SSH 握手）。
	testProbeTimeout = 30 * time.Second
)

// Pool 实现 Executor。
type Pool struct {
	cfg Config
	log *slog.Logger
	ssh sshx.Executor // SSH 通道提供者（PTYOpener 能力在 openChannel 内断言）

	global *semaphore.Weighted

	mu      sync.Mutex
	buckets map[string]*bucket
	closed  bool
}

type bucket struct {
	slots chan struct{}
}

// New 构造执行器并应用契约默认值。ssh 为 SSH 通道提供者：netdev 调
// ssh.Session（Target→sshx.Target 映射在包内）打开 PTY 作为 Channel；
// 主机密钥校验/TOFU 完全复用 sshx（5.2），netdev 不重复实现。
func New(cfg Config, ssh sshx.Executor, log *slog.Logger) *Pool {
	if cfg.MaxPerServer <= 0 {
		cfg.MaxPerServer = 2
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 32
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 15 * time.Second
	}
	if cfg.ExpectTick <= 0 {
		cfg.ExpectTick = 100 * time.Millisecond
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pool{
		cfg:     cfg,
		log:     log,
		ssh:     ssh,
		global:  semaphore.NewWeighted(int64(cfg.MaxConnections)),
		buckets: make(map[string]*bucket),
	}
}

// acquire 取得该服务器的并发槽位与全局配额；连接为一次性，槽位在通道
// 关闭后释放。
func (p *Pool) acquire(ctx context.Context, t Target) (release func(), err error) {
	if t.ServerID == "" || t.Host == "" || t.Username == "" {
		return nil, fmt.Errorf("netdev: server id, host and username are required")
	}
	if err := p.global.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("netdev: connection limit: %w", err)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.global.Release(1)
		return nil, fmt.Errorf("netdev: pool closed")
	}
	b := p.buckets[t.ServerID]
	if b == nil {
		b = &bucket{slots: make(chan struct{}, p.cfg.MaxPerServer)}
		p.buckets[t.ServerID] = b
	}
	p.mu.Unlock()

	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		p.global.Release(1)
		return nil, ctx.Err()
	}
	return func() {
		<-b.slots
		p.global.Release(1)
	}, nil
}

// Invalidate 关闭并移除该服务器的槽位桶（在用连接一次性、自然结束；
// 对 server 类设备为幂等空操作——契约 2.6）。
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	delete(p.buckets, serverID)
	p.mu.Unlock()
}

// Close 停止接受新执行（在用一次性连接自然结束）。
func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	p.buckets = make(map[string]*bucket)
	p.mu.Unlock()
	return nil
}

var _ Executor = (*Pool)(nil)

// Execute 在一条独立连接上执行单条 CLI 命令（契约 2.8 约定 3 流程）。
func (p *Pool) Execute(ctx context.Context, t Target, req RunRequest) (RunResult, error) {
	if req.Command == "" {
		return RunResult{}, errors.New("netdev: empty command")
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

	start := time.Now()
	res := RunResult{CleartextWarning: t.CleartextWarning()}

	release, err := p.acquire(ctx, t)
	if err != nil {
		return res, err
	}
	defer release()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prof := ProfileOf(t.Brand)
	ch, err := p.openChannel(runCtx, t)
	if err != nil {
		return res, err
	}

	s := p.startExpect(runCtx, t, prof, ch, timeout, maxOut)
	executed := false
	defer func() {
		if !executed {
			s.close()
		}
	}()

	if err := s.login(t); err != nil {
		s.close()
		executed = true
		res.DurationMS = time.Since(start).Milliseconds()
		return res, err
	}
	s.reset()

	s.disablePager()

	needExit1, err := s.runCommand(t, req.Command)
	out := s.cleanOutput(req.Command)
	s.close()
	executed = true

	res.DurationMS = time.Since(start).Milliseconds()
	res.Combined = out
	res.Truncated = s.truncated
	if err != nil {
		res.ExitCode = 1
		return res, err
	}
	if needExit1 || s.hitError(out) {
		res.ExitCode = 1
		return res, nil
	}
	res.ExitCode = 0
	return res, nil
}

// Test 按品牌探针命令走一次完整 Execute（含登录与分页关闭，契约 5.2B）。
// SSH 通道的 Fingerprint/HostKey/FirstSeen 由 sshx 拨号（Test 握手）返回并
// 透传（调用方写回 UpdateHostKey）；Telnet 通道 Fingerprint 恒空串、
// FirstSeen 恒 false。ServerVersion 从探针输出解析版本串，失败取原始首行。
func (p *Pool) Test(ctx context.Context, t Target) (TestResult, error) {
	start := time.Now()
	res := TestResult{CleartextWarning: t.CleartextWarning()}

	if t.AccessProto != telnetAccess {
		if p.ssh == nil {
			return res, errors.New("netdev: ssh executor is not configured")
		}
		sshRes, err := p.ssh.Test(ctx, sshTarget(t))
		if err != nil {
			return res, fmt.Errorf("netdev: ssh handshake: %w", err)
		}
		res.Fingerprint = sshRes.Fingerprint
		res.HostKey = sshRes.HostKey
		res.FirstSeen = sshRes.FirstSeen
	}

	probeCtx, cancel := context.WithTimeout(ctx, testProbeTimeout)
	defer cancel()
	out, err := p.Execute(probeCtx, t, RunRequest{
		Command:        ProfileOf(t.Brand).ProbeCmd,
		Timeout:        testProbeTimeout,
		MaxOutputBytes: defaultMaxOutput,
	})
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return res, err
	}
	res.ServerVersion = parseSoftwareVersion(t.Brand, out.Combined)
	return res, nil
}
