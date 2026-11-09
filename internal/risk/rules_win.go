// rules_win.go 实现 CONTRACT.md 7.3B 的 Windows 分册（W1）与 7.4.16-20 的
// Windows 防绕过补充：PowerShell/cmd 语义规范化（NFKC、`;`/`|`/`&&`/`||`/`&`
// 分段、`$(...)` 与分组子表达式与 `{...}` 脚本块递归、最大深度 4）、别名等价
// 表展开、包装器剥离（powershell/pwsh/cmd/Start-Process/Invoke-Command），
// 逐段命中 WIN_* 规则取最高级别。
//
// 与 Linux 分册（rules.go）互不混用：Linux 规则在 Windows 目标上不生效，
// 反之亦然。失败关闭：解析失败、超深度、规避检测迹象一律 WIN_OBFUSCATION
// (forbidden)；语义不明但无危险词 WIN_DEFAULT_ASK (needs_approval)。
package risk

import (
	"path"
	"strings"
)

// WinFactsCommand 是 os=windows 的 server_facts 固定模板内部脚本
// （CONTRACT.md 7.5；四期 C1 追加容器运行时探测段与 container_runtime 键，
// 探测语句同 5.2C Windows 模板——Windows 不探测 ctr）。契约原文整条模板为
// `powershell -NoProfile -NonInteractive -Command "<本脚本>"`，SSH 通道由
// agent 包装该外壳，WinRM 通道由 winrmx 以 PowerShell 端点直接运行。
//
// 已知限制（README 29）：模板为多行语句块，SSH 通道做无引号前缀拼接时
// 外层 shell 可能拆解多行参数（评审指出，真实 Windows 未接入未实测）。
// 不做单行折叠：本模板含嵌套 pipeline/哈希表且被 winFactsTemplates 精确
// 匹配集引用，手工折叠的语义风险大于收益；接入真实 Windows 时随实测一
// 并处理（探测模板 detectWindows 已单行化）。
const WinFactsCommand = `$ProgressPreference='SilentlyContinue';
$cr='none';
if (Get-Command docker -ErrorAction SilentlyContinue) {
  $null = docker version --format '{{.Server.Version}}' 2>$null;
  if ($LASTEXITCODE -eq 0) { $cr='docker' }
};
if ($cr -eq 'none' -and (Get-Command podman -ErrorAction SilentlyContinue)) {
  $null = podman info 2>$null;
  if ($LASTEXITCODE -eq 0) { $cr='podman' }
};
[pscustomobject]@{
  hostname=$env:COMPUTERNAME;
  os=(Get-CimInstance Win32_OperatingSystem).Caption;
  os_version=[Environment]::OSVersion.VersionString;
  kernel='';
  arch=$env:PROCESSOR_ARCHITECTURE;
  uptime_seconds=[int]((Get-Date)-(Get-CimInstance Win32_OperatingSystem).LastBootUpTime).TotalSeconds;
  cpu_count=[Environment]::ProcessorCount;
  load_1=$null; load_5=$null; load_15=$null;
  memory_total_bytes=[int64](Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory;
  memory_available_bytes=[int64](Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory*1KB;
  disks=@(Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3' | ForEach-Object {
    [pscustomobject]@{mount=$_.DeviceID; filesystem=$_.VolumeName; size_bytes=[int64]$_.Size;
      used_bytes=[int64]($_.Size-$_.FreeSpace); available_bytes=[int64]$_.FreeSpace}});
  ip_addresses=@((Get-NetIPAddress -AddressFamily IPv4 | Where-Object {$_.IPAddress -ne '127.0.0.1'}).IPAddress);
  container_runtime=$cr
} | ConvertTo-Json -Compress -Depth 3`

// winFactsTemplates 是 WIN_SAFE_FACTS 精确匹配的模板规范化形式集合：
// 内部脚本本体与带 powershell 外壳的整条模板。
var winFactsTemplates = func() map[string]bool {
	m := map[string]bool{}
	if n, err := normalize(WinFactsCommand); err == nil {
		m[n] = true
	}
	if n, err := normalize(`powershell -NoProfile -NonInteractive -Command "` + WinFactsCommand + `"`); err == nil {
		m[n] = true
	}
	return m
}()

// ---------------------------------------------------------------------------
// PowerShell/cmd 分词与分段
// ---------------------------------------------------------------------------

type winWord struct {
	text   string // 去引号内容（保留 $var、$(...) 原文）
	raw    string // 原始写法（含引号与反引号），用于识别拼接混淆
	quoted bool
}

type winSegment struct {
	words  []winWord
	subs   []string // $(...) 子表达式与 (...) 分组表达式的内部脚本
	blocks []string // { ... } 脚本块内容（Invoke-Command -ScriptBlock 等）
	redir  bool     // 含 > / < 重定向（PS 的 > 等价 Out-File）
	pipeTo bool     // 该段后紧跟 |
}

var errWinUnbalanced = &riskError{"unbalanced powershell syntax"}

// parseWinCommand 把规范化后的 PowerShell/cmd 命令切成分段；失败返回错误
// （调用方转为 WIN_OBFUSCATION）。
func parseWinCommand(s string) ([]winSegment, error) {
	var (
		segs    []winSegment
		cur     winSegment
		textBuf []byte
		rawBuf  []byte
		quoted  bool
		hasW    bool
	)

	finishWord := func() {
		if hasW {
			cur.words = append(cur.words, winWord{text: string(textBuf), raw: string(rawBuf), quoted: quoted})
			textBuf, rawBuf = textBuf[:0], rawBuf[:0]
			quoted = false
			hasW = false
		}
	}
	finishSegment := func(pipe bool) {
		finishWord()
		if len(cur.words) > 0 || len(cur.subs) > 0 || len(cur.blocks) > 0 || cur.redir {
			cur.pipeTo = pipe
			segs = append(segs, cur)
		}
		cur = winSegment{}
	}

	i := 0
	for i < len(s) {
		ch := s[i]

		switch ch {
		case '\'':
			quoted = true
			rawBuf = append(rawBuf, '\'')
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				textBuf = append(textBuf, s[j])
				rawBuf = append(rawBuf, s[j])
				j++
			}
			if j >= len(s) {
				return nil, errUnterminatedQuote
			}
			rawBuf = append(rawBuf, '\'')
			hasW = true
			i = j + 1
			continue

		case '"':
			quoted = true
			rawBuf = append(rawBuf, '"')
			j := i + 1
			closed := false
			for j < len(s) {
				c := s[j]
				if c == '`' && j+1 < len(s) {
					// PS 双引号内反引号转义。
					textBuf = append(textBuf, s[j+1])
					rawBuf = append(rawBuf, s[j], s[j+1])
					j += 2
					continue
				}
				if c == '"' {
					closed = true
					rawBuf = append(rawBuf, '"')
					j++
					break
				}
				if c == '$' && j+1 < len(s) && s[j+1] == '(' {
					inner, end, err := scanBalanced(s, j+1)
					if err != nil {
						return nil, err
					}
					cur.subs = append(cur.subs, inner)
					textBuf = append(textBuf, s[j:end]...)
					rawBuf = append(rawBuf, s[j:end]...)
					j = end
					continue
				}
				textBuf = append(textBuf, c)
				rawBuf = append(rawBuf, c)
				j++
			}
			if !closed {
				return nil, errUnterminatedQuote
			}
			hasW = true
			i = j
			continue

		case '`':
			// PS 反引号转义：引号外同样合法（`n 等）。转义字符进 text，
			// raw 保留反引号——词内反引号拼接标识符由混淆检查识别。
			if i+1 >= len(s) {
				return nil, errWinUnbalanced
			}
			textBuf = append(textBuf, s[i+1])
			rawBuf = append(rawBuf, s[i], s[i+1])
			quoted = true
			hasW = true
			i += 2
			continue

		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				inner, end, err := scanBalanced(s, i+1)
				if err != nil {
					return nil, err
				}
				cur.subs = append(cur.subs, inner)
				textBuf = append(textBuf, s[i:end]...)
				rawBuf = append(rawBuf, s[i:end]...)
				hasW = true
				i = end
				continue
			}
			textBuf = append(textBuf, ch)
			rawBuf = append(rawBuf, ch)
			hasW = true
			i++

		case '{':
			// 脚本块：提取内容递归分类。
			depth := 0
			j := i
			for j < len(s) {
				switch s[j] {
				case '\'':
					k := j + 1
					for k < len(s) && s[k] != '\'' {
						k++
					}
					j = k
				case '"':
					k := j + 1
					for k < len(s) && s[k] != '"' {
						if s[k] == '`' {
							k++
						}
						k++
					}
					j = k
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						goto blockDone
					}
				}
				j++
			}
			if depth != 0 {
				return nil, errWinUnbalanced
			}
		blockDone:
			cur.blocks = append(cur.blocks, s[i+1:j])
			textBuf = append(textBuf, s[i:j+1]...)
			rawBuf = append(rawBuf, s[i:j+1]...)
			hasW = true
			i = j + 1
			continue

		case '(':
			// 词后紧跟 '(' 是 PowerShell 的调用/方法语法（IEX(...)、
			// $x.DownloadFile(...)）：结束当前词，分组内容提取为子表达式。
			finishWord()
			inner, end, err := scanBalanced(s, i)
			if err != nil {
				return nil, err
			}
			cur.subs = append(cur.subs, inner)
			i = end
			continue

		case ')':
			return nil, &riskError{"unexpected ')'"}

		case '}':
			if !hasW {
				return nil, errWinUnbalanced
			}
			textBuf = append(textBuf, ch)
			rawBuf = append(rawBuf, ch)
			hasW = true
			i++

		case ';', '\n':
			finishSegment(false)
			i++

		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				finishSegment(false)
				i += 2
				continue
			}
			// PS 调用操作符 & cmd 与 cmd 的命令分隔 & 同形：分段处理，
			// 段首独立 & 词在分类时跳过（不放宽也不误杀）。
			finishSegment(false)
			i++

		case '|':
			if i+1 < len(s) && (s[i+1] == '|' || s[i+1] == '&') {
				finishSegment(false)
				i += 2
				continue
			}
			finishSegment(true)
			i++

		case '>', '<':
			// 重定向：2>&1 / > / >> / < 。目标词继续按普通词读取。
			cur.redir = true
			textBuf = append(textBuf, ch)
			rawBuf = append(rawBuf, ch)
			hasW = true
			i++

		case ' ', '\t', '\r':
			finishWord()
			i++

		default:
			textBuf = append(textBuf, ch)
			rawBuf = append(rawBuf, ch)
			hasW = true
			i++
		}
	}
	finishSegment(false)
	return segs, nil
}

