// Package audit 实现只追加审计：异步批量写入与哈希链。
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"aiops/internal/store"
)

const (
	channelCapacity  = 1024
	batchSize        = 100
	batchInterval    = 500 * time.Millisecond
	closeFlushBudget = 5 * time.Second
)

// ErrClosed 表示 writer 已关闭。
var ErrClosed = errors.New("audit: writer closed")

// Event 是一条待写审计事件。
type Event struct {
	ActorType      string
	ActorID        string
	Action         string
	ResourceType   string
	ResourceID     string
	RequestID      string
	ConversationID *string
	ServerID       *string
	IP             string
	Detail         map[string]any
}

// Writer 是单实例审计写入器。
type Writer struct {
	st    store.Store
	log   *slog.Logger
	clock func() time.Time

	events  chan store.CreateAuditParams
	flushCh chan flushRequest
	stopCh  chan struct{}
	doneCh  chan struct{}

	loopCtx    context.Context
	loopCancel context.CancelFunc

	startOnce sync.Once
	stopOnce  sync.Once
	doneOnce  sync.Once

	started atomic.Bool
	closed  atomic.Bool
	pending atomic.Int64
}

type flushRequest struct {
	done chan error
}

// NewWriter 构造审计 writer。进程内只允许一个。
func NewWriter(st store.Store, log *slog.Logger, clock func() time.Time) *Writer {
	if log == nil {
		log = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Writer{
		st:         st,
		log:        log,
		clock:      clock,
		events:     make(chan store.CreateAuditParams, channelCapacity),
		flushCh:    make(chan flushRequest),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		loopCtx:    ctx,
		loopCancel: cancel,
	}
}

// Start 启动后台批量写入循环，并从数据库恢复链尾用于日志核对。
func (w *Writer) Start(ctx context.Context) {
	w.startOnce.Do(func() {
		w.started.Store(true)
		if w.st != nil {
			recoverCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			last, err := w.st.AuditLogs().LastHash(recoverCtx)
			cancel()
			if err != nil {
				w.log.Warn("audit_chain_recovery_failed", "error", err)
			} else {
				w.log.Info("audit_chain_recovered", "last_hash", last)
			}
		}
		go w.run(ctx)
	})
}

// Enqueue 入队一条审计事件；channel 满时阻塞直到有空间或 ctx 取消（背压）。
func (w *Writer) Enqueue(ctx context.Context, e Event) error {
	if w.closed.Load() {
		return ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	params := store.CreateAuditParams{
		CreatedAt:      w.clock().UTC(),
		ActorType:      e.ActorType,
		ActorID:        e.ActorID,
		Action:         e.Action,
		ResourceType:   e.ResourceType,
		ResourceID:     e.ResourceID,
		RequestID:      e.RequestID,
		ConversationID: e.ConversationID,
		ServerID:       e.ServerID,
		IP:             e.IP,
		Detail:         sanitizeDetail(e.Detail),
	}
	select {
	case w.events <- params:
		w.pending.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.stopCh:
		return ErrClosed
	}
}

// Flush 等待当前已入队事件落库。
func (w *Writer) Flush(ctx context.Context) error {
	if !w.started.Load() {
		if w.pending.Load() == 0 {
			return nil
		}
		return ErrClosed
	}
	req := flushRequest{done: make(chan error, 1)}
	select {
	case w.flushCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.doneCh:
		if w.pending.Load() == 0 {
			return nil
		}
		return ErrClosed
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-w.doneCh:
		return nil
	}
}

// Close 停止 writer 并 flush；最多等待 closeFlushBudget，超时返回剩余条数。
func (w *Writer) Close(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.closed.Store(true)
		w.loopCancel()
		close(w.stopCh)
	})
	if !w.started.Load() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	closeCtx, cancel := context.WithTimeout(ctx, closeFlushBudget)
	defer cancel()
	select {
	case <-w.doneCh:
		return nil
	case <-closeCtx.Done():
		return fmt.Errorf("audit: close timed out with %d entries pending", w.pending.Load())
	}
}

func (w *Writer) run(parent context.Context) {
	timer := time.NewTimer(batchInterval)
	defer timer.Stop()

	batch := make([]store.CreateAuditParams, 0, batchSize)
	shutdown := func() {
		w.drainEvents(&batch)
		if len(batch) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), closeFlushBudget)
			if err := w.flushWithRetry(ctx, batch); err != nil {
				w.log.Error("audit_final_flush_failed",
					"error", err,
					"remaining", w.pending.Load(),
				)
			}
			cancel()
			batch = batch[:0]
		}
		w.doneOnce.Do(func() { close(w.doneCh) })
	}

	for {
		select {
		case p := <-w.events:
			batch = append(batch, p)
			if len(batch) >= batchSize {
				if err := w.flushWithRetry(w.loopCtx, batch); err != nil {
					if !errors.Is(err, context.Canceled) {
						w.log.Error("audit_batch_flush_failed", "error", err, "count", len(batch))
					}
				} else {
					batch = batch[:0]
				}
				resetTimer(timer, batchInterval)
			}
		case <-timer.C:
			if len(batch) > 0 {
				if err := w.flushWithRetry(w.loopCtx, batch); err != nil {
					if !errors.Is(err, context.Canceled) {
						w.log.Error("audit_batch_flush_failed", "error", err, "count", len(batch))
					}
				} else {
					batch = batch[:0]
				}
			}
			timer.Reset(batchInterval)
		case req := <-w.flushCh:
			w.drainEvents(&batch)
			err := error(nil)
			if len(batch) > 0 {
				err = w.flushWithRetry(w.loopCtx, batch)
				if err == nil {
					batch = batch[:0]
				}
			}
			req.done <- err
			resetTimer(timer, batchInterval)
		case <-w.stopCh:
			shutdown()
			return
		case <-parent.Done():
			shutdown()
			return
		}
	}
}

func (w *Writer) drainEvents(batch *[]store.CreateAuditParams) {
	for {
		select {
		case p := <-w.events:
			*batch = append(*batch, p)
		default:
			return
		}
	}
}

func (w *Writer) flushWithRetry(ctx context.Context, batch []store.CreateAuditParams) error {
	backoff := []time.Duration{
		100 * time.Millisecond,
		500 * time.Millisecond,
		2 * time.Second,
		5 * time.Second,
	}
	for attempt := 0; ; attempt++ {
		err := w.writeBatch(ctx, batch)
		if err == nil {
			w.pending.Add(-int64(len(batch)))
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		delay := 5 * time.Second
		if attempt < len(backoff) {
			delay = backoff[attempt]
		}
		w.log.Warn("audit_flush_retry",
			"error", err,
			"attempt", attempt+1,
			"count", len(batch),
			"delay", delay.String(),
		)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return err
		case <-w.stopCh:
			return err
		}
	}
}

func (w *Writer) writeBatch(ctx context.Context, batch []store.CreateAuditParams) error {
	if w.st == nil {
		return errors.New("audit: nil store")
	}
	return w.st.WithTx(ctx, func(tx store.Store) error {
		prev, err := tx.AuditLogs().LastHash(ctx)
		if err != nil {
			return err
		}
		for i := range batch {
			p := &batch[i]
			p.PrevHash = prev
			p.Hash = computeHash(*p)
			if err := tx.AuditLogs().Append(ctx, *p); err != nil {
				return err
			}
			prev = p.Hash
		}
		return nil
	})
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
