// rbac_round2_test.go 覆盖 RBAC 第 2 轮验收走查的会话/回放目录归属与
// 用户 PATCH 空操作两项（docs/CONTRACT.md 12.3 第 3 条）。
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/store"
)

// listConversationsAs 以指定 cookie 拉会话列表并解析分页响应。
func listConversationsAs(t *testing.T, env *testEnv, cookie, query string) struct {
	Items      []store.Conversation `json:"items"`
	HasMore    bool                 `json:"has_more"`
	NextCursor *struct {
		LastMessageAt time.Time `json:"last_message_at"`
		ID            string    `json:"id"`
	} `json:"next_cursor"`
} {
	t.Helper()
	rec := doAs(env, http.MethodGet, "/api/conversations"+query, "", cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/conversations%s = %d body = %s", query, rec.Code, rec.Body.String())
	}
	var page struct {
		Items      []store.Conversation `json:"items"`
		HasMore    bool                 `json:"has_more"`
		NextCursor *struct {
			LastMessageAt time.Time `json:"last_message_at"`
			ID            string    `json:"id"`
		} `json:"next_cursor"`
	}
	decodeBody(t, rec, &page)
	return page
}

// TestConversationListFiltersByOwnerInsideWindow 复现第 2 轮走查 high：
// 他人会话占满首页窗口时，当前用户自己的会话必须仍可见、且 next_cursor
// 可取（此前 Go 侧过滤导致 items=[] 且 next_cursor=null，历史全不可达）。
func TestConversationListFiltersByOwnerInsideWindow(t *testing.T) {
	env := newTestEnv(t)
	ops := addTestUser(t, env, "ops-window", "ops-pw", store.RoleOperator, true)
	opsCookie := loginAs(t, env, "ops-window", "ops-pw")

	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// admin 的 12 条会话更「新」（排在前面），operator 自己 2 条更旧。
	for i := 0; i < 12; i++ {
		at := base.Add(-time.Duration(i) * time.Minute)
		env.st.conversations[fmt.Sprintf("admin-conv-%02d", i)] = store.Conversation{
			ID:            fmt.Sprintf("11111111-1111-4111-8111-1111111111%02d", i),
			Title:         "admin-" + fmt.Sprint(i),
			UserID:        "user-1",
			CreatedAt:     at,
			UpdatedAt:     at,
			LastMessageAt: at,
		}
	}
	own := make([]store.Conversation, 0, 2)
	for i := 0; i < 2; i++ {
		at := base.Add(-time.Hour - time.Duration(i)*time.Minute)
		conv := store.Conversation{
			ID:            fmt.Sprintf("22222222-2222-4222-8222-2222222222%02d", i),
			Title:         "own-" + fmt.Sprint(i),
			UserID:        ops.ID,
			CreatedAt:     at,
			UpdatedAt:     at,
			LastMessageAt: at,
		}
		env.st.conversations[conv.ID] = conv
		own = append(own, conv)
	}

	// limit=1：必须返回自己最新那条，并且 has_more 与游标成对出现
	// （不能是 items=[] + has_more=true + next_cursor=null）。
	page := listConversationsAs(t, env, opsCookie, "?limit=1")
	if len(page.Items) != 1 || page.Items[0].ID != own[0].ID {
		t.Fatalf("limit=1 items = %+v, want own[0]=%s", page.Items, own[0].ID)
	}
	if !page.HasMore || page.NextCursor == nil {
		t.Fatalf("limit=1 has_more=%v next_cursor=%+v, want both set", page.HasMore, page.NextCursor)
	}
	if page.NextCursor.ID != own[0].ID {
		t.Fatalf("next_cursor id = %s, want %s", page.NextCursor.ID, own[0].ID)
	}

	// 用游标翻到第二页：只剩自己第二条，无更多。
	q := "?limit=1&before_last_message_at=" +
		url.QueryEscape(page.NextCursor.LastMessageAt.Format(time.RFC3339Nano)) +
		"&before_id=" + page.NextCursor.ID
	page = listConversationsAs(t, env, opsCookie, q)
	if len(page.Items) != 1 || page.Items[0].ID != own[1].ID || page.HasMore || page.NextCursor != nil {
		t.Fatalf("page2 = %+v (has_more=%v next=%+v)", page.Items, page.HasMore, page.NextCursor)
	}

	// limit=200 全量：只返回自己的 2 条（他人的 12 条不得出现）。
	page = listConversationsAs(t, env, opsCookie, "?limit=200")
	if len(page.Items) != 2 || page.HasMore {
		t.Fatalf("limit=200 items = %d has_more=%v, want 2/false", len(page.Items), page.HasMore)
	}
	for _, item := range page.Items {
		// Conversation.UserID 是 json:"-"（API 不暴露 owner），因此以 id 集合
		// 断言：返回的必须恰好是自己那两条。
		if item.ID != own[0].ID && item.ID != own[1].ID {
			t.Fatalf("leaked foreign conversation %s", item.ID)
		}
	}

	// admin 拥有 audit:read：第 4 轮走查 medium 起与 /api/replays 同源，
	// 拿全量 14 条（12 自己 + ops 的 2 条），不再是「只看自己」。
	adminPage := listConversationsAs(t, env, env.cookie, "?limit=200")
	if len(adminPage.Items) != 14 {
		t.Fatalf("admin (audit:read) items = %d, want 14", len(adminPage.Items))
	}
}

