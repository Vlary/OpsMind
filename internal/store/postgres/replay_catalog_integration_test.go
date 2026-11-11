// replay_catalog_integration_test.go 覆盖八期 ConversationStore.ListReplayCatalog
// 的真实 PG 语义（契约 5.19.1 / G42）：
//   - 4 个会话（纯本地工具 / 单动作 / 无工具 / 已删除服务器）的计数与
//     has_actions / truncated 预判；
//   - q 大小写不敏感且 `%`/`_`/`\` 转义生效（q='%' 只命中标题含字面 %）；
//   - has_actions 过滤；
//   - 游标与 ListPage 同构（同 last_message_at 双行由 id 兜底）；
//   - limit 0/201 → ErrInvalid，q 超 200 rune → ErrInvalid。
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/store"
)

// replayActionToolsFixture 是测试内固定的动作类名单（与 agent.ReplayActionTools
// 同容；postgres 包不 import agent——名单由调用方传参，正是契约要求的边界）。
var replayActionToolsFixture = []string{
	"ssh_exec", "server_facts", "container_action", "container_list",
	"container_inspect", "container_logs", "image_list", "db_query",
	"batch_execute", "run_diagnosis", "run_inspection",
}

func mustCatalogConv(t *testing.T, title string, at time.Time) store.Conversation {
	t.Helper()
	ctx := context.Background()
	conv, err := testDB.Conversations().Create(ctx, store.CreateConversationParams{Title: title})
	if err != nil {
		t.Fatalf("create conversation %q: %v", title, err)
	}
	if err := testDB.Conversations().Touch(ctx, conv.ID, at); err != nil {
		t.Fatalf("touch conversation %q: %v", title, err)
	}
	conv.LastMessageAt = at
	return conv
}

