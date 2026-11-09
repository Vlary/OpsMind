// severity_test.go 覆盖六期 BE-P 的 needs_approval 严重度分档（契约
// 7.3E）：warning 集逐册抽查 + 未列即 high（失败关闭）。
package risk

import "testing"

func TestSeverityOfWarningSet(t *testing.T) {
	warning := []string{
		// Linux 分册（7.3）。
		"ENV_DUMP", "PROCESS_SIGNAL", "CONTAINER_LOGS",
		// Windows 分册（7.3B）。
		"WIN_ENV_DUMP", "WIN_PROCESS_CONTROL", "WIN_CONTAINER_LOGS",
	}
	// 网络设备分册（7.3C）：五前缀 × 四语义全枚举（25 ID 集中的 20 个）。
	for _, p := range []string{"NET_CISCO_", "NET_HUAWEI_", "NET_H3C_", "NET_JUNOS_", "NET_GENERIC_"} {
		warning = append(warning,
			p+"CONFIG_DUMP", p+"ENABLE_MODE", p+"CONF_MODE", p+"CAPTURE")
	}
	for _, rule := range warning {
		if got := SeverityOf(rule); got != SeverityWarning {
			t.Errorf("SeverityOf(%s) = %s, want warning", rule, got)
		}
	}
}

func TestSeverityOfHighFallback(t *testing.T) {
	// 未列即 high（契约 7.3E：一切变更与未知，失败关闭）。
	high := []string{
		"SERVICE_CHANGE", "PACKAGE_CHANGE", "FILE_WRITE", "RM_ANY",
		"CONTAINER_CHANGE", "CONTAINER_REMOTE",
		"WIN_SERVICE_CHANGE", "WIN_REG_WRITE", "WIN_ENCODED_EXEC",
		"NET_CISCO_CONFIG_SAVE", "NET_HUAWEI_CONFIG_LOAD",
		"DEFAULT_ASK", "WIN_DEFAULT_ASK", "NET_GENERIC_DEFAULT_ASK",
		"", "SOMETHING_UNKNOWN",
	}
	for _, rule := range high {
		if got := SeverityOf(rule); got != SeverityHigh {
			t.Errorf("SeverityOf(%q) = %s, want high", rule, got)
		}
	}
}
