package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"aiops/internal/store"
)

func genKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return signer, string(pem.EncodeToMemory(block))
}

func genEncryptedKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestBuildAuthMethodsPassword(t *testing.T) {
	methods, err := buildAuthMethods(Target{AuthMethod: store.AuthMethodPassword, Password: "s3cret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("methods = %d, want 1", len(methods))
	}

	_, err = buildAuthMethods(Target{AuthMethod: store.AuthMethodPassword})
	if !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("empty password error = %v, want ErrCredentialMissing", err)
	}
}

func TestBuildAuthMethodsPrivateKey(t *testing.T) {
	_, keyPEM := genKey(t)
	methods, err := buildAuthMethods(Target{AuthMethod: store.AuthMethodPrivateKey, PrivateKey: keyPEM})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("methods = %d, want 1", len(methods))
	}

	enc := genEncryptedKey(t, "hunter2")
	if _, err := buildAuthMethods(Target{
		AuthMethod: store.AuthMethodPrivateKey,
		PrivateKey: enc,
		Passphrase: "hunter2",
	}); err != nil {
		t.Fatalf("encrypted key with correct passphrase: %v", err)
	}
	if _, err := buildAuthMethods(Target{
		AuthMethod: store.AuthMethodPrivateKey,
		PrivateKey: enc,
		Passphrase: "wrong",
	}); err == nil {
		t.Fatal("encrypted key with wrong passphrase should fail")
	}
}

func TestBuildAuthMethodsRejectsUnknown(t *testing.T) {
	if _, err := buildAuthMethods(Target{AuthMethod: "kerberos"}); err == nil {
		t.Fatal("unknown auth method should fail")
	}
}

func TestBuildClientConfig(t *testing.T) {
	cfg, verifier, err := buildClientConfig(Target{
		ServerID:   "srv-1",
		Username:   "deploy",
		AuthMethod: store.AuthMethodPassword,
		Password:   "pw",
	})
	if err != nil {
		t.Fatalf("buildClientConfig: %v", err)
	}
	if cfg.User != "deploy" {
		t.Fatalf("User = %q", cfg.User)
	}
	if cfg.HostKeyCallback == nil {
		t.Fatal("HostKeyCallback must not be nil (InsecureIgnoreHostKey is forbidden)")
	}
	if cfg.ClientVersion != "SSH-2.0-aiops" {
		t.Fatalf("ClientVersion = %q", cfg.ClientVersion)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("Auth = %d methods, want 1", len(cfg.Auth))
	}
	if verifier == nil {
		t.Fatal("verifier is nil")
	}
}

func TestHostKeyTOFU(t *testing.T) {
	signer, _ := genKey(t)
	v := newHostKeyVerifier("srv-1", "")

	if err := v.callback("host:22", nil, signer.PublicKey()); err != nil {
		t.Fatalf("TOFU callback: %v", err)
	}
	if !v.wasFirstSeen() {
		t.Fatal("first_seen should be true on TOFU")
	}
	if got, want := v.fingerprint(), ssh.FingerprintSHA256(signer.PublicKey()); got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
	if !strings.HasPrefix(v.actualLine(), "ssh-ed25519 ") {
		t.Fatalf("host key line = %q, want authorized_keys line", v.actualLine())
	}
}

