// rules_netdev.go 实现 CONTRACT.md 7.3C 的网络设备五分册（N1）。
//
// 网络设备命令按厂商 CLI 语义规范化后**整体分类单条命令**（网络设备 CLI
// 的 `|` 是输出过滤修饰符 begin|include|exclude|section，不是进程管道）。
// 五分册规则语义同构（同一语义各分册 RuleID 只换前缀 NET_CISCO_ /
// NET_HUAWEI_ / NET_H3C_ / NET_JUNIPER_ / NET_GENERIC_）；generic 分册是
// 四家命令词的保守并集。Linux/Windows 分册的规则在网络设备目标上不生效，
// 反之亦然。
//
// 防绕过（写死）：
//   - Cisco 支持任意唯一前缀缩写：cisco/generic 分册对命令词采用保留字
//     前缀归一（命令词若是本分册任一保留字的唯一前缀，视同该保留字）——
//     `sh`→`show`、`conf t`→`configure terminal`、`wri mem`→`write memory`、
//     `eras st`→`erase startup-config`、`relo`→`reload`、`en`→`enable`。
//     前缀有多候选时不归一（落 DEFAULT_ASK，保守方向只会更严）。
//   - huawei/h3c/junos 不支持任意缩写，仅展开显式别名：huawei `sy`→
//     `system-view`、junos `conf`→`configure`。generic 做前缀归一 + 两家
//     属显式别名，保留字取四家并集。
//   - 网络设备 CLI 无 shell 展开语义：出现反引号、`$`、`;`、`&`、`<`、`>`
//     一律按注入企图失败关闭为 forbidden（INVALID）。
//
// 失败关闭：未知命令一律 DEFAULT_ASK（needs_approval）；无法解析 forbidden。
package risk

