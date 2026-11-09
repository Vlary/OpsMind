package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type userStore struct{ q querier }

const userColumns = `id::text, username, password_hash, created_at, updated_at`

func scanUser(row pgx.Row) (store.User, error) {
	var u store.User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	u.CreatedAt = utc(u.CreatedAt)
	u.UpdatedAt = utc(u.UpdatedAt)
	return u, mapError(err)
}

func (s userStore) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, mapError(err)
}

func (s userStore) Create(ctx context.Context, p store.CreateUserParams) (store.User, error) {
	return scanUser(s.q.QueryRow(ctx, `
		INSERT INTO users (username, password_hash)
		VALUES ($1, $2)
		RETURNING `+userColumns, p.Username, p.PasswordHash))
}

func (s userStore) ByUsername(ctx context.Context, username string) (store.User, error) {
	return scanUser(s.q.QueryRow(ctx, `
		SELECT `+userColumns+` FROM users WHERE username = $1`, username))
}

func (s userStore) ByID(ctx context.Context, id string) (store.User, error) {
	return scanUser(s.q.QueryRow(ctx, `
		SELECT `+userColumns+` FROM users WHERE id = $1::uuid`, id))
}

// UsernamesByIDs 批量按 id 查用户名；非法 uuid 的 id 忽略，查不到的 id 不在结果中。
func (s userStore) UsernamesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.q.Query(ctx, `
		SELECT id::text, username FROM users WHERE id = ANY($1::uuid[])`, valid)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make(map[string]string, len(valid))
	for rows.Next() {
		var id, username string
		if err := rows.Scan(&id, &username); err != nil {
			return nil, mapError(err)
		}
		out[id] = username
	}
	return out, mapError(rows.Err())
}

func (s userStore) UpdatePassword(ctx context.Context, id string, p store.UpdatePasswordParams) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1::uuid`,
		id, p.PasswordHash, utc(p.UpdatedAt))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