func TestHostKeyMatchAndMismatch(t *testing.T) {
	signer1, _ := genKey(t)
	signer2, _ := genKey(t)
	trusted := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer1.PublicKey())))

	v := newHostKeyVerifier("srv-1", trusted)
	if err := v.callback("host:22", nil, signer1.PublicKey()); err != nil {
		t.Fatalf("matching key rejected: %v", err)
	}
	if v.wasFirstSeen() {
		t.Fatal("first_seen should be false when a key was already trusted")
	}

	v2 := newHostKeyVerifier("srv-2", trusted)
	err := v2.callback("host:22", nil, signer2.PublicKey())
	if err == nil {
		t.Fatal("changed host key must be rejected")
	}
	var hk *HostKeyError
	if !errors.As(err, &hk) {
		t.Fatalf("error type = %T, want *HostKeyError", err)
	}
	if hk.ServerID != "srv-2" {
		t.Fatalf("ServerID = %q", hk.ServerID)
	}
	if hk.Expected != ssh.FingerprintSHA256(signer1.PublicKey()) {
		t.Fatalf("Expected = %q", hk.Expected)
	}
	if hk.Actual != ssh.FingerprintSHA256(signer2.PublicKey()) {
		t.Fatalf("Actual = %q", hk.Actual)
	}
}

func TestHostKeyUnparseableTrustedKey(t *testing.T) {
	signer, _ := genKey(t)
	v := newHostKeyVerifier("srv-1", "not a key")
	if err := v.callback("host:22", nil, signer.PublicKey()); err == nil {
		t.Fatal("unparseable trusted key must fail closed")
	}
}

func TestDialAddressKeepsDefaultPort(t *testing.T) {
	// 回归：known_hosts 惯例对 22 端口省略端口，knownhosts.Normalize 的
	// 结果绝不能当 TCP 拨号地址，否则 net.Dial 报 missing port。
	if got, want := knownhosts.Normalize("127.0.0.1:22"), "127.0.0.1"; got != want {
		t.Fatalf("knownhosts.Normalize(\"127.0.0.1:22\") = %q, want %q", got, want)
	}
	if got, want := dialAddress(Target{Host: "127.0.0.1", Port: 22}), "127.0.0.1:22"; got != want {
		t.Fatalf("dialAddress(port 22) = %q, want %q", got, want)
	}
	if got, want := dialAddress(Target{Host: "127.0.0.1"}), "127.0.0.1:22"; got != want {
		t.Fatalf("dialAddress(no port) = %q, want %q", got, want)
	}
	if got, want := dialAddress(Target{Host: "10.0.0.1", Port: 2222}), "10.0.0.1:2222"; got != want {
		t.Fatalf("dialAddress(port 2222) = %q, want %q", got, want)
	}
	if got, want := dialAddress(Target{Host: "::1", Port: 22}), "[::1]:22"; got != want {
		t.Fatalf("dialAddress(ipv6) = %q, want %q", got, want)
	}
}

func TestDialDefaultPortUsesPortedAddress(t *testing.T) {
	p := New(Config{DialTimeout: 2 * time.Second, HandshakeTimeout: 2 * time.Second}, nil)
	defer p.Close()

	// 对 127.0.0.1:22 真实拨号。端口未监听时得到 connection refused（错误
	// 文案含 127.0.0.1:22）；有 sshd 时握手/认证失败。两种情况都不允许再
	// 出现修复前的 "missing port in address"。
	rc, _, err := p.dial(context.Background(), Target{
		ServerID:   "srv-dial-22",
		Host:       "127.0.0.1",
		Port:       22,
		Username:   "root",
		AuthMethod: store.AuthMethodPassword,
		Password:   "pw",
	})
	if err == nil {
		// 22 端口恰好可拨通并认证成功（几乎不可能）：连接本身已证明拨号
		// 地址带了端口。
		p.closeRaw(rc)
		return
	}
	if strings.Contains(err.Error(), "missing port in address") {
		t.Fatalf("dial on port 22 must dial a ported address, got: %v", err)
	}
}

func TestConnectionKey(t *testing.T) {
	base := Target{
		ServerID:   "srv-1",
		Host:       "10.0.0.1",
		Port:       22,
		Username:   "root",
		AuthMethod: store.AuthMethodPassword,
	}
	k1 := connectionKey(base)
	if !strings.Contains(k1, "10.0.0.1:22") {
		t.Fatalf("key = %q", k1)
	}

	mutators := []func(*Target){
		func(t *Target) { t.Host = "10.0.0.2" },
		func(t *Target) { t.Port = 2222 },
		func(t *Target) { t.Username = "deploy" },
		func(t *Target) { t.AuthMethod = store.AuthMethodPrivateKey },
		func(t *Target) { t.KnownHostKey = "ssh-ed25519 AAAA" },
	}
	for i, mutate := range mutators {
		tc := base
		mutate(&tc)
		if got := connectionKey(tc); got == k1 {
			t.Fatalf("mutator %d did not change pool key", i)
		}
	}
}

