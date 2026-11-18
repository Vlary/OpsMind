package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"aiops/internal/store"
)

// SMTPConfig 是 invite/reset token 的 SMTP 投递配置（契约 12.3 第 18 条）。
// 通道独立于业务端口；凭据只存在进程内存与配置里，不写日志/审计。
type SMTPConfig struct {
	Host         string
	Port         int
	Username     string
	Password     string
	From         string
	UseTLS       bool // true：加密通道（465=implicit TLS，其余端口 STARTTLS）
	ImplicitTLS  bool // 显式 implicit TLS（非 465 端口时用于自建 SMTPS）
	PublicOrigin string
	Timeout      time.Duration
}

// SMTPTLSMode 是投递实际使用的 TLS 模式（启动日志与文档口径）。
type SMTPTLSMode string

const (
	// SMTPTLSDisabled 表示 UseTLS=false 的明文投递（不推荐，仅内网中继）。
	SMTPTLSDisabled SMTPTLSMode = "disabled"
	// SMTPTLSImplicit 表示连接即 TLS（SMTPS，标准端口 465）。
	SMTPTLSImplicit SMTPTLSMode = "implicit"
	// SMTPTLSStartTLS 表示明文连接后强制 STARTTLS（标准提交端口 587/25）。
	SMTPTLSStartTLS SMTPTLSMode = "starttls"
)

// ResolveSMTPTLSMode 决定投递使用的 TLS 模式：
//
//   - UseTLS=false → 明文（显式关闭）；
//   - UseTLS=true 且端口 465（或显式 implicit）→ implicit TLS；
//   - 其余端口 → STARTTLS。
//
// 走查 high 的根因就是旧实现把 UseTLS 一律当 implicit TLS，默认端口 587
// 的提交服务器收到 TLS ClientHello 后直接断开，管理员永远建不了第二个
// 账号。判定只依赖端口与 UseTLS，便于启动日志打印同一结果。
func ResolveSMTPTLSMode(useTLS bool, port int, implicit bool) SMTPTLSMode {
	if !useTLS {
		return SMTPTLSDisabled
	}
	if implicit || port == 465 {
		return SMTPTLSImplicit
	}
	return SMTPTLSStartTLS
}

// TLSMode 返回该投递器实际使用的 TLS 模式（启动日志用）。
func (d *SMTPDeliverer) TLSMode() SMTPTLSMode {
	return ResolveSMTPTLSMode(d.cfg.UseTLS, d.cfg.Port, d.cfg.ImplicitTLS)
}

// SMTPDeliverer 通过 SMTP 把一次性 token 投递到目标邮箱。
type SMTPDeliverer struct {
	cfg SMTPConfig
}

// NewSMTPDeliverer 校验配置并构造投递器；Host/From 为空返回 ErrDeliveryUnavailable。
func NewSMTPDeliverer(cfg SMTPConfig) (*SMTPDeliverer, error) {
	if strings.TrimSpace(cfg.Host) == "" || strings.TrimSpace(cfg.From) == "" {
		return nil, ErrDeliveryUnavailable
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("auth: invalid smtp port %d", cfg.Port)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	return &SMTPDeliverer{cfg: cfg}, nil
}

// DeliverToken 组装并发送邀请/重置邮件。rawToken 只出现在邮件正文。
func (d *SMTPDeliverer) DeliverToken(ctx context.Context, kind store.UserTokenKind, email, username, rawToken string, expiresAt time.Time) error {
	if email == "" || rawToken == "" {
		return ErrDeliveryUnavailable
	}
	msg, err := buildTokenMessage(d.cfg, kind, email, username, rawToken, expiresAt)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: d.cfg.Timeout}
	switch d.TLSMode() {
	case SMTPTLSImplicit:
		conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(d.cfg.Host, fmt.Sprint(d.cfg.Port)),
			&tls.Config{ServerName: d.cfg.Host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return fmt.Errorf("auth: smtp tls dial: %w", err)
		}
		_ = conn.SetDeadline(time.Now().Add(d.cfg.Timeout))
		client, err := smtp.NewClient(conn, d.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("auth: smtp client: %w", err)
		}
		return d.send(client, email, msg)
	case SMTPTLSStartTLS:
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(d.cfg.Host, fmt.Sprint(d.cfg.Port)))
		if err != nil {
			return fmt.Errorf("auth: smtp dial: %w", err)
		}
		_ = conn.SetDeadline(time.Now().Add(d.cfg.Timeout))
		client, err := smtp.NewClient(conn, d.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("auth: smtp client: %w", err)
		}
		// 提交端口（587/25）在明文连接后升 TLS。走查 high：旧实现按
		// UseTLS=true 直接 implicit TLS，587 上投递必失败。
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			_ = client.Close()
			return fmt.Errorf("auth: smtp server at %s:%d does not advertise STARTTLS",
				d.cfg.Host, d.cfg.Port)
		}
		if err := client.StartTLS(&tls.Config{ServerName: d.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			_ = client.Close()
			return fmt.Errorf("auth: smtp starttls: %w", err)
		}
		return d.send(client, email, msg)
	default:
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(d.cfg.Host, fmt.Sprint(d.cfg.Port)))
		if err != nil {
			return fmt.Errorf("auth: smtp dial: %w", err)
		}
		_ = conn.SetDeadline(time.Now().Add(d.cfg.Timeout))
		client, err := smtp.NewClient(conn, d.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("auth: smtp client: %w", err)
		}
		return d.send(client, email, msg)
	}
}

func (d *SMTPDeliverer) send(client *smtp.Client, email string, msg []byte) error {
	defer func() { _ = client.Close() }()
	if d.cfg.Username != "" {
		auth := smtp.PlainAuth("", d.cfg.Username, d.cfg.Password, d.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("auth: smtp auth: %w", err)
		}
	}
	if err := client.Mail(d.cfg.From); err != nil {
		return fmt.Errorf("auth: smtp mail: %w", err)
	}
	if err := client.Rcpt(email); err != nil {
		return fmt.Errorf("auth: smtp rcpt: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("auth: smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("auth: smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("auth: smtp close data: %w", err)
	}
	if err := client.Quit(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("auth: smtp quit: %w", err)
	}
	return nil
}

// buildTokenMessage 生成纯文本一次性链接邮件（RFC 5322）。
func buildTokenMessage(cfg SMTPConfig, kind store.UserTokenKind, email, username, rawToken string, expiresAt time.Time) ([]byte, error) {
	action := "接受邀请并设置密码"
	path := "/invite"
	if kind == store.UserTokenReset {
		action = "重置密码"
	}
	link := strings.TrimRight(cfg.PublicOrigin, "/") + path +
		"?token=" + url.QueryEscape(rawToken) + "&kind=" + string(kind)
	subject := "[OpsMind] " + action
	body := fmt.Sprintf("你好 %s，\n\n请在 %s 前%s：\n%s\n\n链接仅可使用一次；若非本人操作请忽略本邮件。\n",
		username, expiresAt.UTC().Format(time.RFC3339), action, link)
	msg := strings.Join([]string{
		"From: " + cfg.From,
		"To: " + email,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		body,
	}, "\r\n")
	return []byte(msg), nil
}