// winAliases 是 PowerShell 别名等价表（契约 7.3B 补充），分词后展开。
// curl/wget 在 PowerShell 环境默认是 Invoke-WebRequest 别名而非二进制。
var winAliases = map[string]string{
	"iex": "invoke-expression", "iwr": "invoke-webrequest", "wget": "invoke-webrequest",
	"curl": "invoke-webrequest", "rm": "remove-item", "del": "remove-item",
	"rd": "remove-item", "ri": "remove-item", "erase": "remove-item",
	"gc": "get-content", "cat": "get-content",
	"type": "get-content", "ls": "get-childitem", "dir": "get-childitem", "gci": "get-childitem",
	"ni": "new-item", "sp": "set-itemproperty", "gp": "get-itemproperty",
	"sal": "set-alias", "echo": "write-output",
}

func expandWinAlias(cmd string) string {
	if full, ok := winAliases[cmd]; ok {
		return full
	}
	return cmd
}

// winStructuralKeywords 是复合语句分段后落在段首的 PowerShell 关键字。
var winStructuralKeywords = map[string]bool{
	"if": true, "elseif": true, "else": true, "switch": true, "foreach": true,
	"for": true, "while": true, "do": true, "until": true, "try": true,
	"catch": true, "finally": true, "return": true, "break": true, "continue": true,
	"throw": true, "then": true, "&": true, "in": true, "process": true, "end": true, "begin": true,
}

// ---------------------------------------------------------------------------
// Windows 入口
// ---------------------------------------------------------------------------

// classifyWindows 是 Windows 分册入口（CONTRACT.md 7.3B）。
func classifyWindows(command string) Decision {
	normalized, err := normalize(command)
	if err != nil {
		return winInvalid("", err.Error())
	}
	if normalized == "" {
		return Decision{
			Level:   LevelForbidden,
			RuleID:  "COMMAND_INVALID",
			Reason:  "empty command",
			Reasons: []string{"COMMAND_INVALID: empty command"},
		}
	}

	acc := &accumulator{}

	// server_facts 固定模板：仅精确匹配放行（任何改动走正常分类）。
	if winFactsTemplates[normalized] {
		return Decision{
			Level:      LevelSafe,
			RuleID:     "WIN_SAFE_FACTS",
			Reason:     "fixed read-only facts template",
			Normalized: normalized,
			Reasons:    []string{"WIN_SAFE_FACTS: fixed read-only facts template"},
		}
	}

	// 五期 M1：monitor 采集器的固定指标模板（WIN_SAFE_COLLECT，契约 7.7）
	//——仅精确匹配放行，任何改动走正常分类。
	if winCollectTemplates[normalized] {
		return Decision{
			Level:      LevelSafe,
			RuleID:     "WIN_SAFE_COLLECT",
			Reason:     "fixed read-only metric collect template",
			Normalized: normalized,
			Reasons:    []string{"WIN_SAFE_COLLECT: fixed read-only metric collect template"},
		}
	}

	// 五期 M2：巡检固定模板（WIN_SAFE_INSPECT，契约 7.7）——仅精确匹配
	// 放行（Get-Service/Get-WinEvent 查询与 SSH 外壳包装形态），任何改动
	// 走正常分类。
	if winInspectTemplates[normalized] {
		return Decision{
			Level:      LevelSafe,
			RuleID:     "WIN_SAFE_INSPECT",
			Reason:     "fixed read-only inspection template command",
			Normalized: normalized,
			Reasons:    []string{"WIN_SAFE_INSPECT: fixed read-only inspection template command"},
		}
	}

	// 全局危险载荷词（契约 7.3B：DownloadString/DownloadFile 本身无论是否执行
	// 均 forbidden；FromBase64String、AMSI 补丁、动态构造同理）。在分段前对
	// 整条规范化文本扫描，方法调用 IEX(...).DownloadString(...) 等无空格形式
	// 不会因分词边界漏检。
	if hit, ok := winGlobalForbidden(normalized); ok {
		return Decision{
			Level:      LevelForbidden,
			RuleID:     hit.rule,
			Reason:     hit.reason,
			Normalized: normalized,
			Reasons:    []string{hit.rule + ": " + hit.reason},
		}
	}

	segs, parseErr := parseWinCommand(normalized)
	if parseErr != nil {
		return winInvalid(normalized, parseErr.Error())
	}

	classifyWinSegments(segs, 0, acc)

	if acc.len() == 0 {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "unrecognized command; approval required")
	}
	return Decision{
		Level:      acc.level,
		RuleID:     acc.rule,
		Reason:     acc.reason,
		Normalized: normalized,
		Reasons:    append([]string(nil), acc.reasons...),
	}
}

// classifyWinScript 递归分类子表达式 / 脚本块 / 包装器内部命令。
func classifyWinScript(script string, depth int, acc *accumulator) {
	if depth > maxDepth {
		acc.add(LevelForbidden, "WIN_OBFUSCATION", "nesting depth exceeds 4")
		return
	}
	norm, err := normalize(script)
	if err != nil {
		acc.add(LevelForbidden, "WIN_OBFUSCATION", err.Error())
		return
	}
	if norm == "" {
		// 空子表达式也是被求值的动态语法，失败关闭。
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "empty expression evaluated")
		return
	}
	if winFactsTemplates[norm] {
		acc.add(LevelSafe, "WIN_SAFE_FACTS", "fixed read-only facts template")
		return
	}
	segs, err := parseWinCommand(norm)
	if err != nil {
		acc.add(LevelForbidden, "WIN_OBFUSCATION", err.Error())
		return
	}
	classifyWinSegments(segs, depth, acc)
}

