package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/store"
)

// RBAC 错误码（契约 9.6）。码表由共享 apierr 维护，这里显式给状态码。
const (
	codeLastAdmin           = "LAST_ADMIN"
	codeSelfOperation       = "SELF_OPERATION"
	codeDeliveryUnavailable = "DELIVERY_UNAVAILABLE"
	codeInvitePending       = "INVITE_PENDING"
)

// errInvitePending 表示目标账号仍持有未使用的 invite token（含已过期）：
// 邀请流程尚未完成，直接启用会得到一个没有可用密码的账号，重置密码则会
// 让仍在邮箱里的邀请链接失效且账号仍无法登录。管理员应先「重发邀请」。
var errInvitePending = errors.New("httpapi: unused invite token pending")

var (
	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	emailPattern    = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
)

type createUserRequest struct {
	Username    string     `json:"username"`
	Email       string     `json:"email"`
	Role        store.Role `json:"role"`
	DisplayName string     `json:"display_name"`
}

type updateUserRequest struct {
	Role        *store.Role `json:"role"`
	Enabled     *bool       `json:"enabled"`
	DisplayName *string     `json:"display_name"`
}

// userIDParam 读取路径中的用户 id；空值按 404 处理。
func (d *Deps) userIDParam(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if strings.TrimSpace(id) == "" {
		writeError(c, apierr.NotFound, "user not found")
		return "", false
	}
	return id, true
}

// writeUserStoreError 把用户仓储错误映射为 HTTP 错误：非法 uuid 同样 404，
// 不把解析细节暴露给客户端；唯一键冲突 409；其余记录日志后 500。
//
// 必须终止于 writeError/logHandlerError，不能回调自身——未覆盖的错误若
// 递归会以栈溢出拖垮整个进程（用户管理任一 500 路径都走这里）。
func (d *Deps) writeUserStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalid):
		writeError(c, apierr.NotFound, "user not found")
	case errors.Is(err, store.ErrConflict):
		writeError(c, apierr.Conflict, "username already exists")
	case errors.Is(err, store.ErrLastAdmin):
		writeErrorStatus(c, http.StatusConflict, codeLastAdmin, "cannot remove the last enabled admin")
	default:
		d.logHandlerError(c, "user store operation failed", err)
		writeError(c, apierr.Internal, "internal server error")
	}
}

// handleListUsers 返回用户列表（limit 1..200，默认 50）。
func (d *Deps) handleListUsers(c *gin.Context) {
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	users, err := d.Store.Users().List(c.Request.Context(), limit)
	if err != nil {
		d.writeUserStoreError(c, err)
		return
	}
	if users == nil {
		users = []store.User{}
	}
	c.JSON(http.StatusOK, gin.H{"items": users})
}

// handleGetUser 返回单个用户。
func (d *Deps) handleGetUser(c *gin.Context) {
	id, ok := d.userIDParam(c)
	if !ok {
		return
	}
	user, err := d.Store.Users().ByID(c.Request.Context(), id)
	if err != nil {
		d.writeUserStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, user)
}