import (
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// netProfile 是一个厂商分册的命令词表（契约 7.3C 规则语义表 + 命令词映射）。
type netProfile struct {
	prefix string // RuleID 前缀（NET_CISCO_ 等）

	view      []string // 只读查看首词（cisco/junos: show；huawei/h3c: display；generic: 两者）
	pingWords []string // SAFE_PING 词（ping/traceroute；huawei/h3c 另有 tracert）
	readAllow []string // SAFE_READ 子命令白名单（前缀匹配；不含 dump 子命令）
	dumpWords []string // CONFIG_DUMP 子命令（running-config 等，含 more flash: 类前缀词）

	enableVerbs  []string   // ENABLE_MODE 动词（enable/super；junos 无）
	confMode     [][]string // CONF_MODE 短语
	saveWords    [][]string // CONFIG_SAVE 短语
	loadWords    [][]string // CONFIG_LOAD 短语
	captureWords [][]string // CAPTURE 短语
	reloadWords  [][]string // RELOAD 短语
	factoryWords [][]string // FACTORY_RESET 短语
	bootWords    [][]string // BOOT_CHANGE 短语

	factsTemplates []string // SAFE_FACTS 固定模板（规范化形式，见 netFactsCommandSets）

	// abbrev 启用 cisco 式保留字唯一前缀归一；aliases 是显式别名表
	//（huawei sy→system-view、junos conf→configure）。
	abbrev bool
	alias  map[string]string

	// lexicon 由 init 从上方词表派生：全部保留字（短语逐词展开去重）。
	// verbs 是命令动词集（全部短语首词 + 查看/诊断/enable 动词），供首词
	// 位置的唯一前缀归一（避免 `en` 被白名单子命令 `env` 打成歧义——
	// 契约 7.3C：`en`→`enable`）。subtree 是动词的第二词候选表（命令树
	// 语义：`conf t` 的 `t` 在 configure 子树内唯一 → terminal）。
	lexicon []string
	verbs   []string
	subtree map[string][]string
}

// phrase 把空格分隔的短语拆成词序列。
func phrase(s string) []string { return strings.Fields(s) }

// phrases 批量拆分。
func phrases(list ...string) [][]string {
	out := make([][]string, 0, len(list))
	for _, s := range list {
		out = append(out, phrase(s))
	}
	return out
}

// buildLexicon 从词表派生归一保留字全集（短语逐词展开、去重、保序）。
func (p *netProfile) buildLexicon() *netProfile {
	var words []string
	for _, w := range p.view {
		words = append(words, phrase(w)...)
	}
	for _, w := range p.pingWords {
		words = append(words, phrase(w)...)
	}
	for _, w := range p.readAllow {
		words = append(words, phrase(w)...)
	}
	for _, w := range p.dumpWords {
		words = append(words, phrase(w)...)
	}
	for _, w := range p.enableVerbs {
		words = append(words, phrase(w)...)
	}
	for _, grp := range [][][]string{p.confMode, p.saveWords, p.loadWords, p.captureWords, p.reloadWords, p.factoryWords, p.bootWords} {
		for _, ph := range grp {
			words = append(words, ph...)
		}
	}
	for _, f := range p.factsTemplates {
		words = append(words, phrase(f)...)
	}
	// 输出过滤修饰符参与唯一前缀归一（`| inc x` 的 inc→include）。
	words = append(words, netFilterVerbs...)

	seen := make(map[string]bool, len(words))
	lex := make([]string, 0, len(words))
	for _, w := range words {
		if w == "" || w == "|" || seen[w] {
			continue
		}
		seen[w] = true
		lex = append(lex, w)
	}
	p.lexicon = lex

	// 动词集：全部规则短语首词 + 查看首词 + ping/traceroute + enable 动词
	//+ facts 模板首词（show/display 已含）。
	verbSeen := map[string]bool{}
	var verbs []string
	addVerb := func(v string) {
		if v != "" && v != "|" && !verbSeen[v] {
			verbSeen[v] = true
			verbs = append(verbs, v)
		}
	}
	for _, w := range p.view {
		addVerb(w)
	}
	for _, w := range p.pingWords {
		addVerb(w)
	}
	for _, w := range p.enableVerbs {
		addVerb(w)
	}
	for _, grp := range [][][]string{p.confMode, p.saveWords, p.loadWords, p.captureWords, p.reloadWords, p.factoryWords, p.bootWords} {
		for _, ph := range grp {
			addVerb(ph[0])
		}
	}
	for _, f := range p.factsTemplates {
		fs := phrase(f)
		if len(fs) > 0 {
			addVerb(fs[0])
		}
	}
	p.verbs = verbs

	// 子树：动词 → 第二词候选（命令树唯一前缀消歧用）。查看动词的子树
	// 是白名单 + dump 子命令首词。
	sub := map[string][]string{}
	addSub := func(verb, second string) {
		if verb == "" || second == "" || second == "|" {
			return
		}
		sub[verb] = append(sub[verb], second)
	}
	for _, w := range p.view {
		for _, a := range p.readAllow {
			as := phrase(a)
			addSub(w, as[0])
		}
		for _, d := range p.dumpWords {
			ds := phrase(d)
			addSub(w, ds[0])
		}
	}
	for _, grp := range [][][]string{p.confMode, p.saveWords, p.loadWords, p.captureWords, p.reloadWords, p.factoryWords, p.bootWords} {
		for _, ph := range grp {
			if len(ph) >= 2 {
				addSub(ph[0], ph[1])
			}
		}
	}
	p.subtree = sub
	return p
}

// netFilterVerbs 是网络设备 CLI 的输出过滤修饰符（SAFE_READ/CONFIG_DUMP
// 可带；契约 2.8 约定 2：`|` 不是进程管道）。
var netFilterVerbs = []string{"begin", "include", "exclude", "section"}

// netFactsCommandSets 是各品牌 server_facts 固定模板（契约 7.5；规范小写
// 形式——SAFE_FACTS 与执行模板共用同一拼写）。
var netFactsCommandSets = map[NetBrand][]string{
	NetBrandCisco:   {"show version", "show ip interface brief | exclude unassigned"},
	NetBrandHuawei:  {"display version", "display ip interface brief"},
	NetBrandH3C:     {"display version", "display ip interface brief"},
	NetBrandJunos:   {"show version", "show interfaces terse"},
	NetBrandGeneric: {"show version"},
}

// netProfiles 是五分册词表（命令词按契约 7.3C 映射表写死；匹配统一小写）。
var netProfiles = map[NetBrand]*netProfile{
	NetBrandCisco: (&netProfile{
		prefix:         "NET_CISCO_",
		view:           []string{"show"},
		pingWords:      []string{"ping", "traceroute"},
		readAllow:      []string{"version", "inventory", "clock", "users", "history", "ip interface", "ip route", "ip protocols", "interfaces", "interface", "arp", "cdp neighbors", "lldp neighbors", "mac address-table", "vlan", "spanning-tree", "port-security", "env", "processes", "memory", "tech-support"},
		dumpWords:      []string{"running-config", "startup-config", "log"},
		enableVerbs:    []string{"enable"},
		confMode:       phrases("configure terminal"),
		saveWords:      phrases("write memory", "copy running-config startup-config", "copy running-config tftp:", "copy running-config ftp:"),
		loadWords:      phrases("copy tftp: running-config", "copy ftp: running-config", "configure replace"),
		captureWords:   phrases("monitor capture"),
		reloadWords:    phrases("reload"),
		factoryWords:   phrases("write erase", "erase startup-config", "format flash:", "erase flash:"),
		bootWords:      phrases("boot system", "boot-end-marker"),
		factsTemplates: netFactsCommandSets[NetBrandCisco],
		abbrev:         true,
	}).buildLexicon(),
	NetBrandHuawei: (&netProfile{
		prefix:         "NET_HUAWEI_",
		view:           []string{"display"},
		pingWords:      []string{"ping", "traceroute", "tracert"},
		readAllow:      []string{"version", "device", "clock", "users", "history", "ip interface", "ip routing-table", "interface", "arp", "lldp neighbor", "mac-address", "stp", "vlan", "memory", "cpu-usage", "alarm", "health"},
		dumpWords:      []string{"current-configuration", "saved-configuration"},
		enableVerbs:    []string{"super"},
		confMode:       phrases("system-view"),
		saveWords:      phrases("save"),
		loadWords:      phrases("restore"),
		captureWords:   phrases("capture", "packet-capture"),
		reloadWords:    phrases("reboot"),
		factoryWords:   phrases("reset saved-configuration"),
		bootWords:      phrases("boot-loader", "boot system"),
		factsTemplates: netFactsCommandSets[NetBrandHuawei],
		alias:          map[string]string{"sy": "system-view"},
	}).buildLexicon(),
	NetBrandH3C: (&netProfile{
		prefix:         "NET_H3C_",
		view:           []string{"display"},
		pingWords:      []string{"ping", "traceroute", "tracert"},
		readAllow:      []string{"version", "device", "clock", "users", "history", "ip interface", "ip routing-table", "interface", "arp", "lldp neighbor", "mac-address", "stp", "vlan", "memory", "cpu-usage", "alarm", "health"},
		dumpWords:      []string{"current-configuration", "saved-configuration"},
		enableVerbs:    []string{"super"},
		confMode:       phrases("system-view"),
		saveWords:      phrases("save"),
		loadWords:      phrases("restore"),
		captureWords:   phrases("packet-capture"),
		reloadWords:    phrases("reboot"),
		factoryWords:   phrases("reset saved-configuration"),
		bootWords:      phrases("boot-loader", "boot system"),
		factsTemplates: netFactsCommandSets[NetBrandH3C],
		alias:          map[string]string{"sy": "system-view"},
	}).buildLexicon(),
	NetBrandJunos: (&netProfile{
		prefix:         "NET_JUNOS_",
		view:           []string{"show"},
		pingWords:      []string{"ping", "traceroute"},
		readAllow:      []string{"version", "chassis hardware", "system uptime", "interfaces", "interface terse", "route", "arp", "ldp neighbor", "ntp associations", "system processes"},
		dumpWords:      []string{"configuration", "system configuration", "log"},
		enableVerbs:    nil,                  // junos 无 enable/super（登录即按 class 授权）
		confMode:       phrases("configure"), // 含 configure exclusive/private
		saveWords:      phrases("commit", "rollback"),
		loadWords:      phrases("load override", "load merge", "load set"),
		captureWords:   nil, // junos 无原生 capture
		reloadWords:    phrases("request system reboot", "request system halt", "request system power-off"),
		factoryWords:   phrases("request system zeroize", "load factory-default"),
		bootWords:      phrases("set system boot-loader"),
		factsTemplates: netFactsCommandSets[NetBrandJunos],
		alias:          map[string]string{"conf": "configure"},
	}).buildLexicon(),
	NetBrandGeneric: (&netProfile{
		prefix:    "NET_GENERIC_",
		view:      []string{"show", "display"},
		pingWords: []string{"ping", "traceroute", "tracert"},
		readAllow: []string{ // 四家白名单并集
			"version", "inventory", "clock", "users", "history",
			"ip interface", "ip route", "ip protocols", "ip routing-table",
			"interfaces", "interface", "interface terse",
			"arp", "cdp neighbors", "lldp neighbors", "lldp neighbor",
			"mac address-table", "mac-address", "stp", "vlan", "spanning-tree",
			"port-security", "env", "processes", "memory", "cpu-usage",
			"alarm", "health", "device", "chassis hardware", "system uptime",
			"route", "ldp neighbor", "ntp associations", "system processes",
			"tech-support",
		},
		dumpWords: []string{ // 四家并集（configuration/log 均不在白名单）
			"running-config", "startup-config", "current-configuration",
			"saved-configuration", "configuration", "system configuration", "log",
		},
		enableVerbs: []string{"enable", "super"},
		confMode:    phrases("configure terminal", "system-view", "configure"),
		saveWords: phrases("write memory", "copy running-config startup-config",
			"copy running-config tftp:", "copy running-config ftp:", "save", "commit", "rollback"),
		loadWords: phrases("copy tftp: running-config", "copy ftp: running-config",
			"configure replace", "restore", "load override", "load merge", "load set"),
		captureWords:   phrases("monitor capture", "capture", "packet-capture"),
		reloadWords:    phrases("reload", "reboot", "request system reboot", "request system halt", "request system power-off"),
		factoryWords:   phrases("write erase", "erase startup-config", "format flash:", "erase flash:", "reset saved-configuration", "request system zeroize", "load factory-default"),
		bootWords:      phrases("boot system", "boot-end-marker", "boot-loader", "set system boot-loader"),
		factsTemplates: netFactsCommandSets[NetBrandGeneric],
		abbrev:         true,
		alias:          map[string]string{"sy": "system-view", "conf": "configure"},
	}).buildLexicon(),
}

// netProfileFor 取分册；未知品牌失败关闭到 generic 保守并集。
func netProfileFor(brand NetBrand) *netProfile {
	if p, ok := netProfiles[brand]; ok {
		return p
	}
	return netProfiles[NetBrandGeneric]
}

// ClassifyNet 是网络设备分册入口（契约 7.3C）。永不返回 error；brand 取
// NetBrandGeneric（或未知值）时用 generic 保守并集分册。空命令/超长/NUL/
// 无法分词按失败关闭处理。
func ClassifyNet(command string, brand NetBrand) Decision {
	p := netProfileFor(brand)

	if command == "" {
		return netInvalidDecision(p, "empty command")
	}
	if len(command) > MaxCommandBytes {
		return netInvalidDecision(p, "command exceeds 16384 bytes")
	}
	if strings.IndexByte(command, 0) >= 0 {
		return netInvalidDecision(p, "command contains NUL byte")
	}

	words, normErr := netNormalize(p, command)
	if normErr != "" {
		// netNormalize 仅在注入字符/NFKC 膨胀超限时失败，均为失败关闭。
		return netInvalidDecision(p, normErr)
	}
	if len(words) == 0 {
		return netInvalidDecision(p, "empty command")
	}

	normalized := strings.Join(words, " ")

	// forbidden 优先（任何 forbidden 命中不进入审批）。
	if matchAnyPhrase(words, p.reloadWords) {
		return netDecision(p, LevelForbidden, "RELOAD", "device reload or power-off", normalized)
	}
	if matchAnyPhrase(words, p.factoryWords) {
		return netDecision(p, LevelForbidden, "FACTORY_RESET", "configuration erase or factory reset", normalized)
	}
	if matchAnyPhrase(words, p.bootWords) {
		return netDecision(p, LevelForbidden, "BOOT_CHANGE", "boot image change", normalized)
	}

	// needs_approval 组。
	if dump, sub := netViewSub(words, p); dump {
		_ = sub
		return netDecision(p, LevelNeedsApproval, "CONFIG_DUMP", "full configuration or log output may contain secrets", normalized)
	}
	if netEnableMode(words, p) {
		return netDecision(p, LevelNeedsApproval, "ENABLE_MODE", "privileged mode entry (secondary prompt)", normalized)
	}
	if matchAnyPhrase(words, p.confMode) {
		return netDecision(p, LevelNeedsApproval, "CONF_MODE", "configuration mode entry", normalized)
	}
	if matchAnyPhrase(words, p.saveWords) {
		return netDecision(p, LevelNeedsApproval, "CONFIG_SAVE", "configuration persist, export or commit", normalized)
	}
	if matchAnyPhrase(words, p.loadWords) {
		return netDecision(p, LevelNeedsApproval, "CONFIG_LOAD", "external configuration injection", normalized)
	}
	if matchAnyPhrase(words, p.captureWords) {
		return netDecision(p, LevelNeedsApproval, "CAPTURE", "packet capture may expose cleartext credentials", normalized)
	}

	// safe 组：SAFE_READ 先判（facts 模板是白名单子集，手工命令归
	// SAFE_READ；SAFE_FACTS 仅兜底模板中白名单外的命令）。
	if safe, _ := netSafeRead(words, p); safe {
		return netDecision(p, LevelSafe, "SAFE_READ", "read-only view command", normalized)
	}
	if containsWord(p.pingWords, words[0]) && netLegalTail(words[1:]) {
		return netDecision(p, LevelSafe, "SAFE_PING", "network diagnostics", normalized)
	}
	for _, tpl := range p.factsTemplates {
		if normalized == tpl {
			return netDecision(p, LevelSafe, "SAFE_FACTS", "fixed read-only facts template", normalized)
		}
	}

	// 失败关闭：未命中任何规则（含一切配置态动词）全部落 DEFAULT_ASK。
	return netDecision(p, LevelNeedsApproval, "DEFAULT_ASK", "unrecognized command; approval required", normalized)
}

func netDecision(p *netProfile, level Level, rule, reason, normalized string) Decision {
	full := p.prefix + rule
	return Decision{
		Level:      level,
		RuleID:     full,
		Reason:     reason,
		Normalized: normalized,
		Reasons:    []string{full + ": " + reason},
	}
}

func netInvalidDecision(p *netProfile, reason string) Decision {
	return netDecision(p, LevelForbidden, "INVALID", reason, "")
}

// netNormalize 做厂商 CLI 语义规范化并分词；返回非空 errReason 表示失败
// 关闭（注入字符/NFKC 膨胀超限）：
//  1. NFKC；膨胀超限失败关闭（NFKC 先行——全角 `＄`/`；` 等变体归一后
//     同样被注入字符检查覆盖）；
//  2. 注入字符检查（反引号/$/;/&/< >——网络设备 CLI 无 shell 展开语义，
//     出现即视为注入企图）；
//  3. `|` 两侧补空白（`show run|inc x` 与 `show run | inc x` 等价）；
//  4. 压空白小写分词；
//  5. 缩写/别名展开（cisco 式保留字唯一前缀归一或显式别名）。
func netNormalize(p *netProfile, command string) (words []string, errReason string) {
	s := norm.NFKC.String(command)
	if len(s) > MaxCommandBytes {
		return nil, "command exceeds 16384 bytes after normalization"
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '`', '$', ';', '&', '<', '>':
			return nil, "command contains shell metacharacters"
		}
	}
	s = strings.ReplaceAll(s, "|", " | ")
	s = strings.ToLower(s)
	words = strings.Fields(s)
	if len(words) == 0 {
		return nil, "" // 纯空白命令：调用方按 empty command 处理
	}
	if !p.abbrev && len(p.alias) == 0 {
		return words, ""
	}
	for i, w := range words {
		if w == "|" {
			continue
		}
		if repl, ok := p.alias[w]; ok {
			words[i] = repl
			continue
		}
		if !p.abbrev {
			continue
		}
		// 位置感知的 cisco 式唯一前缀归一：
		//   首词在动词集内归一（`en` 不被白名单 `env` 打成歧义）；
		//   第二词优先在首词子树内归一（命令树语义：`conf t`→
		//   `configure terminal`、`eras st`→`erase startup-config`）；
		//   其余词在保留字全集内归一（`| inc`→`| include`）。
		// 多候选或零候选不归一（保持原词，落 DEFAULT_ASK 保守方向）。
		switch i {
		case 0:
			if full, ok := netUniquePrefix(p.verbs, w); ok {
				words[i] = full
			}
		case 1:
			if full, ok := netUniquePrefix(p.subtree[words[0]], w); ok {
				words[i] = full
			} else if full, ok := netUniquePrefix(p.lexicon, w); ok {
				words[i] = full
			}
		default:
			if full, ok := netUniquePrefix(p.lexicon, w); ok {
				words[i] = full
			}
		}
	}
	return words, ""
}

