// metrics.go 实现五期 M1 的 MetricStore：server_metrics 批量插入 +
// server_metrics_latest UPSERT（同一事务）、窗口查询（单机取窗口内最新
// limit 个点后升序返回——评估器回退路径与时间序列 API 共用；LIMIT 硬
// 上限 2880）、latest 读取与保留清理（契约 4.16/4.17/11 节）。
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
// 超限 ErrInvalid——契约 11 节 #3）。latest 只允许被不早于当前行的样本
// 覆盖（WHERE created_at <= EXCLUDED.created_at）：时钟回拨/补采/双实例
// 写入旧时刻点时，不能让「当前值」倒退到旧样本，否则 overview、Top12 与
// 折线最后一点互相矛盾（第 10 轮 medium）。
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
				updated_at = EXCLUDED.updated_at
			WHERE server_metrics_latest.created_at <= EXCLUDED.created_at`,
			it.ServerID, utc(it.CreatedAt), it.CPU, it.Mem, it.Disk, it.Load1, it.NetIn, it.NetOut, utc(it.CreatedAt))
		if err != nil {
			return mapError(err)
		}
	}
	return nil
}

// latestColumns 里带 servers.os（走查第 15 轮 medium）：前端按平台切换
// 指标卡副文案，Windows 资产不能再被标成 /proc/stat、/proc/meminfo、
// loadavg 1 分钟。
const latestColumns = `l.server_id, coalesce(s.name, ''), s.os, l.created_at, `

func scanLatest(row pgx.Row) (store.ServerMetricLatest, error) {
	var m store.ServerMetricLatest
	err := row.Scan(&m.ServerID, &m.ServerName, &m.OS, &m.CreatedAt, &m.CPU, &m.Mem, &m.Disk,
		&m.Load1, &m.NetIn, &m.NetOut, &m.UpdatedAt)
	if err != nil {
		return store.ServerMetricLatest{}, mapError(err)
	}
	m.CreatedAt = utc(m.CreatedAt)
	m.UpdatedAt = utc(m.UpdatedAt)
	return m, nil
}

// Latest 返回每台最新值（JOIN servers.name；删除后空串）。
//
// 只返回 device_kind='server' 的资产（走查第 9 轮 medium）：网络设备不采集
// （契约 2.10 约定 1），但资产从 server 改成 network_device 时历史
// server_metrics_latest 可能残留；不过滤会把交换机算进「已采集服务器」、
// 服务器下拉与 Top 12。改类型时还会主动清理残留（见 serverStore.Update）。
func (s metricStore) Latest(ctx context.Context) ([]store.ServerMetricLatest, error) {
	rows, err := s.q.Query(ctx, `
		SELECT `+latestColumns+metricValueColumns+`, l.updated_at
		FROM server_metrics_latest l
		JOIN servers s ON s.id = l.server_id AND s.device_kind = 'server'
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

// LatestByServer 返回单台最新值；网络设备不采集（契约 2.10 约定 1），
// 即使残留 latest 行也返回 ErrNotFound，与「无指标」同口径。
func (s metricStore) LatestByServer(ctx context.Context, serverID string) (store.ServerMetricLatest, error) {
	return scanLatest(s.q.QueryRow(ctx, `
		SELECT `+latestColumns+metricValueColumns+`, l.updated_at
		FROM server_metrics_latest l
		JOIN servers s ON s.id = l.server_id AND s.device_kind = 'server'
		WHERE l.server_id = $1::uuid`, serverID))
}