func winInvalid(normalized, reason string) Decision {
	return Decision{
		Level:      LevelForbidden,
		RuleID:     "WIN_OBFUSCATION",
		Reason:     reason,
		Normalized: normalized,
		Reasons:    []string{"WIN_OBFUSCATION: " + reason},
	}
}

func classifyWinSegment(seg winSegment, depth int, acc *accumulator, pipelinePrev []string) {
	if depth > maxDepth {
		acc.add(LevelForbidden, "WIN_OBFUSCATION", "nesting depth exceeds 4")
		return
	}
	for _, sub := range seg.subs {
		classifyWinScript(sub, depth+1, acc)
	}
	// 子表达式是被求值的动态路径，静态分类无法证明替换结果安全：
	// 含替换的段至少 needs_approval（失败关闭，与 Linux 分册同语义）。
	if len(seg.subs) > 0 {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "subexpression evaluation requires approval")
	}
	for _, block := range seg.blocks {
		classifyWinScript(block, depth+1, acc)
	}
	if len(seg.blocks) > 0 {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "script block requires approval")
	}
	if len(seg.words) == 0 && (len(seg.subs) > 0 || len(seg.blocks) > 0) {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "expression evaluated without a command")
	}
	classifyWinWords(seg, depth, pipelinePrev, acc)
}

// ---------------------------------------------------------------------------
// 逐段分类：包装器剥离 + 主命令规则表
// ---------------------------------------------------------------------------

func classifyWinWords(seg winSegment, depth int, pipelinePrev []string, acc *accumulator) {
	words := seg.words
	i := 0
	// 跳过 $var=... 赋值前缀与结构关键字。
	for i < len(words) && isWinAssignment(words[i].text) {
		i++
	}
	for i < len(words) && winStructuralKeywords[strings.ToLower(words[i].text)] {
		i++
	}
	if i >= len(words) {
		if len(words) > 0 && (len(seg.subs) == 0 && len(seg.blocks) == 0) {
			acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "statement structure without a runnable command")
		}
		return
	}

	// 反引号拼接混淆：词的原始写法含反引号且内容与原文不一致
	//（i`e`x → iex），无法静态还原真实标识符。
	for _, w := range words[i:] {
		if strings.Contains(w.raw, "`") && !strings.Contains(w.text, "`") && w.raw != w.text {
			acc.add(LevelForbidden, "WIN_OBFUSCATION", "backtick identifier splicing")
			return
		}
	}

	rest, handled := stripWinWrappers(words[i:], depth, acc)
	if handled {
		return
	}
	if len(rest) == 0 {
		return
	}
	name := strings.ToLower(rest[0].text)
	if name == "" {
		if len(seg.subs) > 0 || len(seg.blocks) > 0 {
			acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "expression evaluated without a command")
		}
		return
	}
	// 命令名为变量、含 $(...) 拼接或 ${} 切片构造 → forbidden（契约 7.4.19）。
	if strings.HasPrefix(name, "$") || strings.Contains(name, "${") || strings.Contains(name, "$(") {
		acc.add(LevelForbidden, "WIN_OBFUSCATION", "dynamic command name")
		return
	}
	cmd := trimExecutable(name)
	cmd = expandWinAlias(cmd)

	args := make([]string, 0, len(rest)-1)
	for _, w := range rest[1:] {
		args = append(args, strings.ToLower(w.text))
	}

	classifyWinCommand(cmd, args, seg, depth, pipelinePrev, acc)
}

// isWinAssignment 判断 $var=... 形式的赋值前缀。
func isWinAssignment(s string) bool {
	if !strings.HasPrefix(s, "$") {
		return false
	}
	return strings.Contains(s, "=")
}

// trimExecutable 去掉路径前缀与 .exe/.bat/.cmd/.ps1 后缀的规范命令名
// （C:\Windows\System32\reg.exe → reg；./x.ps1 → x.ps1 保留后缀语义）。
func trimExecutable(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	base := path.Base(strings.TrimRight(name, "/"))
	for _, ext := range []string{".exe", ".com"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	return base
}

// stripWinWrappers 剥离 powershell/pwsh/cmd/调用包装器；handled=true 表示
// 内部脚本已递归分类或已直接定级，调用方无需继续。
func stripWinWrappers(words []winWord, depth int, acc *accumulator) ([]winWord, bool) {
	if len(words) == 0 {
		return words, false
	}
	cmd := trimExecutable(strings.ToLower(words[0].text))
	rest := words[1:]

	switch cmd {
	case "powershell", "pwsh":
		classifyWinPowershellArgs(rest, acc)
		// 剩余脚本递归分类交给 classifyWinPowershellArgs 内部处理。
		return nil, true

	case "cmd":
		// cmd /c ... /k ...：内部命令递归分类，内部无任何命中时兜底审批；
		// 否则 cmd 外壳本身 DEFAULT_ASK。
		var inner []winWord
		for i, w := range rest {
			t := strings.ToLower(w.text)
			if t == "/c" || t == "/k" {
				inner = rest[i+1:]
				break
			}
		}
		if inner != nil {
			before := acc.len()
			// 引号包裹的复合命令（cmd /c "net stop winrm"）按文本递归分类。
			classifyWinScript(strings.Join(wordTexts(inner), " "), depth+1, acc)
			if acc.len() == before {
				acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "cmd wrapper command requires approval")
			}
			return nil, true
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "cmd shell invocation")
		return nil, true

	case "start-process":
		classifyWinStartProcess(rest, acc)
		return nil, true

	case "invoke-command":
		// 契约 WIN_REMOTE_EXEC：-ScriptBlock/-ComputerName 一律 forbidden；
		// 脚本块内容已由解析器提取至 seg.blocks 递归分类。
		acc.add(LevelForbidden, "WIN_REMOTE_EXEC", "Invoke-Command executes a script block (potentially remote)")
		return nil, true
	}
	return words, false
}

// classifyWinPowershellArgs 处理 powershell/pwsh 参数（契约 7.4.17）：
// -EncodedCommand/-enc/-e、-w hidden/-WindowStyle Hidden、-nop+-exec bypass
// 直接 forbidden；-Command/-c 递归分类内部脚本；-File .ps1 → needs_approval。
func classifyWinPowershellArgs(rest []winWord, acc *accumulator) {
	nop, execBypass := false, false
	for i := 0; i < len(rest); i++ {
		a := strings.ToLower(rest[i].text)
		name, _, hasVal := strings.Cut(strings.TrimLeft(a, "-"), ":")
		switch name {
		case "encodedcommand", "enc", "e":
			acc.add(LevelForbidden, "WIN_ENCODED_EXEC", "base64 encoded command is not decoded for review")
			return
		case "windowstyle", "w":
			val := ""
			if hasVal {
				_, val, _ = strings.Cut(a, ":")
			} else if i+1 < len(rest) {
				val = strings.ToLower(rest[i+1].text)
			}
			if val == "hidden" || val == "h" {
				acc.add(LevelForbidden, "WIN_ENCODED_EXEC", "hidden window execution")
				return
			}
		case "command", "c":
			script := strings.Join(wordTexts(rest[i+1:]), " ")
			if script == "" {
				acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "powershell -Command without script")
				return
			}
			before := acc.len()
			classifyWinScript(script, 1, acc)
			if acc.len() == before {
				acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "nested powershell invocation")
			}
			return
		case "file", "f":
			acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "powershell -File runs a script file")
			return
		case "nop", "noprofile":
			nop = true
		case "exec", "executionpolicy":
			val := ""
			if hasVal {
				_, val, _ = strings.Cut(a, ":")
			} else if i+1 < len(rest) {
				val = strings.ToLower(rest[i+1].text)
			}
			if strings.Contains(val, "bypass") {
				execBypass = true
			}
		}
		if nop && execBypass {
			acc.add(LevelForbidden, "WIN_ENCODED_EXEC", "-nop with -exec bypass")
			return
		}
	}
	if nop && execBypass {
		acc.add(LevelForbidden, "WIN_ENCODED_EXEC", "-nop with -exec bypass")
		return
	}
	acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "powershell invocation")
}

