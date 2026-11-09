package cryptox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	for _, plain := range []string{"", "secret", "密码 🔐 with unicode", strings.Repeat("x", 4096)} {
		ct, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plain, err)
		}
		if !strings.HasPrefix(ct, "v1:") {
			t.Fatalf("ciphertext prefix = %q, want v1:", ct)
		}
		got, err := c.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plain {
			t.Fatalf("round trip = %q, want %q", got, plain)
		}
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	c := newTestCipher(t)
	a, err := c.Encrypt("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two encryptions produced identical ciphertext")
	}
	pa := strings.Split(a, ":")
	pb := strings.Split(b, ":")
	if len(pa) != 3 || len(pb) != 3 {
		t.Fatalf("unexpected ciphertext format: %q / %q", a, b)
	}
	if pa[1] == pb[1] {
		t.Fatal("nonce was reused")
	}
}

func TestDecryptTampered(t *testing.T) {
	c := newTestCipher(t)
	ct, err := c.Encrypt("top secret")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(ct, ":")
	nonce, _ := base64.StdEncoding.DecodeString(parts[1])
	sealed, _ := base64.StdEncoding.DecodeString(parts[2])

	tamperedNonce := append([]byte(nil), nonce...)
	tamperedNonce[0] ^= 0xff
	tamperedCT := append([]byte(nil), sealed...)
	tamperedCT[0] ^= 0xff
	tamperedTag := append([]byte(nil), sealed...)
	tamperedTag[len(tamperedTag)-1] ^= 0xff

	cases := map[string]string{
		"nonce":   "v1:" + base64.StdEncoding.EncodeToString(tamperedNonce) + ":" + parts[2],
		"body":    "v1:" + parts[1] + ":" + base64.StdEncoding.EncodeToString(tamperedCT),
		"tag":     "v1:" + parts[1] + ":" + base64.StdEncoding.EncodeToString(tamperedTag),
		"version": "v2:" + parts[1] + ":" + parts[2],
		"format":  "not-a-ciphertext",
	}
	for name, bad := range cases {
		if _, err := c.Decrypt(bad); err == nil {
			t.Fatalf("%s: Decrypt accepted tampered ciphertext", name)
		}
	}
}

func TestDecryptWrongKey(t *testing.T) {
	a := newTestCipher(t)
	b := newTestCipher(t)
	ct, err := a.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decrypt(ct); err == nil {
		t.Fatal("Decrypt with wrong key succeeded")
	}
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("New accepted 16-byte key")
	}
	if _, err := New(make([]byte, 33)); err == nil {
		t.Fatal("New accepted 33-byte key")
	}
}

func TestLoadMasterKeyFromEnv(t *testing.T) {
	key := bytes.Repeat([]byte{7}, keySize)
	got, err := LoadMasterKey(t.TempDir(), base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("unexpected key")
	}

	if _, err := LoadMasterKey(t.TempDir(), "not-base64!"); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	if _, err := LoadMasterKey(t.TempDir(), base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestLoadMasterKeyGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadMasterKey(dir, "")
	if err != nil {
		t.Fatalf("first LoadMasterKey: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, masterKeyFile))
	if err != nil {
		t.Fatalf("stat master.key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("master.key permissions = %o, want 600", perm)
	}

	second, err := LoadMasterKey(dir, "")
	if err != nil {
		t.Fatalf("second LoadMasterKey: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second load returned a different key")
	}
}

func TestLoadMasterKeyRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadMasterKey(dir, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, masterKeyFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(dir, ""); err == nil {
		t.Fatal("LoadMasterKey accepted a 0644 key file")
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"abc":      "****",
		"abcd":     "****abcd",
		"sk-12345": "****2345",
		"密钥密钥密钥":   "****密钥密钥",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Fatalf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MaskHint(""); got != "****" {
		t.Fatalf("MaskHint(\"\") = %q", got)
	}
	if got := MaskHint("1234"); got != "****1234" {
		t.Fatalf("MaskHint(1234) = %q", got)
	}
}
