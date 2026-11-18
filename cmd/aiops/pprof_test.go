package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/httpapi"
	"aiops/internal/store"
)

type fakeSessionAuth struct {
	user store.User
	err  error
}

func (f fakeSessionAuth) Authenticate(context.Context, string) (store.User, error) {
	return f.user, f.err
}

func TestRequireSessionEnforcesPprofPermission(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	cases := []struct {
		name   string
		auth   sessionAuthenticator
		cookie bool
		want   int
	}{
		{"anonymous", fakeSessionAuth{user: store.User{Role: store.RoleAdmin, Enabled: true}}, false, http.StatusUnauthorized},
		{"expired session", fakeSessionAuth{err: auth.ErrAuthRequired}, true, http.StatusUnauthorized},
		{"operator cannot read pprof", fakeSessionAuth{user: store.User{ID: "u", Role: store.RoleOperator, Enabled: true}}, true, http.StatusForbidden},
		{"approver cannot read pprof", fakeSessionAuth{user: store.User{ID: "u", Role: store.RoleApprover, Enabled: true}}, true, http.StatusForbidden},
		{"auditor cannot read pprof", fakeSessionAuth{user: store.User{ID: "u", Role: store.RoleAuditor, Enabled: true}}, true, http.StatusForbidden},
		{"admin with must_change denied", fakeSessionAuth{user: store.User{ID: "u", Role: store.RoleAdmin, Enabled: true, MustChangePassword: true}}, true, http.StatusForbidden},
		{"admin allowed", fakeSessionAuth{user: store.User{ID: "u", Role: store.RoleAdmin, Enabled: true}}, true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: httpapi.SessionCookieName, Value: "token"})
			}
			rec := httptest.NewRecorder()
			requireSession(ok, tc.auth).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestRequireSessionNilAuthIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	requireSession(http.NotFoundHandler(), nil).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil auth status = %d, want 404", rec.Code)
	}
}

func TestConfigureTokenDeliveryInjectsSMTP(t *testing.T) {
	svc := auth.NewService(nil, auth.Config{})
	if err := configureTokenDelivery(svc, &config.Config{}, slog.Default()); err != nil {
		t.Fatalf("no SMTP: %v", err)
	}
	if svc.CanDeliver() {
		t.Fatal("deliverer injected without SMTP host")
	}

	cfg := &config.Config{
		SmtpHost: "smtp.example.com", SmtpPort: 587,
		SmtpUsername: "ops@example.com", SmtpFrom: "ops@example.com", SmtpUseTLS: true,
		PublicOrigin: "https://opsmind.example.com",
	}
	if err := configureTokenDelivery(svc, cfg, slog.Default()); err != nil {
		t.Fatalf("with SMTP: %v", err)
	}
	if !svc.CanDeliver() {
		t.Fatal("SMTP host configured but no deliverer injected")
	}
}
