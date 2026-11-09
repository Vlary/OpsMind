package auth

import (
	"sync"
	"time"
)

const (
	// loginLimiterMaxEntries 防止 IP map 无界增长。
	loginLimiterMaxEntries = 4096
	// loginLimiterIdleTTL 是桶的清理阈值。
	loginLimiterIdleTTL = 10 * time.Minute
)

// LoginLimiter 是按 IP 的令牌桶限流器。
//
// 每个 IP 一个桶：初始 capacity 个 token，每 refill 补充一个。IP map 上限
// 4096；每次 Allow 顺带清理 10 分钟未访问的 IP；达到上限时拒绝新 IP。
type LoginLimiter struct {
	mu       sync.Mutex
	capacity int
	refill   time.Duration
	clock    func() time.Time
	buckets  map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// NewLoginLimiter 构造限流器；capacity=5、refill=12s 为契约默认。
func NewLoginLimiter(capacity int, refill time.Duration, clock func() time.Time) *LoginLimiter {
	if capacity <= 0 {
		capacity = 1
	}
	if refill <= 0 {
		refill = time.Second
	}
	if clock == nil {
		clock = time.Now
	}
	return &LoginLimiter{
		capacity: capacity,
		refill:   refill,
		clock:    clock,
		buckets:  make(map[string]*bucket, capacity),
	}
}

// Allow 判断该 IP 是否允许登录；拒绝时返回距下次可用的等待时间。
func (l *LoginLimiter) Allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	l.purgeLocked(now)

	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= loginLimiterMaxEntries {
			return false, l.refill
		}
		b = &bucket{tokens: float64(l.capacity), lastSeen: now}
		l.buckets[ip] = b
	}

	elapsed := now.Sub(b.lastSeen)
	if elapsed > 0 {
		b.tokens += float64(elapsed) / float64(l.refill)
		if b.tokens > float64(l.capacity) {
			b.tokens = float64(l.capacity)
		}
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) * float64(l.refill))
	if wait <= 0 {
		wait = l.refill
	}
	return false, wait
}

// purgeLocked 删除超过 loginLimiterIdleTTL 未访问的桶；调用方必须持锁。
func (l *LoginLimiter) purgeLocked(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.lastSeen) > loginLimiterIdleTTL {
			delete(l.buckets, ip)
		}
	}
}
