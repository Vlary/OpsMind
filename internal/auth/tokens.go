package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// IssueInviteToken 为已创建但未启用的用户签发一次性 invite token 并投递。
//
// invite 行按迁移 00012 的 CHECK 要求 user_id 为 NULL，带 username/email/role；
// 接受时按 username 找到已存在的停用行并启用。token 明文只交给投递通道。
func (s *Service) IssueInviteToken(ctx context.Context, user store.User, role store.Role, creatorID, creatorIP string) (store.UserToken, error) {
	return s.IssueInviteTokenTx(ctx, s.st, user, role, creatorID, creatorIP)
}

// IssueInviteTokenTx 在调用方事务内创建 invite token 并投递：投递失败时
// 调用方回滚整个事务（用户行/token/审计一并撤销），管理员可用同名重试
// （评审 medium：此前 token 在 tx 外签发，投递失败留下永久占用用户名）。
func (s *Service) IssueInviteTokenTx(ctx context.Context, tx store.Store, user store.User, role store.Role, creatorID, creatorIP string) (store.UserToken, error) {
	if !s.CanDeliver() {
		return store.UserToken{}, ErrDeliveryUnavailable
	}
	if !role.Valid() {
		return store.UserToken{}, store.ErrInvalid
	}
	if user.Username == "" || user.Email == "" {
		return store.UserToken{}, store.ErrInvalid
	}
	raw, hash, err := NewSessionToken()
	if err != nil {
		return store.UserToken{}, err
	}
	now := s.cfg.Clock().UTC()
	expires := now.Add(OneTimeTokenTTL)
	token, err := tx.Users().CreateToken(ctx, store.CreateUserTokenParams{
		Kind:             store.UserTokenInvite,
		Username:         user.Username,
		Email:            user.Email,
		Role:             &role,
		TokenHash:        hash,
		ExpiresAt:        expires,
		CreatedBy:        optionalString(creatorID),
		CreatorIP:        creatorIP,
		DeliveredChannel: "smtp",
		CreatedAt:        now,
	})
	if err != nil {
		return store.UserToken{}, err
	}
	if err := s.DeliverToken(ctx, store.UserTokenInvite, user.Email, user.Username, raw, expires); err != nil {
		return store.UserToken{}, err
	}
	return token, nil
}

// IssueResetToken 为已存在用户签发一次性 reset token 并投递。
// 会话吊销由调用方在同一事务内完成（Users().UpdatePassword 之外另行
// Sessions().DeleteByUser），本方法只负责 token。
func (s *Service) IssueResetToken(ctx context.Context, user store.User, creatorID, creatorIP string) (store.UserToken, error) {
	return s.IssueResetTokenTx(ctx, s.st, user, creatorID, creatorIP)
}

// IssueResetTokenTx 在调用方事务内创建 reset token 并投递（失败整体回滚，
// 调用方据此把「吊销会话」放在投递成功之后，避免 503 却已踢人下线）。
func (s *Service) IssueResetTokenTx(ctx context.Context, tx store.Store, user store.User, creatorID, creatorIP string) (store.UserToken, error) {
	if !s.CanDeliver() {
		return store.UserToken{}, ErrDeliveryUnavailable
	}
	if user.ID == "" || user.Email == "" {
		return store.UserToken{}, ErrDeliveryUnavailable
	}
	raw, hash, err := NewSessionToken()
	if err != nil {
		return store.UserToken{}, err
	}
	now := s.cfg.Clock().UTC()
	expires := now.Add(OneTimeTokenTTL)
	token, err := tx.Users().CreateToken(ctx, store.CreateUserTokenParams{
		Kind:             store.UserTokenReset,
		UserID:           &user.ID,
		Username:         user.Username,
		Email:            user.Email,
		TokenHash:        hash,
		ExpiresAt:        expires,
		CreatedBy:        optionalString(creatorID),
		CreatorIP:        creatorIP,
		DeliveredChannel: "smtp",
		CreatedAt:        now,
	})
	if err != nil {
		return store.UserToken{}, err
	}
	if err := s.DeliverToken(ctx, store.UserTokenReset, user.Email, user.Username, raw, expires); err != nil {
		return store.UserToken{}, err
	}
	return token, nil
}

// AcceptToken 兑换一次性 invite/reset token 并设置新密码（不写审计）。
//
// 保留给离线工具/单测等无 HTTP 上下文的调用方；HTTP 路径必须使用
// AcceptTokenAudited，让审计与 token/用户变更同事务提交。
//
// invite：激活已存在的停用用户（或补建缺失行）；reset：重置已存在用户密码。
// 两种兑换都会吊销该用户全部 auth session。创建者 IP 兑换返回 ErrSelfRedeem。
func (s *Service) AcceptToken(ctx context.Context, rawToken, newPassword, displayName, ip string) (store.UserTokenKind, string, error) {
	kind, userID, _, err := s.AcceptTokenAudited(ctx, rawToken, newPassword, displayName, ip, nil)
	return kind, userID, err
}

