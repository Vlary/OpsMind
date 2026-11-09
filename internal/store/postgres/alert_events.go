// alert_events.go 实现五期 M1 的 AlertEventStore（契约 3.4/4.20）。
// Create 冲突（部分唯一索引 alert_events_open_uniq 命中）映射 ErrConflict，
// 由评估器吞掉；Resolve/SetNotifyStatus 均为条件更新。
package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aiops/internal/store"
)

type alertEventStore struct{ q querier }

const alertEventColumns = `e.id, e.rule_id, r.name, e.server_id, coalesce(s.name, ''),
	r.metric, e.status, e.severity, e.value, e.message, e.fired_at, e.resolved_at,
	e.last_notified_at, e.notify_status, e.created_at, e.updated_at`

const alertEventFrom = `
	FROM alert_events e
	JOIN alert_rules r ON r.id = e.rule_id
	LEFT JOIN servers s ON s.id = e.server_id`

func scanAlertEvent(row interface{ Scan(dest ...any) error }) (store.AlertEvent, error) {
	var e store.AlertEvent
	err := row.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.ServerID, &e.ServerName,
		&e.Metric, &e.Status, &e.Severity, &e.Value, &e.Message, &e.FiredAt,
		&e.ResolvedAt, &e.LastNotifiedAt, &e.NotifyStatus, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return store.AlertEvent{}, mapError(err)
	}
	e.FiredAt = utc(e.FiredAt)
	e.CreatedAt = utc(e.CreatedAt)
	e.UpdatedAt = utc(e.UpdatedAt)
	return e, nil
}

// Create 插入 firing 事件；severity/metric 取规则快照（契约 4.20）。
// INSERT ... RETURNING 无表别名：先取裸列，再由入参规则补 RuleName/Metric
// （与 JOIN 读取路径同形）。
func (s alertEventStore) Create(ctx context.Context, rule store.AlertRule, serverID string, value float64, message string, firedAt time.Time) (store.AlertEvent, error) {
	var e store.AlertEvent
	err := s.q.QueryRow(ctx, `
		INSERT INTO alert_events (rule_id, server_id, status, severity, value,
			message, fired_at, notify_status)
		VALUES ($1::uuid, $2::uuid, 'firing', $3, $4, $5, $6, 'none')
		RETURNING id, rule_id, server_id, status, severity, value, message,
			fired_at, resolved_at, last_notified_at, notify_status, created_at, updated_at`,
		rule.ID, serverID, string(rule.Severity), value, message, utc(firedAt)).
		Scan(&e.ID, &e.RuleID, &e.ServerID, &e.Status, &e.Severity, &e.Value, &e.Message,
			&e.FiredAt, &e.ResolvedAt, &e.LastNotifiedAt, &e.NotifyStatus, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return store.AlertEvent{}, mapError(err)
	}
	e.RuleName = rule.Name
	e.Metric = rule.Metric
	e.FiredAt = utc(e.FiredAt)
	e.CreatedAt = utc(e.CreatedAt)
	e.UpdatedAt = utc(e.UpdatedAt)
	return e, nil
}

// List 按 fired_at DESC, id DESC 分页（BeforeID 不含；limit 1..200）。
func (s alertEventStore) List(ctx context.Context, f store.AlertEventFilter) ([]store.AlertEvent, bool, error) {
	n, err := pageLimit(f.Limit)
	if err != nil {
		return nil, false, err
	}
	where := []string{"TRUE"}
	args := make([]any, 0, 5)
	add := func(cond string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add("e.status = $%d", string(f.Status))
	}
	if f.Severity != "" {
		add("e.severity = $%d", string(f.Severity))
	}
	if f.ServerID != "" {
		add("e.server_id = $%d::uuid", f.ServerID)
	}
	if f.BeforeID > 0 {
		add("(e.fired_at, e.id) < (SELECT fired_at, id FROM alert_events WHERE id = $%d)", f.BeforeID)
	}
	args = append(args, n+1)
	query := `SELECT ` + alertEventColumns + alertEventFrom + `
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY e.fired_at DESC, e.id DESC
		LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()
	items := make([]store.AlertEvent, 0, n)
	for rows.Next() {
		item, err := scanAlertEvent(rows)
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

func (s alertEventStore) Get(ctx context.Context, id int64) (store.AlertEvent, error) {
	return scanAlertEvent(s.q.QueryRow(ctx, `SELECT `+alertEventColumns+alertEventFrom+` WHERE e.id = $1`, id))
}

// Resolve 置 resolved；仅 firing 行可更新，0 行返回 ErrConflict（并发/
// 重复恢复），行不存在返回 ErrNotFound。成功后经 Get 返回 JOIN 装配行。
func (s alertEventStore) Resolve(ctx context.Context, id int64, at time.Time) (store.AlertEvent, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE alert_events SET status = 'resolved', resolved_at = $2, updated_at = now()
		WHERE id = $1 AND status = 'firing'`, id, utc(at))
	if err != nil {
		return store.AlertEvent{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return store.AlertEvent{}, gerr // 不存在
		}
		return store.AlertEvent{}, store.ErrConflict // 已 resolved
	}
	return s.Get(ctx, id)
}

// SetNotifyStatus 只更新 notify_status 与 last_notified_at。
func (s alertEventStore) SetNotifyStatus(ctx context.Context, id int64, status store.NotifyStatus, at time.Time) error {
	_, err := s.q.Exec(ctx, `
		UPDATE alert_events SET notify_status = $2, last_notified_at = $3, updated_at = now()
		WHERE id = $1`, id, string(status), utc(at))
	return mapError(err)
}

func (s alertEventStore) CountFiring(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM alert_events WHERE status = 'firing'`).Scan(&n)
	return n, mapError(err)
}

func (s alertEventStore) CountFiringByServer(ctx context.Context, serverID string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM alert_events WHERE status = 'firing' AND server_id = $1::uuid`, serverID).Scan(&n)
	return n, mapError(err)
}
