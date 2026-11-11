// replay_catalog_test.go 覆盖八期 GET /api/replays（契约 2.12 约定 4 /
// 5.19.1 / G42 的 handler 部分）：ACL（未登录 401）、分页（游标 + 缺省 50 +
// 上限 200 + 越界 400）、q 过滤与超长 400、has_actions 过滤、空数组非 null、
// 游标必须成对。
//
// store 侧 SQL 的集成断言（真实 PG）在 internal/store/postgres 的
// replay_catalog_integration_test.go；此处只测 handler 与内存 fake 的语义。
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiops/internal/store"
)

// seedReplayCatalog 构造四个会话（契约 G42 的形状）：纯本地工具 / 单动作 /
// 无工具 / 已删除服务器的动作；返回按 last_message_at DESC 的顺序。
func seedReplayCatalog(t *testing.T, env *testEnv) (localID, actionID, emptyID, orphanID string) {
	t.Helper()
	const (
		catLocalID  = "11111111-1111-4111-8111-111111111111"
		catActionID = "22222222-2222-4222-8222-222222222222"
		catEmptyID  = "33333333-3333-4333-8333-333333333333"
		catOrphanID = "44444444-4444-4444-8444-444444444444"
	)
	base := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	env.st.mu.Lock()
	defer env.st.mu.Unlock()

	mk := func(id, title string, at time.Time) {
		env.st.conversations[id] = store.Conversation{ID: id, Title: title, LastMessageAt: at}
	}
	// 会话 ID 用 uuid 字面量：目录游标要求 before_id 是 uuid（契约 5.19.1
	// 复用 queryConversationCursor 的三条 400 分支）。
	mk(catLocalID, "本地查询", base.Add(3*time.Minute))
	mk(catActionID, "动作会话", base.Add(2*time.Minute))
	mk(catEmptyID, "空会话", base.Add(time.Minute))
	mk(catOrphanID, "已删资产", base)

	addCall := func(seq int64, convID, name string, status store.ToolCallStatus) {
		env.st.toolCalls = append(env.st.toolCalls, store.ToolCall{
			ID: fmt.Sprintf("tc-%d", seq), Seq: seq, ConversationID: convID,
			TurnID: "turn-1", ProviderCallID: fmt.Sprintf("call-%d", seq), Name: name,
			Arguments: json.RawMessage(`{}`), RiskLevel: "safe", Status: status,
			StartedAt: base.Add(time.Duration(seq) * time.Second),
		})
	}
	addCall(1, catLocalID, "list_servers", store.ToolSucceeded)
	addCall(2, catLocalID, "get_metrics", store.ToolSucceeded)
	addCall(3, catActionID, "ssh_exec", store.ToolSucceeded)
	// 已删除资产：动作调用仍然计数（has_actions 是「发生过的动作事实」）。
	addCall(4, catOrphanID, "container_list", store.ToolFailed)

	convAction := catActionID
	env.st.approvals["ap-catalog"] = store.Approval{
		ID: "ap-catalog", ConversationID: &convAction, Source: "chat",
		Command: "uptime", RiskLevel: "needs_approval", Status: store.ApprovalApproved,
		ServerName: "web-1", RequestedAt: base.Add(90 * time.Second),
	}
	return catLocalID, catActionID, catEmptyID, catOrphanID
}

func TestReplayCatalogACLAndShape(t *testing.T) {
	env := newTestEnv(t)

	// ACL：未登录 401（与 GET /api/conversations 同权限口径）。
	req := httptest.NewRequest(http.MethodGet, "/api/replays", nil)
	req.Header.Set("Origin", testOrigin)
	raw := httptest.NewRecorder()
	env.router.ServeHTTP(raw, req)
	if raw.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", raw.Code)
	}

	// 空库：items=[]（不是 null）、has_more=false、next_cursor=null。
	rec := env.do(http.MethodGet, "/api/replays", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty catalog status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items      []store.ReplayCatalogItem `json:"items"`
		HasMore    bool                      `json:"has_more"`
		NextCursor *conversationCursor       `json:"next_cursor"`
	}
	decodeBody(t, rec, &page)
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("items = %+v, want []", page.Items)
	}
	if page.HasMore || page.NextCursor != nil {
		t.Fatalf("has_more=%v next_cursor=%v", page.HasMore, page.NextCursor)
	}
	if body := rec.Body.String(); !containsJSONKey(body, `"items":[]`) {
		t.Fatalf("items must serialize as [] not null: %s", body)
	}
}

