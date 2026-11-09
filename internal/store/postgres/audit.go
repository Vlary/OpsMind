package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type auditStore struct{ q querier }

const auditColumns = `id, created_at, actor_type, actor_id, action, resource_type,
	resource_id, request_id, coalesce(conversation_id::text, ''),
	coalesce(server_id::text, ''), ip, detail, prev_hash, hash`

func scanAuditLog(row pgx.Row) (store.AuditLog, error) {
	var a store.AuditLog
	var conversationID, serverID string
	var detail []byte
	err := row.Scan(&a.ID, &a.CreatedAt, &a.ActorType, &a.ActorID, &a.Action,
		&a.ResourceType, &a.ResourceID, &a.RequestID, &conversationID,
		&serverID, &a.IP, &detail, &a.PrevHash, &a.Hash)
	if err != nil {
		return store.AuditLog{}, mapError(err)
	}
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	a.Detail = append([]byte(nil), detail...)
	a.ConversationID = nullableString(conversationID)
	a.ServerID = nullableString(serverID)
	a.CreatedAt = utc(a.CreatedAt)
	return a, nil
}

func (s auditStore) Append(ctx context.Context, p store.CreateAuditParams) error {
	detail := p.Detail
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	_, err := s.q.Exec(ctx, `
		INSERT INTO audit_logs (created_at, actor_type, actor_id, action,
			resource_type, resource_id, request_id, conversation_id, server_id,
			ip, detail, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid, $9::uuid, $10, $11::jsonb, $12, $13)`,
		utc(p.CreatedAt), p.ActorType, p.ActorID, p.Action,
		p.ResourceType, p.ResourceID, p.RequestID,
		ptrOrNil(p.ConversationID), ptrOrNil(p.ServerID), p.IP,
		[]byte(detail), p.PrevHash, p.Hash)
	return mapError(err)
}

func (s auditStore) List(ctx context.Context, f store.AuditFilter) ([]store.AuditLog, bool, error) {
	if f.BeforeID < 0 {
		return nil, false, store.ErrInvalid
	}
	n, err := pageLimit(f.Limit)
	if err != nil {
		return nil, false, err
	}
	// 按实际过滤条件拼接 SQL，避免 `conversation_id::text = $n` 这类表达式
	// 使 (conversation_id, created_at) 等 uuid 索引失效。
	where := []string{"TRUE"}
	args := make([]any, 0, 7)
	add := func(cond string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.BeforeID > 0 {
		add("id < $%d", f.BeforeID)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.ResourceType != "" {
		add("resource_type = $%d", f.ResourceType)
	}
	if f.ResourceID != "" {
		add("resource_id = $%d", f.ResourceID)
	}
	if f.ConversationID != "" {
		add("conversation_id = $%d::uuid", f.ConversationID)
	}
	if f.ServerID != "" {
		add("server_id = $%d::uuid", f.ServerID)
	}
	// 五期 M3（audit_search 工具）：时间上下界，零值不过滤；REST /api/audit
	// 不传这两个字段，行为不变。
	if !f.FromTime.IsZero() {
		add("created_at >= $%d", f.FromTime)
	}
	if !f.ToTime.IsZero() {
		add("created_at <= $%d", f.ToTime)
	}
	args = append(args, n+1)
	query := `SELECT ` + auditColumns + `
		FROM audit_logs
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY id DESC
		LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()

	items := make([]store.AuditLog, 0, n)
	for rows.Next() {
		item, err := scanAuditLog(rows)
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
	return items, hasMore, nil
}

func (s auditStore) LastHash(ctx context.Context) (string, error) {
	var hash string
	err := s.q.QueryRow(ctx, `SELECT hash FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zeroHash, nil
		}
		return "", mapError(err)
	}
	return hash, nil
}
