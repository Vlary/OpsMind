package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type messageStore struct{ q querier }

const messageColumns = `id::text, seq, conversation_id::text, turn_id::text, role,
	content, reasoning, reasoning_duration_ms, status, error_code, model,
	prompt_tokens, completion_tokens, created_at`

func scanMessage(row pgx.Row) (store.Message, error) {
	var m store.Message
	err := row.Scan(&m.ID, &m.Seq, &m.ConversationID, &m.TurnID, &m.Role,
		&m.Content, &m.Reasoning, &m.ReasoningDurationMS, &m.Status, &m.ErrorCode, &m.Model,
		&m.PromptTokens, &m.CompletionTokens, &m.CreatedAt)
	m.CreatedAt = utc(m.CreatedAt)
	return m, mapError(err)
}

func (s messageStore) Create(ctx context.Context, p store.CreateMessageParams) (store.Message, error) {
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return scanMessage(s.q.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, turn_id, role, content, reasoning,
			reasoning_duration_ms, status, error_code, model, prompt_tokens,
			completion_tokens, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+messageColumns,
		p.ConversationID, p.TurnID, p.Role, p.Content, p.Reasoning,
		p.ReasoningDurationMS, p.Status, p.ErrorCode, p.Model, p.PromptTokens,
		p.CompletionTokens, utc(createdAt)))
}

func (s messageStore) ListPage(ctx context.Context, conversationID string, beforeSeq int64, limit int) ([]store.Message, bool, error) {
	if conversationID == "" || beforeSeq < 0 {
		return nil, false, store.ErrInvalid
	}
	n, err := pageLimit(limit)
	if err != nil {
		return nil, false, err
	}
	if err := ensureConversation(ctx, s.q, conversationID); err != nil {
		return nil, false, err
	}

	rows, err := s.q.Query(ctx, `
		SELECT `+messageColumns+`
		FROM messages
		WHERE conversation_id = $1::uuid
		  AND ($2::bigint = 0 OR seq < $2)
		ORDER BY seq DESC
		LIMIT $3`, conversationID, beforeSeq, n+1)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Message, 0, n)
	for rows.Next() {
		item, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapError(err)
	}

	hasMore := len(items) > n
	if hasMore {
		items = items[:n]
	}
	// 查询是 seq DESC；契约要求返回升序。
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items, hasMore, nil
}
