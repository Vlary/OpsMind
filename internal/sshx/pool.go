package sshx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// rawConn 包装一条物理连接；close 保证只执行一次并释放全局配额。
type rawConn struct {
	client  *ssh.Client
	once    sync.Once
	release func()
}

func (rc *rawConn) close() {
	if rc == nil {
		return
	}
	rc.once.Do(func() {
		if rc.client != nil {
			_ = rc.client.Close()
		}
		if rc.release != nil {
			rc.release()
		}
	})
}

// pooledConn 是连接池中的一条 SSH 连接。
type pooledConn struct {
	raw         *rawConn
	client      *ssh.Client
	hostKey     string
	fingerprint string
	firstSeen   bool
	lastUsed    time.Time
}

// bucket 是单服务器的连接桶。
//
// slots 限制并发使用数（= 连接数上限）；idle 保存可复用连接。
// inUse + len(idle) 恒不超过 MaxPerServer。
type bucket struct {
	serverID string
	key      string
	slots    chan struct{}

	mu     sync.Mutex
	idle   []*pooledConn
	closed bool
}

func newBucket(serverID, key string, max int) *bucket {
	return &bucket{
		serverID: serverID,
		key:      key,
		slots:    make(chan struct{}, max),
	}
}

// closeAll 关闭桶并关闭所有空闲连接；使用中的连接在释放时关闭。
func (b *bucket) closeAll() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	idle := b.idle
	b.idle = nil
	b.mu.Unlock()
	for _, c := range idle {
		c.raw.close()
	}
}

func (b *bucket) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// connectionKey 计算连接池键：serverID + host:port + username + auth method +
// 已信任主机密钥摘要。任一变化都会让旧桶失效。
func connectionKey(target Target) string {
	port := target.Port
	if port <= 0 {
		port = defaultPortNumber
	}
	hostKeyHash := ""
	if target.KnownHostKey != "" {
		sum := sha256.Sum256([]byte(target.KnownHostKey))
		hostKeyHash = hex.EncodeToString(sum[:])
	}
	return fmt.Sprintf("%s|%s:%d|%s|%s|%s",
		target.ServerID, target.Host, port, target.Username, target.AuthMethod, hostKeyHash)
}

// bucketFor 返回（必要时创建或替换）该服务器的连接桶。
func (p *Pool) bucketFor(serverID, key string) (*bucket, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	b := p.buckets[serverID]
	var stale *bucket
	if b != nil && b.key != key {
		stale = b
		b = nil
	}
	if b == nil {
		b = newBucket(serverID, key, p.cfg.MaxPerServer)
		p.buckets[serverID] = b
	}
	p.mu.Unlock()

	if stale != nil {
		stale.closeAll()
	}
	return b, nil
}

// acquire 取得一条可用连接（复用空闲连接或新建）。
func (p *Pool) acquire(ctx context.Context, target Target) (*bucket, *pooledConn, error) {
	b, err := p.bucketFor(target.ServerID, connectionKey(target))
	if err != nil {
		return nil, nil, err
	}

	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		<-b.slots
		return nil, nil, ErrPoolClosed
	}
	if n := len(b.idle); n > 0 {
		c := b.idle[n-1]
		b.idle = b.idle[:n-1]
		b.mu.Unlock()
		return b, c, nil
	}
	b.mu.Unlock()

	c, err := p.dialConn(ctx, target)
	if err != nil {
		<-b.slots
		return nil, nil, err
	}

	// 建连期间桶可能被 Invalidate/Close。
	if b.isClosed() {
		c.raw.close()
		<-b.slots
		return nil, nil, ErrPoolClosed
	}
	return b, c, nil
}

// release 把健康的连接放回空闲列表并归还并发配额。
func (p *Pool) release(b *bucket, c *pooledConn) {
	b.mu.Lock()
	if b.closed || p.isClosed() {
		b.mu.Unlock()
		c.raw.close()
		<-b.slots
		return
	}
	c.lastUsed = time.Now()
	b.idle = append(b.idle, c)
	b.mu.Unlock()
	<-b.slots
}

