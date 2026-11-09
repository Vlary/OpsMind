package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/config"
)

// TestTrustProxyDoesNotTrustArbitraryClients 验证开启 AIOPS_TRUST_PROXY 时
// 只有环回代理可以提供 X-Forwarded-For；直连客户端伪造 XFF 不影响 ClientIP。
func TestTrustProxyDoesNotTrustArbitraryClients(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		wantIP     string
	}{
		{"direct client ignored", "203.0.113.9:5555", "203.0.113.9"},
		{"loopback proxy trusted", "127.0.0.1:5555", "1.1.1.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			router := NewRouter(&Deps{
				Config: &config.Config{
					TrustProxy:     true,
					AllowedOrigins: []string{smokeTestOrigin},
					WebDir:         "",
				},
				Log:   log,
				Clock: time.Now,
			})

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("X-Forwarded-For", "1.1.1.1")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) == 0 || lines[len(lines)-1] == "" {
				t.Fatalf("no access log emitted")
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
				t.Fatalf("parse access log: %v (%s)", err, lines[len(lines)-1])
			}
			if got := entry["ip"]; got != tc.wantIP {
				t.Fatalf("ClientIP = %v, want %s", got, tc.wantIP)
			}
		})
	}
}

const smokeTestOrigin = "http://localhost:5173"
