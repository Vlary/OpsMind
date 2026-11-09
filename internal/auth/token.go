package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost 是契约固定的密码哈希成本。
const BcryptCost = 12

// ErrPasswordHashing 表示 bcrypt 输入不合法（例如密码超过 72 字节）。
var ErrPasswordHashing = errors.New("auth: password hashing failed")

// HashPassword 使用 bcrypt cost 12 生成哈希。
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPasswordHashing, err)
	}
	return string(hash), nil
}

// VerifyPassword 校验密码哈希；任何错误都返回 false。
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// NewSessionToken 生成 32 字节随机 token，返回 base64url(raw) 与其 SHA-256。
//
// 数据库只保存 hash；Authenticate 对请求中的 raw token 做同样的 SHA-256。
func NewSessionToken() (raw string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: generate session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	hash = make([]byte, len(sum))
	copy(hash, sum[:])
	return raw, hash, nil
}

// HashToken 返回原始 token 的 SHA-256，供查询与删除使用。
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	out := make([]byte, len(sum))
	copy(out, sum[:])
	return out
}
