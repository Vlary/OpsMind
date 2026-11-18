package auth

import (
	"context"
	"errors"
	"time"

	"aiops/internal/store"
)

// OneTimeTokenTTL 是 invite/reset token 的有效期（契约 12.3 第 18 条）。
const OneTimeTokenTTL = 30 * time.Minute

var (
	// ErrDeliveryUnavailable 表示没有可用的 SMTP/IM 投递通道；handler 映射
	// 503 DELIVERY_UNAVAILABLE，且不创建用户/不签发 token。
	ErrDeliveryUnavailable = errors.New("auth: token delivery unavailable")
	// ErrInvalidToken 表示 invite/reset token 不存在、已使用或已过期。
	ErrInvalidToken = errors.New("auth: invalid or expired token")
	// ErrSelfRedeem 表示 token 创建者 IP 尝试兑换自己的 token。
	ErrSelfRedeem = errors.New("auth: self redeem forbidden")
)

// Deliverer 是 invite/reset token 的独立投递通道（SMTP/IM）。
//
// rawToken 只在投递瞬间存在；数据库只存 SHA-256。实现不得记录明文。
type Deliverer interface {
	DeliverToken(ctx context.Context, kind store.UserTokenKind, email, username, rawToken string, expiresAt time.Time) error
}

// SetDeliverer 在装配期注入投递通道（构造后可替换，读写加锁）。
func (s *Service) SetDeliverer(d Deliverer) {
	s.deliverMu.Lock()
	s.deliverer = d
	s.deliverMu.Unlock()
}

func (s *Service) currentDeliverer() Deliverer {
	s.deliverMu.RLock()
	defer s.deliverMu.RUnlock()
	return s.deliverer
}

// CanDeliver 报告是否配置了投递通道。未配置时创建用户/重置密码返回
// ErrDeliveryUnavailable，而不是签发一个永远无法送达的 token。
func (s *Service) CanDeliver() bool {
	return s.currentDeliverer() != nil
}

// DeliverToken 通过独立通道投递一次性 token。
func (s *Service) DeliverToken(ctx context.Context, kind store.UserTokenKind, email, username, rawToken string, expiresAt time.Time) error {
	d := s.currentDeliverer()
	if d == nil || email == "" {
		return ErrDeliveryUnavailable
	}
	if err := d.DeliverToken(ctx, kind, email, username, rawToken, expiresAt); err != nil {
		return errors.Join(ErrDeliveryUnavailable, err)
	}
	return nil
}
