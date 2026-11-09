package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type sessionStore struct{ q querier }

const sessionColumns = `id::text, user_id::text, token_hash, expires_at, last_seen_at, ip, user_agent, created_at`

func scanSession(row pgx.Row) (store.Session, error) {
	var s store.Session
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.ExpiresAt, &s.LastSeenAt, &s.IP, &s.UserAgent, &s.CreatedAt)
	s.ExpiresAt = utc(s.ExpiresAt)
	s.LastSeenAt = utc(s.LastSeenAt)
	s.CreatedAt = utc(s.CreatedAt)
	return s, mapError(err)
}

func (s sessionStore) Create(ctx context.Context, p store.CreateSessionParams) (store.Session, error) {
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return scanSession(s.q.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, expires_at, last_seen_at, ip, user_agent, created_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
		RETURNING `+sessionColumns,
		p.UserID, p.TokenHash, utc(p.ExpiresAt), utc(createdAt), p.IP, p.UserAgent, utc(createdAt)))
}

func (s sessionStore) ByTokenHash(ctx context.Context, tokenHash []byte) (store.Session, error) {
	return scanSession(s.q.QueryRow(ctx, `
		SELECT `+sessionColumns+` FROM sessions WHERE token_hash = $1`, tokenHash))
}

func (s sessionStore) Touch(ctx context.Context, id string, at time.Time) error {
	tag, err := s.q.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1::uuid`, id, utc(at))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s sessionStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "sessions", id)
}

func (s sessionStore) DeleteByUser(ctx context.Context, userID string) error {
	_, err := s.q.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1::uuid`, userID)
	return mapError(err)
}

func (s sessionStore) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, utc(before))
	return tag.RowsAffected(), mapError(err)
}
