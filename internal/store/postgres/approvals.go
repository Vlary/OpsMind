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
	a.edited_command, a.edited_risk_level, a.edited_rule_id,
	a.requested_by::text, coalesce(rb.username, ''), a.legacy, a.break_glass,
	a.grant_kind, a.grant_expires_at, a.consumed_at, a.policy_from, a.policy_to`

const approvalJoin = ` LEFT JOIN servers s ON s.id = a.server_id
	LEFT JOIN users rb ON rb.id = a.requested_by`

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
		&a.EditedCommand, &a.EditedRiskLevel, &a.EditedRuleID,
		&a.RequestedBy, &a.RequestedByName, &a.Legacy, &a.BreakGlass,
		&a.GrantKind, &a.GrantExpiresAt, &a.ConsumedAt, &a.PolicyFrom, &a.PolicyTo)
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
	if a.GrantExpiresAt != nil {
		grantExpires := utc(*a.GrantExpiresAt)
		a.GrantExpiresAt = &grantExpires
	}
	if a.ConsumedAt != nil {
		consumed := utc(*a.ConsumedAt)
		a.ConsumedAt = &consumed
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
	// 00012：requested_by NOT NULL。显式 RequestedBy 优先（chat=会话 owner /
	// console=HTTP 用户）；空串回落到会话 owner，再回落到最早用户（迁移回填语义）。
	return scanApproval(s.q.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO approvals (conversation_id, tool_call_id, source, server_id, command,
				risk_level, rule_id, reason, status, approval_mode, requested_at, expires_at, requested_by,
				grant_kind, grant_expires_at, policy_from, policy_to)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, 'pending', $9, $10, $11,
				COALESCE($12::uuid,
					(SELECT user_id FROM conversations WHERE id = $1::uuid),
					(SELECT id FROM users ORDER BY created_at ASC, id ASC LIMIT 1)),
				$13, $14, $15, $16)
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM inserted a`+approvalJoin,
		ptrOrNil(p.ConversationID), ptrOrNil(p.ToolCallID), source, ptrOrNil(p.ServerID),
		p.Command, p.RiskLevel, p.RuleID, p.Reason, approvalMode, utc(requestedAt), utc(p.ExpiresAt),
		ptrOrNil(optionalID(p.RequestedBy)),
		ptrOrNil(p.GrantKind), utcPtr(p.GrantExpiresAt), ptrOrNil(p.PolicyFrom), ptrOrNil(p.PolicyTo)))
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

// decideSoDClause 是 Decide 的职责分离 SQL 条件（契约 12.3 第 6 条）。
// 独立成常量便于守卫测试断言：应用层先读后写不是唯一防线。
const decideSoDClause = `AND (legacy OR $3::uuid IS NULL OR requested_by <> $3::uuid)`

// decideExpiryClause 是 Decide 的 TTL SQL 兜底（第 11 轮走查 high）：
// approve/deny 只能在 expires_at 晚于决策时刻（$5=decided_at）时落库，
// 与 Service.Decide 的 now >= ExpiresAt 判定同源；expired（Cancel/重启
// 收敛）是系统迁移，不受 TTL 限制。应用层先读后写不是唯一防线：读到时
// 未过期、写入时已过期的竞态也由本条件拒绝。
const decideExpiryClause = `AND (expires_at > $5 OR $2 = 'expired')`

