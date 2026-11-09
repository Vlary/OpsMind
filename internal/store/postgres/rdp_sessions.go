// W4：rdp_sessions 表的 PostgreSQL 实现（CONTRACT.md 3.4/4.14）。
//
// 行为约定（与 terminal_sessions.go 同构）：
//   - Close 用 UPDATE ... WHERE status='active' RETURNING，0 行时区分
//     ErrNotFound（行不存在）与 ErrConflict（已 closed，并发双断开时
//     先到者生效）。
//   - TouchActivity 只对 active 行生效，0 行不算错误。
//   - recording_file 不经参数传递：文件名恒为 <id>.guac，Close 时
//     由 SQL 按 bytes_written 是否大于 0 派生（契约 4.14 CHECK 约束）。
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

type rdpSessionStore struct{ q querier }

// RdpSessions 挂载 RDP 会话仓储（方法定义在本文件，避免多人编辑 store.go）。
func (s *Store) RdpSessions() store.RdpSessionStore { return rdpSessionStore{q: s.q} }

const rdpSessionColumns = `t.id::text, t.server_id::text, coalesce(s.name, ''),
	t.width, t.height, t.dpi, t.status, t.ended_reason, t.recording_file,
	t.bytes_written, t.started_at, t.last_activity_at, t.ended_at,
	t.created_at, t.updated_at`

// 纯 JOIN 子句：CTE（FROM inserted/updated t）与实表查询共用。
const rdpSessionJoin = `
	LEFT JOIN servers s ON s.id = t.server_id`

const rdpSessionFrom = ` FROM rdp_sessions t` + rdpSessionJoin

func scanRdpSession(row pgx.Row) (store.RdpSession, error) {
	var t store.RdpSession
	err := row.Scan(&t.ID, &t.ServerID, &t.ServerName,
		&t.Width, &t.Height, &t.DPI, &t.Status, &t.EndedReason, &t.RecordingFile,
		&t.BytesWritten, &t.StartedAt, &t.LastActivityAt, &t.EndedAt,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return store.RdpSession{}, mapError(err)
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

func (s rdpSessionStore) Create(ctx context.Context, p store.CreateRdpSessionParams) (store.RdpSession, error) {
	startedAt := p.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return scanRdpSession(s.q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO rdp_sessions (server_id, user_id, width, height, dpi, started_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
			RETURNING *
		)
		SELECT `+rdpSessionColumns+` FROM inserted t`+rdpSessionJoin,
		p.ServerID, p.UserID, p.Width, p.Height, p.DPI, utc(startedAt)))
}

func (s rdpSessionStore) Get(ctx context.Context, id string) (store.RdpSession, error) {
	return scanRdpSession(s.q.QueryRow(ctx,
		`SELECT `+rdpSessionColumns+rdpSessionFrom+` WHERE t.id = $1::uuid`, id))
}

func (s rdpSessionStore) List(ctx context.Context, f store.InteractiveSessionFilter) ([]store.RdpSession, error) {
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
	query := `SELECT ` + rdpSessionColumns + rdpSessionFrom + `
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.RdpSession, 0, n)
	for rows.Next() {
		item, err := scanRdpSession(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}

func (s rdpSessionStore) TouchActivity(ctx context.Context, id string, at time.Time, bytesWritten int64) error {
	_, err := s.q.Exec(ctx, `
		UPDATE rdp_sessions
		SET last_activity_at = $2, bytes_written = $3, updated_at = now()
		WHERE id = $1::uuid AND status = 'active'`,
		id, utc(at), bytesWritten)
	return mapError(err)
}

func (s rdpSessionStore) Close(ctx context.Context, p store.CloseInteractiveSessionParams) (store.RdpSession, error) {
	// recording_file 由会话 id 派生（文件名即 UUID，不可预测）：有录制时
	// <id>.guac，无录制保持空串，满足 CHECK (recording_file IN ('') OR ...)。
	session, err := scanRdpSession(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE rdp_sessions
			SET status = 'closed', ended_reason = $2, ended_at = $3,
				bytes_written = $4,
				recording_file = CASE WHEN $4::bigint > 0 THEN id::text || '.guac' ELSE '' END,
				updated_at = now()
			WHERE id = $1::uuid AND status = 'active'
			RETURNING *
		)
		SELECT `+rdpSessionColumns+` FROM updated t`+rdpSessionJoin,
		p.ID, p.Reason, utc(p.EndedAt), p.BytesWritten))
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.RdpSession{}, err
	}
	// 0 行：区分行不存在与已 closed（并发双断开时先到者生效）。
	var current store.InteractiveSessionStatus
	checkErr := s.q.QueryRow(ctx,
		`SELECT status FROM rdp_sessions WHERE id = $1::uuid`, p.ID).Scan(&current)
	if checkErr != nil {
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return store.RdpSession{}, store.ErrNotFound
		}
		return store.RdpSession{}, mapError(checkErr)
	}
	return store.RdpSession{}, store.ErrConflict
}

func (s rdpSessionStore) CountActive(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx,
		`SELECT count(*) FROM rdp_sessions WHERE status = 'active'`).Scan(&n)
	return n, mapError(err)
}

func (s rdpSessionStore) CountActiveByServer(ctx context.Context, serverID string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx,
		`SELECT count(*) FROM rdp_sessions WHERE status = 'active' AND server_id = $1::uuid`,
		serverID).Scan(&n)
	return n, mapError(err)
}

var _ store.RdpSessionStore = rdpSessionStore{}