// handleCreateUser 创建停用用户并签发 invite token（经独立通道投递）。
//
// 响应不包含 token；无投递通道时 503 DELIVERY_UNAVAILABLE 且不创建行。
func (d *Deps) handleCreateUser(c *gin.Context) {
	var req createUserRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	if !usernamePattern.MatchString(req.Username) {
		writeError(c, apierr.ValidationFailed, "invalid username")
		return
	}
	if req.Email == "" || !emailPattern.MatchString(req.Email) {
		writeError(c, apierr.ValidationFailed, "invalid email")
		return
	}
	if !req.Role.Valid() {
		writeError(c, apierr.ValidationFailed, "invalid role")
		return
	}
	if utf8.RuneCountInString(req.DisplayName) > 100 {
		writeError(c, apierr.ValidationFailed, "display_name is too long")
		return
	}
	if d.Auth == nil || !d.Auth.CanDeliver() {
		writeErrorStatus(c, http.StatusServiceUnavailable, codeDeliveryUnavailable, "no delivery channel available")
		return
	}

	ctx := c.Request.Context()

	// 邀请用户尚无可用密码：写入随机 bcrypt 哈希（明文不落任何地方），
	// 只有兑换 invite token 后才能登录。
	raw, _, err := auth.NewSessionToken()
	if err != nil {
		d.logHandlerError(c, "create user token failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}
	hash, err := auth.HashPassword(raw)
	if err != nil {
		d.logHandlerError(c, "create user hash failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}

	actor, _ := userFromContext(c)
	createdBy := actor.ID
	var created store.User
	var auditEv audit.Event
	var outboxUsed bool
	// 用户行、invite token、SMTP 投递与审计在同一事务内完成：投递失败即
	// 回滚用户行与 token（不写 user.create），管理员可用同名重试；否则
	// 会留下一个永久占用用户名、且界面无重发/删除入口的僵尸账号（评审
	// medium，实测再次 POST 同名返回 409）。
	//
	// 同名但尚未启用、仍持有未使用 invite token（含已过期）的账号走
	// 「重发邀请」路径：作废旧链接、同步角色/展示名、重新签发并投递。
	// 过期邀请因此仍有可用出路，不会被 409 卡死。
	err = d.Store.WithTx(ctx, func(tx store.Store) error {
		existing, gerr := tx.Users().ByUsername(ctx, req.Username)
		switch {
		case gerr == nil:
			if existing.Enabled {
				return store.ErrConflict
			}
			unused, uerr := tx.Users().UnusedInviteToken(ctx, req.Username)
			if uerr != nil {
				return uerr
			}
			if !unused {
				return store.ErrConflict
			}
			now := d.Clock().UTC()
			if _, rerr := tx.Users().RevokeUnusedInvites(ctx, req.Username, now); rerr != nil {
				return rerr
			}
			displayName := existing.DisplayName
			if req.DisplayName != "" {
				displayName = req.DisplayName
			}
			// 第 9 轮走查 medium：管理员在创建表单里用同名 + 新邮箱重发
			// 邀请时，界面承诺投递到「目标邮箱」，但此前 Update 只同步
			// role/display_name，账号与投递仍用旧邮箱。这里把本次请求的
			// 邮箱一并写入，IssueInviteTokenTx 随后按更新后的行投递。
			emailChanged := req.Email != existing.Email
			updated, uerr := tx.Users().Update(ctx, existing.ID, store.UpdateUserParams{
				Role:        &req.Role,
				DisplayName: &displayName,
				Email:       &req.Email,
				UpdatedAt:   now,
			})
			if uerr != nil {
				return uerr
			}
			created = updated
			// 投递仍在事务内：失败则旧链接、角色变更一并回滚。
			if _, derr := d.Auth.IssueInviteTokenTx(ctx, tx, created, req.Role, actor.ID, d.requestIP(c)); derr != nil {
				return derr
			}
			auditEv = audit.Event{
				ActorType:    "user",
				ActorID:      actor.ID,
				Action:       "user.invite_resent",
				ResourceType: "user",
				ResourceID:   created.ID,
				Detail: map[string]any{
					"target_user_id": created.ID,
					"username":       created.Username,
					"role":           string(created.Role),
					"invite_pending": true,
					// 审计只记录「邮箱是否变化」这一事实，不落邮箱明文
					//（避免把 PII 写进审计详情）。
					"email_changed": emailChanged,
				},
			}
		case errors.Is(gerr, store.ErrNotFound):
			u, cerr := tx.Users().Create(ctx, store.CreateUserParams{
				Username:     req.Username,
				PasswordHash: hash,
				Role:         req.Role,
				Enabled:      false,
				DisplayName:  req.DisplayName,
				Email:        req.Email,
				CreatedBy:    &createdBy,
			})
			if cerr != nil {
				return cerr
			}
			created = u
			// 投递发生在事务内：失败返回 ErrDeliveryUnavailable 触发整体回滚。
			if _, derr := d.Auth.IssueInviteTokenTx(ctx, tx, created, req.Role, actor.ID, d.requestIP(c)); derr != nil {
				return derr
			}
			auditEv = audit.Event{
				ActorType:    "user",
				ActorID:      actor.ID,
				Action:       "user.create",
				ResourceType: "user",
				ResourceID:   created.ID,
				Detail: map[string]any{
					"target_user_id": created.ID,
					"username":       created.Username,
					"role":           string(created.Role),
					"invite_pending": true,
				},
			}
		default:
			return gerr
		}
		used, err := d.auditInTxCtx(c, tx, auditEv)
		outboxUsed = used
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrDeliveryUnavailable):
			// 通道不可用与投递失败分开措辞：前者管理员应先配置 SMTP，
			// 后者可原样重试（用户行已回滚，同名不会 409）。
			writeErrorStatus(c, http.StatusServiceUnavailable, codeDeliveryUnavailable,
				"invite delivery failed; the user was not created, retry is safe")
		case errors.Is(err, store.ErrConflict):
			writeError(c, apierr.Conflict, "username already exists")
		default:
			d.writeUserStoreError(c, err)
		}
		return
	}
	if !outboxUsed {
		d.auditEnqueue(c, auditEv)
	}

	created.InvitePending = true
	c.JSON(http.StatusCreated, created)
}

// handleUpdateUser 修改角色/启停/展示名。
//
// 护栏（契约 12.3 第 14/15 条）：
//   - 不能停用自己、不能改自己的 role → 409 SELF_OPERATION；
//   - 不能停用/降级最后一个启用的管理员 → 409 LAST_ADMIN（store 在同一
//     事务取 advisory lock 后判定，deferred trigger 兜底）；
//   - role/enabled 变更在同一事务内用 Sessions().DeleteByUser 吊销目标全部
//     auth session。
func (d *Deps) handleUpdateUser(c *gin.Context) {
	id, ok := d.userIDParam(c)
	if !ok {
		return
	}
	var req updateUserRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	if req.Role == nil && req.Enabled == nil && req.DisplayName == nil {
		writeError(c, apierr.ValidationFailed, "at least one field is required")
		return
	}
	if req.Role != nil && !req.Role.Valid() {
		writeError(c, apierr.ValidationFailed, "invalid role")
		return
	}
	if req.DisplayName != nil && utf8.RuneCountInString(*req.DisplayName) > 100 {
		writeError(c, apierr.ValidationFailed, "display_name is too long")
		return
	}

	ctx := c.Request.Context()
	target, err := d.Store.Users().ByID(ctx, id)
	if err != nil {
		d.writeUserStoreError(c, err)
		return
	}
	actor, _ := userFromContext(c)
	disabling := req.Enabled != nil && !*req.Enabled
	if target.ID == actor.ID && (req.Role != nil || disabling) {
		writeErrorStatus(c, http.StatusConflict, codeSelfOperation, "cannot disable yourself or change your own role")
		return
	}

	// 契约 9.7 动作名：user.update（含角色变更，详情在 changes）、
	// user.enable/user.disable。会话吊销与审计 outbox 同事务提交。
	//
	// 只有「实际变化」才吊销会话 / 闭环交互连接（第 2 轮走查 medium）：
	// 管理员在用户页多点一次保存、或并发两人对同一人提交相同 role/enabled，
	// 不应把目标账号全部会话踢下线。此前按「请求里带了这个字段」判定，
	// 已启用账号再提交 enabled=true 也会 401 掉对方的 /auth/me。
	roleChanged := req.Role != nil && *req.Role != target.Role
	enabledChanged := req.Enabled != nil && *req.Enabled != target.Enabled
	displayNameChanged := req.DisplayName != nil && *req.DisplayName != target.DisplayName
	revokeRequired := roleChanged || enabledChanged

	action := "user.update"
	detail := map[string]any{"target_user_id": id}
	changes := map[string]any{}
	if req.Role != nil {
		changes["role"] = string(*req.Role)
	}
	if req.Enabled != nil {
		changes["enabled"] = *req.Enabled
	}
	if req.DisplayName != nil {
		changes["display_name"] = *req.DisplayName
	}
	detail["changes"] = changes
	if req.Role != nil && *req.Role != target.Role {
		detail["from_role"] = string(target.Role)
		detail["to_role"] = string(*req.Role)
	}
	if req.Enabled != nil && *req.Enabled != target.Enabled {
		if *req.Enabled {
			action = "user.enable"
		} else {
			action = "user.disable"
			detail["via"] = "PATCH"
		}
		detail["enabled"] = *req.Enabled
	}
	if req.DisplayName != nil {
		detail["display_name_changed"] = displayNameChanged
	}
	// 「请求里带了字段」与「值真的变了」是两件事：重复提交同值不应吊销会话
	//（第 2 轮走查 medium），审计里也据此区分空操作。
	//
	// 第 4 轮走查 low：changed 只表示权限类变更（role/enabled）；仅改展示名
	// 必须为 false（契约 9.7「仅展示名更新或重复提交同值时为 false」），
	// 展示名变化由 display_name_changed 单独表达，审计员才能用 changed 区分
	// 「权限变更」与「改昵称」。
	detail["changed"] = roleChanged || enabledChanged
	auditEv := audit.Event{
		ActorType:    "user",
		ActorID:      actor.ID,
		Action:       action,
		ResourceType: "user",
		ResourceID:   id,
		Detail:       detail,
	}

	now := d.Clock().UTC()
	var updated store.User
	outboxUsed := false
	err = d.Store.WithTx(ctx, func(tx store.Store) error {
		// 待接受邀请（含邀请已过期）的停用账号没有可用密码：直接启用会得到
		// 一个无法登录的账号，且仍留在邮箱里的邀请链接也随之失效。必须由
		// 管理员先「重发邀请」（POST /api/users 同名重发），走接受流程。
		if enabledChanged && *req.Enabled && !target.Enabled {
			unused, uerr := tx.Users().UnusedInviteToken(ctx, target.Username)
			if uerr != nil {
				return uerr
			}
			if unused {
				return errInvitePending
			}
		}
		u, err := tx.Users().Update(ctx, id, store.UpdateUserParams{
			Role:        req.Role,
			Enabled:     req.Enabled,
			DisplayName: req.DisplayName,
			UpdatedAt:   now,
		})
		if err != nil {
			return err
		}
		updated = u
		// 角色变更/停用立即吊销目标全部 auth session（含当前会话），并在
		// 同一事务收敛其 pending 审批、awaiting_approval tool_call、已批准
		// 未消费的 terminal grant 与 batch awaiting_approval 台——否则账号
		// 重新启用后旧审批仍可被批准并触发执行（评审 high）。
		// 仅展示名变更、或提交与现状相同的 role/enabled 不吊销。
		if revokeRequired {
			if err := tx.Sessions().DeleteByUser(ctx, id); err != nil {
				return err
			}
			if err := revokePendingInTx(ctx, tx, id, now); err != nil {
				return err
			}
		}
		used, err := d.auditInTxCtx(c, tx, auditEv)
		outboxUsed = used
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, errInvitePending):
			writeErrorStatus(c, http.StatusConflict, codeInvitePending,
				"user still has an unused invitation; resend the invitation instead")
		case errors.Is(err, store.ErrLastAdmin):
			writeErrorStatus(c, http.StatusConflict, codeLastAdmin, "cannot remove the last enabled admin")
		case errors.Is(err, store.ErrNotFound):
			writeError(c, apierr.NotFound, "user not found")
		case errors.Is(err, store.ErrConflict):
			writeError(c, apierr.Conflict, "resource conflict")
		case errors.Is(err, store.ErrInvalid):
			writeError(c, apierr.NotFound, "user not found")
		default:
			d.logHandlerError(c, "update user failed", err)
			writeError(c, apierr.Internal, "internal server error")
		}
		return
	}

	// 停用/改角色后立即断开已建立的交互 WS 与 SSE，并唤醒等待中的审批方
	//（评审发现：仅删 auth session 不足以让已有 shell 停止执行）。
	// 与事务内吊销同口径：只在 role/enabled 实际变化时执行。
	if revokeRequired {
		d.revokeAfterCommit(ctx, id)
	}
	if !outboxUsed {
		d.auditEnqueue(c, auditEv)
	}
	c.JSON(http.StatusOK, updated)
}

// handleResetUserPassword 签发一次性 reset token 并吊销目标全部会话。
//
// 不接收新密码、不在响应中返回 token；管理员不知道最终密码。
// 吊销方式：Sessions().DeleteByUser(target.ID)（先于签发执行，投递失败也
// 不会留下仍然有效的旧会话）。
func (d *Deps) handleResetUserPassword(c *gin.Context) {
	id, ok := d.userIDParam(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	target, err := d.Store.Users().ByID(ctx, id)
	if err != nil {
		d.writeUserStoreError(c, err)
		return
	}
	actor, _ := userFromContext(c)
	if target.ID == actor.ID {
		writeErrorStatus(c, http.StatusConflict, codeSelfOperation, "cannot reset your own password")
		return
	}
	if d.Auth == nil || !d.Auth.CanDeliver() {
		writeErrorStatus(c, http.StatusServiceUnavailable, codeDeliveryUnavailable, "no delivery channel available")
		return
	}

	// 先投递再吊销，且两者与审计在同一事务：投递失败（SMTP 断连等）时
	// 整体回滚——token 不落库、会话不吊销、审计不写，管理员看到的 503
	// 与真实状态一致（评审 medium：此前先吊销会话再投递，503 时用户已被
	// 踢下线而管理员以为没生效）。expires_at 在写 outbox 之前就已确定，
	// 审计页可据 detail 复原令牌有效期。
	var token store.UserToken
	var outboxUsed bool
	resetAudit := audit.Event{
		ActorType:    "user",
		ActorID:      actor.ID,
		Action:       "user.credential_reset",
		ResourceType: "user",
		ResourceID:   target.ID,
		Detail:       map[string]any{"target_user_id": target.ID, "action": "reset"},
	}
	// 契约 9.7：reset 与 revoke-sessions 一样会吊销目标全部会话，必须
	// 同时写 user.sessions_revoked（detail 带 action=reset），否则审计员
	// 按「吊销用户会话」筛选/检索时看不到这次踢下线（第 9 轮走查 low）。
	// detail 不含 token/密码，changes 只表达「会话被吊销」。
	revokeAudit := audit.Event{
		ActorType:    "user",
		ActorID:      actor.ID,
		Action:       "user.sessions_revoked",
		ResourceType: "user",
		ResourceID:   target.ID,
		Detail: map[string]any{
			"target_user_id": target.ID,
			"action":         "reset",
			"changes":        map[string]any{"sessions": "revoked"},
		},
	}
	if err := d.Store.WithTx(ctx, func(tx store.Store) error {
		// 待接受邀请（含邀请已过期）的账号还没有可用密码：重置密码只换密码，
		// acceptReset 不会把 enabled 置 true，管理员会看到 202「已发送」但
		// 目标接受后仍无法登录；同时旧邀请链接立即失效。明确拒绝并提示
		// 重发邀请，避免把用户引到死路。
		if !target.Enabled {
			unused, uerr := tx.Users().UnusedInviteToken(ctx, target.Username)
			if uerr != nil {
				return uerr
			}
			if unused {
				return errInvitePending
			}
		}
		tok, terr := d.Auth.IssueResetTokenTx(ctx, tx, target, actor.ID, d.requestIP(c))
		if terr != nil {
			return terr
		}
		token = tok
		resetAudit.Detail["expires_at"] = tok.ExpiresAt.UTC().Format(time.RFC3339)
		if err := tx.Sessions().DeleteByUser(ctx, target.ID); err != nil {
			return err
		}
		if err := revokePendingInTx(ctx, tx, target.ID, d.Clock().UTC()); err != nil {
			return err
		}
		used, aerr := d.auditInTxCtx(c, tx, resetAudit)
		if aerr != nil {
			return aerr
		}
		if used {
			// outbox 模式下两条审计必须与业务行同事务；任一失败整体回滚。
			if _, aerr := d.auditInTxCtx(c, tx, revokeAudit); aerr != nil {
				return aerr
			}
		}
		outboxUsed = used
		return nil
	}); err != nil {
		switch {
		case errors.Is(err, errInvitePending):
			writeErrorStatus(c, http.StatusConflict, codeInvitePending,
				"user still has an unused invitation; resend the invitation instead")
		case errors.Is(err, auth.ErrDeliveryUnavailable):
			writeErrorStatus(c, http.StatusServiceUnavailable, codeDeliveryUnavailable,
				"reset delivery failed; no session was revoked, retry is safe")
		case errors.Is(err, store.ErrNotFound):
			writeError(c, apierr.NotFound, "user not found")
		default:
			d.logHandlerError(c, "issue reset token failed", err)
			writeError(c, apierr.Internal, "internal server error")
		}
		return
	}
	d.revokeAfterCommit(ctx, target.ID)

	if !outboxUsed {
		d.auditEnqueue(c, resetAudit)
		d.auditEnqueue(c, revokeAudit)
	}
	c.JSON(http.StatusAccepted, gin.H{
		"user_id":    target.ID,
		"expires_at": token.ExpiresAt,
		"delivered":  true,
	})
}

// handleRevokeUserSessions 吊销目标全部 auth session。
func (d *Deps) handleRevokeUserSessions(c *gin.Context) {
	id, ok := d.userIDParam(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	target, err := d.Store.Users().ByID(ctx, id)
	if err != nil {
		d.writeUserStoreError(c, err)
		return
	}
	actor, _ := userFromContext(c)
	auditEv := audit.Event{
		ActorType:    "user",
		ActorID:      actor.ID,
		Action:       "user.sessions_revoked",
		ResourceType: "user",
		ResourceID:   target.ID,
		Detail:       map[string]any{"target_user_id": target.ID},
	}
	outboxUsed := false
	if err := d.Store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Sessions().DeleteByUser(ctx, target.ID); err != nil {
			return err
		}
		if err := revokePendingInTx(ctx, tx, target.ID, d.Clock().UTC()); err != nil {
			return err
		}
		used, err := d.auditInTxCtx(c, tx, auditEv)
		outboxUsed = used
		return err
	}); err != nil {
		d.logHandlerError(c, "revoke user sessions failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}
	d.revokeAfterCommit(ctx, target.ID)
	if !outboxUsed {
		d.auditEnqueue(c, auditEv)
	}
	c.Status(http.StatusNoContent)
}
