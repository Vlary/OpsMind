// Package config 集中读取、校验环境配置。
//
// 只依赖标准库；不读数据库、不启动服务。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是进程配置。字段与 CONTRACT.md 5.12 完全一致。
type Config struct {
	Addr              string
	DatabaseURL       string
	MasterKeyB64      string
	DataDir           string
	AdminPassword     string
	AllowedOrigins    []string
	PublicOrigin      string
	SecureCookies     bool
	TrustProxy        bool
	WebDir            string
	LogLevel          slog.Level
	SessionTTL        time.Duration
	ApprovalTTL       time.Duration
	StreamMaxDuration time.Duration
	MaxStreams        int
	// W3：终端会话治理（CONTRACT.md 5.12）。
	RecordingsDir   string        // AIOPS_RECORDINGS_DIR，默认 "<DataDir>/recordings"
	TermMaxSessions int           // AIOPS_TERM_MAX_SESSIONS，默认 4，1..16
	TermIdleTimeout time.Duration // AIOPS_TERM_IDLE_TIMEOUT，默认 10m
	TermMaxDuration time.Duration // AIOPS_TERM_MAX_DURATION，默认 8h
	// W4：RDP 网关治理（CONTRACT.md 5.12）。
	GuacdAddr        string        // AIOPS_GUACD_ADDR，默认 ""（RDP 禁用；compose 内 guacd:4822）
	RdpRecordingsDir string        // AIOPS_RDP_RECORDINGS_DIR，默认 "<DataDir>/rdp-recordings"
	RdpMaxSessions   int           // AIOPS_RDP_MAX_SESSIONS，默认 2，1..8
	RdpIdleTimeout   time.Duration // AIOPS_RDP_IDLE_TIMEOUT，默认 30m
	RdpMaxDuration   time.Duration // AIOPS_RDP_MAX_DURATION，默认 8h
	// 00012：invite/reset token 的 SMTP 投递通道（契约 12.3 第 18 条）。
	// 未配置 SMTPHost 时保持无通道（POST /users 与 reset 返回 503
	// DELIVERY_UNAVAILABLE），绝不签发无法送达的 token。
	SmtpHost     string // AIOPS_SMTP_HOST
	SmtpPort     int    // AIOPS_SMTP_PORT，默认 587（STARTTLS；465 走 implicit TLS）
	SmtpUsername string // AIOPS_SMTP_USERNAME
	SmtpPassword string // AIOPS_SMTP_PASSWORD
	SmtpFrom     string // AIOPS_SMTP_FROM，默认取 SmtpUsername
	// SmtpUseTLS 默认 true：465 走 implicit TLS，其余端口明文连接后强制
	// STARTTLS。走查 high：旧实现把任意端口的 UseTLS=true 都当 implicit
	// TLS，默认 587 的提交服务器必投递失败。
	SmtpUseTLS bool // AIOPS_SMTP_USE_TLS
	// SmtpImplicitTLS 强制 implicit TLS（非 465 的自建 SMTPS 端口）。
	SmtpImplicitTLS bool // AIOPS_SMTP_IMPLICIT_TLS，默认 false
	// 00012：单管理员降级模式的第二人引导（契约 5.10 / 12.3 第 25 条）。
	// 非空时首次启动为该地址创建 approver 邀请；为空则进入降级模式并写
	// rbac_single_admin_warning。
	BootstrapApproverEmail string // AIOPS_BOOTSTRAP_APPROVER_EMAIL
}

