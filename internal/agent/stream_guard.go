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
	// owners 记录每个活跃流的发起人身份（userID / authSessionID），供
	// 登出（按 auth session）与停用/降级/吊销（按用户）取消对应流。
	// 与 cancels 同键，release/Shutdown 时一并清理。
	owners map[string]streamOwner
	closed bool
}

// streamOwner 是活跃流的归属身份（空串表示未追踪）。
type streamOwner struct {
	userID        string
	authSessionID string
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
		owners:  make(map[string]streamOwner),
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
			delete(g.owners, conversationID)
			g.mu.Unlock()
		})
	}, nil
}

// Watch 把会话流的取消函数登记到守卫上，供 ShutdownActive 在服务关闭时
// 主动取消活跃流。守卫已关闭时立即执行 cancel（新流不允许在关闭后继续）。
// cancel 会在流结束（release）或 ShutdownActive 时被调用/清除，无需手动注销。
func (g *StreamGuard) Watch(conversationID string, cancel func()) {
	g.WatchOwned(conversationID, "", "", cancel)
}

// WatchOwned 登记取消函数并记录发起人身份（logout 按 auth session、
// 停用/降级/吊销按用户取消对应流；契约 12.4 第 5 条）。
func (g *StreamGuard) WatchOwned(conversationID, userID, authSessionID string, cancel func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		cancel()
		return
	}
	g.cancels[conversationID] = cancel
	g.owners[conversationID] = streamOwner{userID: userID, authSessionID: authSessionID}
}

// CancelByUser 取消该用户发起的全部活跃流（停用/降级/吊销会话）。
func (g *StreamGuard) CancelByUser(userID string) {
	if userID == "" {
		return
	}
	g.cancelMatching(func(o streamOwner) bool { return o.userID == userID })
}

// CancelByAuthSession 取消某个 auth session 发起的全部活跃流（登出）。
func (g *StreamGuard) CancelByAuthSession(authSessionID string) {
	if authSessionID == "" {
		return
	}
	g.cancelMatching(func(o streamOwner) bool { return o.authSessionID == authSessionID })
}

// cancelMatching 收集匹配的取消函数后在锁外调用（取消可能触发落库）。
func (g *StreamGuard) cancelMatching(match func(streamOwner) bool) {
	g.mu.Lock()
	cancels := make([]func(), 0, len(g.cancels))
	for id, cancel := range g.cancels {
		if match(g.owners[id]) {
			cancels = append(cancels, cancel)
			delete(g.cancels, id)
			delete(g.owners, id)
		}
	}
	g.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
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
		delete(g.owners, id)
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
