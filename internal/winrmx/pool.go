// pool.go 是 winrmx 的连接池与 Executor 实现（CONTRACT.md 5.2A）。
//
// 按 ServerID 分桶（键含 host:port:username:authtype），桶内并发上限
// MaxPerServer（默认 2，WinRM 服务端 MaxShellsPerUser=5 的保守取值）；
// 全局信号量默认 16；空闲桶超过 IdleTimeout（60s）被回收。Invalidate
// 语义同 sshx：服务器配置变更后调用方必须使旧桶失效。
package winrmx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/masterzen/winrm"
	"golang.org/x/sync/semaphore"

	"aiops/internal/store"
)

// Config 是连接池配置（契约 5.2A）。
type Config struct {
	MaxPerServer   int           // 2
	IdleTimeout    time.Duration // 60s
	MaxConnections int           // 16
	DialTimeout    time.Duration // 10s
	RequestTimeout time.Duration // 55s（服务端 MaxTimeoutms=60000 硬上限内留余量）
}

// Pool 实现 Executor。
type Pool struct {
	cfg    Config
	log    *slog.Logger
	global *semaphore.Weighted

	mu      sync.Mutex
	buckets map[string]*winBucket
	closed  bool

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// winBucket 是一个 ServerID 的执行桶：缓存按目标键构造的 winrm 客户端
// （HTTP transport 复用 keep-alive 连接）并用信号量限制桶内并发。
type winBucket struct {
	key      string
	client   *winrm.Client
	sem      chan struct{}
	lastUsed time.Time
}

var _ Executor = (*Pool)(nil)

// New 构造连接池并应用契约默认值。
func New(cfg Config, log *slog.Logger) *Pool {
	if cfg.MaxPerServer <= 0 {
		cfg.MaxPerServer = 2
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 16
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTTL
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pool{
		cfg:     cfg,
		log:     log,
		global:  semaphore.NewWeighted(int64(cfg.MaxConnections)),
		buckets: make(map[string]*winBucket),
		stop:    make(chan struct{}),
	}
	p.wg.Add(1)
	go p.reapIdle()
	return p
}

// targetKey 是桶内客户端的失效键：任一要素变化都要重建客户端。
func targetKey(t Target) string {
	https := "0"
	if t.UseHTTPS {
		https = "1"
	}
	port := t.Port
	if port == 0 {
		port = defaultWinRMPort
	}
	return fmt.Sprintf("%s:%d:%s:%s:%s", t.Host, port, t.Username, t.AuthType, https)
}

// formatISODuration 把 RequestTimeout 编码为 WS-Man OperationTimeout 的
// ISO8601 duration（"PT55S"）。
func formatISODuration(d time.Duration) string {
	secs := int64(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("PT%dS", secs)
}

// makeParameters 构造 WS-Man 参数（OperationTimeout 取 RequestTimeout，
// NTLM 通道装配 ClientNTLM transport；Basic 走默认 transport）。
func (p *Pool) makeParameters(authType store.AuthMethod) *winrm.Parameters {
	params := winrm.NewParameters(formatISODuration(p.cfg.RequestTimeout), "en-US", 153600)
	if authType == store.AuthMethodPasswordNTLM {
		params.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	}
	return params
}

// buildClient 按 Target 构造 masterzen/winrm 客户端。
//
// password_ntlm：TransportDecorator 使用 ClientNTLM（Negotiate/NTLMv2，
// HTTP 下自带消息级签名/封包）；password_basic：默认 transport，每个请求
// 带 Basic 凭据（契约 5.2A；库本身没有 NewBasicAuth 构造器，Basic 即默认
// transporter 的 SetBasicAuth 行为）。
func (p *Pool) buildClient(target Target) (*winrm.Client, error) {
	switch target.AuthType {
	case store.AuthMethodPasswordNTLM, store.AuthMethodPasswordBasic:
	default:
		return nil, ErrUnsupportedAuth
	}
	if target.Host == "" || target.Username == "" {
		return nil, ErrCredentialMissing
	}
	port := target.Port
	if port == 0 {
		port = defaultWinRMPort
	}
	if !validPort(port) {
		return nil, fmt.Errorf("winrmx: invalid port %d", port)
	}
	endpoint := winrm.NewEndpoint(target.Host, port, target.UseHTTPS, false, nil, nil, nil, p.cfg.DialTimeout)
	return winrm.NewClientWithParameters(endpoint, target.Username, target.Password, p.makeParameters(target.AuthType))
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// acquire 取全局与桶内配额，返回目标桶（必要时重建客户端）。
func (p *Pool) acquire(ctx context.Context, target Target) (*winBucket, error) {
	if err := p.global.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	key := targetKey(target)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			p.global.Release(1)
			return nil, ErrPoolClosed
		}
		b := p.buckets[target.ServerID]
		if b == nil || b.key != key {
			client, err := p.buildClient(target)
			if err != nil {
				p.mu.Unlock()
				p.global.Release(1)
				return nil, err
			}
			b = &winBucket{
				key:      key,
				client:   client,
				sem:      make(chan struct{}, p.cfg.MaxPerServer),
				lastUsed: time.Now(),
			}
			p.buckets[target.ServerID] = b
		}
		p.mu.Unlock()

		// 桶内配额：非阻塞抢占，满则等待 ctx（不在锁内阻塞，避免拖住回收）。
		select {
		case b.sem <- struct{}{}:
			b.muTouch()
			return b, nil
		case <-ctx.Done():
			p.global.Release(1)
			return nil, ctx.Err()
		}
	}
}

func (b *winBucket) muTouch() {
	// lastUsed 只在持有桶引用的执行路径上更新；竞争写误差不影响回收正确性。
	b.lastUsed = time.Now()
}

// release 归还桶内与全局配额。
func (p *Pool) release(b *winBucket) {
	<-b.sem
	p.global.Release(1)
}

// Test 建立 WinRM 会话并执行探针命令（计算机名 + PowerShell 版本），
// 返回延迟与 server_version；无主机密钥语义（契约 5.2A）。
func (p *Pool) Test(ctx context.Context, target Target) (TestResult, error) {
	start := time.Now()
	res, err := p.run(ctx, target, probeCommand, 30*time.Second, defaultMaxOutput)
	if err != nil {
		return TestResult{}, err
	}
	version := ""
	for _, line := range strings.Split(res.Combined, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			version = line // 最后一个非空行：$PSVersionTable.PSVersion
		}
	}
	return TestResult{
		LatencyMS:     time.Since(start).Milliseconds(),
		ServerVersion: version,
		Fingerprint:   "",
		FirstSeen:     false,
	}, nil
}

// Execute 以 PowerShell 端点运行命令并附加 "; exit $LASTEXITCODE"
// 取原生命令退出码；stdout/stderr 合并、256KB 截断（契约 5.2A）。
func (p *Pool) Execute(ctx context.Context, target Target, req RunRequest) (RunResult, error) {
	if req.Command == "" {
		return RunResult{}, errors.New("winrmx: empty command")
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
	script := req.Command + "; exit $LASTEXITCODE"
	return p.run(ctx, target, script, timeout, maxOut)
}

// run 取配额后执行脚本；ctx 超时统一在内部叠加。
func (p *Pool) run(ctx context.Context, target Target, script string, timeout time.Duration, maxOut int64) (RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	b, err := p.acquire(ctx, target)
	if err != nil {
		return RunResult{}, err
	}
	defer p.release(b)

	res, err := runScript(ctx, b.client, script, timeout, maxOut)
	if err != nil {
		if ctx.Err() != nil {
			return res, fmt.Errorf("winrmx: command timed out: %w", ctx.Err())
		}
		return res, err
	}
	return res, nil
}

// Invalidate 关闭并移除该服务器的执行桶（同 sshx 语义）。
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	delete(p.buckets, serverID)
	p.mu.Unlock()
}

// Close 停止空闲回收并清空所有桶。
func (p *Pool) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()

	p.mu.Lock()
	p.closed = true
	p.buckets = make(map[string]*winBucket)
	p.mu.Unlock()
	return nil
}

// reapIdle 周期回收超过 IdleTimeout 未使用的桶。
func (p *Pool) reapIdle() {
	defer p.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			now := time.Now()
			p.mu.Lock()
			for id, b := range p.buckets {
				if now.Sub(b.lastUsed) > p.cfg.IdleTimeout && len(b.sem) == 0 {
					delete(p.buckets, id)
				}
			}
			p.mu.Unlock()
		}
	}
}

// cappedWriter 截断输出但继续消费远端数据，避免远端因管道写满而阻塞
// （与 sshx 同构）。
type cappedWriter struct {
	mu        sync.Mutex
	buf       []byte
	max       int64
	truncated bool
}

func newCappedWriter(max int64) *cappedWriter {
	return &cappedWriter{max: max}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remain := w.max - int64(len(w.buf))
	if remain <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remain {
		w.buf = append(w.buf, p[:remain]...)
		w.truncated = true
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func (w *cappedWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

var _ io.Writer = (*cappedWriter)(nil)