func TestCappedWriter(t *testing.T) {
	w := newCappedWriter(4)
	n, err := w.Write([]byte("abcdefgh"))
	if err != nil || n != 8 {
		t.Fatalf("Write = (%d, %v), want (8, nil) so remote keeps streaming", n, err)
	}
	if got := w.String(); got != "abcd" {
		t.Fatalf("String = %q, want abcd", got)
	}
	if !w.Truncated() {
		t.Fatal("Truncated should be true")
	}

	w2 := newCappedWriter(8)
	if _, err := w2.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if w2.Truncated() {
		t.Fatal("not truncated yet")
	}
	if _, err := io.WriteString(w2, "def"); err != nil {
		t.Fatal(err)
	}
	if w2.String() != "abcdef" || w2.Truncated() {
		t.Fatalf("got %q truncated=%v", w2.String(), w2.Truncated())
	}
}

func TestNewDefaults(t *testing.T) {
	p := New(Config{}, nil)
	defer p.Close()

	if p.cfg.MaxPerServer != 4 {
		t.Fatalf("MaxPerServer = %d", p.cfg.MaxPerServer)
	}
	if p.cfg.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %v", p.cfg.IdleTimeout)
	}
	if p.cfg.MaxConnections != 64 {
		t.Fatalf("MaxConnections = %d", p.cfg.MaxConnections)
	}
	if p.cfg.DialTimeout != 10*time.Second || p.cfg.HandshakeTimeout != 15*time.Second {
		t.Fatalf("timeouts = %v / %v", p.cfg.DialTimeout, p.cfg.HandshakeTimeout)
	}
}

func TestBucketKeyChangeAndInvalidate(t *testing.T) {
	p := New(Config{}, nil)
	defer p.Close()

	b1, err := p.bucketFor("srv-1", "key-a")
	if err != nil {
		t.Fatalf("bucketFor: %v", err)
	}
	again, err := p.bucketFor("srv-1", "key-a")
	if err != nil || again != b1 {
		t.Fatalf("same key should reuse bucket (%v)", err)
	}

	b2, err := p.bucketFor("srv-1", "key-b")
	if err != nil {
		t.Fatalf("bucketFor: %v", err)
	}
	if b2 == b1 {
		t.Fatal("changed key must create a new bucket")
	}
	if !b1.isClosed() {
		t.Fatal("stale bucket should be closed")
	}

	p.Invalidate("srv-1")
	if !b2.isClosed() {
		t.Fatal("Invalidate should close the bucket")
	}
	p.mu.Lock()
	_, exists := p.buckets["srv-1"]
	p.mu.Unlock()
	if exists {
		t.Fatal("Invalidate should remove the bucket")
	}
}

func TestExecuteEmptyCommand(t *testing.T) {
	p := New(Config{}, nil)
	defer p.Close()

	_, err := p.Execute(context.Background(), Target{}, RunRequest{})
	if err == nil {
		t.Fatal("empty command must fail")
	}
}

func TestAcquireClosedPool(t *testing.T) {
	p := New(Config{}, nil)
	_ = p.Close()

	_, _, err := p.acquire(context.Background(), Target{
		ServerID:   "srv-1",
		Host:       "127.0.0.1",
		Port:       22,
		Username:   "root",
		AuthMethod: store.AuthMethodPassword,
		Password:   "pw",
	})
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("acquire on closed pool error = %v, want ErrPoolClosed", err)
	}
}
