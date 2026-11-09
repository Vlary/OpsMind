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

type approvalStore struct{ q querier }

const approvalColumns = `a.id::text, a.conversation_id::text, a.tool_call_id::text, a.source,
	coalesce(a.server_id::text, ''), coalesce(s.name, ''),
	a.command, a.risk_level, a.rule_id, a.reason, a.status, a.approval_mode,
	a.requested_at, a.expires_at, a.decided_at, coalesce(a.decided_by::text, ''), a.note,
	a.edited_command, a.edited_risk_level, a.edited_rule_id`

const approvalJoin = ` LEFT JOIN servers s ON s.id = a.server_id`

const approvalFrom = ` FROM approvals a` + approvalJoin

// scanApproval 对 conversation_id/tool_call_id 做 NULL 透明读取（四期 C1，
// 契约 3.4）：列 NULL ↔ Go nil（console 审批）；source 缺省 'chat' 防御
// 00006 之前语义的行。approval_mode 缺省 'standard' 防御 00008 之前的行
// （迁移 DEFAULT 已回填，双保险）。
func scanApproval(row pgx.Row) (store.Approval, error) {
	var a store.Approval
	var serverID, decidedBy string
	var conversationID, toolCallID *string
	err := row.Scan(&a.ID, &conversationID, &toolCallID, &a.Source,
		&serverID, &a.ServerName, &a.Command, &a.RiskLevel, &a.RuleID, &a.Reason, &a.Status, &a.ApprovalMode,
		&a.RequestedAt, &a.ExpiresAt, &a.DecidedAt, &decidedBy, &a.Note,
		&a.EditedCommand, &a.EditedRiskLevel, &a.EditedRuleID)
	if err != nil {
		return store.Approval{}, mapError(err)
	}
	a.ConversationID = conversationID
	a.ToolCallID = toolCallID
	if a.Source == "" {
		a.Source = "chat"
	}
	if a.ApprovalMode == "" {
		a.ApprovalMode = store.ApprovalModeStandard
	}
	a.ServerID = nullableString(serverID)
	a.DecidedBy = nullableString(decidedBy)
	a.RequestedAt = utc(a.RequestedAt)
	a.ExpiresAt = utc(a.ExpiresAt)
	if a.DecidedAt != nil {
		decided := utc(*a.DecidedAt)
		a.DecidedAt = &decided
	}
	return a, nil
}

func (s approvalStore) Create(ctx context.Context, p store.CreateApprovalParams) (store.Approval, error) {
	requestedAt := p.CreatedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	source := p.Source
	if source == "" {
		source = "chat"
	}
	// 六期 BE-P（契约 3.4）：ApprovalMode 空串按 'standard' 落库。
	approvalMode := p.ApprovalMode
	if approvalMode == "" {
		approvalMode = string(store.ApprovalModeStandard)
	}
	// ServerName 不落库：Create 返回对象用调用方传入的值填充；后续查询重新 JOIN。
	return scanApproval(s.q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO approvals (conversation_id, tool_call_id, source, server_id, command,
				risk_level, rule_id, reason, status, approval_mode, requested_at, expires_at)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, 'pending', $9, $10, $11)
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM inserted a`+approvalJoin,
		ptrOrNil(p.ConversationID), ptrOrNil(p.ToolCallID), source, ptrOrNil(p.ServerID),
		p.Command, p.RiskLevel, p.RuleID, p.Reason, approvalMode, utc(requestedAt), utc(p.ExpiresAt)))
}

func (s approvalStore) Get(ctx context.Context, id string) (store.Approval, error) {
	return scanApproval(s.q.QueryRow(ctx,
		`SELECT `+approvalColumns+approvalFrom+` WHERE a.id = $1::uuid`, id))
}

func (s approvalStore) List(ctx context.Context, conversationID string, status store.ApprovalStatus, limit int) ([]store.Approval, error) {
	n, err := pageLimit(limit)
	if err != nil {
		return nil, err
	}
	// 条件按需拼接：`a.conversation_id::text = $1` 会让
	// approvals_conversation_requested_idx 失效。
	where := []string{"TRUE"}
	args := make([]any, 0, 3)
	if conversationID != "" {
		args = append(args, conversationID)
		where = append(where, fmt.Sprintf("a.conversation_id = $%d::uuid", len(args)))
	}
	if status != "" {
		args = append(args, string(status))
		where = append(where, fmt.Sprintf("a.status = $%d", len(args)))
	}
	args = append(args, n)
	query := `SELECT ` + approvalColumns + approvalFrom + `
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY a.requested_at DESC, a.id DESC
		LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Approval, 0, n)
	for rows.Next() {
		item, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}

func (s approvalStore) Decide(ctx context.Context, p store.DecideApprovalParams) (store.Approval, error) {
	// 六期 BE-P（契约 3.3/3.4）：同一 UPDATE 落库决策状态与 edited_*
	//（未改写为空串——幂等覆盖）。
	approval, err := scanApproval(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET status = $2, decided_by = $3::uuid, note = $4, decided_at = $5,
				edited_command = $6, edited_risk_level = $7, edited_rule_id = $8
			WHERE id = $1::uuid AND status = 'pending'
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin,
		p.ID, p.Status, nullStr(p.DecidedBy), p.Note, utc(p.DecidedAt),
		p.EditedCommand, p.EditedRiskLevel, p.EditedRuleID))
	if err == nil {
		return approval, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Approval{}, err
	}
	// 0 行：区分不存在与已决策。
	var current store.ApprovalStatus
	checkErr := s.q.QueryRow(ctx, `SELECT status FROM approvals WHERE id = $1::uuid`, p.ID).Scan(&current)
	if checkErr != nil {
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return store.Approval{}, store.ErrNotFound
		}
		return store.Approval{}, mapError(checkErr)
	}
	return store.Approval{}, store.ErrConflict
}

func (s approvalStore) Expire(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE approvals SET status = 'expired'
		WHERE status = 'pending' AND expires_at <= $1`, utc(now))
	return tag.RowsAffected(), mapError(err)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
