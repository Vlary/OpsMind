package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

// zeroHash 是空审计链的起始 prev_hash。
const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

type settingStore struct{ q querier }

func scanSetting(row pgx.Row) (store.Setting, error) {
	var s store.Setting
	var raw []byte
	err := row.Scan(&s.Key, &raw, &s.UpdatedAt)
	if err != nil {
		return store.Setting{}, mapError(err)
	}
	s.Value = append(json.RawMessage(nil), raw...)
	s.UpdatedAt = utc(s.UpdatedAt)
	return s, nil
}

func (s settingStore) Get(ctx context.Context, key string) (store.Setting, error) {
	return scanSetting(s.q.QueryRow(ctx,
		`SELECT key, value, updated_at FROM settings WHERE key = $1`, key))
}

func (s settingStore) Set(ctx context.Context, key string, value json.RawMessage, at time.Time) (store.Setting, error) {
	if len(value) == 0 {
		value = json.RawMessage("{}")
	}
	return scanSetting(s.q.QueryRow(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES ($1, $2::jsonb, $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
		RETURNING key, value, updated_at`, key, []byte(value), utc(at)))
}

func (s settingStore) List(ctx context.Context) ([]store.Setting, error) {
	rows, err := s.q.Query(ctx, `SELECT key, value, updated_at FROM settings ORDER BY key ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Setting, 0)
	for rows.Next() {
		item, err := scanSetting(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}