// discard 关闭不可复用（或已损坏）的连接并归还并发配额。
func (p *Pool) discard(b *bucket, c *pooledConn) {
	c.raw.close()
	<-b.slots
}

func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// closeRaw 关闭一条未入池的连接（Test 使用）。
func (p *Pool) closeRaw(rc *rawConn) { rc.close() }

// dialConn 建立连接并包装为池对象。
func (p *Pool) dialConn(ctx context.Context, target Target) (*pooledConn, error) {
	rc, verifier, err := p.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	return &pooledConn{
		raw:         rc,
		client:      rc.client,
		hostKey:     verifier.actualLine(),
		fingerprint: verifier.fingerprint(),
		firstSeen:   verifier.wasFirstSeen(),
		lastUsed:    time.Now(),
	}, nil
}

// dialAddress 返回 TCP 拨号地址；端口缺省按 22 处理，始终带端口。
//
// 不能把 knownhosts.Normalize 的结果当拨号地址：known_hosts 惯例对 22 端口
// 省略 ":22"（127.0.0.1:22 → 127.0.0.1），net.Dial 会报 missing port。
func dialAddress(target Target) string {
	port := target.Port
	if port <= 0 {
		port = defaultPortNumber
	}
	return net.JoinHostPort(target.Host, strconv.Itoa(port))
}

// dial 建立 TCP + SSH 连接。调用方负责关闭返回的连接。
func (p *Pool) dial(ctx context.Context, target Target) (*rawConn, *hostKeyVerifier, error) {
	if target.Host == "" || target.Username == "" {
		return nil, nil, fmt.Errorf("sshx: host and username are required")
	}

	cfg, verifier, err := buildClientConfig(target)
	if err != nil {
		return nil, nil, err
	}
	cfg.Timeout = p.cfg.HandshakeTimeout

	dialAddr := dialAddress(target)
	// knownhosts 语义的规范地址（22 端口省略端口）只用作 known_hosts 匹配键。
	knownAddr := knownhosts.Normalize(dialAddr)

	if err := p.global.Acquire(ctx, 1); err != nil {
		return nil, nil, fmt.Errorf("sshx: connection limit: %w", err)
	}
	release := func() { p.global.Release(1) }

	dialer := net.Dialer{Timeout: p.cfg.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("sshx: dial %s: %w", dialAddr, err)
	}

	// 握手也受 context 与 HandshakeTimeout 双重约束。
	deadline := time.Now().Add(p.cfg.HandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, knownAddr, cfg)
	if err != nil {
		_ = conn.Close()
		release()
		return nil, nil, fmt.Errorf("sshx: handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})

	return &rawConn{client: ssh.NewClient(sshConn, chans, reqs), release: release}, verifier, nil
}

// reapIdle 周期回收空闲超时的连接。
func (p *Pool) reapIdle() {
	defer p.wg.Done()
	interval := p.cfg.IdleTimeout / 4
	if interval > 15*time.Second {
		interval = 15 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case now := <-ticker.C:
			p.mu.Lock()
			buckets := make([]*bucket, 0, len(p.buckets))
			for _, b := range p.buckets {
				buckets = append(buckets, b)
			}
			p.mu.Unlock()

			for _, b := range buckets {
				var expired []*pooledConn
				b.mu.Lock()
				keep := b.idle[:0]
				for _, c := range b.idle {
					if now.Sub(c.lastUsed) >= p.cfg.IdleTimeout {
						expired = append(expired, c)
						continue
					}
					keep = append(keep, c)
				}
				b.idle = keep
				b.mu.Unlock()
				for _, c := range expired {
					c.raw.close()
				}
			}
		}
	}
}

// cappedWriter 截断输出但继续消费远端数据，避免远端因管道写满而阻塞。
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
