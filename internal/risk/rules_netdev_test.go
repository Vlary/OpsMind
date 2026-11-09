// rules_netdev_test.go 覆盖 CONTRACT.md 7.3C 网络设备五分册：每品牌
// safe/needs_approval/forbidden 各若干（每品牌 ≥8 例，含缩写/别名绕过）、
// cisco 缩写归一、huawei/h3c/junos 显式别名、跨分册词表不串、注入形态
// 失败关闭、Classify(OSNetwork) 等价 ClassifyNet(generic)。
package risk

import (
	"strings"
	"testing"
)

// netCase 是网络设备分册表驱动用例：wantRule 为空表示只断言级别。
type netCase struct {
	command  string
	wantRule string
}

func runNetCases(t *testing.T, brand NetBrand, level Level, cases []netCase) {
	t.Helper()
	for _, tc := range cases {
		d := ClassifyNet(tc.command, brand)
		if d.Level != level {
			t.Errorf("ClassifyNet(%q, %s).Level = %s (%s: %s), want %s; reasons=%v",
				tc.command, brand, d.Level, d.RuleID, d.Reason, level, d.Reasons)
			continue
		}
		wantRule := tc.wantRule
		if wantRule != "" {
			prefix := netProfileFor(brand).prefix
			if !strings.HasPrefix(wantRule, prefix) {
				wantRule = prefix + wantRule
			}
			if d.RuleID != wantRule {
				t.Errorf("ClassifyNet(%q, %s).RuleID = %s, want %s", tc.command, brand, d.RuleID, wantRule)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// cisco（含唯一前缀缩写归一）
// ---------------------------------------------------------------------------

func TestNetCiscoSafe(t *testing.T) {
	runNetCases(t, NetBrandCisco, LevelSafe, []netCase{
		{"show version", "SAFE_READ"},
		{"sh ver", "SAFE_READ"}, // 缩写：sh→show、ver→version
		{"show ip interface brief", "SAFE_READ"},
		{"show interfaces", "SAFE_READ"},
		{"show interfaces | include up", "SAFE_READ"}, // 输出过滤修饰符
		{"show version | inc begin-rel", ""},          // inc→include 归一后仍 safe
		{"show cdp neighbors", "SAFE_READ"},
		{"show mac address-table", "SAFE_READ"},
		{"show tech-support", "SAFE_READ"},
		{"ping 10.0.0.1", "SAFE_PING"},
		{"traceroute 10.0.0.1", "SAFE_PING"},
		{"show ip interface brief | exclude unassigned", "SAFE_READ"}, // facts 模板（白名单子集）
	})
}

func TestNetCiscoNeedsApproval(t *testing.T) {
	runNetCases(t, NetBrandCisco, LevelNeedsApproval, []netCase{
		{"show running-config", "CONFIG_DUMP"},
		{"show running-config | include password", "CONFIG_DUMP"}, // 过滤不降级
		{"show startup-config", "CONFIG_DUMP"},
		{"more flash:backup.cfg", "CONFIG_DUMP"},
		{"conf t", "CONF_MODE"}, // 缩写：conf→configure、t 无法唯一归一？
		{"enable", "ENABLE_MODE"},
		{"en", "ENABLE_MODE"}, // 缩写绕过：en→enable
		{"enable 15", "ENABLE_MODE"},
		{"wri mem", "CONFIG_SAVE"}, // 缩写：wri→write、mem→memory
		{"copy running-config tftp:", "CONFIG_SAVE"},
		{"copy tftp: running-config", "CONFIG_LOAD"},
		{"monitor capture cap1", "CAPTURE"},
		{"interface GigabitEthernet0/1", "DEFAULT_ASK"},
		{"no shutdown", "DEFAULT_ASK"},
		{"vlan 10", "DEFAULT_ASK"},
		{"enable secret hunter2", "DEFAULT_ASK"}, // 配置态形态不按 ENABLE_MODE 放行
	})
}

func TestNetCiscoForbidden(t *testing.T) {
	runNetCases(t, NetBrandCisco, LevelForbidden, []netCase{
		{"reload", "RELOAD"},
		{"relo", "RELOAD"}, // 缩写绕过
		{"reload in 10", "RELOAD"},
		{"write erase", "FACTORY_RESET"},
		{"wr er", "FACTORY_RESET"}, // 缩写绕过
		{"erase startup-config", "FACTORY_RESET"},
		{"eras st", "FACTORY_RESET"}, // 缩写绕过
		{"format flash:", "FACTORY_RESET"},
		{"erase flash:", "FACTORY_RESET"},
		{"boot system flash:c2900.bin", "BOOT_CHANGE"},
		{"boot-end-marker", "BOOT_CHANGE"},
		{"reload; show version", "INVALID"},   // 分段企图
		{"show version $(whoami)", "INVALID"}, // 命令替换
		{"`reload`", "INVALID"},               // 反引号
		{"show version > flash:x", "INVALID"}, // 重定向注入
	})
}

// TestNetCiscoAbbrevAmbiguity 验证歧义缩写不归一（多候选落 DEFAULT_ASK）。
func TestNetCiscoAbbrevAmbiguity(t *testing.T) {
	runNetCases(t, NetBrandCisco, LevelNeedsApproval, []netCase{
		// c 是 clock/cdp/copy/capture/configure 等多个保留字的公共前缀。
		{"c", "DEFAULT_ASK"},
		// c 同时是 cdp/clock 的前缀（show 子树多候选，不归一）。
		{"show c", "DEFAULT_ASK"},
	})
}

// ---------------------------------------------------------------------------
// huawei / h3c（显式别名 sy→system-view；不支持任意缩写）
// ---------------------------------------------------------------------------

func TestNetHuaweiSafe(t *testing.T) {
	runNetCases(t, NetBrandHuawei, LevelSafe, []netCase{
		{"display version", "SAFE_READ"},
		{"display device", "SAFE_READ"},
		{"display ip interface brief", "SAFE_READ"},
		{"display ip routing-table", "SAFE_READ"},
		{"display lldp neighbor", "SAFE_READ"},
		{"display memory-usage", "SAFE_READ"}, // 白名单 memory 前缀
		{"display cpu-usage", "SAFE_READ"},
		{"ping 10.0.0.1", "SAFE_PING"},
		{"tracert 10.0.0.1", "SAFE_PING"},
		{"display ip interface brief", "SAFE_READ"}, // facts 模板（白名单子集）
	})
}

func TestNetHuaweiNeedsApproval(t *testing.T) {
	runNetCases(t, NetBrandHuawei, LevelNeedsApproval, []netCase{
		{"display current-configuration", "CONFIG_DUMP"},
		{"display saved-configuration", "CONFIG_DUMP"},
		{"display current-configuration | include password", "CONFIG_DUMP"},
		{"sy", "CONF_MODE"}, // 显式别名
		{"system-view", "CONF_MODE"},
		{"super 3", "ENABLE_MODE"},
		{"super", "ENABLE_MODE"},
		{"save", "CONFIG_SAVE"},
		{"save tftp://10.0.0.9/vrpcfg.zip", "CONFIG_SAVE"},
		{"restore flash:backup.zip", "CONFIG_LOAD"},
		{"capture interface GigabitEthernet0/0/1", "CAPTURE"},
		{"packet-capture", "CAPTURE"},
		{"undo stp enable", "DEFAULT_ASK"},
		{"interface GigabitEthernet0/0/1", "DEFAULT_ASK"},
	})
}

func TestNetHuaweiForbidden(t *testing.T) {
	runNetCases(t, NetBrandHuawei, LevelForbidden, []netCase{
		{"reboot", "RELOAD"},
		{"reboot fast", "RELOAD"},
		{"reset saved-configuration", "FACTORY_RESET"},
		{"boot-loader flash:/vrpv8.cc", "BOOT_CHANGE"},
		{"boot system flash:/vrpv8.cc", "BOOT_CHANGE"},
		{"reboot; display version", "INVALID"},
		{"display version $x", "INVALID"},
	})
}

// TestNetHuaweiNoAbbrev 验证 cisco 式缩写发给 huawei 分册不会因缩写漏判：
// disp/disp ver 落 DEFAULT_ASK（needs_approval 失败关闭），reboot 缩写 reb
// 不是完整保留字也不归一。
func TestNetHuaweiNoAbbrev(t *testing.T) {
	runNetCases(t, NetBrandHuawei, LevelNeedsApproval, []netCase{
		{"disp ver", "DEFAULT_ASK"},
		{"dis version", "DEFAULT_ASK"},
		{"reb", "DEFAULT_ASK"}, // 不归一 → 不是 reboot → 审批兜底
	})
}

func TestNetH3CSafeAndApproval(t *testing.T) {
	runNetCases(t, NetBrandH3C, LevelSafe, []netCase{
		{"display version", "SAFE_READ"},
		{"display interface", "SAFE_READ"},
		{"display mac-address", "SAFE_READ"},
		{"display health", "SAFE_READ"},
		{"ping 192.168.1.1", "SAFE_PING"},
		{"tracert 192.168.1.1", "SAFE_PING"},
	})
	runNetCases(t, NetBrandH3C, LevelNeedsApproval, []netCase{
		{"display current-configuration", "CONFIG_DUMP"},
		{"display saved-configuration | include snmp", "CONFIG_DUMP"},
		{"sy", "CONF_MODE"},
		{"super", "ENABLE_MODE"},
		{"save", "CONFIG_SAVE"},
		{"restore flash:x.cfg", "CONFIG_LOAD"},
		{"packet-capture", "CAPTURE"},
		// h3c 无 `capture` 拼写（契约 7.3C：h3c 仅 packet-capture）。
		{"capture interface GigabitEthernet1/0/1", "DEFAULT_ASK"},
		{"undo info-center enable", "DEFAULT_ASK"},
	})
}

func TestNetH3CForbidden(t *testing.T) {
	runNetCases(t, NetBrandH3C, LevelForbidden, []netCase{
		{"reboot", "RELOAD"},
		{"reset saved-configuration", "FACTORY_RESET"},
		{"boot-loader flash:/boot.bin", "BOOT_CHANGE"},
		{"boot system flash:/boot.bin", "BOOT_CHANGE"},
		{"reset saved-configuration; sy", "INVALID"},
	})
}

// ---------------------------------------------------------------------------
// junos（显式别名 conf→configure；无 enable/super/capture）
// ---------------------------------------------------------------------------

func TestNetJunosSafe(t *testing.T) {
	runNetCases(t, NetBrandJunos, LevelSafe, []netCase{
		{"show version", "SAFE_READ"},
		{"show chassis hardware", "SAFE_READ"},
		{"show interfaces terse", "SAFE_READ"},
		{"show system uptime", "SAFE_READ"},
		{"show route", "SAFE_READ"},
		{"show ntp associations", "SAFE_READ"},
		{"ping 10.0.0.1", "SAFE_PING"},
		{"traceroute 10.0.0.1", "SAFE_PING"},
		{"show interfaces terse", "SAFE_READ"}, // facts 模板（白名单子集）
	})
}

func TestNetJunosNeedsApproval(t *testing.T) {
	runNetCases(t, NetBrandJunos, LevelNeedsApproval, []netCase{
		{"show configuration", "CONFIG_DUMP"},
		{"show system configuration", "CONFIG_DUMP"},
		{"show log messages", "CONFIG_DUMP"},
		{"conf", "CONF_MODE"}, // 显式别名
		{"configure", "CONF_MODE"},
		{"configure exclusive", "CONF_MODE"},
		{"configure private", "CONF_MODE"},
		{"commit", "CONFIG_SAVE"},
		{"commit confirmed 5", "CONFIG_SAVE"},
		{"commit at 02:00", "CONFIG_SAVE"},
		{"rollback 1", "CONFIG_SAVE"},
		{"load override /var/tmp/juniper.conf", "CONFIG_LOAD"},
		{"load merge delta.json", "CONFIG_LOAD"},
		{"load set x.set", "CONFIG_LOAD"},
		{"set interfaces xe-0/0/0 description uplink", "DEFAULT_ASK"},
		{"delete interfaces xe-0/0/0", "DEFAULT_ASK"},
		{"edit protocols", "DEFAULT_ASK"},
		{"exit", "DEFAULT_ASK"},
		{"quit", "DEFAULT_ASK"},
		// junos 无 enable/super：不落 ENABLE_MODE，落 DEFAULT_ASK。
		{"enable", "DEFAULT_ASK"},
	})
}

func TestNetJunosForbidden(t *testing.T) {
	runNetCases(t, NetBrandJunos, LevelForbidden, []netCase{
		{"request system reboot", "RELOAD"},
		{"request system reboot at 02:00", "RELOAD"},
		{"request system halt", "RELOAD"},
		{"request system power-off", "RELOAD"},
		{"request system zeroize", "FACTORY_RESET"},
		{"load factory-default", "FACTORY_RESET"},
		{"set system boot-loader primary boot-loader", "BOOT_CHANGE"},
		{"commit; show version", "INVALID"},
	})
}

// ---------------------------------------------------------------------------
// generic（保守并集：cisco 前缀归一 + sy/conf 别名 + 四家词表）
// ---------------------------------------------------------------------------

func TestNetGenericSafe(t *testing.T) {
	runNetCases(t, NetBrandGeneric, LevelSafe, []netCase{
		{"show version", "SAFE_READ"},
		{"display version", "SAFE_READ"},
		{"sh ver", "SAFE_READ"}, // cisco 归一在 generic 同样生效
		{"display device", "SAFE_READ"},
		{"show chassis hardware", "SAFE_READ"},
		{"display cpu-usage", "SAFE_READ"},
		{"ping 10.0.0.1", "SAFE_PING"},
		{"tracert 10.0.0.1", "SAFE_PING"},
	})
}

func TestNetGenericNeedsApproval(t *testing.T) {
	runNetCases(t, NetBrandGeneric, LevelNeedsApproval, []netCase{
		{"show running-config", "CONFIG_DUMP"},
		{"display current-configuration", "CONFIG_DUMP"},
		{"show configuration", "CONFIG_DUMP"},
		{"show log", "CONFIG_DUMP"},
		{"conf t", "CONF_MODE"},
		{"sy", "CONF_MODE"},
		{"system-view", "CONF_MODE"},
		{"configure", "CONF_MODE"},
		{"en", "ENABLE_MODE"}, // cisco 归一
		{"super 4", "ENABLE_MODE"},
		{"save", "CONFIG_SAVE"},
		{"commit", "CONFIG_SAVE"},
		{"rollback 2", "CONFIG_SAVE"},
		{"restore x.cfg", "CONFIG_LOAD"},
		{"load override x", "CONFIG_LOAD"},
		{"monitor capture c", "CAPTURE"},
		{"capture", "CAPTURE"},
		{"packet-capture", "CAPTURE"},
		{"interface Ten-GigabitEthernet1/0/1", "DEFAULT_ASK"},
		{"undo nat policy", "DEFAULT_ASK"},
	})
}

func TestNetGenericForbidden(t *testing.T) {
	runNetCases(t, NetBrandGeneric, LevelForbidden, []netCase{
		{"reload", "RELOAD"},
		{"relo", "RELOAD"},
		{"reboot", "RELOAD"},
		{"request system halt", "RELOAD"},
		{"write erase", "FACTORY_RESET"},
		{"wr er", "FACTORY_RESET"},
		{"erase startup-config", "FACTORY_RESET"},
		{"reset saved-configuration", "FACTORY_RESET"},
		{"request system zeroize", "FACTORY_RESET"},
		{"load factory-default", "FACTORY_RESET"},
		{"boot system flash:x", "BOOT_CHANGE"},
		{"boot-loader flash:/x", "BOOT_CHANGE"},
		{"set system boot-loader x", "BOOT_CHANGE"},
		{"format flash:", "FACTORY_RESET"},
	})
}

// ---------------------------------------------------------------------------
// 失败关闭与入口等价性
// ---------------------------------------------------------------------------

// TestNetInvalidFailClosed 覆盖空/NUL/超长/注入形态的失败关闭。
func TestNetInvalidFailClosed(t *testing.T) {
	cases := []string{
		"",
		"\x00reload",
		strings.Repeat("a", 16385),
		"reload && show version",
		"show version | include `secret`",
		"show version ${x}",
		"copy tftp: running-config; reload",
	}
	for _, cmd := range cases {
		for _, brand := range []NetBrand{NetBrandCisco, NetBrandHuawei, NetBrandH3C, NetBrandJunos, NetBrandGeneric} {
			d := ClassifyNet(cmd, brand)
			if d.Level != LevelForbidden {
				t.Errorf("ClassifyNet(%q, %s).Level = %s, want forbidden", cmd, brand, d.Level)
			}
		}
	}
	// NFKC 先行：全角变体归一后同样命中注入字符检查（反引号/＄/；）。
	for _, cmd := range []string{"show version ＄x", "reload＆", "show version；"} {
		if d := ClassifyNet(cmd, NetBrandGeneric); d.Level != LevelForbidden {
			t.Errorf("ClassifyNet(%q, generic) = %s, want forbidden (fullwidth metachar)", cmd, d.Level)
		}
	}
}

// TestClassifyOSNetworkRouting 验证 Classify 遇 os=network 等价
// ClassifyNet(command, NetBrandGeneric)（契约 7.3C / 5.2B 注释）。
func TestClassifyOSNetworkRouting(t *testing.T) {
	for _, cmd := range []string{"reload", "show version", "sh ver", "conf t", "vlan 10", "wr er"} {
		viaClassify := Classify(cmd, OSNetwork)
		viaNet := ClassifyNet(cmd, NetBrandGeneric)
		if viaClassify.Level != viaNet.Level || viaClassify.RuleID != viaNet.RuleID {
			t.Errorf("Classify(%q, network) = %+v, want ClassifyNet(generic) = %+v",
				cmd, viaClassify, viaNet)
		}
	}
	// 分册互不混用：linux 危险命令在网络设备分册不生效（落 DEFAULT_ASK），
	// 网络设备命令在 linux 分册不产生 NET_ 前缀规则。
	if d := ClassifyNet("rm -rf /", NetBrandCisco); d.Level != LevelNeedsApproval ||
		d.RuleID != "NET_CISCO_DEFAULT_ASK" {
		t.Errorf("ClassifyNet(rm -rf /, cisco) = %s/%s, want needs_approval/NET_CISCO_DEFAULT_ASK",
			d.Level, d.RuleID)
	}
	if d := Classify("write erase", OSLinux); strings.HasPrefix(d.RuleID, "NET_") {
		t.Errorf("Classify(write erase, linux) produced network rule %s", d.RuleID)
	}
}

// TestNetCrossBrandMiss 验证跨分册词表不串：把 cisco 词发给 huawei 分册
// miss 落 DEFAULT_ASK（保守方向），不产生 cisco 前缀的 RuleID。
func TestNetCrossBrandMiss(t *testing.T) {
	d := ClassifyNet("reload", NetBrandHuawei)
	if d.Level != LevelNeedsApproval || d.RuleID != "NET_HUAWEI_DEFAULT_ASK" {
		t.Errorf("ClassifyNet(reload, huawei) = %s/%s, want needs_approval/NET_HUAWEI_DEFAULT_ASK",
			d.Level, d.RuleID)
	}
	d = ClassifyNet("display version", NetBrandCisco)
	if d.Level != LevelNeedsApproval || d.RuleID != "NET_CISCO_DEFAULT_ASK" {
		t.Errorf("ClassifyNet(display version, cisco) = %s/%s, want needs_approval/NET_CISCO_DEFAULT_ASK",
			d.Level, d.RuleID)
	}
}

// TestNetUnknownBrandFailClosed 未知品牌值失败关闭到 generic 并集。
func TestNetUnknownBrandFailClosed(t *testing.T) {
	d := ClassifyNet("wr er", NetBrand("nokia"))
	if d.Level != LevelForbidden || d.RuleID != "NET_GENERIC_FACTORY_RESET" {
		t.Errorf("ClassifyNet(wr er, nokia) = %s/%s, want forbidden/NET_GENERIC_FACTORY_RESET",
			d.Level, d.RuleID)
	}
}

// TestNetDecisionFields 校验 Decision 字段完整性（Normalized 为归一化串）。
func TestNetDecisionFields(t *testing.T) {
	d := ClassifyNet("  SH   VER ", NetBrandCisco)
	if d.Normalized != "show version" {
		t.Errorf("Normalized = %q, want %q", d.Normalized, "show version")
	}
	if d.Level != LevelSafe || d.RuleID != "NET_CISCO_SAFE_READ" {
		t.Errorf("ClassifyNet(SH VER, cisco) = %s/%s, want safe/NET_CISCO_SAFE_READ", d.Level, d.RuleID)
	}
	if len(d.Reasons) == 0 || d.Reason == "" {
		t.Errorf("decision missing reason fields: %+v", d)
	}
}
