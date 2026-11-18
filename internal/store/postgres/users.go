package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type userStore struct{ q querier }

// userColumns 是所有用户读取的统一投影。
//
// invite_pending 是派生字段：username 上存在未使用的 invite token（含已
// 过期）。过期邀请同样意味着账号没有可用密码，界面必须提供「重发邀请」
// 而不是「启用/重置密码」；因此不要把过期行排除在外。
const userColumns = `u.id::text, u.username, u.password_hash, u.role, u.enabled,
	u.display_name, u.email,
	EXISTS (
		SELECT 1 FROM user_tokens t
		WHERE t.kind = 'invite' AND t.used_at IS NULL
		  AND t.username = u.username
	) AS invite_pending,
	u.must_change_password, u.created_by::text, u.last_login_at, u.created_at, u.updated_at`

func scanUser(row pgx.Row) (store.User, error) {
	var u store.User
	var createdBy *string
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled,
		&u.DisplayName, &u.Email, &u.InvitePending,
		&u.MustChangePassword, &createdBy, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return store.User{}, mapError(err)
	}
	u.CreatedBy = createdBy
	u.CreatedAt = utc(u.CreatedAt)
	u.UpdatedAt = utc(u.UpdatedAt)
	if u.LastLoginAt != nil {
		at := utc(*u.LastLoginAt)
		u.LastLoginAt = &at
	}
	return u, nil
}

func (s userStore) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, mapError(err)
}

func (s userStore) List(ctx context.Context, limit int) ([]store.User, error) {
	n, err := pageLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.Query(ctx, `
		SELECT `+userColumns+`
		FROM users u
		ORDER BY u.created_at ASC, u.username ASC
		LIMIT $1`, n)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	out := make([]store.User, 0, n)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, mapError(rows.Err())
}

func (s userStore) Create(ctx context.Context, p store.CreateUserParams) (store.User, error) {
	role := p.Role
	if role == "" {
		role = store.RoleOperator
	}
	if !role.Valid() {
		return store.User{}, store.ErrInvalid
	}
	return scanUser(s.q.QueryRow(ctx, `
		INSERT INTO users AS u (username, password_hash, role, enabled, display_name, email,
		                   must_change_password, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+userColumns,
		p.Username, p.PasswordHash, role, p.Enabled, p.DisplayName, p.Email,
		p.MustChangePassword, ptrOrNil(p.CreatedBy)))
}

// Update 更新角色/启停/展示名。
//
// 守卫顺序（契约 12.3 第 14 条）：先在同一事务上取
// pg_advisory_xact_lock(hashtext('rbac.admin_guard'))，再读取目标当前行并
// 判断「是否删掉最后一个启用的管理员」；命中时返回 store.ErrLastAdmin。
// 迁移 00012 的 deferred trigger 是 commit 时的硬兜底。
func (s userStore) Update(ctx context.Context, id string, p store.UpdateUserParams) (store.User, error) {
	if _, err := uuid.Parse(id); err != nil {
		return store.User{}, store.ErrInvalid
	}
	if p.Role != nil && !p.Role.Valid() {
		return store.User{}, store.ErrInvalid
	}

	if _, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rbac.admin_guard'))`); err != nil {
		return store.User{}, mapError(err)
	}

	current, err := s.byIDForUpdate(ctx, id)
	if err != nil {
		return store.User{}, err
	}
	wouldDropAdmin := current.Role == store.RoleAdmin && current.Enabled &&
		((p.Role != nil && *p.Role != store.RoleAdmin) || (p.Enabled != nil && !*p.Enabled))
	if wouldDropAdmin {
		var enabledAdmins int64
		if err := s.q.QueryRow(ctx, `
			SELECT count(*) FROM users WHERE role = 'admin' AND enabled`).Scan(&enabledAdmins); err != nil {
			return store.User{}, mapError(err)
		}
		if enabledAdmins <= 1 {
			return store.User{}, store.ErrLastAdmin
		}
	}

	return scanUser(s.q.QueryRow(ctx, `
		UPDATE users u
		SET role = COALESCE($2, u.role),
		    enabled = COALESCE($3, u.enabled),
		    display_name = COALESCE($4, u.display_name),
		    email = COALESCE($5, u.email),
		    updated_at = $6
		WHERE u.id = $1::uuid
		RETURNING `+userColumns,
		id, p.Role, p.Enabled, p.DisplayName, p.Email, utc(p.UpdatedAt)))
}

// byIDForUpdate 在 Update 的同一事务里锁定目标行，避免并发改角色/停用。
func (s userStore) byIDForUpdate(ctx context.Context, id string) (store.User, error) {
	var u store.User
	var createdBy *string
	err := s.q.QueryRow(ctx, `
		SELECT u.id::text, u.username, u.password_hash, u.role, u.enabled,
		       u.display_name, u.email, u.must_change_password,
		       u.created_by::text, u.last_login_at, u.created_at, u.updated_at
		FROM users u WHERE u.id = $1::uuid FOR UPDATE`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Enabled,
			&u.DisplayName, &u.Email, &u.MustChangePassword,
			&createdBy, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return store.User{}, mapError(err)
	}
	u.CreatedBy = createdBy
	return u, nil
}

func (s userStore) ByUsername(ctx context.Context, username string) (store.User, error) {
	return scanUser(s.q.QueryRow(ctx, `
		SELECT `+userColumns+` FROM users u WHERE u.username = $1`, username))
}

func (s userStore) ByID(ctx context.Context, id string) (store.User, error) {
	return scanUser(s.q.QueryRow(ctx, `
		SELECT `+userColumns+` FROM users u WHERE u.id = $1::uuid`, id))
}

func (s userStore) CountEnabledAdmins(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `
		SELECT count(*) FROM users WHERE role = 'admin' AND enabled`).Scan(&n)
	return n, mapError(err)
}

