package approvals

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在全部测试结束后检查 goroutine 泄漏。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
