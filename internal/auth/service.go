// Package auth 负责管理员引导、bcrypt、会话 token、登录限流与鉴权。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"aiops/internal/store"
)

var (
	// ErrInvalidCredentials 表示用户名或密码错误（不对外区分）。
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrAuthRequired 表示缺少、无效或过期的会话。
	ErrAuthRequired = errors.New("auth: authentication required")
	// ErrPasswordInvalid 表示新密码不满足长度要求（1..72 字节，bcrypt 明文上限）。
	ErrPasswordInvalid = errors.New("auth: invalid new password")
)

const (
	adminUsername      = "admin"
	adminCreatedAtKey  = "bootstrap.admin_created_at"
	generatedPwdLength = 24
	// maxPasswordBytes 是 bcrypt 明文长度上限；超限密码无法再通过
	// CompareHashAndPassword 校验，必须在设置时拒绝，避免改完即锁死。
	maxPasswordBytes = 72
)

// Config 是认证服务配置。
type Config struct {
	SessionTTL time.Duration
	// SessionCleanupInterval 是过期会话清理周期；0 表示 TTL/24（下限 1 分钟）。
	// 仅供测试注入更短周期。
	SessionCleanupInterval time.Duration
	Clock                  func() time.Time
}

// LoginResult 是登录成功结果。
type LoginResult struct {
	Token     string
	ExpiresAt time.Time
	User      store.User
}

// Service 是认证服务。
type Service struct {
	st  store.Store
	cfg Config

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	wg        sync.WaitGroup
}

// NewService 构造认证服务。
func NewService(st store.Store, cfg Config) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Service{st: st, cfg: cfg, stop: make(chan struct{})}
}

// EnsureAdmin 在用户表为空时创建 admin 账号，并在同一事务写入
// settings["bootstrap.admin_created_at"]。
//
// adminPassword 为空时生成 24 字符 URL-safe 随机密码，并把
// "admin password: <明文>" 写到 out（仅此一次）。已有用户时直接返回。
func (s *Service) EnsureAdmin(ctx context.Context, adminPassword string, out io.Writer) error {
	count, err := s.st.Users().Count(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	generated := false
	if adminPassword == "" {
		adminPassword, err = randomPassword(generatedPwdLength)
		if err != nil {
			return err
		}
		generated = true
	}

	hash, err := HashPassword(adminPassword)
	if err != nil {
		return err
	}

	createdAt := s.cfg.Clock().UTC()
	value, err := json.Marshal(createdAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("auth: encode bootstrap timestamp: %w", err)
	}

	err = s.st.WithTx(ctx, func(tx store.Store) error {
		// 事务内重新计数，降低多实例并发创建的概率。
		n, err := tx.Users().Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return errAdminExists
		}
		if _, err := tx.Users().Create(ctx, store.CreateUserParams{
			Username:     adminUsername,
			PasswordHash: hash,
		}); err != nil {
			return err
		}
		_, err = tx.Settings().Set(ctx, adminCreatedAtKey, value, createdAt)
		return err
	})
	if errors.Is(err, errAdminExists) || errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}

	if generated && out != nil {
		if _, err := fmt.Fprintf(out, "admin password: %s\n", adminPassword); err != nil {
			return fmt.Errorf("auth: write generated admin password: %w", err)
		}
	}
	return nil
}

// errAdminExists 在事务内发现并发创建时用于回滚。
var errAdminExists = errors.New("auth: admin already exists")

// Login 校验密码并创建会话。
//
// 用户不存在时仍对固定 dummy hash 执行一次 bcrypt，避免时间侧信道。
func (s *Service) Login(ctx context.Context, password, ip, userAgent string) (LoginResult, error) {
	user, err := s.st.Users().ByUsername(ctx, adminUsername)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			verifyDummy(password)
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, err
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return LoginResult{}, ErrInvalidCredentials
	}

	raw, hash, err := NewSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	now := s.cfg.Clock().UTC()
	session, err := s.st.Sessions().Create(ctx, store.CreateSessionParams{
		UserID:    user.ID,
		TokenHash: hash,
		ExpiresAt: now.Add(s.cfg.SessionTTL),
		IP:        ip,
		UserAgent: userAgent,
		CreatedAt: now,
	})
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: raw, ExpiresAt: session.ExpiresAt, User: user}, nil
}