// netUniquePrefix 在保留字全集中找 w 的唯一前缀候选；命中唯一时返回完整拼写。
// 多候选或零候选不归一（保持原词，落 DEFAULT_ASK 保守方向）。
func netUniquePrefix(lexicon []string, w string) (string, bool) {
	if w == "" {
		return "", false
	}
	found := ""
	for _, cand := range lexicon {
		if cand == w {
			return w, false // 完整词无需归一
		}
		if strings.HasPrefix(cand, w) {
			if found != "" {
				return "", false // 多候选：歧义，不归一
			}
			found = cand
		}
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// matchAnyPhrase 判定词序列是否以任一短语开头（逐词相等；cisco `reload in`
// 命中 ["reload"]）。
func matchAnyPhrase(words []string, list [][]string) bool {
	for _, ph := range list {
		if len(ph) == 0 || len(words) < len(ph) {
			continue
		}
		match := true
		for i := range ph {
			if words[i] != ph[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// netEnableMode 判定 enable/super（可带数字级别参数）；
// `enable secret <pw>` 等其他形态不命中（落 DEFAULT_ASK）。
func netEnableMode(words []string, p *netProfile) bool {
	if len(words) == 0 || !containsWord(p.enableVerbs, words[0]) {
		return false
	}
	if len(words) == 1 {
		return true
	}
	if len(words) == 2 {
		if _, err := strconv.Atoi(words[1]); err == nil {
			return true
		}
	}
	return false
}

// netSplitFilter 把词序列按独立 `|` 切为命令体与过滤段；
// hasFilter 表示携带 `|` 修饰符（其后首词必须是 begin/include/exclude/section）。
func netSplitFilter(words []string) (body []string, filterOK, hasFilter bool) {
	for i, w := range words {
		if w == "|" {
			hasFilter = true
			rest := words[i+1:]
			if len(rest) >= 1 && containsWord(netFilterVerbs, rest[0]) {
				filterOK = true
			}
			return words[:i], filterOK, true
		}
	}
	return words, true, false
}

// netLegalTail 判定参数词是否只含网络设备 CLI 常规字符（防白名单前缀被
// 异常字符续接，如引号/逗号/等号拼接）。
func netLegalTail(words []string) bool {
	for _, w := range words {
		for _, r := range w {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			case r == '.' || r == '-' || r == '_' || r == ':' || r == '/' || r == '%' || r == '*':
			default:
				return false
			}
		}
	}
	return true
}

// netSafeRead 判定 SAFE_READ：首词为厂商只读查看命令、子命令（`|` 之前）
// 属白名单（前缀匹配），可带输出过滤；非法过滤或白名单外子命令不命中。
func netSafeRead(words []string, p *netProfile) (bool, string) {
	if len(words) == 0 || !containsWord(p.view, words[0]) {
		return false, ""
	}
	body, filterOK, hasFilter := netSplitFilter(words)
	sub := strings.Join(body[1:], " ")
	if !netLegalTail(body[1:]) {
		return false, sub
	}
	for _, allow := range p.readAllow {
		if strings.HasPrefix(sub, allow) {
			if hasFilter && !filterOK {
				return false, sub
			}
			return true, sub
		}
	}
	return false, sub
}

// netViewSub 判定 CONFIG_DUMP：首词为查看命令且子命令命中 dump 词表
// （`more flash:` 前缀词由 cisco 分册的 dumpWords 兼容——见 netIsDump）。
func netViewSub(words []string, p *netProfile) (bool, string) {
	if len(words) == 0 {
		return false, ""
	}
	// cisco `more flash:<file>`：任意 flash 文件读取。
	if len(words) >= 2 && words[0] == "more" && strings.HasPrefix(words[1], "flash:") &&
		netProfileIsCiscoLike(p) {
		return true, strings.Join(words[1:], " ")
	}
	if !containsWord(p.view, words[0]) {
		return false, ""
	}
	body, _, _ := netSplitFilter(words)
	sub := strings.Join(body[1:], " ")
	if !netLegalTail(body[1:]) {
		return false, sub
	}
	for _, dw := range p.dumpWords {
		if strings.HasPrefix(sub, dw) {
			return true, sub
		}
	}
	return false, sub
}

// netProfileIsCiscoLike 报告分册词表里是否含 `more flash:` 读取形态
// （cisco 与 generic 并集分册）。
func netProfileIsCiscoLike(p *netProfile) bool {
	return p == netProfiles[NetBrandCisco] || p == netProfiles[NetBrandGeneric]
}

func containsWord(list []string, w string) bool {
	for _, x := range list {
		if x == w {
			return true
		}
	}
	return false
}
