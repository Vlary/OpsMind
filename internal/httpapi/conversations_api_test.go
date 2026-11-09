package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/store"
)

func createConversation(t *testing.T, env *testEnv) string {
	t.Helper()
	rec := env.do(http.MethodPost, "/api/conversations", `{"title":"测试会话"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create conversation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var conv struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &conv)
	return conv.ID
}

func TestConversationCRUD(t *testing.T) {
	env := newTestEnv(t)

	// 空列表返回 [] 而不是 null。
	rec := env.do(http.MethodGet, "/api/conversations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty list body = %s", rec.Body.String())
	}

	// 无 body 创建 → 默认标题。
	rec = env.do(http.MethodPost, "/api/conversations", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create without body status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "新会话") {
		t.Fatalf("default title missing: %s", rec.Body.String())
	}

	id := createConversation(t, env)

	// 获取。
	rec = env.do(http.MethodGet, "/api/conversations/"+id, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "测试会话") {
		t.Fatalf("get status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 重命名。
	rec = env.do(http.MethodPatch, "/api/conversations/"+id, `{"title":"新标题"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "新标题") {
		t.Fatalf("rename status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 删除。
	rec = env.do(http.MethodDelete, "/api/conversations/"+id, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body = %s", rec.Code, rec.Body.String())
	}
	// 再取 → 404。
	rec = env.do(http.MethodGet, "/api/conversations/"+id, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted status = %d", rec.Code)
	}
	code, _ := errorBody(t, rec)
	if code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
}

func TestConversationValidation(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"create title 201 runes", http.MethodPost, "/api/conversations",
			`{"title":"` + strings.Repeat("标", 201) + `"}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"create title whitespace only", http.MethodPost, "/api/conversations",
			`{"title":"   "}`, http.StatusCreated, ""}, // TrimSpace 后落默认标题
		{"create unknown field", http.MethodPost, "/api/conversations",
			`{"title":"x","nope":1}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"rename empty title", http.MethodPatch, "/api/conversations/xxx",
			`{"title":""}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"rename title too long", http.MethodPatch, "/api/conversations/xxx",
			`{"title":"` + strings.Repeat("a", 201) + `"}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"rename missing conversation", http.MethodPatch, "/api/conversations/missing",
			`{"title":"t"}`, http.StatusNotFound, "NOT_FOUND"},
		{"delete missing conversation", http.MethodDelete, "/api/conversations/missing",
			"", http.StatusNotFound, "NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if tc.code != "" {
				code, _ := errorBody(t, rec)
				if code != tc.code {
					t.Fatalf("code = %s, want %s", code, tc.code)
				}
			}
		})
	}
}