func TestReplayCatalogCountsAndFilters(t *testing.T) {
	env := newTestEnv(t)
	localID, actionID, emptyID, orphanID := seedReplayCatalog(t, env)

	// 全量：按 last_message_at DESC（本地 → 动作 → 空 → 已删资产）。
	rec := env.do(http.MethodGet, "/api/replays", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items   []store.ReplayCatalogItem `json:"items"`
		HasMore bool                      `json:"has_more"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(page.Items))
	}
	wantOrder := []string{localID, actionID, emptyID, orphanID}
	for i, id := range wantOrder {
		if page.Items[i].ConversationID != id {
			t.Fatalf("items[%d] = %s, want %s (last_message_at DESC)", i, page.Items[i].ConversationID, id)
		}
	}
	byID := map[string]store.ReplayCatalogItem{}
	for _, it := range page.Items {
		byID[it.ConversationID] = it
	}
	if it := byID[localID]; it.ToolCount != 2 || it.ActionCount != 0 || it.HasActions || it.Truncated {
		t.Fatalf("local row = %+v", it)
	}
	if it := byID[actionID]; it.ToolCount != 1 || it.ActionCount != 1 || it.ApprovalCount != 1 || !it.HasActions {
		t.Fatalf("action row = %+v", it)
	}
	if it := byID[emptyID]; it.ToolCount != 0 || it.ActionCount != 0 || it.ApprovalCount != 0 || it.HasActions {
		t.Fatalf("empty row = %+v", it)
	}
	// 已删除服务器的动作调用仍是动作事实（契约 7.8：不因删除资产丢步骤）。
	if it := byID[orphanID]; it.ActionCount != 1 || !it.HasActions {
		t.Fatalf("orphan row = %+v", it)
	}

	// has_actions=true：只留动作会话与已删资产会话。
	rec = env.do(http.MethodGet, "/api/replays?has_actions=true", "")
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 {
		t.Fatalf("has_actions items = %d, want 2: %+v", len(page.Items), page.Items)
	}
	for _, it := range page.Items {
		if !it.HasActions {
			t.Fatalf("has_actions=false row leaked: %+v", it)
		}
	}

	// q：大小写不敏感子串、字面匹配（% 不作通配符）。
	env.st.mu.Lock()
	conv := env.st.conversations[actionID]
	conv.Title = "100% 动作会话"
	env.st.conversations[actionID] = conv
	env.st.mu.Unlock()

	rec = env.do(http.MethodGet, "/api/replays?q=100%25", "")
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ConversationID != actionID {
		t.Fatalf("q=%% items = %+v, want only the literal-%% title", page.Items)
	}
	rec = env.do(http.MethodGet, "/api/replays?q=ACTION", "")
	decodeBody(t, rec, &page)
	// 中文标题不含 ACTION；标题改过之后也不匹配「动作会话」。
	if len(page.Items) != 0 {
		t.Fatalf("case-insensitive miss expected, got %+v", page.Items)
	}
	rec = env.do(http.MethodGet, "/api/replays?q=%E5%8A%A8%E4%BD%9C", "") // 「动作」
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ConversationID != actionID {
		t.Fatalf("q=动作 items = %+v", page.Items)
	}
}

func TestReplayCatalogPagination(t *testing.T) {
	env := newTestEnv(t)
	_, _, _, _ = seedReplayCatalog(t, env)

	// limit=2：has_more=true，游标=本页末行。
	rec := env.do(http.MethodGet, "/api/replays?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var page struct {
		Items      []store.ReplayCatalogItem `json:"items"`
		HasMore    bool                      `json:"has_more"`
		NextCursor *conversationCursor       `json:"next_cursor"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page1 = %d items has_more=%v cursor=%+v", len(page.Items), page.HasMore, page.NextCursor)
	}
	last := page.Items[1]
	if !page.NextCursor.LastMessageAt.Equal(last.LastMessageAt) || page.NextCursor.ID != last.ConversationID {
		t.Fatalf("next_cursor = %+v, want last row", page.NextCursor)
	}
	// 第二页：游标之后的两行，has_more=false、next_cursor=null。
	path := fmt.Sprintf("/api/replays?limit=2&before_last_message_at=%s&before_id=%s",
		page.NextCursor.LastMessageAt.Format(time.RFC3339Nano), page.NextCursor.ID)
	rec = env.do(http.MethodGet, path, "")
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 || page.HasMore || page.NextCursor != nil {
		t.Fatalf("page2 = %d items has_more=%v cursor=%+v", len(page.Items), page.HasMore, page.NextCursor)
	}
}

func TestReplayCatalogValidation(t *testing.T) {
	env := newTestEnv(t)
	longQ := ""
	for i := 0; i < replayCatalogQueryMaxRunes+1; i++ {
		longQ += "a"
	}
	cases := []struct {
		name string
		path string
	}{
		{"limit 0", "/api/replays?limit=0"},
		{"limit 201", "/api/replays?limit=201"},
		{"limit not a number", "/api/replays?limit=abc"},
		{"q too long", "/api/replays?q=" + longQ},
		{"cursor at without id", "/api/replays?before_last_message_at=2026-10-30T10:00:00Z"},
		{"cursor id without at", "/api/replays?before_id=11111111-1111-1111-1111-111111111111"},
		{"cursor bad timestamp", "/api/replays?before_last_message_at=not-a-time&before_id=11111111-1111-1111-1111-111111111111"},
		{"cursor bad uuid", "/api/replays?before_last_message_at=2026-10-30T10:00:00Z&before_id=nope"},
		{"has_actions not a bool", "/api/replays?has_actions=maybe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodGet, tc.path, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			code, _ := errorBody(t, rec)
			if code != "VALIDATION_FAILED" {
				t.Fatalf("error code = %q, want VALIDATION_FAILED", code)
			}
		})
	}
	// 边界合法值：limit=200、q 恰好 200 字符。
	if rec := env.do(http.MethodGet, "/api/replays?limit=200", ""); rec.Code != http.StatusOK {
		t.Fatalf("limit 200 status = %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/api/replays?q="+longQ[:replayCatalogQueryMaxRunes], ""); rec.Code != http.StatusOK {
		t.Fatalf("q 200 runes status = %d", rec.Code)
	}
}

// containsJSONKey 判断响应体是否含指定 JSON 片段（用于断言 `[]` 而非 null）。
func containsJSONKey(body, fragment string) bool {
	return len(body) >= len(fragment) && indexOf(body, fragment) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
