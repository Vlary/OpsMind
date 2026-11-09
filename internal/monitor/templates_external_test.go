// templates_external_test.go 是 G30 的模板登记断言（外部测试包：生产代码
// 的 monitor→risk 依赖被包边界禁止，测试侧仅做交叉验证）：monitor 的采集
// 模板与 risk 的 SAFE_COLLECT/WIN_SAFE_COLLECT 匹配集逐字节一致，经
// risk.Classify 判 safe。
package monitor_test

import (
	"testing"

	"aiops/internal/monitor"
	"aiops/internal/risk"
)

func TestCollectTemplatesClassifiedSafe(t *testing.T) {
	// Linux 模板本体。
	if d := risk.Classify(monitor.LinuxCollectTemplate, risk.OSLinux); d.Level != risk.LevelSafe || d.RuleID != "SAFE_COLLECT" {
		t.Fatalf("linux template: level=%s rule=%s, want safe/SAFE_COLLECT", d.Level, d.RuleID)
	}
	// 模板任何改动不得再命中（失败关闭）。
	if d := risk.Classify(monitor.LinuxCollectTemplate+"; rm -rf /", risk.OSLinux); d.Level != risk.LevelForbidden {
		t.Fatalf("tampered template must not be safe, got %s", d.Level)
	}
	// Windows 模板本体与 SSH 通道外壳包装形态。
	if d := risk.Classify(monitor.WindowsCollectTemplate, risk.OSWindows); d.Level != risk.LevelSafe || d.RuleID != "WIN_SAFE_COLLECT" {
		t.Fatalf("windows template: level=%s rule=%s, want safe/WIN_SAFE_COLLECT", d.Level, d.RuleID)
	}
	wrapped := "powershell -NoProfile -NonInteractive -Command " + monitor.WindowsCollectTemplate
	if d := risk.Classify(wrapped, risk.OSWindows); d.Level != risk.LevelSafe {
		t.Fatalf("wrapped windows template: level=%s rule=%s", d.Level, d.RuleID)
	}
}

// TestCollectTemplateStringsMatchRisk 契约 7.7 模板与 risk 登记串逐字节一致。
func TestCollectTemplateStringsMatchRisk(t *testing.T) {
	if monitor.LinuxCollectTemplate != risk.LinuxCollectCommand {
		t.Fatal("linux collect template diverges between monitor and risk")
	}
	if monitor.WindowsCollectTemplate != risk.WinCollectCommand {
		t.Fatal("windows collect template diverges between monitor and risk")
	}
}
