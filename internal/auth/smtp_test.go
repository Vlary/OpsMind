package auth

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

func TestNewSMTPDelivererValidation(t *testing.T) {
	if _, err := NewSMTPDeliverer(SMTPConfig{}); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatalf("empty config err = %v, want ErrDeliveryUnavailable", err)
	}
	if _, err := NewSMTPDeliverer(SMTPConfig{Host: "smtp.example.com", Port: 0, From: "ops@example.com"}); err == nil {
		t.Fatal("port 0 must be rejected")
	}
	del, err := NewSMTPDeliverer(SMTPConfig{
		Host: "smtp.example.com", Port: 465, From: "ops@example.com", UseTLS: true,
		PublicOrigin: "https://opsmind.example.com",
	})
	if err != nil || del == nil {
		t.Fatalf("valid config err = %v", err)
	}
}

// TestResolveSMTPTLSMode 锁定「端口决定 TLS 模式」的判定表（走查 high）：
// README 承诺 465 直连 TLS、其余端口 STARTTLS；代码曾把任意 UseTLS=true
// 都当 implicit TLS，默认 587 的提交服务器一律投递失败。
func TestResolveSMTPTLSMode(t *testing.T) {
	cases := []struct {
		name     string
		useTLS   bool
		port     int
		implicit bool
		want     SMTPTLSMode
	}{
		{"plaintext disabled", false, 587, false, SMTPTLSDisabled},
		{"465 implicit", true, 465, false, SMTPTLSImplicit},
		{"587 starttls", true, 587, false, SMTPTLSStartTLS},
		{"25 starttls", true, 25, false, SMTPTLSStartTLS},
		{"2525 starttls", true, 2525, false, SMTPTLSStartTLS},
		{"explicit implicit on non-465", true, 8465, true, SMTPTLSImplicit},
		{"implicit flag ignored when tls off", false, 465, true, SMTPTLSDisabled},
	}
	for _, tc := range cases {
		if got := ResolveSMTPTLSMode(tc.useTLS, tc.port, tc.implicit); got != tc.want {
			t.Errorf("%s: ResolveSMTPTLSMode(%v,%d,%v) = %s, want %s",
				tc.name, tc.useTLS, tc.port, tc.implicit, got, tc.want)
		}
	}
}

// firstPacket 起一个只接受一次连接的 TCP listener：先回 SMTP 220 问候，
// 再读客户端首包字节（STARTTLS 路径要等 greeting 才会发 EHLO）。
// 关闭 listener 由 t.Cleanup 负责。
func firstPacket(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("220 test ESMTP ready\r\n"))
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		ch <- append([]byte(nil), buf[:n]...)
	}()
	return ln.Addr().String(), ch
}

func waitFirstPacket(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("smtp server received no bytes")
		return nil
	}
}

// TestDeliverTokenStartsWithEHLOOnSubmissionPort：默认配置（UseTLS=true、
// 端口 587）必须先明文连接并发送 EHLO，而不是直接 TLS ClientHello。
// 这是纯本地可复现的回归：旧实现首包是 0x16 0x03（TLS record）。
func TestDeliverTokenStartsWithEHLOOnSubmissionPort(t *testing.T) {
	addr, first := firstPacket(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port := atoi(t, portStr)
	del, err := NewSMTPDeliverer(SMTPConfig{
		Host: host, Port: port, From: "ops@example.com", UseTLS: true,
		PublicOrigin: "https://opsmind.example.com", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := del.TLSMode(); got != SMTPTLSStartTLS {
		t.Fatalf("TLSMode = %s, want starttls", got)
	}
	// 投递会失败（对端不是真实 SMTP），但首包已经能证明协议顺序。
	_ = del.DeliverToken(context.Background(), store.UserTokenInvite,
		"alice@example.com", "alice", "tok", time.Now().Add(time.Hour))

	got := waitFirstPacket(t, first)
	if len(got) < 4 || !strings.HasPrefix(strings.ToUpper(string(got)), "EHLO") {
		t.Fatalf("first packet = % x (%q), want plaintext EHLO (not TLS ClientHello)", got, got)
	}
}

// TestDeliverTokenImplicitTLSOn465：显式 implicit TLS（端口 465）必须先发
// TLS ClientHello（record type 0x16），证明两条路径确实分流。
func TestDeliverTokenImplicitTLSOn465(t *testing.T) {
	addr, first := firstPacket(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	del, err := NewSMTPDeliverer(SMTPConfig{
		Host: host, Port: atoi(t, portStr), From: "ops@example.com",
		UseTLS: true, ImplicitTLS: true, PublicOrigin: "https://opsmind.example.com",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := del.TLSMode(); got != SMTPTLSImplicit {
		t.Fatalf("TLSMode = %s, want implicit", got)
	}
	_ = del.DeliverToken(context.Background(), store.UserTokenInvite,
		"alice@example.com", "alice", "tok", time.Now().Add(time.Hour))

	got := waitFirstPacket(t, first)
	if len(got) == 0 || got[0] != 0x16 {
		t.Fatalf("first packet = % x, want TLS handshake record (0x16)", got)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("bad port %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func TestBuildTokenMessageInviteAndReset(t *testing.T) {
	cfg := SMTPConfig{From: "ops@example.com", PublicOrigin: "https://opsmind.example.com/"}
	expires := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	msg, err := buildTokenMessage(cfg, store.UserTokenInvite, "alice@example.com", "alice", "tok/1+2", expires)
	if err != nil {
		t.Fatal(err)
	}
	text := string(msg)
	for _, want := range []string{
		"From: ops@example.com", "To: alice@example.com",
		"https://opsmind.example.com/invite?token=tok%2F1%2B2",
		expires.Format(time.RFC3339), "alice",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("invite message missing %q:\n%s", want, text)
		}
	}
	// 邀请/重置共用 /invite 落地页，kind 区分语义。
	resetMsg, err := buildTokenMessage(cfg, store.UserTokenReset, "bob@example.com", "bob", "tok", expires)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resetMsg), "kind=reset") {
		t.Fatalf("reset message kind missing:\n%s", resetMsg)
	}
}
