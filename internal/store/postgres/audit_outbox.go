package postgres

import (
	"context"
	"time"

	"aiops/internal/store"
)

// AuditOutboxStore 实现（契约 12.3 第 19 条）：业务事务内 INSERT outbox，
// 独立 worker 在同一事务内取链锁、读链尾、追加 audit_logs 并标记 processed。
//
// 该能力挂在 *Store 上（而非 auditStore）：WithTx 返回的 *Store 同样具备，
// 业务 handler 可直接在事务内调用。

// EnqueueAuditOutbox 在调用方事务内写入一条待追加审计载荷。
func (s *Store) EnqueueAuditOutbox(ctx context.Context, payload []byte, at time.Time) error {
	if len(payload) == 0 {
		return store.ErrInvalid
	}
	_, err := s.q.Exec(ctx, `
		INSERT INTO audit_outbox (payload, created_at) VALUES ($1::jsonb, $2)`,
		payload, utc(at))
	return mapError(err)
}

// PendingAuditOutbox 按 id ASC 取未处理行；limit 由调用方钳制（防 OOM）。
func (s *Store) PendingAuditOutbox(ctx context.Context, limit int) ([]store.AuditOutboxEntry, error) {
	if limit <= 0 {
		return nil, store.ErrInvalid
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.q.Query(ctx, `
		SELECT id, payload, created_at, attempts
		FROM audit_outbox
		WHERE processed_at IS NULL
		ORDER BY id ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.AuditOutboxEntry, 0, limit)
	for rows.Next() {
		var entry store.AuditOutboxEntry
		var payload []byte
		if err := rows.Scan(&entry.ID, &payload, &entry.CreatedAt, &entry.Attempts); err != nil {
			return nil, mapError(err)
		}
		entry.Payload = append([]byte(nil), payload...)
		entry.CreatedAt = utc(entry.CreatedAt)
		items = append(items, entry)
	}
	return items, mapError(rows.Err())
}

// ProcessAuditOutbox 在 worker 事务内标记已追加的行（attempts+1）。
func (s *Store) ProcessAuditOutbox(ctx context.Context, ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	// 单次标记上限 500（与 Pending 的上限一致，硬编码防无界参数）。
	if len(ids) > 500 {
		ids = ids[:500]
	}
	_, err := s.q.Exec(ctx, `
		UPDATE audit_outbox
		SET processed_at = $2, attempts = attempts + 1
		WHERE processed_at IS NULL AND id = ANY($1::bigint[])`, ids, utc(at))
	return mapError(err)
}

// LockAuditChain 串行化哈希链追加（同一事务内生效）。
func (s *Store) LockAuditChain(ctx context.Context) error {
	_, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('audit.chain'))`)
	return mapError(err)
}