// TestReplayCatalogFiltersByOwnerInsideWindow 复现第 2 轮走查 high（回放
// 目录同根因）：他人会话占满窗口时目录为空且游标为空；audit:read 仍拿全量。
func TestReplayCatalogFiltersByOwnerInsideWindow(t *testing.T) {
	env := newTestEnv(t)
	ops := addTestUser(t, env, "ops-catalog", "ops-pw", store.RoleOperator, true)
	opsCookie := loginAs(t, env, "ops-catalog", "ops-pw")
	addTestUser(t, env, "auditor-catalog", "aud-pw", store.RoleAuditor, true)
	auditorCookie := loginAs(t, env, "auditor-catalog", "aud-pw")

	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		at := base.Add(-time.Duration(i) * time.Minute)
		env.st.conversations[fmt.Sprintf("admin-cat-%02d", i)] = store.Conversation{
			ID:            fmt.Sprintf("33333333-3333-4333-8333-3333333333%02d", i),
			Title:         "admin-cat-" + fmt.Sprint(i),
			UserID:        "user-1",
			CreatedAt:     at,
			UpdatedAt:     at,
			LastMessageAt: at,
		}
	}
	ownID := "44444444-4444-4444-8444-444444444444"
	ownAt := base.Add(-time.Hour)
	env.st.conversations[ownID] = store.Conversation{
		ID: ownID, Title: "own-cat", UserID: ops.ID,
		CreatedAt: ownAt, UpdatedAt: ownAt, LastMessageAt: ownAt,
	}

	type catalogPage struct {
		Items      []store.ReplayCatalogItem `json:"items"`
		HasMore    bool                      `json:"has_more"`
		NextCursor *conversationCursor       `json:"next_cursor"`
	}
	getCatalog := func(cookie, query string) catalogPage {
		t.Helper()
		rec := doAs(env, http.MethodGet, "/api/replays"+query, "", cookie, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/replays%s = %d body = %s", query, rec.Code, rec.Body.String())
		}
		var page catalogPage
		decodeBody(t, rec, &page)
		return page
	}

	page := getCatalog(opsCookie, "?limit=200")
	if len(page.Items) != 1 || page.Items[0].ConversationID != ownID {
		t.Fatalf("operator catalog = %+v, want only own conversation %s", page.Items, ownID)
	}
	if page.HasMore {
		t.Fatalf("operator catalog has_more = true with a complete window")
	}
	// 缩小窗口到 1：仍返回自己那一条（不是空页），且 has_more=false、
	// 没有游标——他人的 10 条不得参与窗口计数。
	small := getCatalog(opsCookie, "?limit=1")
	if len(small.Items) != 1 || small.Items[0].ConversationID != ownID || small.HasMore || small.NextCursor != nil {
		t.Fatalf("operator catalog limit=1 = %+v (has_more=%v next=%+v), want own row and no cursor", small.Items, small.HasMore, small.NextCursor)
	}

	// audit:read 跨用户全量只读：11 条（10 admin + 1 operator）。
	audPage := getCatalog(auditorCookie, "?limit=200")
	if len(audPage.Items) != 11 {
		t.Fatalf("auditor catalog = %d items, want 11", len(audPage.Items))
	}
}

