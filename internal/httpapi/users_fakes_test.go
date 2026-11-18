package httpapi

import (
	"context"
	"time"

	"aiops/internal/store"
)

// 本文件为 memUsers 补齐 RBAC 用户仓储方法，避免改动共享的 fakes_test.go。
// 仅测试构建；生产实现见 internal/store/postgres/users.go。

// memUserTokens 是单个内存 store 的 token 表上限（写死，防 OOM）；超限
// 整体清空。token 存在 memStore 内，因此能随 WithTx 快照回滚、不串测试。
const memUserTokens = 512

func (r *memUsers) List(_ context.Context, limit int) ([]store.User, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := make([]store.User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, u)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *memUsers) Update(_ context.Context, id string, p store.UpdateUserParams) (store.User, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.users {
		if u.ID != id {
			continue
		}
		if p.Role != nil {
			u.Role = *p.Role
		}
		if p.Enabled != nil {
			u.Enabled = *p.Enabled
		}
		if p.DisplayName != nil {
			u.DisplayName = *p.DisplayName
		}
		if p.Email != nil {
			u.Email = *p.Email
		}
		u.UpdatedAt = p.UpdatedAt
		m.users[name] = u
		return u, nil
	}
	return store.User{}, store.ErrNotFound
}

func (r *memUsers) CountEnabledAdmins(context.Context) (int64, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, u := range m.users {
		if u.Role == store.RoleAdmin && u.Enabled {
			n++
		}
	}
	return n, nil
}

func (r *memUsers) CountEnabledApprovers(context.Context) (int64, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, u := range m.users {
		if u.Enabled && (u.Role == store.RoleAdmin || u.Role == store.RoleApprover) {
			n++
		}
	}
	return n, nil
}

func (r *memUsers) SetMustChangePassword(_ context.Context, id string, mustChange bool, at time.Time) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.users {
		if u.ID == id {
			u.MustChangePassword = mustChange
			u.UpdatedAt = at
			m.users[name] = u
			return nil
		}
	}
	return store.ErrNotFound
}

func (r *memUsers) UpdateLastLogin(_ context.Context, id string, at time.Time) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.users {
		if u.ID == id {
			t := at
			u.LastLoginAt = &t
			m.users[name] = u
			return nil
		}
	}
	return store.ErrNotFound
}

func (r *memUsers) CreateToken(_ context.Context, p store.CreateUserTokenParams) (store.UserToken, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID("token")

	now := p.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	token := store.UserToken{
		ID:               id,
		Kind:             p.Kind,
		UserID:           p.UserID,
		Username:         p.Username,
		Email:            p.Email,
		Role:             p.Role,
		ExpiresAt:        p.ExpiresAt,
		CreatedBy:        p.CreatedBy,
		CreatorIP:        p.CreatorIP,
		DeliveredChannel: p.DeliveredChannel,
		CreatedAt:        now,
	}

	if len(m.tokens) >= memUserTokens {
		m.tokens = make(map[string]store.UserToken, 64)
		m.tokenHashes = make(map[string]string, 64)
	}
	m.tokens[id] = token
	m.tokenHashes[string(p.TokenHash)] = id
	return token, nil
}

func (r *memUsers) TokenByHash(_ context.Context, hash []byte) (store.UserToken, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.tokenHashes[string(hash)]
	if !ok {
		return store.UserToken{}, store.ErrNotFound
	}
	return m.tokens[id], nil
}

func (r *memUsers) MarkTokenUsed(_ context.Context, id string, at time.Time) (bool, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.tokens[id]
	if !ok || token.UsedAt != nil {
		return false, nil
	}
	t := at
	token.UsedAt = &t
	m.tokens[id] = token
	return true, nil
}

// PendingInvite 与生产实现同口径：未使用且未过期。
func (r *memUsers) PendingInvite(_ context.Context, username string) (bool, error) {
	return r.hasUnusedInvite(username, true)
}

// UnusedInviteToken 包含已过期 invite——过期邀请同样没有留下可用密码。
func (r *memUsers) UnusedInviteToken(_ context.Context, username string) (bool, error) {
	return r.hasUnusedInvite(username, false)
}

func (r *memUsers) hasUnusedInvite(username string, unexpiredOnly bool) (bool, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, token := range m.tokens {
		if token.Kind != store.UserTokenInvite || token.UsedAt != nil || token.Username != username {
			continue
		}
		if unexpiredOnly && !token.ExpiresAt.After(now) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (r *memUsers) RevokeUnusedInvites(_ context.Context, username string, at time.Time) (int64, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, token := range m.tokens {
		if token.Kind != store.UserTokenInvite || token.UsedAt != nil || token.Username != username {
			continue
		}
		t := at
		token.UsedAt = &t
		m.tokens[id] = token
		n++
	}
	return n, nil
}