func TestDeleteConversationBlockedWhileStreaming(t *testing.T) {
	env := newTestEnv(t)
	id := createConversation(t, env)

	release, err := env.streams.TryAcquire(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	rec := env.do(http.MethodDelete, "/api/conversations/"+id, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	code, _ := errorBody(t, rec)
	if code != "STREAM_ACTIVE" {
		t.Fatalf("code = %s", code)
	}
}

func TestConversationStoreErrorMapsTo500(t *testing.T) {
	env := newTestEnv(t)
	env.st.errConversationsGet = errStoreFailure
	rec := env.do(http.MethodGet, "/api/conversations/whatever", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	code, _ := errorBody(t, rec)
	if code != "INTERNAL" {
		t.Fatalf("code = %s", code)
	}
	env.st.errConversationsGet = nil
}

var errStoreFailure = errors.New("store failure")

func seedMessages(t *testing.T, env *testEnv, conversationID string, n int) {
	t.Helper()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		role := store.RoleUser
		if i%2 == 1 {
			role = store.RoleAssistant
		}
		if _, err := env.st.Messages().Create(t.Context(), store.CreateMessageParams{
			ConversationID: conversationID, TurnID: "turn", Role: role,
			Content: "hello", Status: store.MessageComplete, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMessageAndToolCallPagination(t *testing.T) {
	env := newTestEnv(t)
	id := createConversation(t, env)
	seedMessages(t, env, id, 3)
	for i := 0; i < 2; i++ {
		if _, err := env.st.ToolCalls().Create(t.Context(), store.CreateToolCallParams{
			ConversationID: id, TurnID: "turn", ProviderCallID: "p1", Name: "list_servers",
			Arguments: []byte(`{}`), Status: store.ToolSucceeded, StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// limit=2 → has_more + next_before_seq。
	rec := env.do(http.MethodGet, "/api/conversations/"+id+"/messages?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items         []store.Message `json:"items"`
		HasMore       bool            `json:"has_more"`
		NextBeforeSeq *int64          `json:"next_before_seq"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 || !page.HasMore || page.NextBeforeSeq == nil || *page.NextBeforeSeq != page.Items[0].Seq {
		t.Fatalf("page = %+v", page)
	}

	// before_seq 翻页：剩下的 2 条（seq 递减），不再有更多。
	rec = env.do(http.MethodGet, "/api/conversations/"+id+"/messages?limit=2&before_seq="+itoa(int(*page.NextBeforeSeq)), "")
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 || page.HasMore || page.Items[0].Seq < page.Items[1].Seq {
		t.Fatalf("second page = %+v", page)
	}

	// 校验失败分支。
	for _, q := range []string{"?limit=0", "?limit=999", "?limit=abc", "?before_seq=-1", "?before_seq=bad"} {
		rec = env.do(http.MethodGet, "/api/conversations/"+id+"/messages"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("messages %s status = %d", q, rec.Code)
		}
	}

	// 工具调用分页同理。
	rec = env.do(http.MethodGet, "/api/conversations/"+id+"/tool-calls?limit=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tool calls status = %d", rec.Code)
	}
	var tcPage struct {
		Items   []store.ToolCall `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	decodeBody(t, rec, &tcPage)
	if len(tcPage.Items) != 1 || !tcPage.HasMore {
		t.Fatalf("tool page = %+v", tcPage)
	}

	// 缺少会话 → 空列表（ListPage 不校验会话存在性）。
	rec = env.do(http.MethodGet, "/api/conversations/none/messages", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("missing conversation messages = %d %s", rec.Code, rec.Body.String())
	}

	// 存储错误 → 500。
	env.st.errMessagesList = errStoreFailure
	rec = env.do(http.MethodGet, "/api/conversations/"+id+"/messages", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("messages store error status = %d", rec.Code)
	}
	env.st.errMessagesList = nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// /tool-calls 必须透出落库的 error_message：刷新/重进会话后，前端历史
// 工具卡才有与 SSE tool_result.error.message 一致的底层原因。
func TestToolCallPageExposesErrorMessage(t *testing.T) {
	env := newTestEnv(t)
	id := createConversation(t, env)
	tc, err := env.st.ToolCalls().Create(t.Context(), store.CreateToolCallParams{
		ConversationID: id, TurnID: "turn", ProviderCallID: "p1", Name: "ssh_exec",
		Arguments: []byte(`{"command":"uptime"}`), Status: store.ToolStarted, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.ToolCalls().Finish(t.Context(), store.FinishToolCallParams{
		ID: tc.ID, Status: store.ToolFailed, ErrorCode: "SSH_EXEC_FAILED",
		ErrorMessage: "sshx: dial 127.0.0.1:22: connect: connection refused",
		FinishedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rec := env.do(http.MethodGet, "/api/conversations/"+id+"/tool-calls", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error_message":"sshx: dial 127.0.0.1:22: connect: connection refused"`) {
		t.Fatalf("response missing error_message: %s", body)
	}
	var page struct {
		Items []store.ToolCall `json:"items"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ErrorMessage == "" {
		t.Fatalf("page = %+v", page)
	}
}

// seedConversationsAt 直接向内存 store 塞 n 个会话：id 为合法 uuid、
// last_message_at 从 base 起每条递减 step（模拟按活跃时间排序的历史会话）。
// 返回按排序（新→旧）排列的会话。
func seedConversationsAt(t *testing.T, env *testEnv, n int, base time.Time, step time.Duration) []store.Conversation {
	t.Helper()
	out := make([]store.Conversation, 0, n)
	for i := 0; i < n; i++ {
		at := base.Add(-time.Duration(i) * step)
		conv := store.Conversation{
			ID: uuid.NewString(), Title: "seed-" + strconv.Itoa(i),
			CreatedAt: at, UpdatedAt: at, LastMessageAt: at,
		}
		env.st.conversations[conv.ID] = conv
		out = append(out, conv)
	}
	return out
}

func TestConversationListCursorPagination(t *testing.T) {
	env := newTestEnv(t)
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	seeded := seedConversationsAt(t, env, 5, base, 7*time.Minute)

	// 默认（无参数）：兼容旧契约，最多 200 条全量返回，无更多。
	rec := env.do(http.MethodGet, "/api/conversations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("default list status = %d body = %s", rec.Code, rec.Body.String())
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
	if len(page.Items) != 5 || page.HasMore || page.NextCursor != nil {
		t.Fatalf("default page = %d items, has_more = %v, next = %+v", len(page.Items), page.HasMore, page.NextCursor)
	}

	// 第一页 limit=2：最新两条 + has_more + 游标（本页末条排序键）。
	rec = env.do(http.MethodGet, "/api/conversations?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d body = %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page1 = %d items, has_more = %v", len(page.Items), page.HasMore)
	}
	if page.Items[0].ID != seeded[0].ID || page.Items[1].ID != seeded[1].ID {
		t.Fatalf("page1 order = %s, %s", page.Items[0].ID, page.Items[1].ID)
	}
	if page.NextCursor.ID != seeded[1].ID || !page.NextCursor.LastMessageAt.Equal(seeded[1].LastMessageAt) {
		t.Fatalf("page1 cursor = %+v", page.NextCursor)
	}

	// 用游标翻第二页：seeded[2]、seeded[3]，仍有更多。
	q := "/api/conversations?limit=2&before_last_message_at=" +
		url.QueryEscape(page.NextCursor.LastMessageAt.Format(time.RFC3339Nano)) +
		"&before_id=" + page.NextCursor.ID
	rec = env.do(http.MethodGet, q, "")
	decodeBody(t, rec, &page)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page2 = %d items, has_more = %v, next = %+v", len(page.Items), page.HasMore, page.NextCursor)
	}
	if page.Items[0].ID != seeded[2].ID || page.Items[1].ID != seeded[3].ID {
		t.Fatalf("page2 order = %s, %s", page.Items[0].ID, page.Items[1].ID)
	}

	// 末页：只剩最旧一条，has_more=false，next_cursor=null。
	q = "/api/conversations?limit=2&before_last_message_at=" +
		url.QueryEscape(page.NextCursor.LastMessageAt.Format(time.RFC3339Nano)) +
		"&before_id=" + page.NextCursor.ID
	rec = env.do(http.MethodGet, q, "")
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ID != seeded[4].ID || page.HasMore || page.NextCursor != nil {
		t.Fatalf("last page = %+v", page)
	}

	// 游标与 limit 的校验失败分支。
	for _, bad := range []string{
		"?limit=0", "?limit=999", "?limit=abc",
		"?before_id=" + seeded[0].ID,
		"?before_last_message_at=" + url.QueryEscape(base.Format(time.RFC3339Nano)),
		"?before_last_message_at=not-a-time&before_id=" + seeded[0].ID,
		"?before_last_message_at=" + url.QueryEscape(base.Format(time.RFC3339Nano)) + "&before_id=not-a-uuid",
	} {
		rec = env.do(http.MethodGet, "/api/conversations"+bad, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("conversations %s status = %d", bad, rec.Code)
		}
	}
}
