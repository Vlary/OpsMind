package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type conversationStore struct{ q querier }

const conversationColumns = `id::text, title, coalesce(provider_id::text, ''), model,
	approval_mode, created_at, updated_at, last_message_at`

func scanConversation(row pgx.Row) (store.Conversation, error) {
	var c store.Conversation
	var providerID string
	var approvalMode *string
	err := row.Scan(&c.ID, &c.Title, &providerID, &c.Model,
		&approvalMode, &c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt)
	if approvalMode != nil {
		mode := store.ApprovalMode(*approvalMode)
		c.ApprovalMode = &mode
	}
	c.ProviderID = nullableString(providerID)
	c.CreatedAt = utc(c.CreatedAt)
	c.UpdatedAt = utc(c.UpdatedAt)
	c.LastMessageAt = utc(c.LastMessageAt)
	return c, mapError(err)
}

// ListPage 按 last_message_at DESC, id DESC 游标分页返回会话。
// cursor.ID 为空表示第一页；否则返回排序严格排在 (cursor.LastMessageAt,
// cursor.ID) 之后的更早会话。多查一条判定 has_more。
func (s conversationStore) ListPage(ctx context.Context, cursor store.ConversationListCursor, limit int) ([]store.Conversation, bool, error) {
	limit = fixedPageLimit(limit)
	var (
		rows pgx.Rows
		err  error
	)
	if cursor.ID == "" {
		rows, err = s.q.Query(ctx, `
			SELECT `+conversationColumns+`
			FROM conversations
			ORDER BY last_message_at DESC, id DESC
			LIMIT $1`, limit+1)
	} else {
		rows, err = s.q.Query(ctx, `
			SELECT `+conversationColumns+`
			FROM conversations
			WHERE (last_message_at, id) < ($1, $2::uuid)
			ORDER BY last_message_at DESC, id DESC
			LIMIT $3`, utc(cursor.LastMessageAt), cursor.ID, limit+1)
	}
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Conversation, 0)
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := mapError(rows.Err()); err != nil {
		return nil, false, err
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

func (s conversationStore) Get(ctx context.Context, id string) (store.Conversation, error) {
	return scanConversation(s.q.QueryRow(ctx,
		`SELECT `+conversationColumns+` FROM conversations WHERE id = $1::uuid`, id))
}

func (s conversationStore) Create(ctx context.Context, p store.CreateConversationParams) (store.Conversation, error) {
	title := p.Title
	if title == "" {
		title = "新会话"
	}
	return scanConversation(s.q.QueryRow(ctx, `
		INSERT INTO conversations (title)
		VALUES ($1)
		RETURNING `+conversationColumns, title))
}

func (s conversationStore) UpdateTitle(ctx context.Context, id, title string, at time.Time) (store.Conversation, error) {
	return scanConversation(s.q.QueryRow(ctx, `
		UPDATE conversations SET title = $2, updated_at = $3
		WHERE id = $1::uuid
		RETURNING `+conversationColumns, id, title, utc(at)))
}

func (s conversationStore) BindModel(ctx context.Context, id, providerID, model string, at time.Time) (store.Conversation, error) {
	return scanConversation(s.q.QueryRow(ctx, `
		UPDATE conversations
		SET provider_id = NULLIF($2, '')::uuid, model = $3, updated_at = $4
		WHERE id = $1::uuid
		RETURNING `+conversationColumns, id, providerID, model, utc(at)))
}

func (s conversationStore) SetApprovalMode(ctx context.Context, id string, mode *store.ApprovalMode, at time.Time) (store.Conversation, error) {
	// 六期 BE-P（契约 3.4）：mode=nil 清除覆盖（NULL=跟随全局）；非法值由
	// 列 CHECK 兜底（驱动错误经 mapError 归 ErrInvalid 语义）。
	var modeValue any
	if mode != nil {
		modeValue = string(*mode)
	}
	return scanConversation(s.q.QueryRow(ctx, `
		UPDATE conversations SET approval_mode = $2, updated_at = $3
		WHERE id = $1::uuid
		RETURNING `+conversationColumns, id, modeValue, utc(at)))
}

func (s conversationStore) Touch(ctx context.Context, id string, at time.Time) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE conversations SET last_message_at = $2, updated_at = $2
		WHERE id = $1::uuid`, id, utc(at))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s conversationStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "conversations", id)
}

// ensureConversation 用于分页查询前的存在性判断；不存在返回 store.ErrNotFound。
func ensureConversation(ctx context.Context, q querier, id string) error {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM conversations WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		return mapError(err)
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}
