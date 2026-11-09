// Package risk 负责 SSH 命令风险分级。
//
// 分类流程遵循 CONTRACT.md 第 7.4 节：拒绝空/NUL/超长输入 → NFKC 规范化 →
// 切注释 → 按未引用的 shell 分隔符分段 → 递归提取命令替换与子 shell →
// 逐段分词、剥离包装器 → 命中 7.3 节规则表，取最高级别。
//
// 设计原则是失败关闭：无法证明安全的输入一律至少 needs_approval，
// 解析失败或发现规避检测迹象一律 forbidden。
package risk

import (
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Level 是风险级别。
type Level string

const (
	LevelSafe          Level = "safe"
	LevelNeedsApproval Level = "needs_approval"
	LevelForbidden     Level = "forbidden"
)

// OS 是分类目标操作系统：决定规则分册（契约 7.3 / 7.3B / 7.3C）。空串按
// linux 处理（preview 无服务器上下文时的缺省）。
//
// N1：OSNetwork 走 7.3C 网络设备分册——Classify 遇 OSNetwork 等价于
// ClassifyNet(command, NetBrandGeneric)（generic 分册为保守并集，失败
// 关闭）；知道厂商的调用方（agent、preview-risk 带 netdev_brand）必须
// 直接调 ClassifyNet。
type OS string

const (
	OSLinux   OS = "linux"
	OSWindows OS = "windows"
	OSNetwork OS = "network" // N1：仅 device_kind=network_device
)

// NetBrand 是网络设备厂商分册键（契约 7.3C），与 store.NetdevBrand 一一对应。
type NetBrand string

const (
	NetBrandCisco   NetBrand = "cisco"
	NetBrandHuawei  NetBrand = "huawei"
	NetBrandH3C     NetBrand = "h3c"
	NetBrandJunos   NetBrand = "junos"
	NetBrandGeneric NetBrand = "generic"
)

// Decision 是一次分类结果。
//
// RuleID/Reason 是最先命中的最高级别规则；Reasons 汇总全部命中，便于审计与
// 前端展示。追加字段不改变契约既有字段语义。
type Decision struct {
	Level      Level    `json:"risk_level"`
	RuleID     string   `json:"rule_id"`
	Reason     string   `json:"reason"`
	Normalized string   `json:"normalized_command"`
	Reasons    []string `json:"reasons"`
}

const (
	// MaxCommandBytes 是单条命令的硬上限（CONTRACT.md 7.4 第 1 条）。
	MaxCommandBytes = 16384
	// maxDepth 是嵌套分类（命令替换、子 shell、shell -c、ssh、容器 exec）的最大深度。
	maxDepth = 4
)

// Classify 对命令做风险分类；永不返回 error。
//
// os=OSLinux 走第 7.3 节 shell 分册，os=OSWindows 走 7.3B PowerShell/cmd
// 分册，os=OSNetwork 走 7.3C 网络设备分册的 generic 分册（保守并集）；
// 三套规则互不混用。空命令、含 NUL、超过 16384 字节按
// COMMAND_INVALID 失败关闭为 forbidden；分词/引号不闭合等解析失败按
// OBFUSCATION（Windows 为 WIN_OBFUSCATION）失败关闭为 forbidden。
func Classify(command string, os OS) Decision {
	if command == "" {
		return Decision{
			Level:   LevelForbidden,
			RuleID:  "COMMAND_INVALID",
			Reason:  "empty command",
			Reasons: []string{"COMMAND_INVALID: empty command"},
		}
	}
	if len(command) > MaxCommandBytes {
		return Decision{
			Level:   LevelForbidden,
			RuleID:  "COMMAND_INVALID",
			Reason:  "command exceeds 16384 bytes",
			Reasons: []string{"COMMAND_INVALID: command exceeds 16384 bytes"},
		}
	}
	if strings.IndexByte(command, 0) >= 0 {
		return Decision{
			Level:   LevelForbidden,
			RuleID:  "COMMAND_INVALID",
			Reason:  "command contains NUL byte",
			Reasons: []string{"COMMAND_INVALID: command contains NUL byte"},
		}
	}

	if os == OSWindows {
		return classifyWindows(command)
	}
	if os == OSNetwork {
		// 契约 7.3C：无厂商上下文的 network 目标用 generic 保守并集分册；
		// 知道厂商的调用方必须直接调 ClassifyNet。
		return ClassifyNet(command, NetBrandGeneric)
	}

	normalized, err := normalize(command)
	if err != nil {
		return invalidDecision("", err.Error())
	}
	if normalized == "" {
		return Decision{
			Level:   LevelForbidden,
			RuleID:  "COMMAND_INVALID",
			Reason:  "empty command",
			Reasons: []string{"COMMAND_INVALID: empty command"},
		}
	}

	// 与分段无关的检查（fork bomb、$IFS）。
	acc := &accumulator{}
	if hit, ok := globalForbidden(normalized); ok {
		acc.add(LevelForbidden, hit.rule, hit.reason)
	}

	// server_facts 使用的固定只读模板允许直接判定为 safe（仅精确匹配）。
	if !IsAtLeast(acc.level, LevelNeedsApproval) && isFactsTemplate(normalized) {
		return Decision{
			Level:      LevelSafe,
			RuleID:     "SAFE_FACTS",
			Reason:     "fixed read-only facts template",
			Normalized: normalized,
			Reasons:    []string{"SAFE_FACTS: fixed read-only facts template"},
		}
	}

	// 五期 M1：monitor 采集器的固定指标模板（SAFE_COLLECT，契约 7.7）——
	// 整串规范化精确匹配，同 SAFE_FACTS 机制。
	if !IsAtLeast(acc.level, LevelNeedsApproval) && isCollectTemplate(normalized) {
		return Decision{
			Level:      LevelSafe,
			RuleID:     "SAFE_COLLECT",
			Reason:     "fixed read-only metric collect template",
			Normalized: normalized,
			Reasons:    []string{"SAFE_COLLECT: fixed read-only metric collect template"},
		}
	}

	// 五期 M2：巡检固定模板（SAFE_INSPECT，契约 7.7）——逐条独立命令的
	// 整串规范化精确匹配（如 systemctl --failed 自然分类为 DEFAULT_ASK，
	// 巡检模板豁免放行；任何改动走正常分类——失败关闭）。
	if !IsAtLeast(acc.level, LevelNeedsApproval) && isInspectTemplate(normalized) {
		return Decision{
			Level:      LevelSafe,
			RuleID:     "SAFE_INSPECT",
			Reason:     "fixed read-only inspection template command",
			Normalized: normalized,
			Reasons:    []string{"SAFE_INSPECT: fixed read-only inspection template command"},
		}
	}

	segments, parseErr := parseCommand(normalized)
	if parseErr != nil {
		acc.add(LevelForbidden, "OBFUSCATION", parseErr.Error())
		return Decision{
			Level:      acc.level,
			RuleID:     acc.rule,
			Reason:     acc.reason,
			Normalized: normalized,
			Reasons:    append([]string(nil), acc.reasons...),
		}
	}

	for i := range segments {
		classifySegment(segments[i], 0, acc)
	}
	classifyPipelines(segments, acc)

	if acc.len() == 0 {
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "unrecognized command; approval required")
	}

	return Decision{
		Level:      acc.level,
		RuleID:     acc.rule,
		Reason:     acc.reason,
		Normalized: normalized,
		Reasons:    append([]string(nil), acc.reasons...),
	}
}

