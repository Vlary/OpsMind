// batch.go 实现五期 M3 的 BatchJobStore：batch_jobs/batch_results 的 CRUD、
// 聚合计数派生（TotalCount/DoneCount 查询时 COUNT，不落列）、按 approval_id
// 回填入口（契约 4.25/4.26/3.4）。排序固定：batch_jobs.created_at DESC,
// id DESC；batch_results.created_at ASC, id ASC（与 server_ids 输入顺序对齐
// ——由 Create 顺序保证）。Output 截断发生在 batchx（256KB），store 不二次
// 截断。
package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type batchJobStore struct{ q querier }

// batchResultColumns JOIN servers.name 填充 ServerName（服务器删除后空串）；
// 另 LEFT JOIN approvals 带出 edited_command（六期打磨 sprint 5：批量详情
// 要显示「实际执行」的改写命令，逐行回取审批会在单 job 50 台时打 50 次
// HTTP）。approvals 行被删时 batch_results.approval_id 经 FK ON DELETE
// SET NULL 置空，COALESCE 兜底为空串。
const batchResultColumns = `r.id::text, r.batch_job_id::text, r.server_id::text,
	coalesce(s.name, ''), r.risk_level, r.rule_id, r.approval_id::text, r.status,
	coalesce(a.edited_command, ''), r.exit_code, r.output, r.truncated, r.error_code,
	r.duration_ms, r.created_at, r.updated_at`

const batchResultFrom = ` FROM batch_results r LEFT JOIN servers s ON s.id = r.server_id
	LEFT JOIN approvals a ON a.id = r.approval_id`

func scanBatchResult(row pgx.Row) (store.BatchResult, error) {
	var r store.BatchResult
	var approvalID *string
	err := row.Scan(&r.ID, &r.BatchJobID, &r.ServerID, &r.ServerName,
		&r.RiskLevel, &r.RuleID, &approvalID, &r.Status, &r.EditedCommand,
		&r.ExitCode, &r.Output, &r.Truncated, &r.ErrorCode, &r.DurationMS,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return store.BatchResult{}, mapError(err)
	}
	r.ApprovalID = approvalID
	r.CreatedAt = utc(r.CreatedAt)
	r.UpdatedAt = utc(r.UpdatedAt)
	return r, nil
}

// batchJobColumns 内联 batch_results 聚合：total=全部台数，done=终态台数
// （含 skipped/denied/expired/cancelled——契约 3.2 DoneCount 注释）。
const batchJobColumns = `j.id::text, j.description, j.command, j.server_ids::text[],
	j.source, j.conversation_id::text, j.status,
	coalesce(count(r.id), 0),
	coalesce(count(r.id) FILTER (WHERE r.status IN ('executed','failed','denied','expired','skipped','cancelled')), 0),
	j.finished_at, j.created_by::text, j.created_at, j.updated_at`

const batchJobFrom = ` FROM batch_jobs j LEFT JOIN batch_results r ON r.batch_job_id = j.id`

func scanBatchJob(row pgx.Row) (store.BatchJob, error) {
	var j store.BatchJob
	var createdBy *string
	err := row.Scan(&j.ID, &j.Description, &j.Command, &j.ServerIDs, &j.Source,
		&j.ConversationID, &j.Status, &j.TotalCount, &j.DoneCount,
		&j.FinishedAt, &createdBy, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return store.BatchJob{}, mapError(err)
	}
	j.CreatedBy = createdBy
	if j.ServerIDs == nil {
		j.ServerIDs = []string{}
	}
	j.CreatedAt = utc(j.CreatedAt)
	j.UpdatedAt = utc(j.UpdatedAt)
	if j.FinishedAt != nil {
		fin := utc(*j.FinishedAt)
		j.FinishedAt = &fin
	}
	return j, nil
}