// CountEnabledApprovers 返回可承担第二人职责的启用账号数（admin/approver）：
// 单管理员降级模式判定的唯一事实源（契约 12.3 第 11/25 条）。
func (s userStore) CountEnabledApprovers(ctx context.Context) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `
		SELECT count(*) FROM users WHERE role IN ('admin','approver') AND enabled`).Scan(&n)
	return n, mapError(err)
}

// SetMustChangePassword 只更新 must_change_password（离线 admin reset 路径）。
func (s userStore) SetMustChangePassword(ctx context.Context, id string, mustChange bool, at time.Time) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE users SET must_change_password = $2, updated_at = $3 WHERE id = $1::uuid`,
		id, mustChange, utc(at))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UsernamesByIDs 批量按 id 查用户名；非法 uuid 的 id 忽略，查不到的 id 不在结果中。
func (s userStore) UsernamesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.q.Query(ctx, `
		SELECT id::text, username FROM users WHERE id = ANY($1::uuid[])`, valid)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make(map[string]string, len(valid))
	for rows.Next() {
		var id, username string
		if err := rows.Scan(&id, &username); err != nil {
			return nil, mapError(err)
		}
		out[id] = username
	}
	return out, mapError(rows.Err())
}

func (s userStore) UpdatePassword(ctx context.Context, id string, p store.UpdatePasswordParams) error {
	var mustChange any
	if p.MustChangePassword != nil {
		mustChange = *p.MustChangePassword
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE users
		SET password_hash = $2,
		    must_change_password = COALESCE($3, must_change_password),
		    updated_at = $4
		WHERE id = $1::uuid`,
		id, p.PasswordHash, mustChange, utc(p.UpdatedAt))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s userStore) UpdateLastLogin(ctx context.Context, id string, at time.Time) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE users SET last_login_at = $2 WHERE id = $1::uuid`, id, utc(at))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------ user_tokens

const userTokenColumns = `id::text, kind, user_id::text, username, email, role,
	expires_at, used_at, created_by::text, creator_ip, delivered_channel, created_at`

func scanUserToken(row pgx.Row) (store.UserToken, error) {
	var t store.UserToken
	var userID, createdBy *string
	var role *store.Role
	err := row.Scan(&t.ID, &t.Kind, &userID, &t.Username, &t.Email, &role,
		&t.ExpiresAt, &t.UsedAt, &createdBy, &t.CreatorIP, &t.DeliveredChannel, &t.CreatedAt)
	if err != nil {
		return store.UserToken{}, mapError(err)
	}
	t.UserID = userID
	t.CreatedBy = createdBy
	t.Role = role
	t.ExpiresAt = utc(t.ExpiresAt)
	t.CreatedAt = utc(t.CreatedAt)
	if t.UsedAt != nil {
		at := utc(*t.UsedAt)
		t.UsedAt = &at
	}
	return t, nil
}

func (s userStore) CreateToken(ctx context.Context, p store.CreateUserTokenParams) (store.UserToken, error) {
	if p.Kind != store.UserTokenInvite && p.Kind != store.UserTokenReset {
		return store.UserToken{}, store.ErrInvalid
	}
	if len(p.TokenHash) != 32 {
		return store.UserToken{}, store.ErrInvalid
	}
	if p.Role != nil && !p.Role.Valid() {
		return store.UserToken{}, store.ErrInvalid
	}
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return scanUserToken(s.q.QueryRow(ctx, `
		INSERT INTO user_tokens (kind, user_id, username, email, role, token_hash,
		                         expires_at, created_by, creator_ip, delivered_channel, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+userTokenColumns,
		p.Kind, ptrOrNil(p.UserID), p.Username, p.Email, p.Role, p.TokenHash,
		utc(p.ExpiresAt), ptrOrNil(p.CreatedBy), p.CreatorIP, p.DeliveredChannel, utc(createdAt)))
}

func (s userStore) TokenByHash(ctx context.Context, tokenHash []byte) (store.UserToken, error) {
	return scanUserToken(s.q.QueryRow(ctx, `
		SELECT `+userTokenColumns+` FROM user_tokens WHERE token_hash = $1`, tokenHash))
}

func (s userStore) MarkTokenUsed(ctx context.Context, id string, at time.Time) (bool, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE user_tokens SET used_at = $2 WHERE id = $1::uuid AND used_at IS NULL`,
		id, utc(at))
	if err != nil {
		return false, mapError(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s userStore) PendingInvite(ctx context.Context, username string) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_tokens
			WHERE kind = 'invite' AND used_at IS NULL AND expires_at > now() AND username = $1
		)`, username).Scan(&ok)
	return ok, mapError(err)
}

// UnusedInviteToken 检查未使用的 invite token（**不过滤过期**）：过期邀请
// 依然意味着账号没有可用密码，管理员不能直接启用或重置密码。
func (s userStore) UnusedInviteToken(ctx context.Context, username string) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_tokens
			WHERE kind = 'invite' AND used_at IS NULL AND username = $1
		)`, username).Scan(&ok)
	return ok, mapError(err)
}

// RevokeUnusedInvites 作废 username 的所有未使用 invite token（重发邀请时
// 旧链接立即失效）；返回受影响行数。
func (s userStore) RevokeUnusedInvites(ctx context.Context, username string, at time.Time) (int64, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE user_tokens SET used_at = $2
		WHERE kind = 'invite' AND username = $1 AND used_at IS NULL`,
		username, utc(at))
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}
