package sshx

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"aiops/internal/store"
)

// KnownHostKeyFingerprint 返回 SHA256 指纹；nil 返回空串。
func KnownHostKeyFingerprint(key ssh.PublicKey) string {
	if key == nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

// hostKeyVerifier 实现 TOFU 主机密钥校验。
//
// 首次连接（expected 为空）接受任意密钥并记录；之后把远端密钥与已信任的
// authorized_keys 行逐字节比较，不一致返回 *HostKeyError。
type hostKeyVerifier struct {
	serverID string
	expected string

	mu     sync.Mutex
	key    ssh.PublicKey
	line   string
	actual string
	first  bool
}

func newHostKeyVerifier(serverID, expected string) *hostKeyVerifier {
	return &hostKeyVerifier{serverID: serverID, expected: strings.TrimSpace(expected)}
}

// callback 是 ssh.HostKeyCallback。
func (v *hostKeyVerifier) callback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	v.mu.Lock()
	v.key = key
	v.line = line
	v.actual = ssh.FingerprintSHA256(key)
	v.mu.Unlock()

	if v.expected == "" {
		v.mu.Lock()
		v.first = true
		v.mu.Unlock()
		return nil
	}

	expectedKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(v.expected))
	if err != nil {
		return &HostKeyError{
			ServerID: v.serverID,
			Expected: "unparseable known host key",
			Actual:   ssh.FingerprintSHA256(key),
		}
	}
	if !bytes.Equal(expectedKey.Marshal(), key.Marshal()) {
		return &HostKeyError{
			ServerID: v.serverID,
			Expected: ssh.FingerprintSHA256(expectedKey),
			Actual:   ssh.FingerprintSHA256(key),
		}
	}
	return nil
}

func (v *hostKeyVerifier) fingerprint() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.key == nil {
		return ""
	}
	return ssh.FingerprintSHA256(v.key)
}

func (v *hostKeyVerifier) actualLine() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.line
}

func (v *hostKeyVerifier) wasFirstSeen() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.first
}

// buildClientConfig 构造 ssh.ClientConfig 与主机密钥校验器。
//
// 该函数只依赖 Target，便于单测在不连接真实主机的情况下校验认证参数。
func buildClientConfig(target Target) (*ssh.ClientConfig, *hostKeyVerifier, error) {
	auth, err := buildAuthMethods(target)
	if err != nil {
		return nil, nil, err
	}
	verifier := newHostKeyVerifier(target.ServerID, target.KnownHostKey)
	cfg := &ssh.ClientConfig{
		User:            target.Username,
		Auth:            auth,
		HostKeyCallback: verifier.callback,
		ClientVersion:   "SSH-2.0-aiops",
	}
	return cfg, verifier, nil
}

// buildAuthMethods 根据认证方式构造认证方法；错误信息不包含凭据内容。
func buildAuthMethods(target Target) ([]ssh.AuthMethod, error) {
	switch target.AuthMethod {
	case store.AuthMethodPassword:
		if target.Password == "" {
			return nil, fmt.Errorf("%w: password required", ErrCredentialMissing)
		}
		return []ssh.AuthMethod{ssh.Password(target.Password)}, nil

	case store.AuthMethodPrivateKey:
		if strings.TrimSpace(target.PrivateKey) == "" {
			return nil, fmt.Errorf("%w: private key required", ErrCredentialMissing)
		}
		var (
			signer ssh.Signer
			err    error
		)
		if target.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(target.PrivateKey), []byte(target.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(target.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("sshx: parse private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil

	default:
		return nil, fmt.Errorf("sshx: unsupported auth method %q", target.AuthMethod)
	}
}
