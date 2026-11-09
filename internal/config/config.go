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
