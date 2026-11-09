package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// runStats 是运行期聚合计数。
type runStats struct {
	Chats      atomic.Int64 // 完成的 SSE 聊天轮（读到 done）
	Approvals  atomic.Int64 // 自动批准次数
	Tokens     atomic.Int64 // 读到的 token 事件数
	SSEErrors  atomic.Int64 // SSE 流内 error 事件数（视同服务器错误）
	HTTP5xx    atomic.Int64 // HTTP >= 500 响应数
	CRUDOps    atomic.Int64 // CRUD churn 完成组数
	AuditPages atomic.Int64 // 审计分页页数
}

// bootstrap 用管理员会话建 provider、假服务器记录，并注入 mock。
// 返回已登录的 admin 客户端：其 cookie 会话同时供 sampler 抓取 pprof
// （服务端 /debug/pprof 与 API 同等鉴权，匿名请求 401）。
func bootstrap(ctx context.Context, env *testEnv, rec *opRecorder, stats *runStats) (*loadClient, error) {
	admin, err := newLoadClient(env.serverAddr, 999, rec, func(status int, _, _ string) {
		if status >= 500 {
			stats.HTTP5xx.Add(1)
		}
	})
	if err != nil {
		return nil, err
	}
	if err := admin.login(ctx, "login_bootstrap", env.adminPassword); err != nil {
		return nil, err
	}
	if env.providerID, err = admin.createProvider(ctx, "provider_create_bootstrap", "loadgen-chat-provider", env.mock.URL()); err != nil {
		return nil, fmt.Errorf("bootstrap provider: %w", err)
	}
	if env.chatServerID, err = admin.createServer(ctx, "server_create_bootstrap", "loadgen-ssh-target"); err != nil {
		return nil, fmt.Errorf("bootstrap server: %w", err)
	}
	env.mock.SetServerID(env.chatServerID)
	fmt.Printf("loadgen: bootstrap ok (provider=%s server=%s)\n", env.providerID, env.chatServerID)
	return admin, nil
}

// loadController 汇集一次负载运行的可变状态。
type loadController struct {
	env   *testEnv
	mc    modeConfig
	rec   *opRecorder
	stats *runStats
}

// runWorkers 启动 N 个并发会话 worker 并阻塞到 duration 结束。
func (lc *loadController) runWorkers(ctx context.Context, concurrency int, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			lc.worker(ctx, id)
		}(i)
	}
	wg.Wait()
	return nil
}

func (lc *loadController) worker(ctx context.Context, id int) {
	client, err := newLoadClient(lc.env.serverAddr, id, lc.rec, func(status int, _, _ string) {
		if status >= 500 {
			lc.stats.HTTP5xx.Add(1)
		}
	})
	if err != nil {
		fmt.Fprintf(stderr(), "loadgen: worker %d build client: %v\n", id, err)
		return
	}
	if err := client.login(ctx, "login_worker", lc.env.adminPassword); err != nil {
		fmt.Fprintf(stderr(), "loadgen: worker %d login: %v\n", id, err)
		return
	}

	seq := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		seq++
		lc.chatRound(ctx, client, id, seq)
		lc.crudRound(ctx, client, id, seq)
		lc.auditRound(ctx, client)
		if lc.mc.loopPause > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(lc.mc.loopPause):
			}
		}
	}
}

// errStreamDone 是读到 done 事件的哨兵：scanSSE 视为正常结束。
var errStreamDone = fmt.Errorf("stream done")

func stderr() *os.File { return os.Stderr }

// chatRound 一轮 SSE 聊天：建会话 → 流式聊天（逐 token + 自动批准）→ 删除会话。
//
// approval_required 到来时服务端正阻塞在审批等待上（internal/agent/runner.go
// approvals.Wait），在 SSE 读回调内同步调用批准接口即可解锁该流。
func (lc *loadController) chatRound(ctx context.Context, c *loadClient, id, seq int) {
	convID, err := c.createConversation(ctx, "conversation_create", fmt.Sprintf("loadgen-w%d-s%d", id, seq))
	if err != nil {
		return
	}
	defer func() { _ = c.deleteConversation(context.WithoutCancel(ctx), "conversation_delete", convID) }()

	var (
		tokens   int64
		doneSeen bool
	)
	err = c.openSSE(ctx, "chat_sse", "/api/chat", map[string]any{
		"conversation_id": convID,
		"provider_id":     lc.env.providerID,
		"content":         fmt.Sprintf("load round worker=%d seq=%d", id, seq),
	}, func(ev sseEvent) error {
		switch ev.Name {
		case "token":
			tokens++
		case "approval_required":
			var ap struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(ev.Data), &ap) != nil || ap.ID == "" {
				return fmt.Errorf("approval_required without id: %s", ev.Data)
			}
			if err := c.decideApproval(ctx, "approval_decide", ap.ID); err != nil {
				return fmt.Errorf("auto-approve %s: %w", ap.ID, err)
			}
			lc.stats.Approvals.Add(1)
		case "done":
			doneSeen = true
			return errStreamDone
		case "error":
			lc.stats.SSEErrors.Add(1)
			return fmt.Errorf("sse error event: %s", ev.Data)
		}
		return nil
	})
	if err != nil && !doneSeen {
		return
	}
	if doneSeen {
		lc.stats.Chats.Add(1)
		lc.stats.Tokens.Add(tokens)
	}
}

// crudRound 一轮 CRUD churn：server create→patch→delete；provider create→delete。
func (lc *loadController) crudRound(ctx context.Context, c *loadClient, id, seq int) {
	name := fmt.Sprintf("churn-w%d-s%d", id, seq)

	srvID, err := c.createServer(ctx, "server_create", name)
	if err != nil {
		return
	}
	if err := c.patchServer(ctx, "server_patch", srvID, name+"-renamed"); err != nil {
		return
	}
	if err := c.deleteServer(ctx, "server_delete", srvID); err != nil {
		return
	}

	provID, err := c.createProvider(ctx, "provider_create", name, lc.env.mock.URL())
	if err != nil {
		return
	}
	if err := c.deleteProvider(ctx, "provider_delete", provID); err != nil {
		return
	}
	lc.stats.CRUDOps.Add(1)
}

// auditRound 一轮审计分页查询（最多 3 页）。
func (lc *loadController) auditRound(ctx context.Context, c *loadClient) {
	before := int64(0)
	for page := 0; page < 3; page++ {
		n, hasMore, next, err := c.auditPage(ctx, "audit_page", before)
		if err != nil {
			return
		}
		lc.stats.AuditPages.Add(1)
		if !hasMore || next <= 0 || n == 0 {
			return
		}
		before = next
	}
}
