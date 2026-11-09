package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Watch/ShutdownActive 支撑服务关闭时取消活跃 SSE 流：http.Server.Shutdown
// 不会断开活跃请求，必须由守卫主动取消（cmd/aiops 经 RegisterOnShutdown 调用）。
func TestWatchAndShutdownActive(t *testing.T) {
	g := NewStreamGuard(4)

	release, err := g.TryAcquire("conv-1")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.Watch("conv-1", cancel)

	select {
	case <-ctx.Done():
		t.Fatal("stream cancelled before ShutdownActive")
	default:
	}

	g.ShutdownActive()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stream not cancelled by ShutdownActive")
	}

	// 关闭后 Watch 的新流必须立即取消。
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	g.Watch("conv-2", cancel2)
	select {
	case <-ctx2.Done():
	default:
		t.Fatal("Watch after shutdown must cancel immediately")
	}
}

func TestWatchReleaseClearsCancel(t *testing.T) {
	g := NewStreamGuard(4)

	release, err := g.TryAcquire("conv-1")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.Watch("conv-1", cancel)
	release()

	g.ShutdownActive()
	select {
	case <-ctx.Done():
		t.Fatal("released stream must not be cancelled by ShutdownActive")
	case <-time.After(50 * time.Millisecond):
	}
}

// ShutdownActive 不持有任何活跃流时也应标记关闭（幂等）。
func TestShutdownActiveIdempotent(t *testing.T) {
	g := NewStreamGuard(4)
	g.ShutdownActive()
	g.ShutdownActive()
	if _, err := g.TryAcquire("conv-1"); err != nil {
		t.Fatalf("acquire after shutdown (guard 只负责取消，不负责拒绝): %v", err)
	}
}

// 并发 Watch/释放/Shutdown 不竞态（-race 下验证）。
func TestWatchConcurrent(t *testing.T) {
	g := NewStreamGuard(8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "conv"
			release, err := g.TryAcquire(id)
			if err != nil {
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			g.Watch(id, cancel)
			<-ctx.Done()
			release()
		}(i)
	}
	time.Sleep(10 * time.Millisecond)
	g.ShutdownActive()
	wg.Wait()
}