// Start 启动过期会话清理（janitor）。
//
// 过期会话不能只靠「同一 token 再次提交」惰性删除——其余会话行会永久残留。
// 周期为 TTL/24（下限 1 分钟）；清理仅由 Stop 停止，不订阅 signal ctx，
// 与审计/审批服务的关闭顺序保持一致。
func (s *Service) Start() {
	s.startOnce.Do(func() {
		interval := s.cfg.SessionCleanupInterval
		if interval <= 0 {
			interval = s.cfg.SessionTTL / 24
			if interval < time.Minute {
				interval = time.Minute
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-s.stop:
					return
				case now := <-ticker.C:
					// 清理失败只跳过本轮，下一轮重试；不影响鉴权路径。
					_, _ = s.st.Sessions().DeleteExpired(context.Background(), now.UTC())
				}
			}
		}()
	})
}

// Stop 停止后台清理并等待退出。
func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Authenticate 按原始 token 查找会话与用户。
//
// token 为空/无效/过期一律返回 ErrAuthRequired；过期会话顺带删除。
func (s *Service) Authenticate(ctx context.Context, rawToken string) (store.User, error) {
	if rawToken == "" {
		return store.User{}, ErrAuthRequired
	}
	session, err := s.st.Sessions().ByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrAuthRequired
		}
		return store.User{}, err
	}
	now := s.cfg.Clock().UTC()
	if !session.ExpiresAt.After(now) {
		if delErr := s.st.Sessions().Delete(ctx, session.ID); delErr != nil {
			// 删除失败不改变对客户端的结论。
			_ = delErr
		}
		return store.User{}, ErrAuthRequired
	}
	user, err := s.st.Users().ByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrAuthRequired
		}
		return store.User{}, err
	}
	// 刷新 last_seen_at 供观测（登录时间、活跃度）；失败不影响鉴权。
	_ = s.st.Sessions().Touch(ctx, session.ID, now)
	return user, nil
}

// Logout 删除当前会话；不存在时视为成功（幂等）。
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	session, err := s.st.Sessions().ByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := s.st.Sessions().Delete(ctx, session.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// ChangePassword 校验旧密码后把用户密码更新为新密码，并在同一事务内吊销
// 该用户的全部会话（含当前会话）——凭据轮换后旧会话必须立即失效。
//
// 旧密码错误返回 ErrInvalidCredentials；新密码为空或超过 72 字节返回
// ErrPasswordInvalid（bcrypt 超限的密码此后无法通过校验，须在此拒绝）。
func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	if len(newPassword) == 0 || len(newPassword) > maxPasswordBytes {
		return ErrPasswordInvalid
	}
	user, err := s.st.Users().ByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidCredentials
		}
		return err
	}
	if !VerifyPassword(user.PasswordHash, oldPassword) {
		return ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	now := s.cfg.Clock().UTC()
	return s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Users().UpdatePassword(ctx, userID, store.UpdatePasswordParams{
			PasswordHash: hash,
			UpdatedAt:    now,
		}); err != nil {
			return err
		}
		return tx.Sessions().DeleteByUser(ctx, userID)
	})
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// verifyDummy 对固定 hash 做一次同成本校验，用于用户不存在时抹平时间差。
func verifyDummy(password string) {
	dummyOnce.Do(func() {
		hash, err := bcryptGenerate([]byte("aiops-dummy-password"), BcryptCost)
		if err == nil {
			dummyHash = hash
		}
	})
	if len(dummyHash) > 0 {
		_ = VerifyPassword(string(dummyHash), password)
	}
}

// bcryptGenerate 独立成变量便于测试观察成本；默认实现见 password.go。
var bcryptGenerate = func(password []byte, cost int) ([]byte, error) {
	return generateBcrypt(password, cost)
}

// randomPassword 生成 n 字符的 URL-safe 随机密码。
func randomPassword(n int) (string, error) {
	rawLen := (n*6 + 7) / 8 // 每个 base64url 字符承载 6 bit
	if rawLen < 16 {
		rawLen = 16
	}
	buf := make([]byte, rawLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate admin password: %w", err)
	}
	out := base64.RawURLEncoding.EncodeToString(buf)
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}
