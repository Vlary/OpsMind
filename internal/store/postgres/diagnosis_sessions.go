// diagnosis_sessions.go 实现五期 M2 的 DiagnosisSessionStore（契约 3.4/4.21）。
// UpdateStatus 为 CAS：期望状态不匹配（0 行）返回 ErrConflict；状态机由
// 引擎驱动，store 只做条件写入。
package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"aiops/internal/store"
)

type diagnosisSessionStore struct{ q querier }

const diagnosisSessionColumns = `d.id, d.alert_event_id, d.server_id, coalesce(s.name, ''),
	d.conversation_id, d.trigger, d.status, d.report, d.error_code, d.started_at,
	d.finished_at, d.expires_at, d.created_at, d.updated_at`

const diagnosisSessionFrom = `
	FROM diagnosis_sessions d
	LEFT JOIN servers s ON s.id = d.server_id`

func scanDiagnosisSession(row interface{ Scan(dest ...any) error }) (store.DiagnosisSession, error) {
	var d store.DiagnosisSession
	err := row.Scan(&d.ID, &d.AlertEventID, &d.ServerID, &d.ServerName,
		&d.ConversationID, &d.Trigger, &d.Status, &d.Report, &d.ErrorCode,
		&d.StartedAt, &d.FinishedAt, &d.ExpiresAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return store.DiagnosisSession{}, mapError(err)
	}
	d.ExpiresAt = utc(d.ExpiresAt)
	d.CreatedAt = utc(d.CreatedAt)
	d.UpdatedAt = utc(d.UpdatedAt)
	return d, nil
}

// Create 插入 pending 会话（契约 4.21：expires_at 由建者给定）。ServerName
// 经子查询装配（与 JOIN 读取路径同形）。
func (s diagnosisSessionStore) Create(ctx context.Context, p store.CreateDiagnosisParams) (store.DiagnosisSession, error) {
	var d store.DiagnosisSession
	err := s.q.QueryRow(ctx, `
		INSERT INTO diagnosis_sessions (alert_event_id, server_id, conversation_id,
			trigger, status, expires_at)
		VALUES ($1, $2::uuid, $3::uuid, $4, 'pending', $5)
		RETURNING id, alert_event_id, server_id,
			(SELECT name FROM servers WHERE id = $2::uuid), conversation_id,
			trigger, status, report, error_code, started_at, finished_at,
			expires_at, created_at, updated_at`,
		p.AlertEventID, p.ServerID, ptrOrNil(p.ConversationID), string(p.Trigger), utc(p.ExpiresAt)).
		Scan(&d.ID, &d.AlertEventID, &d.ServerID, &d.ServerName, &d.ConversationID,
			&d.Trigger, &d.Status, &d.Report, &d.ErrorCode,
			&d.StartedAt, &d.FinishedAt, &d.ExpiresAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return store.DiagnosisSession{}, mapError(err)
	}
	d.ExpiresAt = utc(d.ExpiresAt)
	d.CreatedAt = utc(d.CreatedAt)
	d.UpdatedAt = utc(d.UpdatedAt)
	return d, nil
}

func (s diagnosisSessionStore) Get(ctx context.Context, id string) (store.DiagnosisSession, error) {
	return scanDiagnosisSession(s.q.QueryRow(ctx,
		`SELECT `+diagnosisSessionColumns+diagnosisSessionFrom+` WHERE d.id = $1::uuid`, id))
}

// List 按 created_at DESC 分页；status/serverID 为空表示不过滤。
func (s diagnosisSessionStore) List(ctx context.Context, status store.DiagnosisStatus, serverID string, limit int) ([]store.DiagnosisSession, error) {
	n, err := pageLimit(limit)
	if err != nil {
		return nil, err
	}
	where := "TRUE"
	args := make([]any, 0, 3)
	if status != "" {
		args = append(args, string(status))
		where = "d.status = $1"
	}
	if serverID != "" {
		args = append(args, serverID)
		where += " AND d.server_id = $" + strconv.Itoa(len(args)) + "::uuid"
	}
	args = append(args, n)
	rows, err := s.q.Query(ctx, `SELECT `+diagnosisSessionColumns+diagnosisSessionFrom+
		` WHERE `+where+` ORDER BY d.created_at DESC, d.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	items := make([]store.DiagnosisSession, 0, n)
	for rows.Next() {
		item, err := scanDiagnosisSession(rows)
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

// UpdateStatus 是引擎专用的 CAS 状态迁移（契约 3.4）：
//   - From 非空时 WHERE status=From；0 行返回 ErrConflict。
//   - To ∈ {diagnosing} 写 started_at=Timestamp；To ∈ 终态（report 之后
//     由引擎分两次迁移，finished_at 在 awaiting_fix/终态时写）。
//   - Report 非 nil 时覆盖 report（jsonb）；ErrorCode 非空时覆盖 error_code。
func (s diagnosisSessionStore) UpdateStatus(ctx context.Context, p store.UpdateDiagnosisStatusParams) (store.DiagnosisSession, error) {
	if p.ID == "" {
		return store.DiagnosisSession{}, store.ErrInvalid
	}
	args := make([]any, 0, 6)
	args = append(args, p.ID)
	set := []string{"status = $2", "updated_at = now()"}
	args = append(args, string(p.To))

	if p.To == store.DiagnosisDiagnosing {
		set = append(set, "started_at = $"+strconv.Itoa(len(args)+1))
		args = append(args, utc(p.Timestamp))
	}
	if p.To == store.DiagnosisAwaitingFix {
		// 契约 4.21：expires_at 在进入 awaiting_fix 时设置（+30m，与表
		// DEFAULT 同源的固定窗口）。
		set = append(set, "expires_at = $"+strconv.Itoa(len(args)+1)+"::timestamptz + interval '30 minutes'")
		args = append(args, utc(p.Timestamp))
	}
	switch p.To {
	case store.DiagnosisFailed, store.DiagnosisDone, store.DiagnosisExpired:
		set = append(set, "finished_at = $"+strconv.Itoa(len(args)+1))
		args = append(args, utc(p.Timestamp))
	}
	if p.Report != nil {
		set = append(set, "report = $"+strconv.Itoa(len(args)+1)+"::jsonb")
		args = append(args, []byte(p.Report))
	}
	if p.ErrorCode != "" {
		set = append(set, "error_code = $"+strconv.Itoa(len(args)+1))
		args = append(args, p.ErrorCode)
	}
	where := "id = $1::uuid"
	if p.From != "" {
		args = append(args, string(p.From))
		where += " AND status = $" + strconv.Itoa(len(args))
	}

	tag, err := s.q.Exec(ctx, `UPDATE diagnosis_sessions SET `+strings.Join(set, ", ")+
		` WHERE `+where, args...)
	if err != nil {
		return store.DiagnosisSession{}, mapError(err)
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.Get(ctx, p.ID); gerr != nil {
			return store.DiagnosisSession{}, gerr
		}
		return store.DiagnosisSession{}, store.ErrConflict
	}
	return s.Get(ctx, p.ID)
}

// ExpireStale 把 awaiting_fix 且 expires_at<now 的会话置 expired（30 分钟
// 窗口到期；引擎每 30s 扫描调用）。
func (s diagnosisSessionStore) ExpireStale(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE diagnosis_sessions SET status = 'expired', finished_at = $1,
			updated_at = now()
		WHERE status = 'awaiting_fix' AND expires_at < $1`, utc(now))
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}