func invalidDecision(normalized, reason string) Decision {
	return Decision{
		Level:      LevelForbidden,
		RuleID:     "OBFUSCATION",
		Reason:     reason,
		Normalized: normalized,
		Reasons:    []string{"OBFUSCATION: " + reason},
	}
}

// IsAtLeast 判断 level 是否不低于 threshold。
func IsAtLeast(level, threshold Level) bool {
	return rank(level) >= rank(threshold)
}

func rank(l Level) int {
	switch l {
	case LevelForbidden:
		return 3
	case LevelNeedsApproval:
		return 2
	case LevelSafe:
		return 1
	default:
		return 0
	}
}

type accumulator struct {
	level   Level
	rule    string
	reason  string
	reasons []string
}

// add 记录一条命中；更高级别覆盖主规则，同级保留首个（规则表按危险度排序）。
func (a *accumulator) add(level Level, rule, reason string) {
	if level == "" {
		level = LevelNeedsApproval
	}
	a.reasons = append(a.reasons, rule+": "+reason)
	if rank(level) > rank(a.level) {
		a.level = level
		a.rule = rule
		a.reason = reason
	}
}

func (a *accumulator) len() int { return len(a.reasons) }

// ruleHit 是全局检查命中的规则。
type ruleHit struct {
	rule   string
	reason string
}

// isForkBomb 识别经典 :(){ :|:& };: 及任意函数名自调用变体。
// RE2 不支持反向引用，因此手工解析函数定义。
func isForkBomb(compact string) bool {
	if strings.Contains(compact, ":(){:|:&};:") {
		return true
	}
	idx := strings.Index(compact, "(){")
	if idx <= 0 {
		return false
	}
	name := compact[:idx]
	if !isShellIdent(name) {
		return false
	}
	rest := compact[idx+3:]
	end := strings.Index(rest, "};")
	if end < 0 {
		return false
	}
	body := rest[:end]
	if !strings.Contains(body, "|") || !strings.Contains(body, "&") {
		return false
	}
	return strings.Contains(body, name+"|"+name+"&") || strings.Contains(body, name+"&|"+name)
}

func isShellIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// globalForbidden 对规范化文本做与分段无关的检查。
func globalForbidden(normalized string) (ruleHit, bool) {
	compact := strings.Join(strings.Fields(normalized), "")
	if isForkBomb(compact) {
		return ruleHit{"FORK_BOMB", "fork bomb pattern"}, true
	}
	// $IFS 是绕过空格的经典手法，任何位置出现都视为规避检测。
	if strings.Contains(normalized, "$IFS") || strings.Contains(normalized, "${IFS}") {
		return ruleHit{"OBFUSCATION", "IFS variable used to obfuscate arguments"}, true
	}
	return ruleHit{}, false
}

// normalize 执行 NFKC、反斜杠续行合并、注释切除与空白压缩。
//
// 注释只在未引用且位于词首（前一个字符是空白或分隔符）时成立；
// 引号内的空白不压缩，引号原样保留交给分词器处理。
func normalize(input string) (string, error) {
	s := norm.NFKC.String(input)

	var b strings.Builder
	b.Grow(len(s))

	var quote byte // 0, '\'' or '"'
	var prev byte
	var lastOut byte
	put := func(c byte) {
		b.WriteByte(c)
		lastOut = c
	}
	i := 0
	for i < len(s) {
		ch := s[i]
		switch quote {
		case '\'':
			put(ch)
			if ch == '\'' {
				quote = 0
			}
			prev = ch
			i++
			continue
		case '"':
			if ch == '\\' && i+1 < len(s) {
				put(ch)
				b.WriteByte(s[i+1])
				prev = s[i+1]
				i += 2
				continue
			}
			put(ch)
			if ch == '"' {
				quote = 0
			}
			prev = ch
			i++
			continue
		}

		switch ch {
		case '\\':
			if i+1 < len(s) && s[i+1] == '\n' {
				// 反斜杠 + 换行：续行，两个字符都丢弃。
				i += 2
				continue
			}
			put(ch)
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				prev = s[i+1]
				i += 2
				continue
			}
			prev = ch
			i++
		case '\'':
			quote = '\''
			put(ch)
			prev = ch
			i++
		case '"':
			quote = '"'
			put(ch)
			prev = ch
			i++
		case '#':
			if prev == 0 || isSeparatorByte(prev) || prev == ' ' || prev == '\t' {
				// 注释到行尾；保留换行作为分段符由分词器处理。
				for i < len(s) && s[i] != '\n' {
					i++
				}
				prev = '\n'
				continue
			}
			put(ch)
			prev = ch
			i++
		case ' ', '\t', '\r':
			if b.Len() > 0 {
				if lastOut == ' ' || lastOut == '\n' {
					i++
					continue
				}
				put(' ')
			}
			prev = ' '
			i++
		default:
			put(ch)
			prev = ch
			i++
		}
	}
	if quote != 0 {
		return "", errUnterminatedQuote
	}
	// NFKC 可把短输入膨胀到超过 MaxCommandBytes（如连字/全角分解），
	// 入口的原始长度检查拦不住；膨胀超限同样失败关闭。
	if b.Len() > MaxCommandBytes {
		return "", &riskError{"command exceeds 16384 bytes after normalization"}
	}
	return strings.TrimSpace(b.String()), nil
}

// isSeparatorByte 判断字符是否是 shell 分隔符（用于注释判定）。
func isSeparatorByte(ch byte) bool {
	switch ch {
	case ';', '|', '&', '(', ')', '\n':
		return true
	}
	return false
}

var errUnterminatedQuote = &riskError{"unterminated quoted string"}

type riskError struct{ msg string }

func (e *riskError) Error() string { return e.msg }

// underAny 判断路径是否位于指定根之下（含根本身）。
func underAny(p string, roots ...string) bool {
	for _, root := range roots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// cleanTarget 去掉引号后做 path.Clean；保留通配符与变量。
func cleanTarget(t string) string {
	t = strings.TrimSpace(t)
	if len(t) >= 2 && ((t[0] == '"' && t[len(t)-1] == '"') || (t[0] == '\'' && t[len(t)-1] == '\'')) {
		t = t[1 : len(t)-1]
	}
	if t == "" {
		return ""
	}
	if strings.Contains(t, "$") || strings.Contains(t, "~") {
		return t
	}
	return path.Clean(t)
}

// hasUnresolvedVar 判断目标是否含无法静态解析的变量（$HOME/${HOME} 例外）。
func hasUnresolvedVar(s string) bool {
	rest := strings.ReplaceAll(s, "${HOME}", "")
	rest = strings.ReplaceAll(rest, "$HOME", "")
	return strings.Contains(rest, "$")
}
