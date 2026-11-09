// alert_rules.go 实现五期 M1 的 AlertRuleStore（契约 3.4/4.19）。
// 排序固定 created_at ASC, id ASC；唯一名冲突映射 ErrConflict。
package postgres

import (
	"context"
	"time"

	"aiops/internal/store"
)

type alertRuleStore struct{ q querier }

const alertRuleColumns = `id, name, metric, comparator, threshold, duration_windows,
	cooldown_seconds, severity, silent_until, notify_channel_id, enabled, created_at, updated_at`

func scanAlertRule(row interface{ Scan(dest ...any) error }) (store.AlertRule, error) {
	var r store.AlertRule
	err := row.Scan(&r.ID, &r.Name, &r.Metric, &r.Comparator, &r.Threshold,
		&r.DurationWindows, &r.CooldownSeconds, &r.Severity, &r.SilentUntil,
		&r.NotifyChannelID, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return store.AlertRule{}, mapError(err)
	}
	r.CreatedAt = utc(r.CreatedAt)
	r.UpdatedAt = utc(r.UpdatedAt)
	return r, nil
}

func (s alertRuleStore) List(ctx context.Context) ([]store.AlertRule, error) {
	rows, err := s.q.Query(ctx, `SELECT `+alertRuleColumns+` FROM alert_rules ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := make([]store.AlertRule, 0, 8)
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	return items, mapError(rows.Err())
}

func (s alertRuleStore) Get(ctx context.Context, id string) (store.AlertRule, error) {
	return scanAlertRule(s.q.QueryRow(ctx, `SELECT `+alertRuleColumns+` FROM alert_rules WHERE id = $1::uuid`, id))
}

func (s alertRuleStore) Create(ctx context.Context, p store.CreateAlertRuleParams) (store.AlertRule, error) {
	return scanAlertRule(s.q.QueryRow(ctx, `
		INSERT INTO alert_rules (name, metric, comparator, threshold, duration_windows,
			cooldown_seconds, severity, silent_until, notify_channel_id, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid, $10)
		RETURNING `+alertRuleColumns,
		p.Name, string(p.Metric), string(p.Comparator), p.Threshold, p.DurationWindows,
		p.CooldownSeconds, string(p.Severity), timePtr(p.SilentUntil), ptrOrNil(p.NotifyChannelID), p.Enabled))
}

func (s alertRuleStore) Update(ctx context.Context, id string, p store.UpdateAlertRuleParams) (store.AlertRule, error) {
	if p.SilentUntil != nil && p.ClearSilentUntil {
		return store.AlertRule{}, store.ErrInvalid
	}
	if p.NotifyChannelID != nil && p.ClearNotifyChannel {
		return store.AlertRule{}, store.ErrInvalid
	}

	cur, err := scanAlertRule(s.q.QueryRow(ctx, `SELECT `+alertRuleColumns+` FROM alert_rules WHERE id = $1::uuid`, id))
	if err != nil {
		return store.AlertRule{}, err
	}

	name := cur.Name
	if p.Name != nil {
		name = *p.Name
	}
	metric := cur.Metric
	if p.Metric != nil {
		metric = *p.Metric
	}
	comparator := cur.Comparator
	if p.Comparator != nil {
		comparator = *p.Comparator
	}
	threshold := cur.Threshold
	if p.Threshold != nil {
		threshold = *p.Threshold
	}
	windows := cur.DurationWindows
	if p.DurationWindows != nil {
		windows = *p.DurationWindows
	}
	cooldown := cur.CooldownSeconds
	if p.CooldownSeconds != nil {
		cooldown = *p.CooldownSeconds
	}
	severity := cur.Severity
	if p.Severity != nil {
		severity = *p.Severity
	}
	silent := cur.SilentUntil
	switch {
	case p.ClearSilentUntil:
		silent = nil
	case p.SilentUntil != nil:
		silent = p.SilentUntil
	}
	channel := cur.NotifyChannelID
	switch {
	case p.ClearNotifyChannel:
		channel = nil
	case p.NotifyChannelID != nil:
		channel = p.NotifyChannelID
	}
	enabled := cur.Enabled
	if p.Enabled != nil {
		enabled = *p.Enabled
	}

	return scanAlertRule(s.q.QueryRow(ctx, `
		UPDATE alert_rules SET
			name = $2, metric = $3, comparator = $4, threshold = $5,
			duration_windows = $6, cooldown_seconds = $7, severity = $8,
			silent_until = $9, notify_channel_id = $10::uuid, enabled = $11,
			updated_at = now()
		WHERE id = $1::uuid
		RETURNING `+alertRuleColumns,
		id, name, string(metric), string(comparator), threshold, windows, cooldown,
		string(severity), timePtr(silent), ptrOrNil(channel), enabled))
}

func (s alertRuleStore) Delete(ctx context.Context, id string) error {
	tag, err := s.q.Exec(ctx, `DELETE FROM alert_rules WHERE id = $1::uuid`, id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// timePtr 把 *time.Time 转为 SQL 参数（nil ↔ NULL；统一 UTC）。
func timePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return utc(*t)
}
