package agent

import (
	"errors"
	"sync"
)

var (
	// ErrStreamActive 表示该会话已有活跃流。
	ErrStreamActive = errors.New("agent: conversation already has an active stream")
	// ErrTooManyStreams 表示全局流并发达到上限。
	ErrTooManyStreams = errors.New("agent: global stream limit reached")
)

// StreamGuard 是进程内 SSE 流占用表。
type StreamGuard struct {
	mu      sync.Mutex
	max     int
	active  map[string]struct{}
	cancels map[string]func()
	closed  bool
}

// NewStreamGuard 构造流守卫。
func NewStreamGuard(max int) *StreamGuard {
	if max <= 0 {
		max = 20
	}
	return &StreamGuard{
		max:     max,
		active:  make(map[string]struct{}),
		cancels: make(map[string]func()),
	}
}

// TryAcquire 占用一个会话流；release 幂等。
func (g *StreamGuard) TryAcquire(conversationID string) (release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.active[conversationID]; ok {
		return nil, ErrStreamActive
	}
	if len(g.active) >= g.max {
		return nil, ErrTooManyStreams
	}
	g.active[conversationID] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.active, conversationID)
			delete(g.cancels, conversationID)
			g.mu.Unlock()
		})
	}, nil
}

// Watch 把会话流的取消函数登记到守卫上，供 ShutdownActive 在服务关闭时
// 主动取消活跃流。守卫已关闭时立即执行 cancel（新流不允许在关闭后继续）。
// cancel 会在流结束（release）或 ShutdownActive 时被调用/清除，无需手动注销。
func (g *StreamGuard) Watch(conversationID string, cancel func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		cancel()
		return
	}
	g.cancels[conversationID] = cancel
}

// ShutdownActive 取消全部活跃流，并让后续 Watch 立即取消。
// 供 http.Server 的 RegisterOnShutdown 回调使用：Shutdown 不会断开活跃
// 请求，绑请求 ctx 的 SSE 长流若不主动取消，排空窗口内永远等不完。
func (g *StreamGuard) ShutdownActive() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	for id, cancel := range g.cancels {
		cancel()
		delete(g.cancels, id)
	}
}

// Active 判断会话是否有活跃流。
func (g *StreamGuard) Active(conversationID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.active[conversationID]
	return ok
}

// Count 返回当前活跃流数量。
func (g *StreamGuard) Count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.active)
}