func classifyWinStartProcess(rest []winWord, acc *accumulator) {
	for _, w := range rest {
		a := strings.ToLower(w.text)
		switch {
		case a == "-verb" || strings.HasPrefix(a, "-verb"):
			acc.add(LevelNeedsApproval, "WIN_PROCESS_CONTROL", "Start-Process elevation")
			return
		}
	}
	target := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0].text, "-") {
		target = strings.ToLower(rest[0].text)
	}
	switch {
	case strings.HasSuffix(target, ".ps1"), strings.HasSuffix(target, ".bat"),
		strings.HasSuffix(target, ".cmd"), strings.HasSuffix(target, ".vbs"),
		strings.HasSuffix(target, ".js"), strings.Contains(target, "http:"),
		strings.Contains(target, "https:"):
		acc.add(LevelForbidden, "WIN_LOLBIN_EXEC", "Start-Process launches a script or downloaded payload")
	default:
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "Start-Process invocation")
	}
}

func wordTexts(words []winWord) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, w.text)
	}
	return out
}

// ---------------------------------------------------------------------------
// Windows 主规则表（CONTRACT.md 7.3B，写死）
// ---------------------------------------------------------------------------

// winSafeReadCommands 是 WIN_SAFE_READ 只读白名单（契约 7.3B 清单；别名经
// winAliases 展开后匹配）。
var winSafeReadCommands = map[string]bool{
	"get-service": true, "get-process": true, "get-childitem": true,
	"get-content": true, "get-item": true, "get-date": true, "get-host": true,
	"get-location": true, "get-netipaddress": true, "get-nettcpconnection": true,
	"get-disk": true, "get-volume": true, "get-computerinfo": true,
	"get-ciminstance": true, "get-wmiobject": true, "get-eventlog": true,
	"get-winevent": true, "get-scheduledtask": true, "get-localuser": true,
	"measure-object": true, "select-object": true, "sort-object": true,
	"where-object": true, "format-list": true, "format-table": true,
	"ipconfig": true, "systeminfo": true, "tasklist": true, "whoami": true,
	"hostname": true, "ver": true, "vol": true, "tree": true, "findstr": true,
	"find": true, "quser": true, "qwinsta": true, "ping": true, "tracert": true,
	"pathping": true, "nslookup": true,
}

// winCriticalServices 是停止/禁用后可能立即失联或失去审计的服务
// （契约 WIN_CRITICAL_SERVICE_STOP）。
var winCriticalServices = map[string]bool{
	"sshd": true, "winrm": true, "wuauserv": true, "eventlog": true,
	"w32time": true, "mpssvc": true, "windefend": true, "mssqlserver": true,
	"sqlserveragent": true, "termservice": true,
}

// winCriticalProcesses 是终止后可能使系统崩溃/失去审计的进程。
var winCriticalProcesses = map[string]bool{
	"lsass": true, "winlogon": true, "csrss": true, "svchost": true,
	"services": true,
}

// winProtectedRoots 是 WIN_RM_PROTECTED 的系统目录根（契约 + 盘根推广：
// 任意盘符的 X:\ 与 X:\* 同为保护目标）。
var winProtectedRoots = []string{
	"c:/windows", "c:/program files", "c:/users", "c:/programdata",
}

// winSensitiveFiles 是 WIN_ENV_DUMP 的敏感文件路径片段。
var winSensitiveFiles = []string{
	".aws/credentials", ".aws\\credentials", ".kube/config", ".kube\\config",
	"unattend.xml", "sysprep.xml", "id_rsa", ".ssh/id_rsa",
}

