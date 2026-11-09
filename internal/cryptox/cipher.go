// Package cryptox 提供主密钥加载与 AES-256-GCM 加解密。
//
// 密文格式固定为 v1:<base64(nonce)>:<base64(ciphertext||tag)>，AAD 固定为
// "aiops:v1"。错误信息不包含明文、密文或密钥内容。
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	versionPrefix = "v1"
	aad           = "aiops:v1"
	nonceSize     = 12
	keySize       = 32
	gcmTagSize    = 16
)

var (
	// ErrInvalidMasterKey 表示主密钥长度或编码不合法。
	ErrInvalidMasterKey = errors.New("cryptox: master key must be 32 bytes")
	// ErrInvalidCiphertext 表示密文格式或认证标签不合法。
	ErrInvalidCiphertext = errors.New("cryptox: invalid ciphertext")
)

// Cipher 持有 32 字节 AES-256 主密钥。
type Cipher struct {
	key []byte
}

// New 校验并复制 32 字节主密钥。
func New(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != keySize {
		return nil, ErrInvalidMasterKey
	}
	key := make([]byte, len(masterKey))
	copy(key, masterKey)
	return &Cipher{key: key}, nil
}

// Encrypt 加密明文，输出格式 v1:<b64 nonce>:<b64 ciphertext+tag>。
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("cryptox: init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cryptox: init gcm: %w", err)
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cryptox: generate nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	return versionPrefix + ":" +
		base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密 Encrypt 的输出。
func (c *Cipher) Decrypt(ciphertext string) (string, error) {
	parts := strings.Split(ciphertext, ":")
	if len(parts) != 3 || parts[0] != versionPrefix {
		return "", ErrInvalidCiphertext
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(nonce) != nonceSize {
		return "", ErrInvalidCiphertext
	}
	sealed, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(sealed) < gcmTagSize {
		return "", ErrInvalidCiphertext
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	plain, err := gcm.Open(nil, nonce, sealed, []byte(aad))
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	return string(plain), nil
}

// Mask 返回密钥的展示掩码：有值时 "****"+末 4 位，少于 4 位时为 "****"，
// 空串返回空串。掩码只用于展示，绝不返回明文。
func Mask(secret string) string {
	if secret == "" {
		return ""
	}
	runes := []rune(secret)
	if len(runes) < 4 {
		return "****"
	}
	return "****" + string(runes[len(runes)-4:])
}

// MaskHint 根据已保存的末 4 位提示生成掩码；hint 不足 4 位时返回 "****"。
func MaskHint(hint string) string {
	if utf8.RuneCountInString(hint) < 4 {
		return "****"
	}
	return "****" + hint
}
