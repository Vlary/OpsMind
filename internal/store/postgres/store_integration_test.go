package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/store"
)

var testDB *Store

func cleanDB(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE users, sessions, servers, databases, providers, conversations,
		         messages, tool_calls, approvals, audit_logs, settings
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func mustUser(t *testing.T) store.User {
	t.Helper()
	u, err := testDB.Users().Create(context.Background(), store.CreateUserParams{
		Username:     "admin",
		PasswordHash: "$2a$12$abcdefghijklmnopqrstuv",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestUsersAndSessions(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	user := mustUser(t)
	if n, err := testDB.Users().Count(ctx); err != nil || n != 1 {
		t.Fatalf("Count = %d, %v", n, err)
	}
	if _, err := testDB.Users().Create(ctx, store.CreateUserParams{Username: "admin", PasswordHash: "x"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate user error = %v, want ErrConflict", err)
	}
	if got, err := testDB.Users().ByUsername(ctx, "admin"); err != nil || got.ID != user.ID {
		t.Fatalf("ByUsername = %+v, %v", got, err)
	}
	if got, err := testDB.Users().ByID(ctx, user.ID); err != nil || got.Username != "admin" {
		t.Fatalf("ByID = %+v, %v", got, err)
	}
	if _, err := testDB.Users().ByID(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ByID missing error = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := []byte(strings.Repeat("h", 32))
	session, err := testDB.Sessions().Create(ctx, store.CreateSessionParams{
		UserID: user.ID, TokenHash: hash,
		ExpiresAt: now.Add(time.Hour), IP: "127.0.0.1", UserAgent: "test", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := testDB.Sessions().ByTokenHash(ctx, hash)
	if err != nil || got.ID != session.ID {
		t.Fatalf("ByTokenHash = %+v, %v", got, err)
	}
	if err := testDB.Sessions().Touch(ctx, session.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := testDB.Sessions().Touch(ctx, uuid.NewString(), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Touch missing = %v", err)
	}

	expiredHash := []byte(strings.Repeat("e", 32))
	if _, err := testDB.Sessions().Create(ctx, store.CreateSessionParams{
		UserID: user.ID, TokenHash: expiredHash,
		ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	deleted, err := testDB.Sessions().DeleteExpired(ctx, now)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpired = %d, %v", deleted, err)
	}
	if err := testDB.Sessions().DeleteByUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteByUser: %v", err)
	}
	if _, err := testDB.Sessions().ByTokenHash(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session still exists: %v", err)
	}
}

func TestServersCRUD(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	created, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "web-1", Host: "10.0.0.10", Port: 22, Username: "root",
		Credentials: store.ServerCredentials{
			AuthMethod:        store.AuthMethodPassword,
			PasswordEncrypted: "enc-password",
		},
		Tags:            []string{"prod", "web"},
		AutoExecEnabled: false,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if !created.HasPassword || created.HasPrivateKey {
		t.Fatalf("credential flags wrong: %+v", created)
	}
	if strings.Join(created.Tags, ",") != "prod,web" {
		t.Fatalf("tags = %v", created.Tags)
	}

	updated, err := testDB.Servers().UpdateHostKey(ctx, created.ID, "SHA256:abc", "ssh-ed25519 AAAA", time.Now())
	if err != nil {
		t.Fatalf("UpdateHostKey: %v", err)
	}
	if updated.HostKeyFingerprint != "SHA256:abc" || updated.HostKeyVerifiedAt == nil {
		t.Fatalf("host key not stored: %+v", updated)
	}

	newName := "web-renamed"
	auto := true
	renamed, err := testDB.Servers().Update(ctx, created.ID, store.UpdateServerParams{
		Name: &newName, AutoExecEnabled: &auto,
	})
	if err != nil {
		t.Fatalf("update name: %v", err)
	}
	if renamed.Name != newName || !renamed.AutoExecEnabled {
		t.Fatalf("update not applied: %+v", renamed)
	}
	if renamed.HostKeyFingerprint != "SHA256:abc" {
		t.Fatal("renaming cleared the host key")
	}

	newHost := "10.0.0.20"
	moved, err := testDB.Servers().Update(ctx, created.ID, store.UpdateServerParams{Host: &newHost})
	if err != nil {
		t.Fatalf("update host: %v", err)
	}
	if moved.HostKeyFingerprint != "" || moved.HostKeyPublic != "" || moved.HostKeyVerifiedAt != nil {
		t.Fatalf("host change did not clear host key: %+v", moved)
	}

	if _, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "web-renamed", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "x"},
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate server error = %v", err)
	}

	list, err := testDB.Servers().List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	if err := testDB.Servers().Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := testDB.Servers().Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
}

func TestDatabasesCRUD(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	created, err := testDB.Databases().Create(ctx, store.CreateDatabaseParams{
		Name: "orders", Engine: store.EnginePostgres, Host: "db", Port: 5432,
		DatabaseName: "orders", Username: "aiops",
		Credentials: store.DatabaseCredentials{PasswordEncrypted: "enc"},
		SSLMode:     store.SSLModeRequire,
	})
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if !created.HasPassword {
		t.Fatal("HasPassword = false")
	}

	updated, err := testDB.Databases().Update(ctx, created.ID, store.UpdateDatabaseParams{
		Credentials: &store.DatabaseCredentials{},
	})
	if err != nil {
		t.Fatalf("clear password: %v", err)
	}
	if updated.HasPassword || updated.PasswordEncrypted != "" {
		t.Fatalf("password not cleared: %+v", updated)
	}

	sqlite, err := testDB.Databases().Create(ctx, store.CreateDatabaseParams{
		Name: "local", Engine: store.EngineSQLite, SQLitePath: "/tmp/local.db", SSLMode: store.SSLModeDisable,
	})
	if err != nil {
		t.Fatalf("create sqlite: %v", err)
	}
	if sqlite.SQLitePath != "/tmp/local.db" {
		t.Fatalf("sqlite path = %q", sqlite.SQLitePath)
	}
	if _, err := testDB.Databases().Create(ctx, store.CreateDatabaseParams{
		Name: "bad", Engine: store.EngineSQLite, SQLitePath: "relative.db",
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("relative sqlite path error = %v, want ErrInvalid", err)
	}

	list, err := testDB.Databases().List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	if err := testDB.Databases().Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestProvidersCRUD(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	created, err := testDB.Providers().Create(ctx, store.CreateProviderParams{
		Name: "custom-1", Preset: store.PresetCustom, Kind: store.ProviderOpenAICompatible,
		BaseURL: "https://example.com/v1", DefaultModel: "model-a",
		APIKey: &store.ProviderAPIKey{Encrypted: "enc-key", Hint: "5678"},
		Models: []string{"model-a", "model-b"},
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if !created.HasAPIKey || created.APIKeyMasked != "****5678" {
		t.Fatalf("mask = %q, has = %v", created.APIKeyMasked, created.HasAPIKey)
	}
	if created.APIKeyEncrypted == "" {
		t.Fatal("encrypted key missing")
	}

	cleared, err := testDB.Providers().Update(ctx, created.ID, store.UpdateProviderParams{
		APIKey: &store.ProviderAPIKey{},
	})
	if err != nil {
		t.Fatalf("clear api key: %v", err)
	}
	if cleared.HasAPIKey || cleared.APIKeyMasked != "" || cleared.APIKeyEncrypted != "" {
		t.Fatalf("api key not cleared: %+v", cleared)
	}

	models := []string{"only"}
	updated, err := testDB.Providers().Update(ctx, created.ID, store.UpdateProviderParams{Models: &models})
	if err != nil {
		t.Fatalf("update models: %v", err)
	}
	if len(updated.Models) != 1 || updated.Models[0] != "only" {
		t.Fatalf("models = %v", updated.Models)
	}
	if _, err := testDB.Providers().Get(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
}

func TestConversationProviderBindingAndPagination(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	provider, err := testDB.Providers().Create(ctx, store.CreateProviderParams{
		Name: "p", Preset: store.PresetCustom, Kind: store.ProviderOpenAICompatible,
		BaseURL: "https://example.com/v1", DefaultModel: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := testDB.Conversations().Create(ctx, store.CreateConversationParams{})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if conv.Title != "新会话" {
		t.Fatalf("default title = %q", conv.Title)
	}
	bound, err := testDB.Conversations().BindModel(ctx, conv.ID, provider.ID, "m", time.Now())
	if err != nil {
		t.Fatalf("BindModel: %v", err)
	}
	if bound.ProviderID == nil || *bound.ProviderID != provider.ID {
		t.Fatalf("provider not bound: %+v", bound)
	}
	if err := testDB.Providers().Delete(ctx, provider.ID); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	after, err := testDB.Conversations().Get(ctx, conv.ID)
	if err != nil {
		t.Fatalf("Get after provider delete: %v", err)
	}
	if after.ProviderID != nil {
		t.Fatalf("provider_id = %v, want NULL", *after.ProviderID)
	}

	if _, err := testDB.Conversations().UpdateTitle(ctx, conv.ID, "renamed", time.Now()); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	if err := testDB.Conversations().Touch(ctx, conv.ID, time.Now()); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	list, _, err := testDB.Conversations().ListPage(ctx, store.ConversationListCursor{}, 0)
	if err != nil || len(list) != 1 || list[0].Title != "renamed" {
		t.Fatalf("ListPage = %+v, %v", list, err)
	}

	turnID := uuid.NewString()
	for i := 0; i < 5; i++ {
		_, err := testDB.Messages().Create(ctx, store.CreateMessageParams{
			ConversationID: conv.ID, TurnID: turnID, Role: store.RoleUser,
			Content: "m", Status: store.MessageComplete,
		})
		if err != nil {
			t.Fatalf("create message %d: %v", i, err)
		}
	}
	page, hasMore, err := testDB.Messages().ListPage(ctx, conv.ID, 0, 2)
	if err != nil || !hasMore || len(page) != 2 {
		t.Fatalf("first page = %d, %v, %v", len(page), hasMore, err)
	}
	if page[0].Seq >= page[1].Seq {
		t.Fatalf("page not ascending: %d, %d", page[0].Seq, page[1].Seq)
	}
	older, hasMore, err := testDB.Messages().ListPage(ctx, conv.ID, page[0].Seq, 2)
	if err != nil || !hasMore || len(older) != 2 {
		t.Fatalf("second page = %d, %v, %v", len(older), hasMore, err)
	}
	if older[0].Seq >= page[0].Seq {
		t.Fatal("second page overlaps first page")
	}
	if _, _, err := testDB.Messages().ListPage(ctx, conv.ID, 0, 201); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("limit 201 error = %v", err)
	}
	if _, _, err := testDB.Messages().ListPage(ctx, uuid.NewString(), 0, 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing conversation error = %v", err)
	}

	// 六期打磨 sprint 2：reasoning_duration_ms 往返（NULL ↔ nil，0/正数保留）。
	duration := int64(2500)
	withReasoning, err := testDB.Messages().Create(ctx, store.CreateMessageParams{
		ConversationID: conv.ID, TurnID: turnID, Role: store.RoleAssistant,
		Content: "answer", Reasoning: "think", ReasoningDurationMS: &duration,
		Status: store.MessageComplete,
	})
	if err != nil {
		t.Fatalf("create reasoning message: %v", err)
	}
	if withReasoning.ReasoningDurationMS == nil || *withReasoning.ReasoningDurationMS != duration {
		t.Fatalf("created reasoning_duration_ms = %v, want %d", withReasoning.ReasoningDurationMS, duration)
	}
	latest, _, err := testDB.Messages().ListPage(ctx, conv.ID, 0, 1)
	if err != nil || len(latest) != 1 {
		t.Fatalf("latest message page = %d, %v", len(latest), err)
	}
	if latest[0].ReasoningDurationMS == nil || *latest[0].ReasoningDurationMS != duration {
		t.Fatalf("read reasoning_duration_ms = %v, want %d", latest[0].ReasoningDurationMS, duration)
	}
	plain, err := testDB.Messages().Create(ctx, store.CreateMessageParams{
		ConversationID: conv.ID, TurnID: turnID, Role: store.RoleAssistant,
		Content: "plain", Status: store.MessageComplete,
	})
	if err != nil {
		t.Fatalf("create plain message: %v", err)
	}
	if plain.ReasoningDurationMS != nil {
		t.Fatalf("plain reasoning_duration_ms = %d, want NULL", *plain.ReasoningDurationMS)
	}
	zero := int64(0)
	zeroed, err := testDB.Messages().Create(ctx, store.CreateMessageParams{
		ConversationID: conv.ID, TurnID: turnID, Role: store.RoleAssistant,
		Content: "fast", Reasoning: "t", ReasoningDurationMS: &zero,
		Status: store.MessageComplete,
	})
	if err != nil {
		t.Fatalf("create zero-duration message: %v", err)
	}
	if zeroed.ReasoningDurationMS == nil || *zeroed.ReasoningDurationMS != 0 {
		t.Fatalf("zero reasoning_duration_ms = %v, want 0", zeroed.ReasoningDurationMS)
	}

	// 级联删除：消息随会话消失。
	if err := testDB.Conversations().Delete(ctx, conv.ID); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if _, _, err := testDB.Messages().ListPage(ctx, conv.ID, 0, 10); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("messages survived cascade: %v", err)
	}
}

func TestConversationListPageCursor(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	// seed 5 个会话，last_message_at 严格递减（每分钟一个）。
	base := time.Now().UTC().Truncate(time.Microsecond)
	var seeded []store.Conversation
	for i := 0; i < 5; i++ {
		conv, err := testDB.Conversations().Create(ctx, store.CreateConversationParams{
			Title: "cursor-" + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		at := base.Add(-time.Duration(i) * time.Minute)
		if err := testDB.Conversations().Touch(ctx, conv.ID, at); err != nil {
			t.Fatalf("touch %d: %v", i, err)
		}
		conv.LastMessageAt = at
		seeded = append(seeded, conv)
	}

	// 第一页 limit=2：最新两条 + has_more。
	items, hasMore, err := testDB.Conversations().ListPage(ctx, store.ConversationListCursor{}, 2)
	if err != nil || len(items) != 2 || !hasMore {
		t.Fatalf("page1 = %d items, more=%v, err=%v", len(items), hasMore, err)
	}
	if items[0].ID != seeded[0].ID || items[1].ID != seeded[1].ID {
		t.Fatalf("page1 order = %s, %s", items[0].ID, items[1].ID)
	}

	// 游标翻页（模拟前端回传上一页末条排序键）。
	page := items
	pages := [][]store.Conversation{items}
	for hasMore {
		cursor := store.ConversationListCursor{
			LastMessageAt: page[len(page)-1].LastMessageAt,
			ID:            page[len(page)-1].ID,
		}
		page, hasMore, err = testDB.Conversations().ListPage(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("next page: %v", err)
		}
		pages = append(pages, page)
	}
	// 5 条 → 2+2+1，无重复无遗漏。
	var got []string
	for _, p := range pages {
		for _, c := range p {
			got = append(got, c.ID)
		}
	}
	if len(got) != 5 {
		t.Fatalf("paged ids = %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("duplicated id %s in %v", id, got)
		}
		seen[id] = true
	}
	// 末条应是最旧会话。
	if got[len(got)-1] != seeded[4].ID {
		t.Fatalf("oldest = %s, want %s", got[len(got)-1], seeded[4].ID)
	}

	// 游标早于所有会话 → 空页、无更多。
	items, hasMore, err = testDB.Conversations().ListPage(ctx, store.ConversationListCursor{
		LastMessageAt: base.Add(-time.Hour), ID: uuid.NewString(),
	}, 2)
	if err != nil || len(items) != 0 || hasMore {
		t.Fatalf("exhausted page = %+v, more=%v, err=%v", items, hasMore, err)
	}

	// limit 上限截断（fixedPageLimit：0 → 200）。
	items, _, err = testDB.Conversations().ListPage(ctx, store.ConversationListCursor{}, 0)
	if err != nil || len(items) != 5 {
		t.Fatalf("full list = %d items, err=%v", len(items), err)
	}
}

func TestToolCallsAndApprovals(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := testDB.Conversations().Create(ctx, store.CreateConversationParams{Title: "c"})
	if err != nil {
		t.Fatal(err)
	}

	turnID := uuid.NewString()
	var calls []store.ToolCall
	for i := 0; i < 3; i++ {
		tc, err := testDB.ToolCalls().Create(ctx, store.CreateToolCallParams{
			ConversationID: conv.ID, TurnID: turnID,
			ProviderCallID: "call-" + uuid.NewString(), Name: "ssh_exec",
			Arguments: []byte(`{"cmd":"ls"}`), RiskLevel: "safe", Status: store.ToolStarted,
			StartedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("create tool call: %v", err)
		}
		calls = append(calls, tc)
	}
	if _, err := testDB.ToolCalls().Create(ctx, store.CreateToolCallParams{
		ConversationID: conv.ID, TurnID: turnID,
		ProviderCallID: calls[0].ProviderCallID, Name: "ssh_exec",
		Arguments: []byte(`{}`), RiskLevel: "safe", Status: store.ToolStarted,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate provider call error = %v", err)
	}

	page, hasMore, err := testDB.ToolCalls().ListPage(ctx, conv.ID, 0, 2)
	if err != nil || !hasMore || len(page) != 2 {
		t.Fatalf("tool page = %d, %v, %v", len(page), hasMore, err)
	}
	if page[0].Seq >= page[1].Seq {
		t.Fatal("tool calls not ascending")
	}
	// 六期打磨 sprint 1（迁移 00010）：approval_mode 快照往返（显式 none →
	// 读回 none；未记录 → 空串，前端不渲染门限徽标）。
	withMode, err := testDB.ToolCalls().Create(ctx, store.CreateToolCallParams{
		ConversationID: conv.ID, TurnID: turnID,
		ProviderCallID: "call-mode-" + uuid.NewString(), Name: "ssh_exec",
		Arguments: []byte(`{"cmd":"uptime"}`), RiskLevel: "needs_approval",
		Status: store.ToolStarted, StartedAt: time.Now(),
		ApprovalMode: store.ApprovalModeNone,
	})
	if err != nil {
		t.Fatalf("create tool call with mode: %v", err)
	}
	if withMode.ApprovalMode != string(store.ApprovalModeNone) {
		t.Fatalf("approval_mode = %q, want none", withMode.ApprovalMode)
	}
	byID, err := testDB.ToolCalls().ByID(ctx, withMode.ID)
	if err != nil || byID.ApprovalMode != string(store.ApprovalModeNone) {
		t.Fatalf("ByID approval_mode = (%q, %v)", byID.ApprovalMode, err)
	}
	if calls[0].ApprovalMode != "" {
		t.Fatalf("未指定门限的调用 approval_mode = %q, want 空串（NULL）", calls[0].ApprovalMode)
	}
	finished, err := testDB.ToolCalls().Finish(ctx, store.FinishToolCallParams{
		ID: calls[0].ID, Status: store.ToolSucceeded,
		Result: []byte(`{"exit_code":0}`), FinishedAt: time.Now(),
	})
	if err != nil || finished.FinishedAt == nil {
		t.Fatalf("Finish = %+v, %v", finished, err)
	}
	if _, err := testDB.ToolCalls().SetStatus(ctx, calls[0].ID, store.ToolAwaitingApproval, "", ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// 失败终态的底层错误消息必须落库并读回：历史工具卡与 SSE 实时事件一致。
	failedCall, err := testDB.ToolCalls().Finish(ctx, store.FinishToolCallParams{
		ID: calls[2].ID, Status: store.ToolFailed, ErrorCode: "SSH_EXEC_FAILED",
		ErrorMessage: "sshx: dial 127.0.0.1:22: connect: connection refused",
		FinishedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Finish failed-state: %v", err)
	}
	if failedCall.ErrorMessage != "sshx: dial 127.0.0.1:22: connect: connection refused" {
		t.Fatalf("ErrorMessage = %q", failedCall.ErrorMessage)
	}
	if got, err := testDB.ToolCalls().ByID(ctx, calls[2].ID); err != nil || got.ErrorMessage != failedCall.ErrorMessage {
		t.Fatalf("ByID ErrorMessage = %q, %v", got.ErrorMessage, err)
	}

	requested := time.Now().UTC()
	approval, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		ConversationID: &conv.ID, ToolCallID: &calls[1].ID, ServerID: &server.ID,
		ServerName: "srv", Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		Reason: "service change", ExpiresAt: requested.Add(2 * time.Minute), CreatedAt: requested,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	got, err := testDB.Approvals().Get(ctx, approval.ID)
	if err != nil {
		t.Fatalf("Get approval: %v", err)
	}
	if got.ServerName != "srv" || got.Status != store.ApprovalPending {
		t.Fatalf("approval = %+v", got)
	}
	list, err := testDB.Approvals().List(ctx, conv.ID, store.ApprovalPending, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("List approvals = %d, %v", len(list), err)
	}
	if empty, err := testDB.Approvals().List(ctx, uuid.NewString(), "", 10); err != nil || len(empty) != 0 {
		t.Fatalf("List approvals other conversation = %d, %v", len(empty), err)
	}
	if _, err := testDB.Approvals().List(ctx, "not-a-uuid", "", 10); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("List approvals invalid conversation = %v, want ErrInvalid", err)
	}

	decided, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{ID: approval.ID, Status: store.ApprovalApproved, Note: "ok", DecidedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decided.Status != store.ApprovalApproved || decided.DecidedAt == nil {
		t.Fatalf("decided = %+v", decided)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{ID: approval.ID, Status: store.ApprovalDenied, DecidedAt: time.Now()}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Decide = %v, want ErrConflict", err)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{ID: uuid.NewString(), Status: store.ApprovalApproved, DecidedAt: time.Now()}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing Decide = %v, want ErrNotFound", err)
	}

	expired, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		ConversationID: &conv.ID, ToolCallID: &calls[2].ID,
		Command: "ls", RiskLevel: "safe", Reason: "r",
		ExpiresAt: requested.Add(-time.Minute), CreatedAt: requested.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create expiring approval: %v", err)
	}
	n, err := testDB.Approvals().Expire(ctx, requested)
	if err != nil || n != 1 {
		t.Fatalf("Expire = %d, %v", n, err)
	}
	expired, err = testDB.Approvals().Get(ctx, expired.ID)
	if err != nil || expired.Status != store.ApprovalExpired {
		t.Fatalf("expired = %+v, %v", expired, err)
	}
}

func TestAuditStoreAndSettings(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	prev := strings.Repeat("0", 64)
	var lastHash string
	for i, action := range []string{"auth.login_succeeded", "server.create", "server.update"} {
		sum := strings.Repeat(string(rune('a'+i)), 64)
		err := testDB.AuditLogs().Append(ctx, store.CreateAuditParams{
			CreatedAt: time.Now().UTC(), ActorType: "user", ActorID: "u",
			Action: action, ResourceType: "server", ResourceID: "s",
			RequestID: "r", IP: "127.0.0.1", Detail: []byte(`{"i":` + string(rune('0'+i)) + `}`),
			PrevHash: prev, Hash: sum,
		})
		if err != nil {
			t.Fatalf("append audit: %v", err)
		}
		prev = sum
		lastHash = sum
	}
	if got, err := testDB.AuditLogs().LastHash(ctx); err != nil || got != lastHash {
		t.Fatalf("LastHash = %q, %v", got, err)
	}
	items, hasMore, err := testDB.AuditLogs().List(ctx, store.AuditFilter{Limit: 2})
	if err != nil || !hasMore || len(items) != 2 {
		t.Fatalf("audit list = %d, %v, %v", len(items), hasMore, err)
	}
	if items[0].ID <= items[1].ID {
		t.Fatal("audit list not descending")
	}
	filtered, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{Action: "server.create"})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("filtered list = %d, %v", len(filtered), err)
	}

	// 会话/服务器过滤必须走 uuid 比较（而非 ::text），并拒绝非法 UUID。
	convID := uuid.NewString()
	srvID := uuid.NewString()
	if err := testDB.AuditLogs().Append(ctx, store.CreateAuditParams{
		CreatedAt: time.Now().UTC(), ActorType: "system", Action: "chat.started",
		ResourceType: "conversation", ResourceID: convID,
		ConversationID: &convID, ServerID: &srvID,
		Detail: []byte(`{}`), PrevHash: lastHash, Hash: strings.Repeat("z", 64),
	}); err != nil {
		t.Fatalf("append filtered audit: %v", err)
	}
	byConv, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{ConversationID: convID})
	if err != nil || len(byConv) != 1 || byConv[0].ConversationID == nil || *byConv[0].ConversationID != convID {
		t.Fatalf("audit conversation filter = %+v, %v", byConv, err)
	}
	byServer, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{ServerID: srvID})
	if err != nil || len(byServer) != 1 {
		t.Fatalf("audit server filter = %d, %v", len(byServer), err)
	}
	if _, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{ConversationID: "not-a-uuid"}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("audit invalid conversation id = %v, want ErrInvalid", err)
	}
	if _, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{Limit: 201}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("audit limit 201 = %v", err)
	}

	if _, err := testDB.Settings().Get(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get missing setting = %v", err)
	}
	if _, err := testDB.Settings().Set(ctx, "bootstrap.admin_created_at", []byte(`"2026-10-23T12:00:00Z"`), time.Now()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := testDB.Settings().Get(ctx, "bootstrap.admin_created_at")
	if err != nil || !strings.Contains(string(got.Value), "2026") {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if all, err := testDB.Settings().List(ctx); err != nil || len(all) != 1 {
		t.Fatalf("List settings = %d, %v", len(all), err)
	}
}

func TestWithTx(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	err := testDB.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.Users().Create(ctx, store.CreateUserParams{Username: "admin", PasswordHash: "x"})
		return err
	})
	if err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	if n, _ := testDB.Users().Count(ctx); n != 1 {
		t.Fatalf("committed count = %d", n)
	}

	boom := errors.New("boom")
	err = testDB.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.Servers().Create(ctx, store.CreateServerParams{
			Name: "rollback", Host: "h", Port: 22, Username: "u",
			Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "e"},
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rollback tx error = %v", err)
	}
	if list, _ := testDB.Servers().List(ctx); len(list) != 0 {
		t.Fatalf("rollback left %d servers", len(list))
	}

	err = testDB.WithTx(ctx, func(tx store.Store) error {
		return tx.WithTx(ctx, func(store.Store) error { return nil })
	})
	if !errors.Is(err, store.ErrTxUnsupported) {
		t.Fatalf("nested tx error = %v, want ErrTxUnsupported", err)
	}
}

// TestApprovalConsoleNullableContext 四期 C1（契约 3.4/4.9）：console 审批
// 的 conversation_id/tool_call_id 为 NULL 透明读写；chat 审批 NULL↔非 NULL
// 互不影响；UpdateContainerRuntime 写回与 network_device 防御。
func TestApprovalConsoleNullableContext(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()

	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-console", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// console 审批：上下文 NULL。
	console, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source:   "console",
		ServerID: &server.ID, ServerName: "srv-console",
		Command: "docker stop 9f2c", RiskLevel: "needs_approval",
		RuleID: "CONTAINER_CHANGE", Reason: "container state change",
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("create console approval: %v", err)
	}
	if console.ConversationID != nil || console.ToolCallID != nil {
		t.Fatalf("console approval context = %v/%v, want nil/nil", console.ConversationID, console.ToolCallID)
	}
	if console.Source != "console" {
		t.Fatalf("source = %q", console.Source)
	}
	// NULL 透明读取。
	got, err := testDB.Approvals().Get(ctx, console.ID)
	if err != nil || got.ConversationID != nil || got.ToolCallID != nil {
		t.Fatalf("get console approval: %v %+v", err, got)
	}
	// console 审批可正常 Decide（批准由 decide handler 执行，store 只管状态）。
	decided, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{ID: console.ID, Status: store.ApprovalApproved, Note: "ok", DecidedAt: time.Now()})
	if err != nil || decided.Status != store.ApprovalApproved {
		t.Fatalf("decide console approval: %v %+v", err, decided)
	}
	// 多个 NULL tool_call_id 合法（UNIQUE NULLS DISTINCT）。
	console2, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "console", ServerID: &server.ID,
		Command: "docker start 9f2c", RiskLevel: "needs_approval",
		RuleID: "CONTAINER_CHANGE", Reason: "r", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("second console approval with NULL tool_call_id: %v", err)
	}
	_ = console2
	// 按 conversation 过滤不含 console 行。
	if items, err := testDB.Approvals().List(ctx, "00000000-0000-0000-0000-000000000000", "", 50); err != nil {
		t.Fatal(err)
	} else {
		for _, ap := range items {
			if ap.Source == "console" {
				t.Fatalf("console approval leaked into conversation filter")
			}
		}
	}

	// chat 审批（既有语义回归）：上下文非 NULL。
	conv, err := testDB.Conversations().Create(ctx, store.CreateConversationParams{Title: "c"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := uuid.NewString()
	tc, err := testDB.ToolCalls().Create(ctx, store.CreateToolCallParams{
		ConversationID: conv.ID, TurnID: turnID, ProviderCallID: "pc-1",
		Name: "ssh_exec", Arguments: []byte(`{}`), RiskLevel: "needs_approval",
		Status: store.ToolStarted, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		ConversationID: &conv.ID, ToolCallID: &tc.ID, ServerID: &server.ID,
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		Reason: "r", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if chat.ConversationID == nil || *chat.ConversationID != conv.ID || chat.ToolCallID == nil {
		t.Fatalf("chat approval context = %+v", chat)
	}
	if chat.Source != "chat" {
		t.Fatalf("chat source = %q（缺省 chat）", chat.Source)
	}

	// UpdateContainerRuntime：docker 写回 + 读回；network_device 只允许 ''。
	updated, err := testDB.Servers().UpdateContainerRuntime(ctx, server.ID, store.ContainerRuntimeDocker, time.Now())
	if err != nil || updated.ContainerRuntime != store.ContainerRuntimeDocker {
		t.Fatalf("update container runtime: %v %+v", err, updated.ContainerRuntime)
	}
	listed, err := testDB.Servers().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range listed {
		if s.ID == server.ID && s.ContainerRuntime != store.ContainerRuntimeDocker {
			t.Fatalf("container_runtime not persisted: %q", s.ContainerRuntime)
		}
	}
	nd, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "sw-c", DeviceKind: store.DeviceKindNetworkDevice, Host: "h", Port: 22,
		Username: "u", NetdevBrand: store.NetdevBrandCisco,
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if nd.ContainerRuntime != store.ContainerRuntimeNone {
		t.Fatalf("network_device runtime = %q, want ''", nd.ContainerRuntime)
	}
	if _, err := testDB.Servers().UpdateContainerRuntime(ctx, nd.ID, store.ContainerRuntimeDocker, time.Now()); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("network_device non-empty runtime = %v, want ErrInvalid", err)
	}
}
