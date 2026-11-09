package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 冲刺 1 打磨：网络层错误必须携带脱敏后的底层原因
// （dial 拒绝 / DNS / TLS），供 httpapi 层透出给运维排障；
// 只写 "network error" 无法区分防火墙、地址错还是域名不存在。

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNetworkErrorSurfacesUnderlyingCause(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = old }()

	client, err := NewOpenAI(ProviderConfig{BaseURL: "http://127.0.0.1:1", APIKey: "k"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable upstream")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("error = %v, want ErrUpstream", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "network error") {
		t.Errorf("message = %q, want network error layer", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("message = %q, want underlying dial error surfaced", msg)
	}
}

func TestNetworkErrorIsSanitized(t *testing.T) {
	const secret = "super-secret-api-key"
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(`Get "http://upstream/v1/models": dial failed, token sk-abcdef123456 and ` + secret)
	})}
	client, err := NewOpenAI(ProviderConfig{BaseURL: "http://upstream", APIKey: secret}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "sk-abcdef") {
		t.Fatalf("error leaks secret: %q", msg)
	}
	if !strings.Contains(msg, "dial failed") {
		t.Errorf("message = %q, want underlying cause kept after sanitization", msg)
	}
}