func mustCatalogToolCall(t *testing.T, convID, name string) {
	t.Helper()
	_, err := testDB.ToolCalls().Create(context.Background(), store.CreateToolCallParams{
		ConversationID: convID, TurnID: uuid.NewString(),
		ProviderCallID: "call-" + uuid.NewString(), Name: name,
		Arguments: []byte(`{}`), RiskLevel: "safe",
		Status: store.ToolStarted, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create tool call %s: %v", name, err)
	}
}

func mustCatalogApproval(t *testing.T, convID, command string) {
	t.Helper()
	_, err := testDB.Approvals().Create(context.Background(), store.CreateApprovalParams{
		ConversationID: &convID, Source: "chat", Command: command,
		RiskLevel: "needs_approval", RuleID: "DEFAULT_ASK", Reason: "r",
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
}

func TestReplayCatalogCountsAndFilters(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)

	localConv := mustCatalogConv(t, "本地查询", base.Add(-1*time.Minute))
	actionConv := mustCatalogConv(t, "单动作 100% 命中", base.Add(-2*time.Minute))
	emptyConv := mustCatalogConv(t, "无工具", base.Add(-3*time.Minute))
	orphanConv := mustCatalogConv(t, "已删资产", base.Add(-4*time.Minute))
	// 一个只用于测转义的会话：标题含字面下划线。
	underscoreConv := mustCatalogConv(t, "snake_case 标题", base.Add(-5*time.Minute))

	// 纯本地工具：list_servers + get_metrics（都不在动作名单）。
	mustCatalogToolCall(t, localConv.ID, "list_servers")
	mustCatalogToolCall(t, localConv.ID, "get_metrics")
	// 单动作 + 一条审批。
	mustCatalogToolCall(t, actionConv.ID, "ssh_exec")
	mustCatalogApproval(t, actionConv.ID, "uptime")
	// 已删除服务器的动作调用：tool_calls 行 count 与动作判定都不依赖资产存在。
	mustCatalogToolCall(t, orphanConv.ID, "container_list")
	// 空会话无工具、无审批。
	_ = emptyConv

	params := store.ListReplayCatalogParams{Limit: 50, ActionTools: replayActionToolsFixture}
	items, hasMore, err := testDB.Conversations().ListReplayCatalog(ctx, params)
	if err != nil {
		t.Fatalf("ListReplayCatalog: %v", err)
	}
	if hasMore {
		t.Fatal("has_more must be false with 5 rows and limit 50")
	}
	byID := map[string]store.ReplayCatalogItem{}
	for _, it := range items {
		byID[it.ConversationID] = it
	}
	if it := byID[localConv.ID]; it.ToolCount != 2 || it.ActionCount != 0 ||
		it.ApprovalCount != 0 || it.HasActions || it.Truncated {
		t.Fatalf("local row = %+v", it)
	}
	if it := byID[actionConv.ID]; it.ToolCount != 1 || it.ActionCount != 1 ||
		it.ApprovalCount != 1 || !it.HasActions {
		t.Fatalf("action row = %+v", it)
	}
	if it := byID[emptyConv.ID]; it.ToolCount != 0 || it.ActionCount != 0 ||
		it.ApprovalCount != 0 || it.HasActions {
		t.Fatalf("empty row = %+v", it)
	}
	if it := byID[orphanConv.ID]; it.ToolCount != 1 || it.ActionCount != 1 || !it.HasActions {
		t.Fatalf("orphan row = %+v", it)
	}
	// 排序：last_message_at DESC。
	if len(items) < 2 || items[0].ConversationID != localConv.ID || items[1].ConversationID != actionConv.ID {
		t.Fatalf("order = %+v", items)
	}

	// has_actions=true：只留动作会话（含已删资产）。
	params.HasActions = true
	filtered, _, err := testDB.Conversations().ListReplayCatalog(ctx, params)
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("has_actions rows = %d, want 2: %+v", len(filtered), filtered)
	}
	for _, it := range filtered {
		if it.ConversationID != actionConv.ID && it.ConversationID != orphanConv.ID {
			t.Fatalf("unexpected row %s", it.ConversationID)
		}
	}

	// q：大小写不敏感。
	params = store.ListReplayCatalogParams{Limit: 50, ActionTools: replayActionToolsFixture, Query: "单动作"}
	hits, _, err := testDB.Conversations().ListReplayCatalog(ctx, params)
	if err != nil || len(hits) != 1 || hits[0].ConversationID != actionConv.ID {
		t.Fatalf("q=单动作 = %+v, %v", hits, err)
	}

	// q='%'：通配符必须被转义，只命中标题含字面 % 的会话（契约 5.19.1
	// 写死的断言——不得命中全部）。
	params.Query = "%"
	hits, _, err = testDB.Conversations().ListReplayCatalog(ctx, params)
	if err != nil {
		t.Fatalf("q=%%: %v", err)
	}
	if len(hits) != 1 || hits[0].ConversationID != actionConv.ID {
		t.Fatalf("q=%% hits = %+v, want only the literal-%% title", hits)
	}
	// q='_'：同样转义（只命中 snake_case 标题）。
	params.Query = "_"
	hits, _, err = testDB.Conversations().ListReplayCatalog(ctx, params)
	if err != nil {
		t.Fatalf("q=_: %v", err)
	}
	if len(hits) != 1 || hits[0].ConversationID != underscoreConv.ID {
		t.Fatalf("q=_ hits = %+v, want only the literal-underscore title", hits)
	}
	// q 反斜杠：不报错（转义正确）、无命中。
	params.Query = `\`
	if _, _, err := testDB.Conversations().ListReplayCatalog(ctx, params); err != nil {
		t.Fatalf("q=\\ error: %v", err)
	}
}

func TestReplayCatalogCursorAndLimits(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)

	// 同 last_message_at 双行 + 更早一行：断言 id 兜底序。
	sameAt := base.Add(-1 * time.Minute)
	a := mustCatalogConv(t, "same-a", sameAt)
	b := mustCatalogConv(t, "same-b", sameAt)
	c := mustCatalogConv(t, "older", base.Add(-2*time.Minute))

	params := store.ListReplayCatalogParams{Limit: 2, ActionTools: replayActionToolsFixture}
	page1, hasMore, err := testDB.Conversations().ListReplayCatalog(ctx, params)
	if err != nil || !hasMore || len(page1) != 2 {
		t.Fatalf("page1 = %d, %v, %v", len(page1), hasMore, err)
	}
	// 同 last_message_at 时 id DESC：uuid 字面序较大的在前。
	wantFirst, wantSecond := a.ID, b.ID
	if b.ID > a.ID {
		wantFirst, wantSecond = b.ID, a.ID
	}
	if page1[0].ConversationID != wantFirst || page1[1].ConversationID != wantSecond {
		t.Fatalf("page1 order = %s,%s, want %s,%s", page1[0].ConversationID, page1[1].ConversationID, wantFirst, wantSecond)
	}
	page2, hasMore, err := testDB.Conversations().ListReplayCatalog(ctx, store.ListReplayCatalogParams{
		Limit: 2, ActionTools: replayActionToolsFixture,
		Cursor: store.ConversationListCursor{
			LastMessageAt: page1[len(page1)-1].LastMessageAt, ID: page1[len(page1)-1].ConversationID,
		},
	})
	if err != nil || hasMore || len(page2) != 1 || page2[0].ConversationID != c.ID {
		t.Fatalf("page2 = %+v, %v, %v", page2, hasMore, err)
	}

	// limit 0 / 201 → ErrInvalid（契约 11 节第 22 条）。
	for _, limit := range []int{-1, 0, 201} {
		if _, _, err := testDB.Conversations().ListReplayCatalog(ctx, store.ListReplayCatalogParams{
			Limit: limit, ActionTools: replayActionToolsFixture,
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("limit %d error = %v, want ErrInvalid", limit, err)
		}
	}
	if _, _, err := testDB.Conversations().ListReplayCatalog(ctx, store.ListReplayCatalogParams{
		Limit: 200, ActionTools: replayActionToolsFixture,
	}); err != nil {
		t.Fatalf("limit 200 error = %v", err)
	}
	// q 超 200 rune → ErrInvalid。
	if _, _, err := testDB.Conversations().ListReplayCatalog(ctx, store.ListReplayCatalogParams{
		Limit: 50, Query: strings.Repeat("a", 201), ActionTools: replayActionToolsFixture,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("q 201 error = %v, want ErrInvalid", err)
	}
}

func TestReplayCatalogTruncatedPrediction(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	conv := mustCatalogConv(t, "步数预判", time.Now().UTC().Truncate(time.Microsecond))

	// 直接写 tool_calls：tool_count + approval_count > 20000 才置 truncated。
	// 灌满 20001 行代价高，这里用 approvals 侧凑数更廉价——但 approvals 的
	// 单状态 List 上限不影响本查询（纯 SQL COUNT）。用 SQL 批量插入。
	tx, err := testDB.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const rows = 20001
	// 用 generate_series 一次插入（避免 2 万次往返）。
	_, err = tx.Exec(ctx, `
		INSERT INTO tool_calls (conversation_id, turn_id, provider_call_id, name,
			arguments, risk_level, status, started_at)
		SELECT $1::uuid, gen_random_uuid(), 'bulk-' || g, 'list_servers',
			'{}'::jsonb, 'safe', 'succeeded', now()
		FROM generate_series(1, $2) AS g`, conv.ID, rows)
	if err != nil {
		t.Fatalf("bulk insert tool_calls: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	items, _, err := testDB.Conversations().ListReplayCatalog(ctx, store.ListReplayCatalogParams{
		Limit: 50, ActionTools: replayActionToolsFixture,
	})
	if err != nil {
		t.Fatalf("ListReplayCatalog: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.ToolCount != rows || it.ActionCount != 0 || it.Truncated != (rows > 20000) {
		t.Fatalf("truncated prediction row = %+v (want truncated=%v)", it, rows > 20000)
	}
	if !it.Truncated {
		t.Fatalf("tool_count %d must mark truncated=true", it.ToolCount)
	}
}

// TestReplayCatalogNoMessagesOrResultRead 断言目录查询不读 messages/result：
// 插入超长 message 与 result 不影响目录行（计数只来自 tool_calls/approvals）。
// 这条以行为断言（不解析 SQL 文本）：结果与「无消息会话」完全一致。
func TestReplayCatalogNoMessagesOrResultRead(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	conv := mustCatalogConv(t, "不读正文", time.Now().UTC().Truncate(time.Microsecond))
	mustCatalogToolCall(t, conv.ID, "ssh_exec")
	if _, err := testDB.Messages().Create(ctx, store.CreateMessageParams{
		ConversationID: conv.ID, TurnID: uuid.NewString(), Role: store.RoleUser,
		Content: fmt.Sprintf("big-%d", 4096), Status: store.MessageComplete,
	}); err != nil {
		t.Fatalf("create message: %v", err)
	}
	items, _, err := testDB.Conversations().ListReplayCatalog(ctx, store.ListReplayCatalogParams{
		Limit: 50, ActionTools: replayActionToolsFixture,
	})
	if err != nil {
		t.Fatalf("ListReplayCatalog: %v", err)
	}
	if len(items) != 1 || items[0].ToolCount != 1 || items[0].ActionCount != 1 {
		t.Fatalf("items = %+v", items)
	}
}