func (s approvalStore) Decide(ctx context.Context, p store.DecideApprovalParams) (store.Approval, error) {
	// 六期 BE-P（契约 3.3/3.4）：同一 UPDATE 落库决策状态与 edited_*
	//（未改写为空串——幂等覆盖）。
	approval, err := scanApproval(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET status = $2, decided_by = $3::uuid, note = $4, decided_at = $5,
				edited_command = $6, edited_risk_level = $7, edited_rule_id = $8
			WHERE id = $1::uuid AND status = 'pending'
				-- 契约 12.3 第 6 条：职责分离在 DB 层再兜一次（应用层先读后写
				-- 不是唯一防线）。legacy 历史行豁免；decided_by 为空（Cancel/
				-- 过期）不参与比较。
				`+decideSoDClause+`
				`+decideExpiryClause+`
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

// ConsumeGrant 原子消费一次 terminal grant（契约 12.3 第 12 条）：
// approved + 未消费 + 未过期 + requested_by=用户 + server_id 匹配 +
// grant_kind 匹配，全部条件在同一条 UPDATE 内判定；单次使用由
// consumed_at IS NULL 保证。
//
// ApprovalID 为空时消费该用户在该服务器上最新一条符合条件的 approved
// grant（前端申请-审批后无需回传 id 即可开会话；仍要求他人批准与单次消费）。
func (s approvalStore) ConsumeGrant(ctx context.Context, p store.ConsumeGrantParams) (store.Approval, error) {
	now := utc(p.Now)
	if p.ApprovalID == "" {
		ap, err := scanApproval(s.q.QueryRow(ctx, `
			WITH candidate AS (
				SELECT id FROM approvals
				WHERE status = 'approved'
					AND source = 'terminal'
					AND consumed_at IS NULL
					AND grant_expires_at > $4
					AND requested_by = $1::uuid
					AND server_id = $2::uuid
					AND grant_kind = $3
				ORDER BY decided_at DESC NULLS LAST, id DESC
				LIMIT 1
				FOR UPDATE
			), updated AS (
				UPDATE approvals SET consumed_at = $4
				WHERE id = (SELECT id FROM candidate)
				RETURNING *
			)
			SELECT `+approvalColumns+` FROM updated a`+approvalJoin,
			p.UserID, p.ServerID, p.GrantKind, now))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return store.Approval{}, store.ErrGrantUnavailable
			}
			return store.Approval{}, err
		}
		return ap, nil
	}
	ap, err := scanApproval(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET consumed_at = $5
			WHERE id = $1::uuid
				AND status = 'approved'
				AND source = 'terminal'
				AND consumed_at IS NULL
				AND grant_expires_at > $5
				AND requested_by = $2::uuid
				AND server_id = $3::uuid
				AND grant_kind = $4
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin,
		p.ApprovalID, p.UserID, p.ServerID, p.GrantKind, now))
	if err == nil {
		return ap, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Approval{}, err
	}
	// 0 行：区分「类型用错」与「没有可消费 grant」（不泄露更细原因）。
	var kind string
	checkErr := s.q.QueryRow(ctx, `
		SELECT coalesce(grant_kind, '') FROM approvals
		WHERE id = $1::uuid AND source = 'terminal'`, p.ApprovalID).Scan(&kind)
	if checkErr != nil {
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return store.Approval{}, store.ErrGrantUnavailable
		}
		return store.Approval{}, mapError(checkErr)
	}
	if kind != p.GrantKind {
		return store.Approval{}, store.ErrGrantKindMismatch
	}
	return store.Approval{}, store.ErrGrantUnavailable
}

// ReleaseGrant 回滚一次刚发生的 grant 消费（第 4 轮走查 medium）：
// 会话 Open 失败时把 consumed_at 清空，授权保留可重试，避免一次
// TERM_SESSION_EXISTS/RDP_SESSION_EXISTS 烧掉他人刚批准的授权。
//
// 精确匹配 consumed_at（本次消费写入的时间戳）与 requested_by：并发下
// 若授权已被本用户的另一次请求重新消费，CAS 失败返回 ErrGrantUnavailable，
// 不会误释放他人的使用。
func (s approvalStore) ReleaseGrant(ctx context.Context, p store.ReleaseGrantParams) (store.Approval, error) {
	ap, err := scanApproval(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET consumed_at = NULL
			WHERE id = $1::uuid
				AND status = 'approved'
				AND consumed_at = $2
				AND requested_by = $3::uuid
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin,
		p.ApprovalID, utc(p.ConsumedAt), p.UserID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Approval{}, store.ErrGrantUnavailable
		}
		return store.Approval{}, err
	}
	return ap, nil
}

// revokeReturnLimit 是收敛类 UPDATE 的 RETURNING 上限（防 OOM 铁律：写死
// 数值）。调用方在同一事务内逐行写审计；超出部分由下一轮 sweeper/调用
// 再次收敛（幂等）。
const revokeReturnLimit = 500