// Load 从环境变量读取配置并校验。
//
// TODO(SCAFFOLD)：本函数只实现契约要求的读取与校验，不含业务行为。
func Load() (*Config, error) {
	// WebDir 需要区分“未设置”（默认 ./web/dist）与“显式空串”（禁用静态站点）。
	webDir := "./web/dist"
	if raw, ok := os.LookupEnv("AIOPS_WEB_DIR"); ok {
		webDir = strings.TrimSpace(raw)
	}

	c := &Config{
		Addr:          normalizeAddr(envDefault(":8080", "AIOPS_ADDR", "PORT")),
		DatabaseURL:   strings.TrimSpace(envAny("AIOPS_DATABASE_URL", "DATABASE_URL")),
		MasterKeyB64:  strings.TrimSpace(os.Getenv("AIOPS_MASTER_KEY")),
		DataDir:       envDefault("./data", "AIOPS_DATA_DIR", "DATA_DIR"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		// 契约 5.12 未给 PublicOrigin 定义默认值：未设置时保持空串，
		// SecureCookies 由“PublicOrigin 是否为 https”推导为 false。
		// https 部署必须显式设置 AIOPS_PUBLIC_ORIGIN（或 AIOPS_SECURE_COOKIES）。
		PublicOrigin:      strings.TrimSpace(os.Getenv("AIOPS_PUBLIC_ORIGIN")),
		WebDir:            webDir,
		MaxStreams:        20,
		SessionTTL:        24 * time.Hour,
		ApprovalTTL:       120 * time.Second,
		StreamMaxDuration: 30 * time.Minute,
		TermMaxSessions:   4,
		TermIdleTimeout:   10 * time.Minute,
		TermMaxDuration:   8 * time.Hour,
		RdpMaxSessions:    2,
		RdpIdleTimeout:    30 * time.Minute,
		RdpMaxDuration:    8 * time.Hour,
	}

	// W3：录制目录缺省 <DataDir>/recordings（契约 5.12）；显式设置优先。
	c.RecordingsDir = envDefault(filepath.Join(c.DataDir, "recordings"), "AIOPS_RECORDINGS_DIR")
	// W4：.guac 录制目录缺省 <DataDir>/rdp-recordings（契约 5.12）。
	c.RdpRecordingsDir = envDefault(filepath.Join(c.DataDir, "rdp-recordings"), "AIOPS_RDP_RECORDINGS_DIR")
	c.GuacdAddr = strings.TrimSpace(os.Getenv("AIOPS_GUACD_ADDR"))

	c.AllowedOrigins = splitOrigins(env("AIOPS_ALLOWED_ORIGINS", "http://localhost:5173,http://localhost:8080"))

	var err error
	if c.LogLevel, err = parseLevel(env("AIOPS_LOG_LEVEL", "info")); err != nil {
		return nil, err
	}
	if c.SecureCookies, err = parseSecureCookies(c.PublicOrigin); err != nil {
		return nil, err
	}
	if c.TrustProxy, err = parseBool("AIOPS_TRUST_PROXY", false); err != nil {
		return nil, err
	}
	if c.SessionTTL, err = parseDuration("AIOPS_SESSION_TTL", c.SessionTTL); err != nil {
		return nil, err
	}
	if c.ApprovalTTL, err = parseDuration("AIOPS_APPROVAL_TTL", c.ApprovalTTL); err != nil {
		return nil, err
	}
	if c.StreamMaxDuration, err = parseDuration("AIOPS_STREAM_MAX_DURATION", c.StreamMaxDuration); err != nil {
		return nil, err
	}
	if c.MaxStreams, err = parseInt("AIOPS_MAX_STREAMS", c.MaxStreams); err != nil {
		return nil, err
	}
	if c.TermIdleTimeout, err = parseDuration("AIOPS_TERM_IDLE_TIMEOUT", c.TermIdleTimeout); err != nil {
		return nil, err
	}
	if c.TermMaxDuration, err = parseDuration("AIOPS_TERM_MAX_DURATION", c.TermMaxDuration); err != nil {
		return nil, err
	}
	if c.TermMaxSessions, err = parseInt("AIOPS_TERM_MAX_SESSIONS", c.TermMaxSessions); err != nil {
		return nil, err
	}
	if c.RdpIdleTimeout, err = parseDuration("AIOPS_RDP_IDLE_TIMEOUT", c.RdpIdleTimeout); err != nil {
		return nil, err
	}
	if c.RdpMaxDuration, err = parseDuration("AIOPS_RDP_MAX_DURATION", c.RdpMaxDuration); err != nil {
		return nil, err
	}
	if c.RdpMaxSessions, err = parseInt("AIOPS_RDP_MAX_SESSIONS", c.RdpMaxSessions); err != nil {
		return nil, err
	}
	// 00012：SMTP 投递（未设置 host 即无通道）。
	c.SmtpHost = strings.TrimSpace(os.Getenv("AIOPS_SMTP_HOST"))
	c.SmtpPort = 587
	if c.SmtpPort, err = parseInt("AIOPS_SMTP_PORT", c.SmtpPort); err != nil {
		return nil, err
	}
	c.SmtpUsername = strings.TrimSpace(os.Getenv("AIOPS_SMTP_USERNAME"))
	c.SmtpPassword = os.Getenv("AIOPS_SMTP_PASSWORD")
	c.SmtpFrom = strings.TrimSpace(os.Getenv("AIOPS_SMTP_FROM"))
	if c.SmtpFrom == "" {
		c.SmtpFrom = c.SmtpUsername
	}
	c.SmtpUseTLS = true
	if c.SmtpUseTLS, err = parseBool("AIOPS_SMTP_USE_TLS", c.SmtpUseTLS); err != nil {
		return nil, err
	}
	c.SmtpImplicitTLS = false
	if c.SmtpImplicitTLS, err = parseBool("AIOPS_SMTP_IMPLICIT_TLS", c.SmtpImplicitTLS); err != nil {
		return nil, err
	}
	c.BootstrapApproverEmail = strings.TrimSpace(os.Getenv("AIOPS_BOOTSTRAP_APPROVER_EMAIL"))

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return errors.New("config: AIOPS_DATABASE_URL is required")
	}
	if c.MasterKeyB64 != "" {
		key, err := base64.StdEncoding.DecodeString(c.MasterKeyB64)
		if err != nil {
			return errors.New("config: AIOPS_MASTER_KEY must be standard base64")
		}
		if len(key) != 32 {
			return errors.New("config: AIOPS_MASTER_KEY must decode to 32 bytes")
		}
	}
	if len(c.AllowedOrigins) == 0 {
		return errors.New("config: AIOPS_ALLOWED_ORIGINS must not be empty")
	}
	for _, origin := range c.AllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			return err
		}
	}
	if c.SessionTTL <= 0 {
		return errors.New("config: AIOPS_SESSION_TTL must be positive")
	}
	if c.ApprovalTTL <= 0 {
		return errors.New("config: AIOPS_APPROVAL_TTL must be positive")
	}
	if c.StreamMaxDuration <= 0 {
		return errors.New("config: AIOPS_STREAM_MAX_DURATION must be positive")
	}
	if c.MaxStreams < 1 || c.MaxStreams > 100 {
		return errors.New("config: AIOPS_MAX_STREAMS must be between 1 and 100")
	}
	if c.TermMaxSessions < 1 || c.TermMaxSessions > 16 {
		return errors.New("config: AIOPS_TERM_MAX_SESSIONS must be between 1 and 16")
	}
	if c.TermIdleTimeout <= 0 {
		return errors.New("config: AIOPS_TERM_IDLE_TIMEOUT must be positive")
	}
	if c.TermMaxDuration <= 0 {
		return errors.New("config: AIOPS_TERM_MAX_DURATION must be positive")
	}
	// W4：RDP 网关校验（契约 5.12）；GuacdAddr 非空时必须是 host:port。
	if c.RdpMaxSessions < 1 || c.RdpMaxSessions > 8 {
		return errors.New("config: AIOPS_RDP_MAX_SESSIONS must be between 1 and 8")
	}
	if c.RdpIdleTimeout <= 0 {
		return errors.New("config: AIOPS_RDP_IDLE_TIMEOUT must be positive")
	}
	if c.RdpMaxDuration <= 0 {
		return errors.New("config: AIOPS_RDP_MAX_DURATION must be positive")
	}
	// 00012：SMTP 通道（可选）。配置了 host 就必须有合法端口与发件人，
	// 否则宁可启动失败，也不要运行时静默 503。
	if c.BootstrapApproverEmail != "" {
		// 引导 approver 只经 SMTP 投递；未配置 SMTP 时该变量无法生效，
		// 必须在启动时失败而不是静默降级（管理员会以为第二人已创建）。
		if c.SmtpHost == "" {
			return errors.New("config: AIOPS_BOOTSTRAP_APPROVER_EMAIL requires AIOPS_SMTP_HOST")
		}
		if !strings.Contains(c.BootstrapApproverEmail, "@") {
			return errors.New("config: AIOPS_BOOTSTRAP_APPROVER_EMAIL must be an email address")
		}
	}
	if c.SmtpHost != "" {
		if c.SmtpPort < 1 || c.SmtpPort > 65535 {
			return errors.New("config: AIOPS_SMTP_PORT must be between 1 and 65535")
		}
		if c.SmtpFrom == "" {
			return errors.New("config: AIOPS_SMTP_FROM or AIOPS_SMTP_USERNAME is required when AIOPS_SMTP_HOST is set")
		}
	}
	if c.GuacdAddr != "" {
		if _, _, err := net.SplitHostPort(c.GuacdAddr); err != nil {
			return errors.New("config: AIOPS_GUACD_ADDR must be host:port")
		}
	}
	return nil
}