// AcceptTokenAudited 与 AcceptToken 相同，并在改动 token/users/sessions 的
// 同一事务内写审计 outbox（契约 12.3 第 19 条）。
//
// auditEvent 由调用方构造（HTTP 层补齐 action/request_id/IP），在用户行
// 生效、token 标记已使用之后、事务提交之前调用；受邀角色可能是 admin，
// 因此「谁在何时授权了这次访问」必须与业务行同提交——客户端取消请求或
// 进程崩溃都不能丢。返回 auditInTx=false 表示存储未实现 outbox（内存
// fake），调用方应在提交后回落到进程内 audit.Enqueue。
func (s *Service) AcceptTokenAudited(ctx context.Context, rawToken, newPassword, displayName, ip string, auditEvent func(kind store.UserTokenKind, userID string) audit.Event) (store.UserTokenKind, string, bool, error) {
	if len(newPassword) == 0 || len(newPassword) > maxPasswordBytes {
		return "", "", false, ErrPasswordInvalid
	}
	if strings.TrimSpace(rawToken) == "" {
		return "", "", false, ErrInvalidToken
	}
	token, err := s.st.Users().TokenByHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", false, ErrInvalidToken
		}
		return "", "", false, err
	}
	now := s.cfg.Clock().UTC()
	if token.UsedAt != nil || !token.ExpiresAt.After(now) {
		return "", "", false, ErrInvalidToken
	}
	if ip != "" && token.CreatorIP != "" && ip == token.CreatorIP {
		return "", "", false, ErrSelfRedeem
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return "", "", false, err
	}
	mustChange := false

	var userID string
	auditInTx := false
	err = s.st.WithTx(ctx, func(tx store.Store) error {
		used, err := tx.Users().MarkTokenUsed(ctx, token.ID, now)
		if err != nil {
			return err
		}
		if !used {
			return ErrInvalidToken
		}
		switch token.Kind {
		case store.UserTokenInvite:
			userID, err = s.acceptInvite(ctx, tx, token, hash, displayName, now)
		case store.UserTokenReset:
			userID, err = s.acceptReset(ctx, tx, token, hash, &mustChange, now)
		default:
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if auditEvent != nil {
			used, aerr := emitAuditTx(ctx, tx, auditEvent(token.Kind, userID), now)
			auditInTx = used
			return aerr
		}
		return nil
	})
	if err != nil {
		return "", "", false, err
	}
	return token.Kind, userID, auditInTx, nil
}

func (s *Service) acceptInvite(ctx context.Context, tx store.Store, token store.UserToken, hash, displayName string, now time.Time) (string, error) {
	if token.Role == nil || !token.Role.Valid() {
		return "", ErrInvalidToken
	}
	existing, err := tx.Users().ByUsername(ctx, token.Username)
	switch {
	case err == nil:
		// 已存在的行必须处于停用态（邀请未接受）；已启用说明 token 是重放的。
		if existing.Enabled {
			return "", ErrInvalidToken
		}
		if displayName == "" {
			displayName = existing.DisplayName
		}
		enabled := true
		updated, err := tx.Users().Update(ctx, existing.ID, store.UpdateUserParams{
			Enabled:     &enabled,
			DisplayName: &displayName,
			UpdatedAt:   now,
		})
		if err != nil {
			return "", err
		}
		if err := tx.Users().UpdatePassword(ctx, updated.ID, store.UpdatePasswordParams{
			PasswordHash:       hash,
			MustChangePassword: boolPtr(false),
			UpdatedAt:          now,
		}); err != nil {
			return "", err
		}
		if err := tx.Sessions().DeleteByUser(ctx, updated.ID); err != nil {
			return "", err
		}
		return updated.ID, nil
	case errors.Is(err, store.ErrNotFound):
		if displayName == "" {
			displayName = token.Username
		}
		created, err := tx.Users().Create(ctx, store.CreateUserParams{
			Username:     token.Username,
			PasswordHash: hash,
			Role:         *token.Role,
			Enabled:      true,
			DisplayName:  displayName,
			Email:        token.Email,
			CreatedBy:    token.CreatedBy,
		})
		if err != nil {
			return "", err
		}
		if err := tx.Sessions().DeleteByUser(ctx, created.ID); err != nil {
			return "", err
		}
		return created.ID, nil
	default:
		return "", err
	}
}

// acceptReset 重置密码并返回目标用户 id（审计 ActorID/ResourceID 需要；
// 此前返回空串导致 reset 兑换的审计无法定位账号）。
func (s *Service) acceptReset(ctx context.Context, tx store.Store, token store.UserToken, hash string, mustChange *bool, now time.Time) (string, error) {
	if token.UserID == nil || *token.UserID == "" {
		return "", ErrInvalidToken
	}
	user, err := tx.Users().ByID(ctx, *token.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrInvalidToken
		}
		return "", err
	}
	if err := tx.Users().UpdatePassword(ctx, user.ID, store.UpdatePasswordParams{
		PasswordHash:       hash,
		MustChangePassword: mustChange,
		UpdatedAt:          now,
	}); err != nil {
		return "", err
	}
	if err := tx.Sessions().DeleteByUser(ctx, user.ID); err != nil {
		return "", err
	}
	return user.ID, nil
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolPtr(v bool) *bool { return &v }
