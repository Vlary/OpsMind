// metrics.go 实现五期 M1 的 MetricStore：server_metrics 批量插入 +
// server_metrics_latest UPSERT（同一事务）、窗口查询（单机取窗口内最新
// limit 个点后升序返回——评估器回退路径与时间序列 API 共用；LIMIT 硬
// 上限 1440）、latest 读取与保留清理（契约 4.16/4.17/11 节）。
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

// metricBatchMax 是单批写库行数上限（契约 11 节 #3：固定 500，防 OOM）。
const metricBatchMax = 500

// metricWindowLimitMax 是窗口查询点数硬上限（契约 11 节 #14：60s 间隔 ×
// 1440 点 = 24h 即契约窗口上限）。
const metricWindowLimitMax = 1440

type metricStore struct{ q querier }

const metricValueColumns = `cpu_pct, mem_pct, disk_pct, load_1, net_in_bps, net_out_bps`

func scanMetric(row pgx.Row) (store.ServerMetric, error) {
	var m store.ServerMetric
	var serverID string
	err := row.Scan(&serverID, &m.CreatedAt, &m.CPU, &m.Mem, &m.Disk, &m.Load1, &m.NetIn, &m.NetOut)
	if err != nil {
		return store.ServerMetric{}, mapError(err)
	}
	m.ServerID = serverID
	m.CreatedAt = utc(m.CreatedAt)
	return m, nil
}

// BatchInsert 批量写入指标点并 UPSERT 每台最新值（同一事务；单批 ≤500，
// 超限 ErrInvalid——契约 11 节 #3）。
func (s metricStore) BatchInsert(ctx context.Context, items []store.InsertMetricParams) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) > metricBatchMax {
		return store.ErrInvalid
	}

	// 已在事务内（WithTx 子 store）：直接复用同一事务，不嵌套。
	if _, ok := s.q.(pgx.Tx); ok {
		return insertMetricBatch(ctx, s.q, items)
	}
	// 池句柄：自开单层事务，server_metrics 与 latest 同事务落库。
	pool, ok := s.q.(interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("postgres: metric batch insert requires pool or tx querier")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := insertMetricBatch(ctx, tx, items); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func insertMetricBatch(ctx context.Context, q querier, items []store.InsertMetricParams) error {
	for _, it := range items {
		_, err := q.Exec(ctx, `
			INSERT INTO server_metrics (server_id, created_at, `+metricValueColumns+`)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
			it.ServerID, utc(it.CreatedAt), it.CPU, it.Mem, it.Disk, it.Load1, it.NetIn, it.NetOut)
		if err != nil {
			return mapError(err)
		}
		_, err = q.Exec(ctx, `
			INSERT INTO server_metrics_latest (server_id, created_at, `+metricValueColumns+`, updated_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (server_id) DO UPDATE SET
				created_at = EXCLUDED.created_at,
				cpu_pct = EXCLUDED.cpu_pct,
				mem_pct = EXCLUDED.mem_pct,
				disk_pct = EXCLUDED.disk_pct,
				load_1 = EXCLUDED.load_1,
				net_in_bps = EXCLUDED.net_in_bps,
				net_out_bps = EXCLUDED.net_out_bps,
				updated_at = EXCLUDED.updated_at`,
			it.ServerID, utc(it.CreatedAt), it.CPU, it.Mem, it.Disk, it.Load1, it.NetIn, it.NetOut, utc(it.CreatedAt))
		if err != nil {
			return mapError(err)
		}
	}
	return nil
}

const latestColumns = `l.server_id, coalesce(s.name, ''), l.created_at, `

func scanLatest(row pgx.Row) (store.ServerMetricLatest, error) {
	var m store.ServerMetricLatest
	err := row.Scan(&m.ServerID, &m.ServerName, &m.CreatedAt, &m.CPU, &m.Mem, &m.Disk,
		&m.Load1, &m.NetIn, &m.NetOut, &m.UpdatedAt)
	if err != nil {
		return store.ServerMetricLatest{}, mapError(err)
	}
	m.CreatedAt = utc(m.CreatedAt)
	m.UpdatedAt = utc(m.UpdatedAt)
	return m, nil
}

// Latest 返回每台最新值（JOIN servers.name；删除后空串）。
func (s metricStore) Latest(ctx context.Context) ([]store.ServerMetricLatest, error) {
	rows, err := s.q.Query(ctx, `
		SELECT `+latestColumns+metricValueColumns+`, l.updated_at
		FROM server_metrics_latest l
		LEFT JOIN servers s ON s.id = l.server_id
		ORDER BY l.server_id ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.ServerMetricLatest, 0, 16)
	for rows.Next() {
		m, err := scanLatest(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, mapError(rows.Err())
}

func (s metricStore) LatestByServer(ctx context.Context, serverID string) (store.ServerMetricLatest, error) {
	return scanLatest(s.q.QueryRow(ctx, `
		SELECT `+latestColumns+metricValueColumns+`, l.updated_at
		FROM server_metrics_latest l
		LEFT JOIN servers s ON s.id = l.server_id
		WHERE l.server_id = $1::uuid`, serverID))
}

// Window 返回窗口内的点；单机路径取最新的 limit 个点后反转为升序
// （LIMIT 与 DESC 组合——ASC+LIMIT 会取到窗口内最早的点，评估器回退
// 查库时会漏报最新越限/误用陈旧数据），全量路径按 ASC 取前 limit 个。
// Limit 硬上限 1440（超出 ErrInvalid），契约五期行为约定：不做降采样。
func (s metricStore) Window(ctx context.Context, f store.MetricWindow) ([]store.ServerMetric, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = metricWindowLimitMax
	}
	if limit > metricWindowLimitMax {
		return nil, store.ErrInvalid
	}
	if f.ServerID != "" {
		// 单机窗口：取窗口内最新的 limit 个点后升序返回。
		rows, err := s.q.Query(ctx, `
			SELECT server_id, created_at, `+metricValueColumns+`
			FROM server_metrics
			WHERE server_id = $1::uuid AND created_at >= $2 AND created_at <= $3
			ORDER BY created_at DESC
			LIMIT $4`, f.ServerID, utc(f.From), utc(f.To), limit)
		if err != nil {
			return nil, mapError(err)
		}
		defer rows.Close()
		desc := make([]store.ServerMetric, 0, 64)
		for rows.Next() {
			m, err := scanMetric(rows)
			if err != nil {
				return nil, err
			}
			desc = append(desc, m)
		}
		if err := rows.Err(); err != nil {
			return nil, mapError(err)
		}
		// 反转为 ASC。
		for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
			desc[i], desc[j] = desc[j], desc[i]
		}
		return desc, nil
	}
	rows, err := s.q.Query(ctx, `
		SELECT server_id, created_at, `+metricValueColumns+`
		FROM server_metrics
		WHERE created_at >= $1 AND created_at <= $2
		ORDER BY created_at ASC
		LIMIT $3`, utc(f.From), utc(f.To), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := make([]store.ServerMetric, 0, 64)
	for rows.Next() {
		m, err := scanMetric(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, mapError(rows.Err())
}

// DeleteBefore 删除保留窗口之前的指标点（30 天保留策略，每日一次；返回
// 删除行数供日志记录）。
func (s metricStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM server_metrics WHERE created_at < $1`, utc(cutoff))
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}
