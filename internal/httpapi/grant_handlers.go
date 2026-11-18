// 00012：JIT 终端/RDP 单次 grant（契约 12.3 第 12 条 / 12.4 第 4 条）。
//
// 流程：POST /api/term|rdp/grants 创建 source=terminal 审批（grant_kind +
// 30 分钟 grant 有效期）→ 有 approval:decide 的他人经
// POST /api/approvals/:id/decide 批准 → POST /api/term|rdp/sessions 携带
// grant_id，在创建会话前原子消费（approved + 未消费 + 未过期 +
// requested_by=本人 + server_id/grant_kind 匹配）。
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/authz"
	"aiops/internal/store"
)

// terminalGrantTTL 是 grant 的有效期（契约 12.5：now()+30m）。
const terminalGrantTTL = 30 * time.Minute

type grantCreateRequest struct {
	ServerID string `json:"server_id"`
}

func (d *Deps) handleCreateTermGrant(c *gin.Context) {
	d.createTerminalGrant(c, "term")
}

func (d *Deps) handleCreateRdpGrant(c *gin.Context) {
	d.createTerminalGrant(c, "rdp")
}

// createTerminalGrant 为当前用户与目标服务器创建待批准的 JIT grant。
func (d *Deps) createTerminalGrant(c *gin.Context, kind string) {
	if !d.requireStore(c) {
		return
	}
	var req grantCreateRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	if _, err := uuid.Parse(req.ServerID); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "server_id must be a uuid")
		return
	}
	server, err := d.Store.Servers().Get(c.Request.Context(), req.ServerID)
	if err != nil {
		d.handleStoreError(c, err, apierr.ServerNotFound)
		return
	}
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	// 契约 12.3 第 11 条：单管理员降级模式不得申请 JIT 终端/RDP 授权。
	if !d.requireSecondApprover(c, authz.PermTerminal, "terminal.grant") {
		return
	}
	if d.Approvals == nil {
		writeErrorStatus(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "approval service unavailable")
		return
	}
	grantKind := kind
	serverID := server.ID
	expires := d.Clock().UTC().Add(terminalGrantTTL)
	ap, err := d.Approvals.Create(c.Request.Context(), approvals.CreateRequest{
		Source:         "terminal",
		ServerID:       &serverID,
		ServerName:     server.Name,
		Command:        "",
		RiskLevel:      "safe",
		Reason:         "jit " + kind + " grant",
		ApprovalMode:   string(store.ApprovalModeStandard),
		RequestID:      c.GetString("request_id"),
		RequestedBy:    user.ID,
		GrantKind:      &grantKind,
		GrantExpiresAt: &expires,
		ExpiresAt:      expires,
	})
	if err != nil {
		d.logHandlerError(c, "create terminal grant failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"approval": ap,
		"grant": gin.H{
			"approval_id":      ap.ID,
			"grant_kind":       kind,
			"server_id":        server.ID,
			"grant_expires_at": expires,
		},
	})
}

// terminalGrantReleaseTimeout 是回滚消费的 DB 操作上限（客户端断开后仍要
// 尽力释放，避免授权因一次失败的 Open 永久不可用）。
const terminalGrantReleaseTimeout = 3 * time.Second

// terminalGrantAuditDetail 是 grant 生命周期审计的 detail（契约 9.7 口径）。
func terminalGrantAuditDetail(ap store.Approval, kind string, at time.Time) map[string]any {
	detail := map[string]any{
		"approval_id":  ap.ID,
		"grant_kind":   kind,
		"requested_by": ap.RequestedBy,
	}
	if ap.ServerID != nil {
		detail["server_id"] = *ap.ServerID
	}
	if ap.ConsumedAt != nil {
		detail["consumed_at"] = ap.ConsumedAt.UTC().Format(time.RFC3339)
	} else if !at.IsZero() {
		detail["consumed_at"] = at.UTC().Format(time.RFC3339)
	}
	return detail
}