// TestUpdateUserNoopDoesNotRevokeSessions 复现第 2 轮走查 medium：对已启用
// 账号重复提交 enabled=true、或对同角色再提交一次 role，不得吊销目标会话。
func TestUpdateUserNoopDoesNotRevokeSessions(t *testing.T) {
	env := newTestEnv(t)
	bob := addTestUser(t, env, "bob-noop", "bob-pw", store.RoleOperator, true)
	bobCookie := loginAs(t, env, "bob-noop", "bob-pw")

	for _, body := range []string{`{"enabled":true}`, `{"role":"operator"}`, `{"enabled":true,"role":"operator"}`} {
		rec := doAs(env, http.MethodPatch, "/api/users/"+bob.ID, body, env.cookie, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d body = %s", body, rec.Code, rec.Body.String())
		}
		if me := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie, ""); me.Code != http.StatusOK {
			t.Fatalf("PATCH %s revoked bob's session (me = %d)", body, me.Code)
		}
	}

	// 真实的角色变更仍必须吊销（不能因本轮改动放宽）。
	rec := doAs(env, http.MethodPatch, "/api/users/"+bob.ID, `{"role":"auditor"}`, env.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("real role change = %d body = %s", rec.Code, rec.Body.String())
	}
	if me := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie, ""); me.Code != http.StatusUnauthorized {
		t.Fatalf("real role change did not revoke session (me = %d)", me.Code)
	}

	// 真实停用同样吊销，并把审计动作区分为 user.disable。
	bob2 := addTestUser(t, env, "bob-second", "bob-pw", store.RoleOperator, true)
	bob2Cookie := loginAs(t, env, "bob-second", "bob-pw")
	rec = doAs(env, http.MethodPatch, "/api/users/"+bob2.ID, `{"enabled":false}`, env.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d body = %s", rec.Code, rec.Body.String())
	}
	if me := doAs(env, http.MethodGet, "/api/auth/me", "", bob2Cookie, ""); me.Code != http.StatusUnauthorized {
		t.Fatalf("disable did not revoke session (me = %d)", me.Code)
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var noopRows, realRows []userAuditRow
	var disableRow *userAuditRow
	for _, row := range env.st.auditLogs {
		switch row.ResourceID {
		case bob.ID:
			var detail map[string]any
			if err := json.Unmarshal(row.Detail, &detail); err != nil {
				t.Fatalf("audit detail: %v", err)
			}
			changed, _ := detail["changed"].(bool)
			entry := userAuditRow{action: row.Action, changed: changed, hasRoleDelta: detail["from_role"] != nil}
			if changed {
				realRows = append(realRows, entry)
			} else {
				noopRows = append(noopRows, entry)
			}
		case bob2.ID:
			var detail map[string]any
			if err := json.Unmarshal(row.Detail, &detail); err != nil {
				t.Fatalf("audit detail: %v", err)
			}
			changed, _ := detail["changed"].(bool)
			entry := userAuditRow{action: row.Action, changed: changed, hasRoleDelta: detail["from_role"] != nil}
			disableRow = &entry
		}
	}
	// 三次同值提交各留一条 changed=false 的 user.update，且都不带角色增量。
	if len(noopRows) != 3 {
		t.Fatalf("noop audits = %d (%+v), want 3", len(noopRows), noopRows)
	}
	for _, row := range noopRows {
		if row.action != "user.update" || row.changed || row.hasRoleDelta {
			t.Fatalf("noop audit = %+v, want user.update with changed=false and no role delta", row)
		}
	}
	// 真实角色变更一条 changed=true 且带 from_role。
	if len(realRows) != 1 || !realRows[0].hasRoleDelta {
		t.Fatalf("real audits = %+v, want one changed=true with from_role", realRows)
	}
	if disableRow == nil || disableRow.action != "user.disable" || !disableRow.changed {
		t.Fatalf("disable audit = %+v, want user.disable with changed=true", disableRow)
	}
}

// userAuditRow 是审计断言的精简视图（避免把整行拷贝进测试）。
type userAuditRow struct {
	action       string
	changed      bool
	hasRoleDelta bool
}

