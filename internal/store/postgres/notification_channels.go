// notification_channels.go 实现五期 M1 的 NotificationChannelStore
// （契约 3.4/4.18）。加解密不在 store：config_enc 只存取密文；响应模型
// ConfigEncrypted 带 json:"-"，密文永不回 API。排序 created_at ASC, id ASC。
package postgres

import (
	"context"

	"aiops/internal/store"
)

type notificationChannelStore struct{ q querier }

const channelColumns = `id, name, kind, config_enc, config_hint, created_at, updated_at`

func scanChannel(row interface{ Scan(dest ...any) error }) (store.NotificationChannel, error) {
	var ch store.NotificationChannel
	err := row.Scan(&ch.ID, &ch.Name, &ch.Kind, &ch.ConfigEncrypted, &ch.ConfigHint,
		&ch.CreatedAt, &ch.UpdatedAt)
	if err != nil {
		return store.NotificationChannel{}, mapError(err)
	}
	ch.HasConfig = ch.ConfigEncrypted != ""
	ch.CreatedAt = utc(ch.CreatedAt)
	ch.UpdatedAt = utc(ch.UpdatedAt)
	return ch, nil
}

func (s notificationChannelStore) List(ctx context.Context) ([]store.NotificationChannel, error) {
	rows, err := s.q.Query(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := make([]store.NotificationChannel, 0, 4)
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, ch)
	}
	return items, mapError(rows.Err())
}

func (s notificationChannelStore) Get(ctx context.Context, id string) (store.NotificationChannel, error) {
	return scanChannel(s.q.QueryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1::uuid`, id))
}

func (s notificationChannelStore) Create(ctx context.Context, p store.CreateNotificationChannelParams) (store.NotificationChannel, error) {
	return scanChannel(s.q.QueryRow(ctx, `
		INSERT INTO notification_channels (name, kind, config_enc, config_hint)
		VALUES ($1, $2, $3, $4)
		RETURNING `+channelColumns, p.Name, string(p.Kind), p.ConfigJSON, p.ConfigHint))
}

func (s notificationChannelStore) Update(ctx context.Context, id string, p store.UpdateNotificationChannelParams) (store.NotificationChannel, error) {
	if p.ConfigJSON != nil && *p.ConfigJSON == "" {
		return store.NotificationChannel{}, store.ErrInvalid
	}
	cur, err := scanChannel(s.q.QueryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1::uuid`, id))
	if err != nil {
		return store.NotificationChannel{}, err
	}
	name := cur.Name
	if p.Name != nil {
		name = *p.Name
	}
	kind := cur.Kind
	if p.Kind != nil {
		kind = *p.Kind
	}
	config := cur.ConfigEncrypted
	if p.ConfigJSON != nil {
		config = *p.ConfigJSON
	}
	hint := cur.ConfigHint
	if p.ConfigHint != nil {
		hint = *p.ConfigHint
	}
	return scanChannel(s.q.QueryRow(ctx, `
		UPDATE notification_channels SET name = $2, kind = $3, config_enc = $4,
			config_hint = $5, updated_at = now()
		WHERE id = $1::uuid
		RETURNING `+channelColumns, id, name, string(kind), config, hint))
}

func (s notificationChannelStore) Delete(ctx context.Context, id string) error {
	tag, err := s.q.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1::uuid`, id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