// consumeTerminalGrant 校验并单次消费 grant；失败已写响应并返回 false。
// 成功返回被消费的审批行（ConsumedAt 非空），供调用方在会话 Open 失败时
// 回滚（第 4 轮走查 medium：此前消费先于 Open，409 会白白烧掉授权）。
func (d *Deps) consumeTerminalGrant(c *gin.Context, grantID, serverID, kind string) (store.Approval, bool) {
	// grantID 为空表示消费该用户在该服务器上最新一条已批准 grant；
	// 非空时必须是合法 uuid（前端申请-审批后可直接开会话）。
	if grantID != "" {
		if _, err := uuid.Parse(grantID); err != nil {
			writeErrorStatus(c, http.StatusForbidden, "TERMINAL_APPROVAL_REQUIRED",
				"an approved terminal grant is required")
			return store.Approval{}, false
		}
	}
	grants := store.GrantStoreOf(d.Store)
	if grants == nil {
		writeErrorStatus(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable,
			"terminal grant store unavailable")
		return store.Approval{}, false
	}
	user, _ := userFromContext(c)
	now := d.Clock().UTC()
	// 消费与 terminal.grant_consumed 审计在同一事务内提交（契约 12.3 第 19
	// 条）：消费成功但进程在入队前崩溃不再丢这条审计。ConsumeGrant 自身是
	// 单条 CAS UPDATE，放进事务后语义不变。
	var ap store.Approval
	outboxUsed := false
	consume := func(tx store.Store) error {
		txGrants := store.GrantStoreOf(tx)
		if txGrants == nil {
			return store.ErrGrantUnavailable
		}
		consumed, cerr := txGrants.ConsumeGrant(c.Request.Context(), store.ConsumeGrantParams{
			ApprovalID: grantID,
			UserID:     user.ID,
			ServerID:   serverID,
			GrantKind:  kind,
			Now:        now,
		})
		if cerr != nil {
			return cerr
		}
		ap = consumed
		sid := serverID
		used, aerr := d.auditInTxCtx(c, tx, audit.Event{
			ActorType:    "user",
			ActorID:      user.ID,
			Action:       "terminal.grant_consumed",
			ResourceType: "approval",
			ResourceID:   consumed.ID,
			ServerID:     &sid,
			Detail:       terminalGrantAuditDetail(consumed, kind, now),
		})
		outboxUsed = used
		return aerr
	}
	var err error
	if store.OutboxOf(d.Store) != nil {
		err = d.Store.WithTx(c.Request.Context(), consume)
	} else {
		err = consume(d.Store)
	}
	switch {
	case err == nil:
		if !outboxUsed {
			sid := serverID
			d.auditEnqueue(c, audit.Event{
				ActorType:    "user",
				ActorID:      user.ID,
				Action:       "terminal.grant_consumed",
				ResourceType: "approval",
				ResourceID:   ap.ID,
				ServerID:     &sid,
				Detail:       terminalGrantAuditDetail(ap, kind, now),
			})
		}
		return ap, true
	case errors.Is(err, store.ErrGrantKindMismatch):
		writeErrorStatus(c, http.StatusForbidden, "TERMINAL_GRANT_KIND_MISMATCH",
			"grant kind does not match the endpoint")
		return store.Approval{}, false
	case errors.Is(err, store.ErrGrantUnavailable), errors.Is(err, store.ErrNotFound):
		writeErrorStatus(c, http.StatusForbidden, "TERMINAL_APPROVAL_REQUIRED",
			"an approved terminal grant is required")
		return store.Approval{}, false
	default:
		d.logHandlerError(c, "consume terminal grant failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return store.Approval{}, false
	}
}

// releaseTerminalGrant 回滚本次请求刚消费、但因会话 Open 失败而未使用的
// 授权（第 4 轮走查 medium）：清空 consumed_at 并写 terminal.grant_released
// 审计，授权保留可重试，避免运维为了一个「已有活动会话」的 409 重新找
// 审批人。使用独立超时 ctx（客户端断开也要尽力释放）。
//
// 回滚失败只记日志/不回滚响应：授权保持已消费是安全方向（fail-closed），
// 但审计里要能看出这次消费没有被会话使用。
func (d *Deps) releaseTerminalGrant(c *gin.Context, ap store.Approval, kind string) {
	user, _ := userFromContext(c)
	d.releaseTerminalGrantForUser(c, ap, kind, user.ID)
}

// releaseTerminalGrantByID 用于 WS 拨号失败路径（第 10 轮走查 medium）：
// WS handler 自行完成 Cookie 鉴权，gin 上下文没有 requireAuth 注入的 user；
// 会话行持久化了本次消费的 approval id，这里用独立超时 ctx 读回审批行
// （取得 consumed_at）再走与同步 Open 失败相同的回滚 + 审计事务。
//
// 读/回滚失败只记日志：授权保持已消费是安全方向（fail-closed），但审计员
// 可从日志与缺失的 terminal.grant_released 看出本次消费未被会话使用。
func (d *Deps) releaseTerminalGrantByID(c *gin.Context, approvalID, userID, kind string) {
	if approvalID == "" || d.Store == nil || d.Store.Approvals() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), terminalGrantReleaseTimeout)
	defer cancel()
	ap, err := d.Store.Approvals().Get(ctx, approvalID)
	if err != nil {
		d.logHandlerError(c, "load consumed terminal grant failed", err)
		return
	}
	d.releaseTerminalGrantForUser(c, ap, kind, userID)
}