// cleanWinPath 做盘符语义规范化：去引号、`\`→`/`、小写、去尾部 `/` 与 `.`。
func cleanWinPath(t string) string {
	t = strings.TrimSpace(t)
	if len(t) >= 2 && ((t[0] == '"' && t[len(t)-1] == '"') || (t[0] == '\'' && t[len(t)-1] == '\'')) {
		t = t[1 : len(t)-1]
	}
	t = strings.ToLower(strings.ReplaceAll(t, `\`, "/"))
	for len(t) > 1 && strings.HasSuffix(t, "/") {
		t = t[:len(t)-1]
	}
	return t
}

// isWinProtectedRM 判断删除目标是否命中保护目录（含 glob 首段匹配）。
func isWinProtectedRM(p string) bool {
	if p == "" {
		return false
	}
	// 任意盘符的盘根本身或盘根通配（c:/、c:/*、d:/、d:/*）。
	if len(p) == 2 && p[1] == ':' {
		return p[0] >= 'a' && p[0] <= 'z'
	}
	if len(p) == 4 && p[1] == ':' && p[2] == '/' && p[3] == '*' {
		return p[0] >= 'a' && p[0] <= 'z'
	}
	// 未解析变量目标：递归/强制删除按最坏情况保护。
	if strings.Contains(p, "$") {
		return true
	}
	if !strings.Contains(p, ":/") {
		return false // 相对路径目标沿用非保护语义
	}
	for _, root := range winProtectedRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
		// 契约形如 C:\Windows*：保护根的 glob 前缀（c:/windows*）。
		if strings.HasPrefix(p, root+"*") {
			return true
		}
	}
	// 首段 glob（c:/wind*、c:/prog*）经展开可能命中保护目录：取盘符后第一段
	// 与保护根去掉 c:/ 前缀后的目录名做 glob 匹配（c:/wind* 命中 c:/windows）。
	if strings.ContainsAny(p, "*?[") {
		first := p
		if idx := strings.Index(p, ":/"); idx >= 0 {
			first = p[idx+2:]
		}
		if idx := strings.IndexByte(first, '/'); idx >= 0 {
			first = first[:idx]
		}
		if first != "" {
			for _, root := range winProtectedRoots {
				base := strings.TrimPrefix(root, "c:/")
				// glob 模式在 first（c:/wind*），保护根目录名作为被测名。
				if matched, err := path.Match(first, base); err == nil && matched {
					return true
				}
			}
		}
	}
	return false
}

// winOperands 返回段的非选项操作数（小写），供管线目标传递与判定。
func winOperands(seg winSegment) []string {
	i := 0
	for i < len(seg.words) && (isWinAssignment(seg.words[i].text) || winStructuralKeywords[strings.ToLower(seg.words[i].text)]) {
		i++
	}
	if i >= len(seg.words) {
		return nil
	}
	name := strings.ToLower(seg.words[i].text)
	if strings.HasPrefix(name, "$") || strings.HasPrefix(name, "(") {
		return nil
	}
	var out []string
	for _, w := range seg.words[i+1:] {
		t := strings.ToLower(w.text)
		if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "/") {
			continue
		}
		out = append(out, t)
	}
	return out
}

// winPrimaryCommand 返回段的主命令（别名展开后）。
func winPrimaryCommand(seg winSegment) (string, bool) {
	i := 0
	for i < len(seg.words) && (isWinAssignment(seg.words[i].text) || winStructuralKeywords[strings.ToLower(seg.words[i].text)]) {
		i++
	}
	if i >= len(seg.words) {
		return "", false
	}
	name := strings.ToLower(seg.words[i].text)
	if strings.HasPrefix(name, "$") || strings.HasPrefix(name, "{") || strings.HasPrefix(name, "(") {
		return "", false
	}
	return expandWinAlias(trimExecutable(name)), true
}

// classifyWinCommand 是主命令规则分发（cmd 已小写并展开别名；args 小写）。
func classifyWinCommand(cmd string, args []string, seg winSegment, depth int, pipelinePrev []string, acc *accumulator) {
	joined := strings.Join(args, " ")

	switch {

	// --- 破坏文件系统/卷影/启动 ------------------------------------------
	case cmd == "format-volume", cmd == "format-partition", cmd == "clear-disk",
		cmd == "initialize-disk", cmd == "remove-partition", cmd == "reset-physicaldisk",
		cmd == "repair-physicaldisk", cmd == "format", cmd == "diskpart",
		cmd == "bootcfg", cmd == "bcdboot", cmd == "repair-bde":
		acc.add(LevelForbidden, "WIN_FORMAT", "filesystem/volume/boot destruction")
		return
	case cmd == "cipher" && contains(args, "/w"):
		acc.add(LevelForbidden, "WIN_FORMAT", "cipher /w wipes free space")
		return
	case cmd == "vssadmin" && contains(args, "delete"):
		acc.add(LevelForbidden, "WIN_FORMAT", "vssadmin deletes shadow copies")
		return
	case cmd == "wbadmin" && contains(args, "delete"):
		acc.add(LevelForbidden, "WIN_FORMAT", "wbadmin deletes backup catalog")
		return
	case cmd == "bcdedit" && contains(args, "/set"):
		acc.add(LevelForbidden, "WIN_FORMAT", "bcdedit rewrites boot configuration")
		return

	// --- 电源 --------------------------------------------------------------
	case cmd == "stop-computer", cmd == "restart-computer", cmd == "shutdown",
		cmd == "psshutdown":
		acc.add(LevelForbidden, "WIN_POWER", "power state change")
		return

	// --- 远程执行 ----------------------------------------------------------
	case cmd == "enter-pssession", cmd == "new-pssession", cmd == "psexec":
		acc.add(LevelForbidden, "WIN_REMOTE_EXEC", "unauditable remote execution")
		return
	case cmd == "invoke-wmimethod" && contains(args, "-computername"):
		acc.add(LevelForbidden, "WIN_REMOTE_EXEC", "remote WMI execution")
		return
	case cmd == "wmic" && strings.Contains(joined, "/node:"):
		acc.add(LevelForbidden, "WIN_REMOTE_EXEC", "wmic remote execution")
		return

	// --- 凭据转储 ----------------------------------------------------------
	case cmd == "mimikatz", cmd == "invoke-mimikatz", cmd == "secretsdump",
		cmd == "ntdsutil", cmd == "get-adreplaccount":
		acc.add(LevelForbidden, "WIN_CRED_ACCESS", "credential dump")
		return
	case cmd == "procdump":
		if contains(args, "-ma") && contains(args, "lsass") {
			acc.add(LevelForbidden, "WIN_CRED_ACCESS", "LSASS memory dump")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_PROCESS_CONTROL", "process dump")
		return
	case cmd == "reg":
		classifyWinReg(args, acc)
		return

	// --- 防护禁用/痕迹清除 --------------------------------------------------
	case cmd == "set-mppreference", cmd == "add-mppreference", cmd == "remove-mppreference":
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "Defender preference tampering")
		return
	case cmd == "uninstall-windowsfeature" && contains(args, "defender"):
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "Defender removal")
		return
	case cmd == "wevtutil" && contains(args, "cl"):
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "event log cleared")
		return
	case cmd == "clear-eventlog", cmd == "clear-winevent", cmd == "remove-eventlog":
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "event log cleared")
		return
	case cmd == "fsutil" && contains(args, "deletejournal"):
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "USN journal deleted")
		return
	case cmd == "clear-history":
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "PowerShell history cleared")
		return
	case cmd == "set-psreadlineoption" && strings.Contains(joined, "historysavepath"):
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "history save path disabled")
		return

	// --- 持久化 --------------------------------------------------------------
	case cmd == "schtasks":
		if contains(args, "/create") {
			acc.add(LevelForbidden, "WIN_PERSISTENCE", "scheduled task created")
			return
		}
		acc.add(LevelForbidden, "WIN_LOLBIN_EXEC", "schtasks launches arbitrary binaries")
		return
	case strings.HasPrefix(cmd, "new-scheduledtask"), cmd == "register-scheduledtask",
		cmd == "register-scheduledjob", cmd == "new-jobtrigger",
		cmd == "register-wmievent", cmd == "register-cimindicationevent":
		acc.add(LevelForbidden, "WIN_PERSISTENCE", "persistence mechanism registered")
		return

	// --- 编码/隐藏执行 --------------------------------------------------------
	case cmd == "certutil":
		if contains(args, "-decode") {
			acc.add(LevelForbidden, "WIN_ENCODED_EXEC", "certutil decodes a payload")
			return
		}
		if contains(args, "-urlcache") {
			acc.add(LevelForbidden, "WIN_DOWNLOAD_EXEC", "certutil downloads remote payload")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "certutil invocation")
		return
	case cmd == "bitsadmin" && contains(args, "/transfer"):
		acc.add(LevelForbidden, "WIN_DOWNLOAD_EXEC", "bitsadmin transfers remote payload")
		return

	// --- LOLBIN ---------------------------------------------------------------
	case cmd == "rundll32", cmd == "regsvr32", cmd == "mshta", cmd == "wscript",
		cmd == "cscript", cmd == "msbuild", cmd == "installutil", cmd == "regsvcs",
		cmd == "regasm", cmd == "msxsl", cmd == "pcalua", cmd == "at":
		acc.add(LevelForbidden, "WIN_LOLBIN_EXEC", "indirect arbitrary code execution")
		return
	case cmd == "forfiles" && contains(args, "/c"):
		acc.add(LevelForbidden, "WIN_LOLBIN_EXEC", "forfiles /c executes a command")
		return

	// --- 别名重命名混淆 ----------------------------------------------------------
	case cmd == "set-alias", cmd == "new-alias":
		acc.add(LevelForbidden, "WIN_OBFUSCATION", "dangerous command renamed via alias")
		return

	// --- 下载器 cmdlet -------------------------------------------------------------
	case cmd == "invoke-webrequest", cmd == "invoke-restmethod", cmd == "start-bitstransfer":
		if strings.Contains(joined, "downloadstring") || strings.Contains(joined, "downloadfile") {
			acc.add(LevelForbidden, "WIN_DOWNLOAD_EXEC", "remote code download")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "network download command")
		return

	// --- Invoke-Expression -----------------------------------------------------------
	case cmd == "invoke-expression":
		for _, a := range args {
			if strings.HasPrefix(a, "$") {
				acc.add(LevelForbidden, "WIN_OBFUSCATION", "dynamic expression from a variable")
				return
			}
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "expression evaluation requires approval")
		return

	// --- 删除 ---------------------------------------------------------------------------
	case cmd == "remove-item":
		classifyWinRM(args, seg, acc)
		return

	// --- 服务 -----------------------------------------------------------------------------
	case cmd == "stop-service", cmd == "disable-service", cmd == "suspend-service":
		classifyWinServiceStop(cmd, args, pipelinePrev, acc)
		return
	case cmd == "set-service":
		if contains(args, "stopped") || contains(args, "disabled") {
			classifyWinServiceStop(cmd, args, pipelinePrev, acc)
			return
		}
		acc.add(LevelNeedsApproval, "WIN_SERVICE_CHANGE", "service configuration change")
		return
	case cmd == "start-service", cmd == "restart-service", cmd == "resume-service",
		cmd == "new-service", cmd == "remove-service":
		acc.add(LevelNeedsApproval, "WIN_SERVICE_CHANGE", "service state change")
		return
	case cmd == "sc":
		classifyWinSC(args, acc)
		return
	case cmd == "net":
		classifyWinNet(args, pipelinePrev, acc)
		return

	// --- 进程 -------------------------------------------------------------------------------
	case cmd == "stop-process", cmd == "taskkill", cmd == "debug-process", cmd == "wait-process":
		classifyWinProcessStop(cmd, args, pipelinePrev, acc)
		return

	// --- 账号 ---------------------------------------------------------------------------------
	case cmd == "new-localuser", cmd == "remove-localuser", cmd == "set-localuser",
		cmd == "new-localgroup", cmd == "remove-localgroup":
		acc.add(LevelNeedsApproval, "WIN_ACCOUNT_CHANGE", "local account change")
		return

	// --- 防火墙/网络 ----------------------------------------------------------------------------
	case cmd == "netsh" && len(args) > 0 && args[0] == "advfirewall":
		acc.add(LevelNeedsApproval, "WIN_FIREWALL_CHANGE", "firewall configuration change")
		return
	case cmd == "new-netfirewallrule", cmd == "remove-netfirewallrule",
		cmd == "disable-netfirewallrule", cmd == "set-netfirewallprofile",
		cmd == "disable-netadapter", cmd == "rename-netadapter",
		cmd == "new-netipaddress", cmd == "remove-netipaddress",
		cmd == "set-dnsclientserveraddress":
		acc.add(LevelNeedsApproval, "WIN_FIREWALL_CHANGE", "network boundary change")
		return
	case cmd == "set-netfirewallrule":
		acc.add(LevelNeedsApproval, "WIN_RDP_CHANGE", "firewall rule change")
		return
	case cmd == "route" && (contains(args, "add") || contains(args, "delete")):
		acc.add(LevelNeedsApproval, "WIN_FIREWALL_CHANGE", "routing table change")
		return

	// --- 注册表 -----------------------------------------------------------------------------------
	case cmd == "set-executionpolicy":
		acc.add(LevelNeedsApproval, "WIN_REG_WRITE", "execution policy change")
		return
	case cmd == "new-itemproperty", cmd == "set-itemproperty", cmd == "remove-itemproperty":
		if strings.Contains(joined, "\\run") || strings.Contains(joined, "/run") {
			acc.add(LevelForbidden, "WIN_PERSISTENCE", "Run key persistence")
			return
		}
		if strings.Contains(joined, "fdenytsconnections") {
			acc.add(LevelNeedsApproval, "WIN_RDP_CHANGE", "RDP access surface change")
			return
		}
		if strings.HasPrefix(joined, "hklm:") || strings.HasPrefix(joined, "hkcu:") ||
			strings.Contains(joined, "hklm:") || strings.Contains(joined, "hkcu:") ||
			strings.Contains(joined, "registry::") {
			acc.add(LevelNeedsApproval, "WIN_REG_WRITE", "registry write")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_FILE_WRITE", "item property write")
		return

	// --- 软件 -------------------------------------------------------------------------------------
	case cmd == "install-package", cmd == "uninstall-package", cmd == "install-windowsfeature",
		cmd == "choco", cmd == "winget", cmd == "msiexec", cmd == "wusa", cmd == "dism":
		acc.add(LevelNeedsApproval, "WIN_SOFTWARE_CHANGE", "software state change")
		return

	// --- 文件/权限 -----------------------------------------------------------------------------------
	case cmd == "set-content", cmd == "add-content", cmd == "out-file", cmd == "set-item",
		cmd == "copy-item", cmd == "move-item", cmd == "new-item",
		cmd == "icacls", cmd == "cacls", cmd == "attrib", cmd == "takeown",
		cmd == "compact", cmd == "expand":
		acc.add(LevelNeedsApproval, "WIN_FILE_WRITE", "file or permission change")
		return

	// --- 环境泄露 -------------------------------------------------------------------------------------
	case cmd == "get-childitem" && len(args) > 0 && strings.HasPrefix(args[0], "env:"):
		acc.add(LevelNeedsApproval, "WIN_ENV_DUMP", "environment variable enumeration")
		return
	case cmd == "get-content", cmd == "get-item":
		for _, f := range winSensitiveFiles {
			if strings.Contains(joined, f) {
				acc.add(LevelNeedsApproval, "WIN_ENV_DUMP", "reads credential-bearing file")
				return
			}
		}
		if strings.Contains(joined, "historysavepath") && cmd == "get-content" {
			acc.add(LevelNeedsApproval, "WIN_ENV_DUMP", "reads shell history")
			return
		}

	// --- 容器（四期 C1，契约 7.3B WIN_CONTAINER_* / 7.3D 同构） --------------------------------------
	case cmd == "docker", cmd == "podman", cmd == "nerdctl", cmd == "compose":
		classifyWinContainer(cmd, args, depth, acc)
		return
	}
	// fallthrough 到白名单/兜底判定。

	// --- 只读白名单 --------------------------------------------------------------------------------------
	if winSafeReadCommands[cmd] && winSegIsClean(seg) {
		acc.add(LevelSafe, "WIN_SAFE_READ", "read-only inspection command")
		return
	}

	// --- 重定向 ------------------------------------------------------------------------------------------
	if seg.redir {
		acc.add(LevelNeedsApproval, "WIN_FILE_WRITE", "output redirection to a file")
		return
	}

	acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "unrecognized command; approval required")
}

// winSegIsClean 判断段是否满足 safe 附加条件：无重定向、无子表达式、无脚本块。
func winSegIsClean(seg winSegment) bool {
	return !seg.redir && len(seg.subs) == 0 && len(seg.blocks) == 0
}

// classifyWinReg 处理 reg.exe 子命令。
func classifyWinReg(args []string, acc *accumulator) {
	if len(args) == 0 {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "reg command")
		return
	}
	sub := args[0]
	joined := strings.Join(args, " ")
	switch sub {
	case "save":
		// SAM/SECURITY/SYSTEM hive 转储；lsass 不是 hive 但 reg save 也按凭据
		// 访问处理（契约 WIN_CRED_ACCESS）。
		if strings.Contains(joined, "sam") || strings.Contains(joined, "security") ||
			strings.Contains(joined, "system") || strings.Contains(joined, "lsass") {
			acc.add(LevelForbidden, "WIN_CRED_ACCESS", "registry hive dumped for credential access")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_REG_WRITE", "registry hive export")
		return
	case "add", "delete", "load", "restore", "import":
		if strings.Contains(joined, "\\run") || strings.Contains(joined, "/run") {
			acc.add(LevelForbidden, "WIN_PERSISTENCE", "Run key persistence")
			return
		}
		if strings.Contains(joined, "fdenytsconnections") {
			acc.add(LevelNeedsApproval, "WIN_RDP_CHANGE", "RDP access surface change")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_REG_WRITE", "registry write")
		return
	default:
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "reg query")
	}
}

// classifyWinRM 处理 Remove-Item（别名 rm/del/rd/eri；cmd 风格 /s /f /q）。
func classifyWinRM(args []string, seg winSegment, acc *accumulator) {
	recursive, force := false, false
	targets := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a == "-recurse", a == "/s", a == "-r":
			recursive = true
		case a == "-force", a == "/f", a == "/q":
			force = true
		case strings.HasPrefix(a, "-"), strings.HasPrefix(a, "/"):
			continue
		default:
			targets = append(targets, a)
		}
	}

	// PowerShell 历史文件删除 → 清除痕迹。
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "historysavepath") || strings.Contains(joined, "consolehost_history") {
		acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "PowerShell history deleted")
		return
	}

	if len(targets) == 0 {
		if recursive || force {
			acc.add(LevelForbidden, "WIN_RM_PROTECTED", "recursive delete with implicit targets")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "remove-item without target")
		return
	}

	for _, t := range targets {
		clean := cleanWinPath(t)
		if recursive || force {
			if isWinProtectedRM(clean) {
				acc.add(LevelForbidden, "WIN_RM_PROTECTED", "delete of system root or critical directory")
				return
			}
			if strings.Contains(clean, "..") {
				acc.add(LevelForbidden, "WIN_RM_PROTECTED", "recursive delete may traverse to a protected root")
				return
			}
		}
	}
	if recursive || force {
		acc.add(LevelNeedsApproval, "WIN_RM_ANY", "recursive/forced delete")
		return
	}
	acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "remove-item command")
}

// classifyWinServiceStop 处理 Stop-Service/Disable-Service/Set-Service Stopped；
// 目标可来自管线前段（Get-Service WinRM | Stop-Service）。
func classifyWinServiceStop(cmd string, args []string, pipelinePrev []string, acc *accumulator) {
	targets := serviceTargets(args)
	if len(targets) == 0 {
		targets = pipelinePrev
	}
	for _, t := range targets {
		if winCriticalServices[t] {
			acc.add(LevelForbidden, "WIN_CRITICAL_SERVICE_STOP", "stop/disable critical service")
			return
		}
	}
	acc.add(LevelNeedsApproval, "WIN_SERVICE_CHANGE", "service state change")
}

// serviceTargets 提取服务操作的目标名（-Name 值或位置参数，含引号剥离与
// .service 后缀剥离）。
func serviceTargets(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-name" && i+1 < len(args) {
			out = append(out, normalizeWinService(args[i+1]))
			i++
			continue
		}
		if strings.HasPrefix(a, "-name:") {
			out = append(out, normalizeWinService(strings.TrimPrefix(a, "-name:")))
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, normalizeWinService(a))
	}
	return out
}

func normalizeWinService(s string) string {
	s = strings.TrimSuffix(s, ".service")
	return strings.ToLower(s)
}

// classifyWinSC 处理 sc.exe。
func classifyWinSC(args []string, acc *accumulator) {
	if len(args) == 0 {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "sc command")
		return
	}
	sub := args[0]
	joined := strings.Join(args, " ")
	switch sub {
	case "stop", "delete", "config":
		if strings.Contains(joined, "binpath=") || strings.Contains(joined, "binpath ") {
			acc.add(LevelForbidden, "WIN_PERSISTENCE", "service binary path rewritten")
			return
		}
		if sub == "config" && strings.Contains(joined, "windefend") && strings.Contains(joined, "start= disabled") {
			acc.add(LevelForbidden, "WIN_DEFENSE_EVADE", "Defender disabled via sc config")
			return
		}
		for _, t := range args[1:] {
			if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "/") || strings.Contains(t, "=") {
				continue
			}
			if winCriticalServices[t] {
				acc.add(LevelForbidden, "WIN_CRITICAL_SERVICE_STOP", "stop/delete critical service")
				return
			}
		}
		if sub == "config" {
			acc.add(LevelNeedsApproval, "WIN_REG_WRITE", "service configuration change")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_SERVICE_CHANGE", "service state change")
		return
	case "start":
		acc.add(LevelNeedsApproval, "WIN_SERVICE_CHANGE", "service state change")
		return
	default:
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "sc query")
	}
}

// classifyWinNet 处理 net.exe。
func classifyWinNet(args []string, pipelinePrev []string, acc *accumulator) {
	if len(args) == 0 {
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "net command")
		return
	}
	joined := strings.Join(args, " ")
	switch args[0] {
	case "stop", "start":
		targets := args[1:]
		if len(targets) == 0 {
			targets = pipelinePrev
		}
		for _, t := range targets {
			if winCriticalServices[t] {
				acc.add(LevelForbidden, "WIN_CRITICAL_SERVICE_STOP", "stop critical service")
				return
			}
		}
		acc.add(LevelNeedsApproval, "WIN_SERVICE_CHANGE", "service state change")
		return
	case "user":
		acc.add(LevelNeedsApproval, "WIN_ACCOUNT_CHANGE", "account change")
		return
	case "localgroup":
		if strings.Contains(joined, "/add") {
			acc.add(LevelNeedsApproval, "WIN_ACCOUNT_CHANGE", "group membership change")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "net localgroup")
		return
	case "accounts":
		acc.add(LevelNeedsApproval, "WIN_ACCOUNT_CHANGE", "account policy change")
		return
	default:
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "net command")
	}
}

// classifyWinProcessStop 处理 Stop-Process/Taskkill。
func classifyWinProcessStop(cmd string, args []string, pipelinePrev []string, acc *accumulator) {
	forced := cmd == "taskkill" && contains(args, "/f")
	forced = forced || (cmd == "stop-process" && contains(args, "-force"))
	targets := processTargets(args)
	if len(targets) == 0 {
		targets = pipelinePrev
	}
	for _, t := range targets {
		if winCriticalProcesses[t] {
			acc.add(LevelForbidden, "WIN_CRITICAL_SERVICE_STOP", "kill critical system process")
			return
		}
	}
	acc.add(LevelNeedsApproval, "WIN_PROCESS_CONTROL", "affects running processes")
}

func processTargets(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-name" && i+1 < len(args) {
			out = append(out, normalizeWinProcess(args[i+1]))
			i++
			continue
		}
		if a == "/im" && i+1 < len(args) {
			out = append(out, normalizeWinProcess(args[i+1]))
			i++
			continue
		}
		if a == "/pid" || a == "-id" {
			i++ // PID 数字对关键进程判定无意义
			continue
		}
		if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "/") {
			continue
		}
		out = append(out, normalizeWinProcess(a))
	}
	return out
}

func normalizeWinProcess(s string) string {
	s = strings.ToLower(s)
	for _, ext := range []string{".exe", ".com"} {
		s = strings.TrimSuffix(s, ext)
	}
	return s
}

// ---------------------------------------------------------------------------
// 管线
// ---------------------------------------------------------------------------

// classifyWinSegments 按管线链分类：后段的目标可来自前段操作数
// （Get-Service WinRM | Stop-Service）。
func classifyWinSegments(segs []winSegment, depth int, acc *accumulator) {
	var prev []string
	for i := range segs {
		classifyWinSegment(segs[i], depth, acc, prev)
		if segs[i].pipeTo {
			prev = winOperands(segs[i])
		} else {
			prev = nil
		}
	}
	classifyWinPipelines(segs, acc)
}

// classifyWinPipelines 检查 | 连接的命令链：下载内容管道到 iex 是
// WIN_DOWNLOAD_EXEC；全部段只读时给出 WIN_SAFE_PIPE。
func classifyWinPipelines(segs []winSegment, acc *accumulator) {
	i := 0
	for i < len(segs) {
		j := i
		for j < len(segs) && segs[j].pipeTo {
			j++
		}
		if j > i && j < len(segs) {
			cmds := make([]string, 0, j-i+1)
			allSafe := true
			for k := i; k <= j; k++ {
				c, ok := winPrimaryCommand(segs[k])
				if !ok {
					allSafe = false
					continue
				}
				cmds = append(cmds, c)
				if !winSafeReadCommands[c] || !winSegIsClean(segs[k]) {
					allSafe = false
				}
			}
			if allSafe && len(cmds) >= 2 {
				acc.add(LevelSafe, "WIN_SAFE_PIPE", "pipeline of read-only commands")
			}
			// 下载管道执行：iwr/wget/curl（展开后 invoke-webrequest）→ iex。
			if len(cmds) >= 2 && cmds[len(cmds)-1] == "invoke-expression" {
				for _, c := range cmds[:len(cmds)-1] {
					switch c {
					case "invoke-webrequest", "invoke-restmethod", "invoke-expression":
						acc.add(LevelForbidden, "WIN_DOWNLOAD_EXEC", "downloaded content piped to Invoke-Expression")
					case "get-content", "format-list", "format-table":
						acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "content piped to Invoke-Expression")
					}
				}
			}
		}
		i = j + 1
	}
}

// winGlobalTokens 是整条命令级危险载荷词（大小写不敏感）。
var winGlobalTokens = []struct{ token, rule, reason string }{
	{"downloadstring", "WIN_DOWNLOAD_EXEC", "DownloadString fetches remote code"},
	{"downloaddata", "WIN_DOWNLOAD_EXEC", "DownloadData fetches remote code"},
	{"downloadfile", "WIN_DOWNLOAD_EXEC", "DownloadFile fetches remote payload"},
	{"frombase64string", "WIN_ENCODED_EXEC", "base64 payload decoded and executed"},
	{"amsiinitfailed", "WIN_OBFUSCATION", "AMSI patching"},
	{"amsi.dll", "WIN_OBFUSCATION", "amsi.dll reflection"},
	{"[scriptblock]::create", "WIN_OBFUSCATION", "dynamic command construction"},
	{"comsvcs.dll", "WIN_CRED_ACCESS", "LSASS minidump via comsvcs.dll"},
	{"ntds.dit", "WIN_CRED_ACCESS", "NTDS.dit copy"},
}

// winGlobalForbidden 对规范化文本做与分段无关的载荷扫描。
func winGlobalForbidden(normalized string) (ruleHit, bool) {
	low := strings.ToLower(normalized)
	for _, tok := range winGlobalTokens {
		if strings.Contains(low, tok.token) {
			return ruleHit{rule: tok.rule, reason: tok.reason}, true
		}
	}
	return ruleHit{}, false
}

// ---------------------------------------------------------------------------
// 容器命令（四期 C1，契约 7.3B WIN_CONTAINER_* 行 / 7.3D 同构）
// ---------------------------------------------------------------------------

// winContainerEscapeMounts 是 Windows 下 CONTAINER_ESCAPE 的宿主敏感挂载根
// （契约 7.3B WIN_CONTAINER_ESCAPE：C:\、C:\Windows、\\.\pipe\docker_engine；
// 判定用盘符语义——大小写不敏感（args 已小写）、\ 与 / 等价）。
var winContainerEscapeMounts = []string{
	"c:/", "c:/windows", `\\.\pipe\docker_engine`, `//./pipe/docker_engine`,
}

// classifyWinContainer 分级 Windows 目标上的 docker/podman/nerdctl/compose
// 命令（args 已小写化）。判定顺序同 7.3D：全局选项（WIN_CONTAINER_REMOTE）
// → 别名前缀归一 → exec/run 载荷递归（Windows 分册）→ 逃逸判定
// （WIN_CONTAINER_ESCAPE）→ 子命令分级表 → WIN_DEFAULT_ASK。
func classifyWinContainer(cmd string, args []string, depth int, acc *accumulator) {
	// 独立 compose 二进制等价 `docker compose`：参数整体走 compose 分级表。
	if cmd == "compose" {
		classifyWinComposeArgs(args, depth, acc)
		return
	}
	// 全局选项检查（WIN_CONTAINER_REMOTE）：选项出现在子命令前。
	idx := containerSubIndex(args)
	if hasContainerRemoteOption(args, idx) {
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_REMOTE", "global option targets a remote daemon or named context")
	}
	if idx < 0 {
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_REMOTE", "container command without a subcommand")
		return
	}
	rest := args[idx:] // 含子命令本身，交别名归一

	sub, rest := normalizeContainerSub(rest)
	switch sub {
	case "compose":
		classifyWinComposeArgs(rest, depth, acc)
		return
	case "exec":
		// 契约 7.3B 补充：docker/podman exec|run（含 compose）的 shell -c
		// 载荷按本分册递归分类取最高级别；无载荷 CONTAINER_CHANGE。
		if script, ok := extractShellC(rest); ok {
			classifyWinScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "container exec")
		return
	case "run":
		classifyWinContainerEscape(rest, acc)
		if script, ok := extractShellC(rest); ok {
			classifyWinScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "container run")
		return
	case "context":
		if containsAny(rest, "create", "use", "rm", "remove") {
			acc.add(LevelNeedsApproval, "WIN_CONTAINER_REMOTE", "context management may redirect the daemon endpoint")
			return
		}
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "container context command")
		return
	case "logs":
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_LOGS", "container logs may contain credentials")
		return
	}

	if isContainerSafeSub(sub, rest) {
		acc.add(LevelSafe, "WIN_CONTAINER_STATUS", "container status query")
		return
	}
	if isContainerChangeSub(sub, rest) {
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "container state change")
		return
	}
	acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "container command")
}

