package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type toolCallStore struct{ q querier }

const toolCallColumns = `id::text, seq, conversation_id::text, turn_id::text,
	provider_call_id, name, arguments, coalesce(result, 'null'::jsonb),
	risk_level, status, error_code, error_message, coalesce(approval_mode, ''),
	started_at, finished_at`

func scanToolCall(row pgx.Row) (store.ToolCall, error) {
	var t store.ToolCall
	var arguments, result []byte
	err := row.Scan(&t.ID, &t.Seq, &t.ConversationID, &t.TurnID,
		&t.ProviderCallID, &t.Name, &arguments, &result,
		&t.RiskLevel, &t.Status, &t.ErrorCode, &t.ErrorMessage, &t.ApprovalMode,
		&t.StartedAt, &t.FinishedAt)
	if err != nil {
		return store.ToolCall{}, mapError(err)
	}
	if len(arguments) == 0 {
		arguments = []byte("{}")
	}
	t.Arguments = append([]byte(nil), arguments...)
	if string(result) != "null" && len(result) > 0 {
		t.Result = append([]byte(nil), result...)
	}
	t.StartedAt = utc(t.StartedAt)
	if t.FinishedAt != nil {
		finished := utc(*t.FinishedAt)
		t.FinishedAt = &finished
	}
	return t, nil
}

func jsonOrEmpty(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

func (s toolCallStore) Create(ctx context.Context, p store.CreateToolCallParams) (store.ToolCall, error) {
	return scanToolCall(s.q.QueryRow(ctx, `
		INSERT INTO tool_calls (conversation_id, turn_id, provider_call_id, name,
			arguments, risk_level, status, started_at, approval_mode)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5::jsonb, $6, $7, $8, $9)
		RETURNING `+toolCallColumns,
		p.ConversationID, p.TurnID, p.ProviderCallID, p.Name,
		jsonOrEmpty(p.Arguments), p.RiskLevel, p.Status, utc(p.StartedAt),
		nullApprovalMode(p.ApprovalMode)))
}

// nullApprovalMode 把空串映射为 SQL NULL（迁移 00010：非命令执行类工具与
// 历史行都是 NULL，CHECK 只约束非 NULL 值）。
func nullApprovalMode(m store.ApprovalMode) any {
	if m == "" {
		return nil
	}
	return string(m)
}

func (s toolCallStore) SetStatus(ctx context.Context, id string, status store.ToolCallStatus, errorCode, errorMessage string) (store.ToolCall, error) {
	return scanToolCall(s.q.QueryRow(ctx, `
		UPDATE tool_calls SET status = $2, error_code = $3, error_message = $4
		WHERE id = $1::uuid
		RETURNING `+toolCallColumns, id, status, errorCode, errorMessage))
}

func (s toolCallStore) Finish(ctx context.Context, p store.FinishToolCallParams) (store.ToolCall, error) {
	var result any
	if len(p.Result) > 0 {
		result = []byte(p.Result)
	}
	return scanToolCall(s.q.QueryRow(ctx, `
		UPDATE tool_calls
		SET status = $2, result = $3::jsonb, error_code = $4, error_message = $5, finished_at = $6
		WHERE id = $1::uuid
		RETURNING `+toolCallColumns,
		p.ID, p.Status, result, p.ErrorCode, p.ErrorMessage, utc(p.FinishedAt)))
}

func (s toolCallStore) ListPage(ctx context.Context, conversationID string, beforeSeq int64, limit int) ([]store.ToolCall, bool, error) {
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
		SELECT `+toolCallColumns+`
		FROM tool_calls
		WHERE conversation_id = $1::uuid
		  AND ($2::bigint = 0 OR seq < $2)
		ORDER BY seq DESC
		LIMIT $3`, conversationID, beforeSeq, n+1)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()

	items := make([]store.ToolCall, 0, n)
	for rows.Next() {
		item, err := scanToolCall(rows)
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
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items, hasMore, nil
}

func (s toolCallStore) ByID(ctx context.Context, id string) (store.ToolCall, error) {
	return scanToolCall(s.q.QueryRow(ctx,
		`SELECT `+toolCallColumns+` FROM tool_calls WHERE id = $1::uuid`, id))
}
