// profile.go 是五厂商 CLI profile（契约 5.2B 写死）。
//
// PromptRe 是提示符判定的终止正则（用户/特权/配置模式都算「已回到提示符」）；
// MoreRe 是分页提示（--More-- / ---- More ----）用于自动续读与清洗。
package netdev

import "regexp"

// Profile 是一个厂商的 CLI 行为参数。
type Profile struct {
	Brand Brand

	// PromptRe 提示符终止正则（匹配缓冲最后一行）。
	PromptRe *regexp.Regexp
	// LoginUserRe / LoginPassRe 是 telnet 登录提示。
	LoginUserRe *regexp.Regexp
	LoginPassRe *regexp.Regexp
	// PagerOffCmd 是分页关闭命令（发送后丢弃其输出；对不支持的设备
	// 按普通输出处理，失败不中断）。
	PagerOffCmd string
	// ErrorRe 是设备错误指示（命中 → ExitCode=1，输出保留）。
	ErrorRe *regexp.Regexp
	// ProbeCmd 是连接测试探针命令。
	ProbeCmd string
	// EnableVerbs 是特权模式进入命令（enable/super 及厂商别名）：
	// 仅这些命令触发的 Password: 提示才自动应答 EnableSecret。
	EnableVerbs []string
}

// MoreRe 匹配主流厂商分页提示（Cisco --More--、Huawei/H3C ---- More ----）。
var MoreRe = regexp.MustCompile(`-{2,4}\s*More\s*-{2,4}`)

var profiles = map[Brand]*Profile{
	BrandCisco: {
		Brand:       BrandCisco,
		PromptRe:    regexp.MustCompile(`[a-zA-Z0-9._-]+[>#] ?$`),
		LoginUserRe: regexp.MustCompile(`(?i)username: ?$`),
		LoginPassRe: regexp.MustCompile(`(?i)password: ?$`),
		PagerOffCmd: "terminal length 0",
		ErrorRe:     regexp.MustCompile(`% Invalid input|% Incomplete command|% Ambiguous command`),
		ProbeCmd:    "show version",
		EnableVerbs: []string{"enable", "en"},
	},
	BrandHuawei: {
		Brand:       BrandHuawei,
		PromptRe:    regexp.MustCompile(`[a-zA-Z0-9._-]+[>\]] ?$`), // <...> 用户 / [...] 特权配置
		LoginUserRe: regexp.MustCompile(`(?i)username: ?$`),
		LoginPassRe: regexp.MustCompile(`(?i)password: ?$`),
		PagerOffCmd: "screen-length 0 temporary",
		ErrorRe:     regexp.MustCompile(`Error: |Unrecognized command`),
		ProbeCmd:    "display version",
		EnableVerbs: []string{"super"},
	},
	BrandH3C: {
		Brand:       BrandH3C,
		PromptRe:    regexp.MustCompile(`[a-zA-Z0-9._-]+[>\]] ?$`), // 同 huawei
		LoginUserRe: regexp.MustCompile(`(?i)login: ?$`),
		LoginPassRe: regexp.MustCompile(`(?i)password: ?$`),
		PagerOffCmd: "screen-length disable",
		ErrorRe:     regexp.MustCompile(`% Unrecognized command|Error: `),
		ProbeCmd:    "display version",
		EnableVerbs: []string{"super"},
	},
	BrandJunos: {
		Brand:       BrandJunos,
		PromptRe:    regexp.MustCompile(`^(root@.{1,64})[>#] ?$`), // > 操作模式 / # 配置模式
		LoginUserRe: regexp.MustCompile(`(?i)login: ?$`),
		LoginPassRe: regexp.MustCompile(`(?i)password: ?$`),
		PagerOffCmd: "set cli screen-length 0",
		ErrorRe:     regexp.MustCompile(`syntax error|unknown command`),
		ProbeCmd:    "show version",
		EnableVerbs: nil, // 登录即按 class 授权
	},
	BrandGeneric: {
		Brand:       BrandGeneric,
		PromptRe:    regexp.MustCompile(`[a-zA-Z0-9._<>#\[\]-]+[>#\]] ?$`), // 四家并集宽匹配
		LoginUserRe: regexp.MustCompile(`(?i)(login|username|user name): ?$`),
		LoginPassRe: regexp.MustCompile(`(?i)password: ?$`),
		PagerOffCmd: "terminal length 0", // 仅 Cisco 系有效；其余设备按普通输出处理
		ErrorRe:     regexp.MustCompile(`(?i)(invalid|error|unrecognized|unknown command|syntax error)`),
		ProbeCmd:    "show version",
		EnableVerbs: []string{"enable", "super"},
	},
}

// ProfileOf 返回厂商 profile；未知值失败关闭到 generic 保守 profile。
func ProfileOf(b Brand) *Profile {
	if p, ok := profiles[b]; ok {
		return p
	}
	return profiles[BrandGeneric]
}

// isEnableCommand 报告命令（首词）是否为特权模式进入命令（含厂商别名）。
func (p *Profile) isEnableCommand(command string) bool {
	first := command
	for i := 0; i < len(command); i++ {
		if command[i] == ' ' || command[i] == '\t' {
			first = command[:i]
			break
		}
	}
	for _, v := range p.EnableVerbs {
		if first == v {
			return true
		}
	}
	return false
}