// scanApprovalRows 把多行审批结果读成切片（上限 limit，防无界读取）。
func scanApprovalRows(rows pgx.Rows, limit int) ([]store.Approval, error) {
	defer rows.Close()
	out := make([]store.Approval, 0, limit)
	for rows.Next() {
		item, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out, mapError(rows.Err())
}

// ExpirePendingGrantsByUser 收敛该用户未消费的 pending terminal grant：
// 停用/改角色/吊销售后授权不得继续可用（契约 12.3 第 12 条）。
//
// 返回受影响的行（有界 revokeReturnLimit）：调用方在同一事务内逐条写
// terminal.grant_expired 审计（走查第 3 轮 high：此前只 UPDATE、无审计，
// 审计员无法证明授权是被系统作废还是丢失）。
func (s approvalStore) ExpirePendingGrantsByUser(ctx context.Context, userID string, now time.Time) ([]store.Approval, error) {
	rows, err := s.q.Query(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET status = 'expired', note = 'rbac:user_revoked', decided_at = $2
			WHERE source = 'terminal' AND status = 'pending' AND requested_by = $1::uuid
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin+`
		LIMIT `+strconv.Itoa(revokeReturnLimit),
		userID, utc(now))
	if err != nil {
		return nil, mapError(err)
	}
	return scanApprovalRows(rows, revokeReturnLimit)
}

// ListAvailableGrants 返回该用户已批准、未消费且未过期的 terminal grant
// （契约 12.5：发起人打开终端/桌面前可先看到自己有哪些授权可用）。
//
// 有界读取：limit 1..200（缺省 50），按 grant_expires_at ASC（最先过期的
// 排前面，UI 可提示最紧迫的一条）。只按 requested_by 过滤——授权是个人
// 凭据，audit:read 也不得枚举他人 grant。
func (s approvalStore) ListAvailableGrants(ctx context.Context, p store.ListAvailableGrantsParams) ([]store.Approval, error) {
	if p.UserID == "" {
		return nil, store.ErrInvalid
	}
	n, err := pageLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	where := `a.source = 'terminal' AND a.status = 'approved'
		AND a.consumed_at IS NULL AND a.grant_expires_at > $1
		AND a.requested_by = $2::uuid`
	args := []any{utc(p.Now), p.UserID}
	if p.GrantKind != "" {
		if p.GrantKind != "term" && p.GrantKind != "rdp" {
			return nil, store.ErrInvalid
		}
		args = append(args, p.GrantKind)
		where += ` AND a.grant_kind = $` + strconv.Itoa(len(args))
	}
	args = append(args, n)
	rows, err := s.q.Query(ctx, `SELECT `+approvalColumns+approvalFrom+`
		WHERE `+where+`
		ORDER BY a.grant_expires_at ASC, a.id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
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

// ExpirePendingByUser 把该用户全部 pending 审批置 expired，并把由这些审批
// 驱动的 awaiting_approval tool_call 收敛为 expired/APPROVAL_CANCELLED
// （契约 12.3 第 15/22 条：停用/降级/吊销会话必须取消待处理审批，不能留下
// re-enable 后仍可批准的旧审批）。
//
// 两条 UPDATE 在调用方事务内执行，业务行与状态收敛同提交。返回受影响的
// 审批行（有界 revokeReturnLimit），调用方在同一事务内逐条写
// approval.expired 审计（走查第 3 轮 high）。
func (s approvalStore) ExpirePendingByUser(ctx context.Context, userID, note string, now time.Time) ([]store.Approval, error) {
	rows, err := s.q.Query(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET status = 'expired', note = $2, decided_at = $3
			WHERE status = 'pending' AND requested_by = $1::uuid
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin+`
		LIMIT `+strconv.Itoa(revokeReturnLimit),
		userID, note, utc(now))
	if err != nil {
		return nil, mapError(err)
	}
	expiredRows, err := scanApprovalRows(rows, revokeReturnLimit)
	if err != nil {
		return nil, err
	}
	// tool_calls 没有 requested_by 列：经 approvals.tool_call_id 反查。
	// 只匹配本次事务刚收敛的行（decided_at = $3），避免把同一用户历史上
	// 其它原因过期的审批关联的活跃 tool_call 误伤。
	if _, err := s.q.Exec(ctx, `
		UPDATE tool_calls t
		SET status = 'expired', error_code = 'APPROVAL_CANCELLED',
		    error_message = 'approval cancelled: requester account revoked'
		FROM approvals a
		WHERE a.tool_call_id = t.id
		  AND a.requested_by = $1::uuid
		  AND a.status = 'expired'
		  AND a.note = $2
		  AND a.decided_at = $3
		  AND t.status = 'awaiting_approval'`,
		userID, note, utc(now)); err != nil {
		return nil, mapError(err)
	}
	return expiredRows, nil
}

// ExpireStaleGrants 把「已批准、未消费、grant_expires_at 已过」的 terminal
// grant 收敛为 expired（走查第 3 轮 medium）：此前只有前端按 grant_expires_at
// 临时推算「已过期」，库里的状态仍是 approved，审计页「终端授权过期」恒空。
//
// 返回受影响的行（有界 limit），调用方在同一事务内逐条写
// terminal.grant_expired 审计。已消费的行不动（消费时 grant 已用尽）。
func (s approvalStore) ExpireStaleGrants(ctx context.Context, now time.Time, limit int) ([]store.Approval, error) {
	if limit <= 0 || limit > revokeReturnLimit {
		limit = revokeReturnLimit
	}
	rows, err := s.q.Query(ctx, `
		WITH candidate AS (
			SELECT id FROM approvals
			WHERE source = 'terminal' AND status = 'approved'
			  AND consumed_at IS NULL
			  AND grant_expires_at IS NOT NULL
			  AND grant_expires_at <= $1
			ORDER BY grant_expires_at ASC, id ASC
			LIMIT `+strconv.Itoa(limit)+`
			FOR UPDATE SKIP LOCKED
		), updated AS (
			UPDATE approvals a
			SET status = 'expired', note = 'grant_expired', decided_at = $1
			FROM candidate c
			WHERE a.id = c.id
			RETURNING a.*
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin,
		utc(now))
	if err != nil {
		return nil, mapError(err)
	}
	return scanApprovalRows(rows, limit)
}

// ExpireApprovedGrantsByUser 把该用户已批准但未消费的 terminal grant 置
// expired：账号重新启用后不得凭旧审批开会话（评审 high 的 re-enable 路径）。
// decided_at 保留原批准时刻（审批事实），note 记录吊销原因。
//
// 返回受影响的行（有界 revokeReturnLimit），调用方在同一事务内逐条写
// terminal.grant_expired 审计（走查第 3 轮 high）。
func (s approvalStore) ExpireApprovedGrantsByUser(ctx context.Context, userID, note string, now time.Time) ([]store.Approval, error) {
	rows, err := s.q.Query(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET status = 'expired', note = $2
			WHERE source = 'terminal' AND status = 'approved' AND consumed_at IS NULL
			  AND requested_by = $1::uuid
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin+`
		LIMIT `+strconv.Itoa(revokeReturnLimit),
		userID, note)
	if err != nil {
		return nil, mapError(err)
	}
	return scanApprovalRows(rows, revokeReturnLimit)
}

// ApproveBreakGlass 主机侧单次批准（契约 12.3 第 11 条）：只动 pending 行，
// 置 approved + break_glass=true，decided_by 保持 NULL（没有 HTTP 用户），
// note 记录 --reason。0 行时区分不存在与已决策。
func (s approvalStore) ApproveBreakGlass(ctx context.Context, approvalID, note string, at time.Time) (store.Approval, error) {
	ap, err := scanApproval(s.q.QueryRow(ctx, `
		WITH updated AS (
			UPDATE approvals
			SET status = 'approved', decided_by = NULL, note = $2,
			    decided_at = $3, break_glass = true
			WHERE id = $1::uuid AND status = 'pending'
			RETURNING *
		)
		SELECT `+approvalColumns+` FROM updated a`+approvalJoin,
		approvalID, note, utc(at)))
	if err == nil {
		return ap, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Approval{}, err
	}
	var current store.ApprovalStatus
	checkErr := s.q.QueryRow(ctx, `SELECT status FROM approvals WHERE id = $1::uuid`, approvalID).Scan(&current)
	if checkErr != nil {
		if errors.Is(checkErr, pgx.ErrNoRows) {
			return store.Approval{}, store.ErrNotFound
		}
		return store.Approval{}, mapError(checkErr)
	}
	return store.Approval{}, store.ErrConflict
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