// classifyWinComposeArgs 分级 Windows 目标上的 `compose <sub>`（同 7.3D 末行）。
func classifyWinComposeArgs(args []string, depth int, acc *accumulator) {
	sub, rest := normalizeContainerSub(args)
	switch sub {
	case "ps", "config", "version", "ls":
		acc.add(LevelSafe, "WIN_CONTAINER_STATUS", "compose status query")
		return
	case "logs":
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_LOGS", "compose logs may contain credentials")
		return
	case "exec":
		if script, ok := extractShellC(rest); ok {
			classifyWinScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "compose exec")
		return
	case "run":
		classifyWinContainerEscape(rest, acc)
		if script, ok := extractShellC(rest); ok {
			classifyWinScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "compose run")
		return
	case "":
		acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "compose command")
		return
	}
	if composeChangeSubs[sub] {
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "compose state change")
		return
	}
	if isContainerChangeSub(sub, rest) {
		acc.add(LevelNeedsApproval, "WIN_CONTAINER_CHANGE", "compose state change")
		return
	}
	acc.add(LevelNeedsApproval, "WIN_DEFAULT_ASK", "compose command")
}

// classifyWinContainerEscape 判定 Windows run 的逃逸面（WIN_CONTAINER_ESCAPE）：
// 特权 flag 与宿主敏感挂载（盘符/命名管道语义，\ 与 / 等价、已小写）。
func classifyWinContainerEscape(args []string, acc *accumulator) {
	for _, a := range args {
		name, value, hasValue := strings.Cut(a, "=")
		switch name {
		case "--privileged":
			acc.add(LevelForbidden, "WIN_CONTAINER_ESCAPE", "privileged container is equivalent to host root")
		case "--pid", "--ipc", "--userns", "--network", "--net":
			if hasValue && value == "host" {
				acc.add(LevelForbidden, "WIN_CONTAINER_ESCAPE", "host namespace sharing is a container escape")
			}
		}
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pid", "--ipc", "--userns", "--network", "--net":
			if i+1 < len(args) && args[i+1] == "host" {
				acc.add(LevelForbidden, "WIN_CONTAINER_ESCAPE", "host namespace sharing is a container escape")
			}
		}
	}
	for i := 0; i < len(args); i++ {
		var src string
		switch {
		case args[i] == "-v" || args[i] == "--volume":
			if i+1 >= len(args) {
				continue
			}
			src = windowsMountSource(args[i+1])
			i++
		case strings.HasPrefix(args[i], "--volume="):
			src = windowsMountSource(strings.TrimPrefix(args[i], "--volume="))
		case args[i] == "--mount" || strings.HasPrefix(args[i], "--mount="):
			src = mountSource(args[i], args, &i)
		default:
			continue
		}
		if src == "" {
			continue
		}
		if strings.Contains(src, "$") {
			acc.add(LevelForbidden, "WIN_CONTAINER_ESCAPE", "mount source contains an unresolved variable")
			continue
		}
		norm := strings.ToLower(strings.ReplaceAll(src, `\`, "/"))
		for _, root := range winContainerEscapeMounts {
			r := strings.ReplaceAll(root, `\`, "/")
			prefix := r
			if !strings.HasSuffix(prefix, "/") && !strings.Contains(prefix, "pipe/") {
				prefix += "/"
			}
			if norm == r || strings.HasPrefix(norm, prefix) || strings.HasPrefix(norm, prefix+"/") {
				acc.add(LevelForbidden, "WIN_CONTAINER_ESCAPE", "host path mounted into container is an escape")
				break
			}
		}
	}
}

// windowsMountSource 提取 Windows -v/--volume 的挂载源：`c:/:/host` → `c:/`、
// `C:\Windows:/win` → `C:\Windows`、`\\.\pipe\docker_engine:/pipe` → 管道名。
// 盘符（单字母+冒号）中的第一个冒号不是挂载分隔符。
func windowsMountSource(vol string) string {
	if len(vol) >= 2 && vol[1] == ':' && isAlphaByte(vol[0]) {
		rest := vol[2:]
		idx := strings.Index(rest, ":")
		if idx < 0 {
			return vol
		}
		return vol[:2+idx]
	}
	idx := strings.Index(vol, ":")
	if idx < 0 {
		return vol
	}
	return vol[:idx]
}
