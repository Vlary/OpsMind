package auth

import "golang.org/x/crypto/bcrypt"

// generateBcrypt 集中 bcrypt 调用，便于在测试中观察成本参数。
func generateBcrypt(password []byte, cost int) ([]byte, error) {
	return bcrypt.GenerateFromPassword(password, cost)
}
