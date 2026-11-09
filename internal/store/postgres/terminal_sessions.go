// W3：terminal_sessions 表的 PostgreSQL 实现（CONTRACT.md 3.4/4.13）。
//
// 行为约定：
//   - Close 用 UPDATE ... WHERE status='active' RETURNING，0 行时区分
//     ErrNotFound（行不存在）与 ErrConflict（已 closed，并发双断开时
//     先到者生效）。
//   - TouchActivity 只对 active 行生效，0 行不算错误。
//   - recording_file 不经参数传递：录制文件名恒为 <id>.cast，Close 时
//     由 SQL 按 bytes_written 是否大于 0 派生（契约 4.13 CHECK 约束）。
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type terminalSessionStore struct{ q querier }

const terminalSessionColumns = `t.id::text, t.server_id::text, coalesce(s.name, ''),
	coalesce(s.os, 'linux'), coalesce(u.username, ''), t.status, t.cols, t.rows,
	t.ended_reason, t.recording_file, t.bytes_written,
	t.started_at, t.last_activity_at, t.ended_at, t.created_at, t.updated_at`

// 纯 JOIN 子句：CTE（FROM inserted t）与实表查询共用。
const terminalSessionJoin = `
	LEFT JOIN servers s ON s.id = t.server_id
	LEFT JOIN users u ON u.id = t.user_id`

const terminalSessionFrom = ` FROM terminal_sessions t` + terminalSessionJoin

func scanTerminalSession(row pgx.Row) (store.TerminalSession, error) {
	var t store.TerminalSession
	err := row.Scan(&t.ID, &t.ServerID, &t.ServerName, &t.ServerOS, &t.Username,
		&t.Status, &t.Cols, &t.Rows, &t.EndedReason, &t.RecordingFile, &t.BytesWritten,
		&t.StartedAt, &t.LastActivityAt, &t.EndedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return store.TerminalSession{}, mapError(err)
	}
	t.HasRecording = t.RecordingFile != ""
	t.StartedAt = utc(t.StartedAt)
	t.LastActivityAt = utc(t.LastActivityAt)
	if t.EndedAt != nil {
		ended := utc(*t.EndedAt)
		t.EndedAt = &ended
	}
	t.CreatedAt = utc(t.CreatedAt)
	t.UpdatedAt = utc(t.UpdatedAt)
	return t, nil
}

func (s terminalSessionStore) Create(ctx context.Context, p store.CreateTerminalSessionParams) (store.TerminalSession, error) {
	startedAt := p.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return scanTerminalSession(s.q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO terminal_sessions (server_id, user_id, cols, rows, started_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			RETURNING *
		)
		SELECT `+terminalSessionColumns+` FROM inserted t`+terminalSessionJoin,
		p.ServerID, p.UserID, p.Cols, p.Rows, utc(startedAt)))
}

func (s terminalSessionStore) Get(ctx context.Context, id string) (store.TerminalSession, error) {
	return scanTerminalSession(s.q.QueryRow(ctx,
		`SELECT `+terminalSessionColumns+terminalSessionFrom+` WHERE t.id = $1::uuid`, id))
}

func (s terminalSessionStore) List(ctx context.Context, f store.InteractiveSessionFilter) ([]store.TerminalSession, error) {
	n, err := pageLimit(f.Limit)
	if err != nil {
		return nil, err
	}
	where := []string{"TRUE"}
	args := make([]any, 0, 3)
	if f.Status != "" {
		args = append(args, string(f.Status))
		where = append(where, fmt.Sprintf("t.status = $%d", len(args)))
	}
	if f.ServerID != "" {
		args = append(args, f.ServerID)
		where = append(where, fmt.Sprintf("t.server_id = $%d::uuid", len(args)))
	}
	args = append(args, n)
	query := `SELECT ` + terminalSessionColumns + terminalSessionFrom + `
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.TerminalSession, 0, n)
	for rows.Next() {
		item, err := scanTerminalSession(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}

func (s terminalSessionStore) TouchActivity(ctx context.Context, id string, at time.Time, bytesWritten int64) error {
	_, err := s.q.Exec(ctx, `
		UPDATE terminal_sessions
		SET last_activity_at = $2, bytes_written = $3, updated_at = now()
		WHERE id = $1::uuid AND status = 'active'`,
		id, utc(at), bytesWritten)
	return mapError(err)
}

func (s terminalSessionStore) Close(ctx context.Context, p store.CloseInteractiveSessionParams) (store.TerminalSession, error) {
	// recording_file 由会话 id 派生（文件名即 UUID，不可预测）：有录制时
	// <id>.cast，无录制保持空串，满足 CHECK (recording_file IN ('') OR ...)。
	session, err := scanTerminalSession(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE terminal_sessions
			SET status = 'closed', ended_reason = $2, ended_at = $3,
				bytes_written = $4,
				recording_file = CASE WHEN $4::bigint > 0 THEN id::text || '.cast' ELSE '' END,
				updated_at = now()
			WHERE id = $1::uuid AND status = 'active'
			RETURNING *
		)
		SELECT `+terminalSessionColumns+` FROM updated t`+terminalSessionJoin,
		p.ID, p.Reason, utc(p.EndedAt), p.BytesWritten))
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.TerminalSession{}, err
	}
	// 0 行：区分行不存在与已 closed（并发双断开时先到者生效）。
	var current store.InteractiveSessionStatus
	checkErr := s.q.QueryRow(ctx,
		`SELECT status FROM terminal_sessions WHERE id = $1::uuid`, p.ID).Scan(&current)
	if checkErr != nil {
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return store.TerminalSession{}, store.ErrNotFound
		}
		return store.TerminalSession{}, mapError(checkErr)
	}
	return store.TerminalSession{}, store.ErrConflict
}

func (s terminalSessionStore) CountActive(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx,
		`SELECT count(*) FROM terminal_sessions WHERE status = 'active'`).Scan(&n)
	return n, mapError(err)
}

func (s terminalSessionStore) CountActiveByServer(ctx context.Context, serverID string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx,
		`SELECT count(*) FROM terminal_sessions WHERE status = 'active' AND server_id = $1::uuid`,
		serverID).Scan(&n)
	return n, mapError(err)
}

var _ store.TerminalSessionStore = terminalSessionStore{}