// OriginAllowed 判断 Origin 是否精确匹配允许列表。
func (c *Config) OriginAllowed(origin string) bool {
	for _, allowed := range c.AllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// RedactedDatabaseURL 返回脱敏后的 DSN，用于日志。
func (c *Config) RedactedDatabaseURL() string {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "[invalid database url]"
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envAny 返回第一个已设置且非空的环境变量。
func envAny(keys ...string) string {
	for _, key := range keys {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
	}
	return ""
}

// envDefault 返回第一个已设置且非空的别名，否则返回 fallback。
func envDefault(fallback string, keys ...string) string {
	if v := envAny(keys...); v != "" {
		return v
	}
	return fallback
}

// normalizeAddr 允许 PORT=8080 这类 http.Server 可接受的简写。
func normalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ":8080"
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr
	}
	return ":" + addr
}

func splitOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: AIOPS_ALLOWED_ORIGINS contains an invalid origin")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: AIOPS_ALLOWED_ORIGINS contains an unsupported scheme")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("config: AIOPS_ALLOWED_ORIGINS must not contain path, query, fragment or userinfo")
	}
	return nil
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: AIOPS_LOG_LEVEL is invalid")
	}
}

func parseSecureCookies(publicOrigin string) (bool, error) {
	if raw, ok := os.LookupEnv("AIOPS_SECURE_COOKIES"); ok && raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return false, fmt.Errorf("config: AIOPS_SECURE_COOKIES is invalid")
		}
		return v, nil
	}
	return strings.HasPrefix(strings.ToLower(publicOrigin), "https://"), nil
}

func parseBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: %s is invalid", key)
	}
	return v, nil
}

func parseDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is invalid", key)
	}
	return v, nil
}

func parseInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is invalid", key)
	}
	return v, nil
}
