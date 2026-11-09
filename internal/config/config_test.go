package config

import (
	"log/slog"
	"testing"
)

func TestOriginAllowed(t *testing.T) {
	c := &Config{AllowedOrigins: []string{"http://localhost:5173", "http://localhost:8080"}}
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://localhost:5173", true},
		{"http://localhost:8080", true},
		{"http://localhost:5173/", false},
		{"https://localhost:5173", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.OriginAllowed(tc.origin); got != tc.want {
			t.Errorf("OriginAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestRedactedDatabaseURL(t *testing.T) {
	c := &Config{DatabaseURL: "postgres://aiops:secret@127.0.0.1:65432/aiops?sslmode=disable"}
	got := c.RedactedDatabaseURL()
	if got == c.DatabaseURL {
		t.Fatalf("RedactedDatabaseURL did not redact: %q", got)
	}
	if want := "postgres://aiops:xxxxx@127.0.0.1:65432/aiops?sslmode=disable"; got != want {
		t.Fatalf("RedactedDatabaseURL = %q, want %q", got, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("AIOPS_DATABASE_URL", "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable")
	t.Setenv("AIOPS_ADDR", "")
	t.Setenv("AIOPS_LOG_LEVEL", "")
	t.Setenv("AIOPS_ALLOWED_ORIGINS", "")
	t.Setenv("AIOPS_PUBLIC_ORIGIN", "")
	t.Setenv("AIOPS_SECURE_COOKIES", "")
	t.Setenv("AIOPS_SESSION_TTL", "")
	t.Setenv("AIOPS_APPROVAL_TTL", "")
	t.Setenv("AIOPS_STREAM_MAX_DURATION", "")
	t.Setenv("AIOPS_MAX_STREAMS", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", c.LogLevel)
	}
	if c.MaxStreams != 20 {
		t.Errorf("MaxStreams = %d, want 20", c.MaxStreams)
	}
	if len(c.AllowedOrigins) != 2 {
		t.Errorf("AllowedOrigins = %v, want 2 entries", c.AllowedOrigins)
	}
	// 契约 5.12 未给 PublicOrigin 定义默认值：未设置时为空，
	// SecureCookies 由「PublicOrigin 是否为 https」推导为 false。
	if c.PublicOrigin != "" {
		t.Errorf("PublicOrigin = %q, want empty when AIOPS_PUBLIC_ORIGIN unset", c.PublicOrigin)
	}
	if c.SecureCookies {
		t.Error("SecureCookies = true, want false when PublicOrigin is empty")
	}
}

// TestLoadSecureCookiesDerivation 覆盖 SecureCookies 的推导：显式覆盖优先，
// 其次按 PublicOrigin 的 scheme 推导。
func TestLoadSecureCookiesDerivation(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("AIOPS_DATABASE_URL", "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable")
		t.Setenv("AIOPS_PUBLIC_ORIGIN", "")
		t.Setenv("AIOPS_SECURE_COOKIES", "")
	}

	t.Run("https public origin", func(t *testing.T) {
		base(t)
		t.Setenv("AIOPS_PUBLIC_ORIGIN", "https://ops.example.com")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if !c.SecureCookies {
			t.Error("SecureCookies = false, want true for https PublicOrigin")
		}
	})

	t.Run("http public origin", func(t *testing.T) {
		base(t)
		t.Setenv("AIOPS_PUBLIC_ORIGIN", "http://localhost:8080")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if c.SecureCookies {
			t.Error("SecureCookies = true, want false for http PublicOrigin")
		}
	})

	t.Run("explicit override wins", func(t *testing.T) {
		base(t)
		t.Setenv("AIOPS_PUBLIC_ORIGIN", "https://ops.example.com")
		t.Setenv("AIOPS_SECURE_COOKIES", "false")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if c.SecureCookies {
			t.Error("SecureCookies = true, want false when AIOPS_SECURE_COOKIES=false overrides https origin")
		}
	})
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("AIOPS_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for missing AIOPS_DATABASE_URL")
	}
}

func TestLoadEnvironmentAliases(t *testing.T) {
	t.Setenv("AIOPS_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable")
	t.Setenv("AIOPS_ADDR", "")
	t.Setenv("PORT", "9090")
	t.Setenv("AIOPS_DATA_DIR", "")
	t.Setenv("DATA_DIR", "/tmp/aiops-data")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", c.Addr)
	}
	if c.DataDir != "/tmp/aiops-data" {
		t.Errorf("DataDir = %q, want /tmp/aiops-data", c.DataDir)
	}
}

func TestWebDirCanBeDisabled(t *testing.T) {
	t.Setenv("AIOPS_DATABASE_URL", "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable")
	t.Setenv("AIOPS_WEB_DIR", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.WebDir != "" {
		t.Fatalf("WebDir = %q, want empty to disable static site", c.WebDir)
	}
}

func TestLoadValidatesMasterKey(t *testing.T) {
	t.Setenv("AIOPS_DATABASE_URL", "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable")
	t.Setenv("AIOPS_MASTER_KEY", "not-base64!")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted invalid base64 master key")
	}

	// 16 字节是合法 base64 但不是 32 字节密钥。
	t.Setenv("AIOPS_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAA==")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a 16-byte master key")
	}

	t.Setenv("AIOPS_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=") // 32 bytes
	if _, err := Load(); err != nil {
		t.Fatalf("Load() rejected a 32-byte master key: %v", err)
	}
}