// releaseTerminalGrantForUser 是回滚的公共实现；userID 是已鉴权发起人
// （ReleaseGrant 在 SQL 内 CAS requested_by，传错只会释放失败）。
func (d *Deps) releaseTerminalGrantForUser(c *gin.Context, ap store.Approval, kind, userID string) {
	if ap.ID == "" || ap.ConsumedAt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), terminalGrantReleaseTimeout)
	defer cancel()
	var released store.Approval
	outboxUsed := false
	release := func(tx store.Store) error {
		txGrants := store.GrantStoreOf(tx)
		if txGrants == nil {
			return store.ErrGrantUnavailable
		}
		row, rerr := txGrants.ReleaseGrant(ctx, store.ReleaseGrantParams{
			ApprovalID: ap.ID,
			UserID:     userID,
			ConsumedAt: *ap.ConsumedAt,
		})
		if rerr != nil {
			return rerr
		}
		released = row
		var sid *string
		if ap.ServerID != nil {
			value := *ap.ServerID
			sid = &value
		}
		used, aerr := d.auditInTx(ctx, tx, audit.Event{
			ActorType:    "user",
			ActorID:      userID,
			Action:       "terminal.grant_released",
			ResourceType: "approval",
			ResourceID:   ap.ID,
			ServerID:     sid,
			RequestID:    c.GetString("request_id"),
			IP:           d.requestIP(c),
			Detail:       terminalGrantAuditDetail(ap, kind, time.Time{}),
		})
		outboxUsed = used
		return aerr
	}
	var err error
	if store.OutboxOf(d.Store) != nil {
		err = d.Store.WithTx(ctx, release)
	} else {
		err = release(d.Store)
	}
	if err != nil {
		d.logHandlerError(c, "release terminal grant failed", err)
		return
	}
	if !outboxUsed && d.Audit != nil {
		// 客户端可能已断开：用独立的 ctx 投递进程内审计（与 outbox 路径
		// 同 payload），避免「状态回滚了但审计丢在断开的请求 ctx 上」。
		_ = d.Audit.Enqueue(ctx, audit.Event{
			ActorType:    "user",
			ActorID:      userID,
			Action:       "terminal.grant_released",
			ResourceType: "approval",
			ResourceID:   released.ID,
			RequestID:    c.GetString("request_id"),
			IP:           d.requestIP(c),
			Detail:       terminalGrantAuditDetail(ap, kind, time.Time{}),
		})
	}
}

// availableGrantPage 是「本人可用 JIT 授权」响应（第 2 轮走查 low）：
// 发起人打开终端/桌面前先看到已批准、未消费、未过期的 grant。
type availableGrantPage struct {
	Items []store.Approval `json:"items"`
	Count int              `json:"count"`
}

// handleListTermGrants 返回当前用户可用的 term 授权。
func (d *Deps) handleListTermGrants(c *gin.Context) {
	d.listAvailableGrants(c, "term")
}

// handleListRdpGrants 返回当前用户可用的 rdp 授权。
func (d *Deps) handleListRdpGrants(c *gin.Context) {
	d.listAvailableGrants(c, "rdp")
}

// listAvailableGrants 只返回 requested_by=当前用户的 approved 且未消费、
// 未过期的 grant。他人授权即便 audit:read 也不可见（授权是个人凭据）。
//
// 有界：limit 1..200（缺省 50），store 单页上限同值，不逐条二次查询。
func (d *Deps) listAvailableGrants(c *gin.Context, kind string) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	grants := store.GrantStoreOf(d.Store)
	if grants == nil {
		writeErrorStatus(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable,
			"terminal grant store unavailable")
		return
	}
	items, err := grants.ListAvailableGrants(c.Request.Context(), store.ListAvailableGrantsParams{
		UserID:    user.ID,
		GrantKind: kind,
		Now:       d.Clock().UTC(),
		Limit:     limit,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.Approval{}
	}
	c.JSON(http.StatusOK, availableGrantPage{Items: items, Count: len(items)})
}
