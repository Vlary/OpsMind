// inspection.go 实现五期 M2 的巡检三仓储（契约 3.4/4.22-4.24）：
// InspectionJobStore（任务 CRUD + MarkRun/ListDue）、InspectionReportStore
// （报告 + 明细 + PreviousScores 趋势对比）。
package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 巡检任务
// ---------------------------------------------------------------------------

type inspectionJobStore struct{ q querier }

const inspectionJobColumns = `id, name, cron, target_tags, template, notify_enabled,
	notify_channel_id, enabled, last_run_at, next_run_at, created_at, updated_at`

func scanInspectionJob(row interface{ Scan(dest ...any) error }) (store.InspectionJob, error) {
	var j store.InspectionJob
	err := row.Scan(&j.ID, &j.Name, &j.Cron, &j.TargetTags, &j.Template,
		&j.NotifyEnabled, &j.NotifyChannelID, &j.Enabled,
		&j.LastRunAt, &j.NextRunAt, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return store.InspectionJob{}, mapError(err)
	}
	if j.TargetTags == nil {
		j.TargetTags = []string{}
	}
	j.CreatedAt = utc(j.CreatedAt)
	j.UpdatedAt = utc(j.UpdatedAt)
	return j, nil
}

func (s inspectionJobStore) List(ctx context.Context) ([]store.InspectionJob, error) {
	rows, err := s.q.Query(ctx, `SELECT `+inspectionJobColumns+
		` FROM inspection_jobs ORDER BY created_at ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := []store.InspectionJob{}
	for rows.Next() {
		item, err := scanInspectionJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return items, nil
}

func (s inspectionJobStore) Get(ctx context.Context, id string) (store.InspectionJob, error) {
	return scanInspectionJob(s.q.QueryRow(ctx,
		`SELECT `+inspectionJobColumns+` FROM inspection_jobs WHERE id = $1::uuid`, id))
}

func (s inspectionJobStore) Create(ctx context.Context, p store.CreateInspectionJobParams) (store.InspectionJob, error) {
	return scanInspectionJob(s.q.QueryRow(ctx, `
		INSERT INTO inspection_jobs (name, cron, target_tags, template,
			notify_enabled, notify_channel_id, enabled, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6::uuid, $7, $8)
		RETURNING `+inspectionJobColumns,
		p.Name, p.Cron, nonNil(p.TargetTags), string(p.Template),
		p.NotifyEnabled, ptrOrNil(p.NotifyChannelID), p.Enabled, p.NextRunAt))
}

func (s inspectionJobStore) Update(ctx context.Context, id string, p store.UpdateInspectionJobParams) (store.InspectionJob, error) {
	set := []string{"updated_at = now()"}
	args := make([]any, 0, 9)
	add := func(expr string, value any) {
		args = append(args, value)
		set = append(set, expr+" = $"+strconv.Itoa(len(args)))
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Cron != nil {
		add("cron", *p.Cron)
	}
	if p.TargetTags != nil {
		add("target_tags", nonNil(*p.TargetTags))
	}
	if p.Template != nil {
		add("template", string(*p.Template))
	}
	if p.NotifyEnabled != nil {
		add("notify_enabled", *p.NotifyEnabled)
	}
	if p.ClearNotifyChannel {
		args = append(args, nil)
		set = append(set, "notify_channel_id = $"+strconv.Itoa(len(args)))
	} else if p.NotifyChannelID != nil {
		args = append(args, *p.NotifyChannelID)
		set = append(set, "notify_channel_id = $"+strconv.Itoa(len(args))+"::uuid")
	}
	if p.Enabled != nil {
		add("enabled", *p.Enabled)
	}
	args = append(args, id)
	tag, err := s.q.Exec(ctx, `UPDATE inspection_jobs SET `+strings.Join(set, ", ")+
		` WHERE id = $`+strconv.Itoa(len(args))+`::uuid`, args...)
	if err != nil {
		return store.InspectionJob{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.InspectionJob{}, store.ErrNotFound
	}
	return s.Get(ctx, id)
}

func (s inspectionJobStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "inspection_jobs", id)
}

// MarkRun 写执行时刻与下一次调度时刻（tick 循环触发后调用）。
func (s inspectionJobStore) MarkRun(ctx context.Context, id string, lastRunAt, nextRunAt time.Time) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE inspection_jobs SET last_run_at = $2, next_run_at = $3, updated_at = now()
		WHERE id = $1::uuid`, id, utc(lastRunAt), utc(nextRunAt))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListDue 返回 enabled 且 next_run_at<=now 的任务（tick 每分钟调用；
// 结果集受任务表规模约束，任务数由页面管理，非无界增长输入）。
func (s inspectionJobStore) ListDue(ctx context.Context, now time.Time) ([]store.InspectionJob, error) {
	rows, err := s.q.Query(ctx, `SELECT `+inspectionJobColumns+
		` FROM inspection_jobs WHERE enabled AND next_run_at <= $1 ORDER BY next_run_at ASC`, utc(now))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := []store.InspectionJob{}
	for rows.Next() {
		item, err := scanInspectionJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// 巡检报告与明细
// ---------------------------------------------------------------------------

type inspectionReportStore struct{ q querier }

const inspectionReportColumns = `r.id, r.job_id, coalesce(j.name, ''), r.trigger,
	r.server_count, r.avg_score, r.anomaly_count, r.started_at, r.finished_at, r.created_at`

const inspectionReportFrom = `
	FROM inspection_reports r
	LEFT JOIN inspection_jobs j ON j.id = r.job_id`

func scanInspectionReport(row interface{ Scan(dest ...any) error }) (store.InspectionReport, error) {
	var r store.InspectionReport
	err := row.Scan(&r.ID, &r.JobID, &r.JobName, &r.Trigger, &r.ServerCount,
		&r.AvgScore, &r.AnomalyCount, &r.StartedAt, &r.FinishedAt, &r.CreatedAt)
	if err != nil {
		return store.InspectionReport{}, mapError(err)
	}
	r.StartedAt = utc(r.StartedAt)
	r.CreatedAt = utc(r.CreatedAt)
	return r, nil
}

func (s inspectionReportStore) Create(ctx context.Context, p store.CreateInspectionReportParams) (store.InspectionReport, error) {
	return scanInspectionReport(s.q.QueryRow(ctx, `
		INSERT INTO inspection_reports (job_id, trigger, server_count, avg_score,
			anomaly_count, started_at, finished_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
		RETURNING id, job_id,
			CASE WHEN $1::uuid IS NULL THEN '' ELSE
				(SELECT name FROM inspection_jobs WHERE id = $1::uuid) END,
			trigger, server_count, avg_score,
			anomaly_count, started_at, finished_at, created_at`,
		ptrOrNil(p.JobID), string(p.Trigger), p.ServerCount, p.AvgScore,
		p.AnomalyCount, utc(p.StartedAt), p.FinishedAt))
}

// Get 返回报告与全部明细（明细数受目标上限 200 约束——契约 11 节 #10）。
func (s inspectionReportStore) Get(ctx context.Context, id string) (store.InspectionReport, []store.InspectionReportItem, error) {
	report, err := scanInspectionReport(s.q.QueryRow(ctx,
		`SELECT `+inspectionReportColumns+inspectionReportFrom+` WHERE r.id = $1::uuid`, id))
	if err != nil {
		return store.InspectionReport{}, nil, err
	}
	rows, err := s.q.Query(ctx, `
		SELECT i.id, i.report_id, i.server_id, coalesce(s.name, ''), i.score,
			i.anomaly_count, i.anomalies, i.raw_output, i.command, i.truncated,
			i.duration_ms, i.error_code, i.created_at
		FROM inspection_report_items i
		LEFT JOIN servers s ON s.id = i.server_id
		WHERE i.report_id = $1::uuid
		ORDER BY i.id ASC`, id)
	if err != nil {
		return store.InspectionReport{}, nil, mapError(err)
	}
	defer rows.Close()
	items := []store.InspectionReportItem{}
	for rows.Next() {
		var it store.InspectionReportItem
		if err := rows.Scan(&it.ID, &it.ReportID, &it.ServerID, &it.ServerName,
			&it.Score, &it.AnomalyCount, &it.Anomalies, &it.RawOutput, &it.Command,
			&it.Truncated, &it.DurationMS, &it.ErrorCode, &it.CreatedAt); err != nil {
			return store.InspectionReport{}, nil, err
		}
		it.CreatedAt = utc(it.CreatedAt)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return store.InspectionReport{}, nil, mapError(err)
	}
	return report, items, nil
}

func (s inspectionReportStore) List(ctx context.Context, f store.InspectionReportFilter) ([]store.InspectionReport, error) {
	n, err := pageLimit(f.Limit)
	if err != nil {
		return nil, err
	}
	where := "TRUE"
	args := make([]any, 0, 2)
	if f.JobID != "" {
		args = append(args, f.JobID)
		where = "r.job_id = $1::uuid"
	}
	args = append(args, n)
	rows, err := s.q.Query(ctx, `SELECT `+inspectionReportColumns+inspectionReportFrom+
		` WHERE `+where+` ORDER BY r.created_at DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := make([]store.InspectionReport, 0, n)
	for rows.Next() {
		item, err := scanInspectionReport(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return items, nil
}

func (s inspectionReportStore) CreateItem(ctx context.Context, p store.CreateInspectionItemParams) (store.InspectionReportItem, error) {
	var it store.InspectionReportItem
	err := s.q.QueryRow(ctx, `
		INSERT INTO inspection_report_items (report_id, server_id, score,
			anomaly_count, anomalies, raw_output, command, truncated, duration_ms, error_code)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5::jsonb, $6, $7, $8, $9, $10)
		RETURNING id, report_id, server_id, ''::text, score, anomaly_count,
			anomalies, raw_output, command, truncated, duration_ms, error_code, created_at`,
		p.ReportID, p.ServerID, p.Score, p.AnomalyCount, []byte(p.Anomalies),
		p.RawOutput, p.Command, p.Truncated, p.DurationMS, p.ErrorCode).
		Scan(&it.ID, &it.ReportID, &it.ServerID, &it.ServerName, &it.Score,
			&it.AnomalyCount, &it.Anomalies, &it.RawOutput, &it.Command,
			&it.Truncated, &it.DurationMS, &it.ErrorCode, &it.CreatedAt)
	if err != nil {
		return store.InspectionReportItem{}, mapError(err)
	}
	it.CreatedAt = utc(it.CreatedAt)
	return it, nil
}

// PreviousScores 取同 job 在 before 之前最近一份报告的 per-server 评分
// （趋势对比；即时巡检 job_id=NULL 无对比——调用方不调）。窗口列用
// started_at（报告落库的 created_at 晚于本份的 started_at，会漏掉上一份）。
func (s inspectionReportStore) PreviousScores(ctx context.Context, jobID string, before time.Time) (map[string]int, error) {
	rows, err := s.q.Query(ctx, `
		SELECT i.server_id, i.score
		FROM inspection_report_items i
		WHERE i.report_id = (
			SELECT id FROM inspection_reports
			WHERE job_id = $1::uuid AND started_at < $2
			ORDER BY started_at DESC LIMIT 1
		)`, jobID, utc(before))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make(map[string]int, 16)
	for rows.Next() {
		var serverID string
		var score int
		if err := rows.Scan(&serverID, &score); err != nil {
			return nil, err
		}
		out[serverID] = score
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}