// Window 返回窗口内的点；单机路径取最新的 limit 个点后反转为升序
// （LIMIT 与 DESC 组合——ASC+LIMIT 会取到窗口内最早的点，评估器回退
// 查库时会漏报最新越限/误用陈旧数据），全量路径按 ASC 取前 limit 个。
// Limit 硬上限 store.MetricWindowLimitMax（超出 ErrInvalid），契约五期行为
// 约定：不做降采样。
// 两条路径都 JOIN servers 过滤 device_kind='server'：网络设备的历史残留
// 点不得再被单机折线或评估器读到。
func (s metricStore) Window(ctx context.Context, f store.MetricWindow) ([]store.ServerMetric, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = store.MetricWindowLimitMax
	}
	if limit > store.MetricWindowLimitMax {
		return nil, store.ErrInvalid
	}
	if f.ServerID != "" {
		// 单机窗口：取窗口内最新的 limit 个点后升序返回。
		rows, err := s.q.Query(ctx, `
			SELECT m.server_id, m.created_at, `+metricValueColumns+`
			FROM server_metrics m
			JOIN servers s ON s.id = m.server_id AND s.device_kind = 'server'
			WHERE m.server_id = $1::uuid AND m.created_at >= $2 AND m.created_at <= $3
			ORDER BY m.created_at DESC
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
		SELECT m.server_id, m.created_at, `+metricValueColumns+`
		FROM server_metrics m
		JOIN servers s ON s.id = m.server_id AND s.device_kind = 'server'
		WHERE m.created_at >= $1 AND m.created_at <= $2
		ORDER BY m.created_at ASC
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

// metricTrendMaxBuckets 是图表聚合的桶数硬上限（契约防 OOM：单次响应
// 点数 ≤2000；前端固定 24/28 桶，这里只做存储层兜底）。
const metricTrendMaxBuckets = 2000

// Trend 按 date_bin 时间桶聚合 CPU/Mem 的 p95/avg/max 与覆盖台数/样本数。
// 单条 SQL、单次分组；WHERE created_at 开区间下界使规划器走
// server_metrics_created_server_idx（00007:34）的范围扫描，不新增第二索引。
// 只返回有样本的桶（无样本桶由 handler 补 null，避免前端连点）。
// JOIN servers 只聚合 device_kind='server'：网络设备遗留的历史点不得
// 进入集群趋势的台数/样本数与 P95（走查第 9 轮 medium）。
//
// 走查第 16 轮 medium：To 是当前桶结束（未来）时刻，时钟超前的机器会把
// 未来样本写进最后一个桶，P95/峰值冲到 100%；这里用 Now 把上界钳到
// 服务端当前时刻，桶结构不变（handler 仍按原窗口补 null 桶）。Now 为零
// 表示不钳制，与 Top12 的时钟超前口径保持一致。
func (s metricStore) Trend(ctx context.Context, q store.MetricTrendQuery) ([]store.MetricTrendPoint, error) {
	if q.BucketSeconds < 60 || q.BucketSeconds > 86400 || !q.To.After(q.From) {
		return nil, store.ErrInvalid
	}
	if int(q.To.Sub(q.From)/time.Duration(q.BucketSeconds)/time.Second) > metricTrendMaxBuckets {
		return nil, store.ErrInvalid
	}
	upper := q.To
	if !q.Now.IsZero() && q.Now.Before(upper) {
		upper = q.Now
	}
	rows, err := s.q.Query(ctx, `
		SELECT date_bin(make_interval(secs => $1::double precision), m.created_at,
				TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket_start,
			percentile_cont(0.95) WITHIN GROUP (ORDER BY m.cpu_pct),
			avg(m.cpu_pct),
			max(m.cpu_pct),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY m.mem_pct),
			avg(m.mem_pct),
			max(m.mem_pct),
			count(DISTINCT m.server_id),
			count(*),
			min(m.created_at),
			max(m.created_at)
		FROM server_metrics m
		JOIN servers s ON s.id = m.server_id AND s.device_kind = 'server'
		WHERE m.created_at >= $2 AND m.created_at < $3 AND m.created_at <= $4
		GROUP BY 1
		ORDER BY 1`, q.BucketSeconds, utc(q.From), utc(q.To), utc(upper))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := make([]store.MetricTrendPoint, 0, 32)
	for rows.Next() {
		var p store.MetricTrendPoint
		if err := rows.Scan(&p.BucketStart, &p.CPUP95, &p.CPUAvg, &p.CPUMax,
			&p.MemP95, &p.MemAvg, &p.MemMax, &p.Servers, &p.Samples,
			&p.FirstSample, &p.LastSample); err != nil {
			return nil, mapError(err)
		}
		p.BucketStart = utc(p.BucketStart)
		p.FirstSample = utc(p.FirstSample)
		p.LastSample = utc(p.LastSample)
		items = append(items, p)
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