func (s batchJobStore) Create(ctx context.Context, p store.CreateBatchJobParams) (store.BatchJob, error) {
	// data-modifying CTE 的插入行对同语句主查询不可见（PostgreSQL 语义），
	// 必须从 inserted 自身 SELECT（approvals.Create 同款）。此时无 results，
	// 聚合列为 0/0；TotalCount 语义=server_ids 台数，Create 返回后由 batchx
	// 逐台建 result 对齐（Get/List 走真实 COUNT）。
	row := s.q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO batch_jobs (description, command, server_ids, source, conversation_id, status, created_by)
			VALUES ($1, $2, $3::uuid[], $4, $5::uuid, 'pending', $6::uuid)
			RETURNING id, description, command, server_ids, source, conversation_id, status, finished_at, created_by, created_at, updated_at
		)
		SELECT j.id::text, j.description, j.command, j.server_ids::text[], j.source,
			j.conversation_id::text, j.status, 0, 0, j.finished_at, j.created_by::text,
			j.created_at, j.updated_at
		FROM inserted j`,
		p.Description, p.Command, nonNil(p.ServerIDs), string(p.Source),
		ptrOrNil(p.ConversationID), ptrOrNil(p.CreatedBy))
	j, err := scanBatchJob(row)
	if err != nil {
		return store.BatchJob{}, err
	}
	j.TotalCount = len(p.ServerIDs)
	return j, nil
}

func (s batchJobStore) Get(ctx context.Context, id string) (store.BatchJob, []store.BatchResult, error) {
	// 聚合查询按 job 分组（GROUP BY j.id），单 job 场景等价。
	j, err := scanBatchJob(s.q.QueryRow(ctx,
		`SELECT `+batchJobColumns+batchJobFrom+` WHERE j.id = $1::uuid GROUP BY j.id`, id))
	if err != nil {
		return store.BatchJob{}, nil, err
	}
	rows, err := s.q.Query(ctx,
		`SELECT `+batchResultColumns+batchResultFrom+
			` WHERE r.batch_job_id = $1::uuid ORDER BY r.created_at ASC, r.id ASC`, id)
	if err != nil {
		return store.BatchJob{}, nil, mapError(err)
	}
	defer rows.Close()
	results := []store.BatchResult{}
	for rows.Next() {
		r, err := scanBatchResult(rows)
		if err != nil {
			return store.BatchJob{}, nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return store.BatchJob{}, nil, mapError(err)
	}
	return j, results, nil
}

func (s batchJobStore) List(ctx context.Context, limit int) ([]store.BatchJob, error) {
	n, err := pageLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.Query(ctx,
		`SELECT `+batchJobColumns+batchJobFrom+
			` GROUP BY j.id ORDER BY j.created_at DESC, j.id DESC LIMIT $1`, n)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	jobs := []store.BatchJob{}
	for rows.Next() {
		j, err := scanBatchJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return jobs, nil
}

// SetStatus 更新 job 状态；终态（done/partial/failed/cancelled）时写
// finished_at（幂等：非终态保持原值）。
func (s batchJobStore) SetStatus(ctx context.Context, id string, status store.BatchStatus, at time.Time) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE batch_jobs SET status = $2, updated_at = $3,
			finished_at = CASE WHEN $2 IN ('done','partial','failed','cancelled') THEN $3 ELSE finished_at END
		WHERE id = $1::uuid`, id, string(status), utc(at))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s batchJobStore) CreateResult(ctx context.Context, p store.CreateBatchResultParams) (store.BatchResult, error) {
	return scanBatchResult(s.q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO batch_results (batch_job_id, server_id, risk_level, rule_id, approval_id, status, error_code)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, $7)
			RETURNING id, batch_job_id, server_id, risk_level, rule_id, approval_id, status, exit_code, output, truncated, error_code, duration_ms, created_at, updated_at
		)
		SELECT `+batchResultColumns+` FROM inserted r
			LEFT JOIN servers s ON s.id = r.server_id
			LEFT JOIN approvals a ON a.id = r.approval_id`,
		p.BatchJobID, p.ServerID, p.RiskLevel, p.RuleID, ptrOrNil(p.ApprovalID),
		string(p.Status), p.ErrorCode))
}

// FinishResult 按 ID 直达回填（ApprovalID 参数保留给按审批定位的调用方
// ——本实现 ID 已直达；契约 3.3 FinishBatchResultParams 注释）。
func (s batchJobStore) FinishResult(ctx context.Context, p store.FinishBatchResultParams) (store.BatchResult, error) {
	return scanBatchResult(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE batch_results SET status = $2, exit_code = $3, output = $4,
				truncated = $5, error_code = $6, duration_ms = $7, updated_at = $8
			WHERE id = $1::uuid
			  -- 状态守卫：仅未收敛态可被回填——Cancel 已置 cancelled / 过期
			  -- 扫描已收敛的行不得被迟到的执行结果覆盖（五期评审发现：
			  -- runDirect 完成的 executed 曾静默覆盖 cancelled）。
			  AND status IN ('pending', 'awaiting_approval')
			RETURNING id, batch_job_id, server_id, risk_level, rule_id, approval_id, status, exit_code, output, truncated, error_code, duration_ms, created_at, updated_at
		)
		SELECT `+batchResultColumns+` FROM updated r
			LEFT JOIN servers s ON s.id = r.server_id
			LEFT JOIN approvals a ON a.id = r.approval_id`,
		p.ID, string(p.Status), p.ExitCode, p.Output, p.Truncated, p.ErrorCode, p.DurationMS, utc(p.FinishedAt)))
}

func (s batchJobStore) ByApproval(ctx context.Context, approvalID string) (store.BatchResult, error) {
	return scanBatchResult(s.q.QueryRow(ctx,
		`SELECT `+batchResultColumns+batchResultFrom+` WHERE r.approval_id = $1::uuid`, approvalID))
}