// TestUpdateUserDisplayNameOnlyAuditChangedFalse 覆盖第 4 轮走查 low：
// 仅修改 display_name 时 user.update 审计的 changed 必须为 false
// （changed 只表示 role/enabled 权限类变更），展示名变化由
// display_name_changed 单独表达（契约 9.7）。
func TestUpdateUserDisplayNameOnlyAuditChangedFalse(t *testing.T) {
	env := newTestEnv(t)
	target := addTestUser(t, env, "nick-user", "nick-pw", store.RoleOperator, true)

	rec := doAs(env, http.MethodPatch, "/api/users/"+target.ID,
		`{"display_name":"运维一号改"}`, env.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("patch display_name = %d body = %s", rec.Code, rec.Body.String())
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}

	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var found bool
	for _, row := range env.st.auditLogs {
		if row.ResourceID != target.ID || row.Action != "user.update" {
			continue
		}
		var detail map[string]any
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			t.Fatalf("audit detail: %v", err)
		}
		found = true
		if changed, _ := detail["changed"].(bool); changed {
			t.Fatalf("display-name-only update must keep changed=false, detail=%v", detail)
		}
		if flagged, _ := detail["display_name_changed"].(bool); !flagged {
			t.Fatalf("display_name_changed = %v, want true", detail["display_name_changed"])
		}
		if name, _ := detail["changes"].(map[string]any)["display_name"].(string); name != "运维一号改" {
			t.Fatalf("changes.display_name = %v", detail["changes"])
		}
	}
	if !found {
		t.Fatal("user.update audit row missing")
	}
}

// TestAvailableGrantsEndpointScopedToCaller 覆盖第 2 轮走查 low 的 handler
// 侧：GET /api/term|rdp/grants 只返回调用者本人的可用授权，approver/auditor
// 没有 terminal:use 时 403（审计员只读也不能枚举他人授权）。
func TestAvailableGrantsEndpointScopedToCaller(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	// 内存 fake 的 Servers().Create 生成非 uuid id；grant 端点要求 uuid，
	// 直接按 term_handlers_test.go 的夹具方式塞一台合法 uuid 的服务器。
	serverID := uuid.NewString()
	env.st.mu.Lock()
	env.st.servers[serverID] = store.Server{
		ID: serverID, Name: "grant-srv", OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root",
		AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc",
	}
	env.st.mu.Unlock()

	// 发起人（operator，有 terminal:use）申请 term 授权 → 由 admin 批准。
	env.ensureRequester(t)
	opsCookie := loginAs(t, env, testRequesterUsername, testPassword)
	rec := doAs(env, http.MethodPost, "/api/term/grants", `{"server_id":"`+serverID+`"}`,
		opsCookie, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create term grant = %d body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Approval store.Approval `json:"approval"`
	}
	decodeBody(t, rec, &created)
	rec = doAs(env, http.MethodPost, "/api/approvals/"+created.Approval.ID+"/decide",
		`{"decision":"approve"}`, env.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve grant = %d body = %s", rec.Code, rec.Body.String())
	}

	// 发起人（operator）看到自己那条。
	rec = doAs(env, http.MethodGet, "/api/term/grants", "", opsCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET term grants = %d body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []store.Approval `json:"items"`
		Count int              `json:"count"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ID != created.Approval.ID || page.Count != 1 {
		t.Fatalf("term grants = %+v (count=%d), want the approved grant", page.Items, page.Count)
	}

	// 管理员（不是发起人）看不到别人的授权。
	rec = doAs(env, http.MethodGet, "/api/term/grants", "", env.cookie, "")
	decodeBody(t, rec, &page)
	if rec.Code != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("admin term grants = %d %+v, want empty 200", rec.Code, page.Items)
	}

	// rdp 维度与 term 隔离：term grant 不出现在 rdp 列表。
	rec = doAs(env, http.MethodGet, "/api/rdp/grants", "", opsCookie, "")
	decodeBody(t, rec, &page)
	if rec.Code != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("rdp grants = %d %+v, want empty 200", rec.Code, page.Items)
	}

	// 审计员 / 审批者没有 terminal:use → 403。
	addTestUser(t, env, "auditor-grants", testPassword, store.RoleAuditor, true)
	auditorCookie := loginAs(t, env, "auditor-grants", testPassword)
	if rec := doAs(env, http.MethodGet, "/api/term/grants", "", auditorCookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("auditor GET term grants = %d, want 403", rec.Code)
	}
	if rec := doAs(env, http.MethodGet, "/api/term/grants", "", env.approverCookie(t), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("approver GET term grants = %d, want 403", rec.Code)
	}

	// 未登录 401。
	if rec := doAs(env, http.MethodGet, "/api/term/grants", "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET term grants = %d, want 401", rec.Code)
	}
}
