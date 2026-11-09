package risk

import (
	"path"
	"regexp"
	"strings"
)

// rules.go 实现 CONTRACT.md 7.3 的规则表与 7.4 的防绕过要求：
// shell 分词、分段、包装器剥离、嵌套分类与逐条规则匹配。

// ---------------------------------------------------------------------------
// 分词与分段
// ---------------------------------------------------------------------------

type word struct {
	text   string // 去引号后的内容
	raw    string // 原始写法（含引号与转义），用于识别反引号/$IFS 等
	quoted bool
}

type redirect struct {
	op     string
	target string
}

type segment struct {
	words    []word
	redirs   []redirect
	substs   []string // 命令替换 / 进程替换 / 子 shell 的内部命令
	subshell bool
	pipeTo   bool // 该段后面紧跟 | 或 |&
}

var errUnbalanced = &riskError{"unbalanced shell syntax"}

// arithClosed 判断 $( ... ) 是否以紧邻的 "))" 收尾。
//
// bash 只在这种形式下才把 $(( ... )) 当算术展开解析；否则（如 `$(()0)`、
// “ $(()`id`) “）会回退成命令替换解析并尝试执行内部内容。因此只有
// 算术收尾成立时才能按纯算术忽略，否则必须按命令替换递归分类（失败关闭）。
func arithClosed(s string, start, end int) bool {
	return end-start >= 4 && s[end-2] == ')' && s[end-1] == ')'
}

// isArithExpansion 判断 $( 后的配对内容 inner 是否构成纯算术展开
// $(( ... ))。bash 只在算术体可解析为算术表达式时才按算术求值，解析失败
// （如 $(($(())))、$(()$((0)))）会回退成命令替换执行内部内容。因此在
// arithClosed 之上增加两条失败关闭约束：
//   - inner != "()"：$(()) 是空命令替换（实际会 fork）；
//   - 算术体通过 isArithBody 结构验证：只允许数字/标识符/运算符/嵌套
//     括号/变量与嵌套 $((...))；出现 $(...)、反引号或其他无法静态证明
//     为算术的内容时按命令替换递归分类。
func isArithExpansion(s string, start, end int, inner string) bool {
	return strings.HasPrefix(inner, "(") && inner != "()" && arithClosed(s, start, end) &&
		isArithBody(inner[1:len(inner)-1])
}

// hasArithOperand 判断（嵌套）算术体是否含至少一个非空白 token；
// 嵌套空括号 () / ( ) 在 bash 中是算术语法错误，会回退成命令替换。
func hasArithOperand(b string) bool {
	return strings.TrimSpace(b) != ""
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isArithBody 验证 $(( BODY )) 的算术体是结构合法的算术表达式：
// 只允许数字/标识符（含 # 进制记法与 .）/运算符/嵌套圆括号/变量引用/
// 嵌套 $((...))，并用"期待操作数/期待运算符"状态机校验 token 顺序——
// 二元运算符缺左操作数（如 <((0))）在 bash 中是算术语法错误，会回退成
// 命令替换执行（< 还是进程替换语义，会 fork）。任何无法静态证明为算术
// 的内容返回 false，调用方按命令替换递归分类（失败关闭）。
func isArithBody(b string) bool {
	expectOperand := true
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '+' || c == '-' || c == '!' || c == '~':
			// 一元（+ - ! ~）/二元（+ -）双态：一元保持期待操作数，
			// 二元切换到期待操作数。
			expectOperand = true
			i++
		case c == '*' || c == '/' || c == '%' || c == '^' || c == '&' ||
			c == '|' || c == '<' || c == '>' || c == '=' || c == ',' ||
			c == '?' || c == ':':
			// 二元/三元运算符必须出现在期待运算符处（有左操作数）；
			// 缺左操作数（如开头的 <()）是 bash 算术语法错误，回退执行。
			if expectOperand {
				return false
			}
			expectOperand = true
			i++
		case isIdentChar(c) || c == '.' || c == '#':
			if !expectOperand {
				return false
			}
			// 消费完整 token（含 base 记法 16#ff 与浮点 1.5），
			// 避免把 # 当独立字符而误拒合法算术。
			for i < len(b) && (isIdentChar(b[i]) || b[i] == '.' || b[i] == '#') {
				i++
			}
			expectOperand = false
		case c == '(':
			if !expectOperand {
				return false
			}
			depth, j := 1, i+1
			for j < len(b) && depth > 0 {
				if b[j] == '(' {
					depth++
				} else if b[j] == ')' {
					depth--
				}
				j++
			}
			if depth != 0 || !hasArithOperand(b[i+1:j-1]) || !isArithBody(b[i+1:j-1]) {
				return false
			}
			expectOperand = false
			i = j
		case c == ')':
			return false
		case c == '$':
			if !expectOperand {
				return false
			}
			if i+1 >= len(b) {
				return false
			}
			switch {
			case b[i+1] == '{':
				end := strings.IndexByte(b[i:], '}')
				if end < 0 {
					return false
				}
				i += end + 1
			case b[i+1] == '(':
				if i+2 >= len(b) || b[i+2] != '(' {
					return false // $(cmd) 命令替换：按替换递归分类
				}
				depth, j := 0, i+1
				for ; j < len(b); j++ {
					if b[j] == '(' {
						depth++
					} else if b[j] == ')' {
						depth--
						if depth == 0 {
							break
						}
					}
				}
				if depth != 0 || !isArithExpansion(b, i, j+1, b[i+2:j]) {
					return false
				}
				i = j + 1
			default:
				// $$、$?、$1、$name 等变量引用：'$' 与后续标识符字符
				// 一起构成完整操作数，变量名不是独立 token。
				i++
				for i < len(b) && isIdentChar(b[i]) {
					i++
				}
			}
			expectOperand = false
		default:
			return false
		}
	}
	// 顶层空体（$(( ))）在 bash 中合法求值为 0；其余必须以操作数收尾。
	return !expectOperand || strings.TrimSpace(b) == ""
}

// parseCommand 把规范化后的命令切分成段；失败返回错误（调用方转为 OBFUSCATION）。
func parseCommand(s string) ([]segment, error) {
	var (
		segs    []segment
		cur     segment
		textBuf []byte // 当前词的 text/raw 用字节缓冲追加，避免 O(n²) 字符串重分配
		rawBuf  []byte
		quoted  bool
		hasW    bool
	)

	appendRune := func(b byte) {
		textBuf = append(textBuf, b)
		rawBuf = append(rawBuf, b)
		hasW = true
	}
	appendStr := func(text, raw string) {
		textBuf = append(textBuf, text...)
		rawBuf = append(rawBuf, raw...)
		hasW = true
	}
	resetWord := func() {
		textBuf, rawBuf = textBuf[:0], rawBuf[:0]
		quoted = false
		hasW = false
	}
	finishWord := func() {
		if hasW {
			cur.words = append(cur.words, word{text: string(textBuf), raw: string(rawBuf), quoted: quoted})
			resetWord()
		}
	}
	finishSegment := func(pipe bool) {
		finishWord()
		if len(cur.words) > 0 || len(cur.redirs) > 0 || len(cur.substs) > 0 || cur.subshell {
			cur.pipeTo = pipe
			segs = append(segs, cur)
		}
		cur = segment{}
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
				if c == '\\' && j+1 < len(s) {
					textBuf = append(textBuf, s[j], s[j+1])
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
					cur.substs = append(cur.substs, inner)
					appendStr(s[j:end], s[j:end])
					j = end
					continue
				}
				if c == '`' {
					inner, end, err := scanBacktick(s, j)
					if err != nil {
						return nil, err
					}
					cur.substs = append(cur.substs, inner)
					appendStr(s[j:end], s[j:end])
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

		case '\\':
			if i+1 >= len(s) {
				return nil, errUnbalanced
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
				if isArithExpansion(s, i, end, inner) {
					// $(( 算术展开，本身不是命令替换；但其中的 $(...) 仍会被
					// shell 执行，必须递归收集后参与风险分类（CONTRACT 7.4）。
					subs, err := collectSubstitutions(inner)
					if err != nil {
						return nil, err
					}
					cur.substs = append(cur.substs, subs...)
					appendStr(s[i:end], s[i:end])
					i = end
					continue
				}
				// 无算术收尾：bash 会回退为命令替换，按命令替换整体分类。
				cur.substs = append(cur.substs, inner)
				appendStr(s[i:end], s[i:end])
				i = end
				continue
			}
			appendRune(ch)
			i++

		case '`':
			inner, end, err := scanBacktick(s, i)
			if err != nil {
				return nil, err
			}
			cur.substs = append(cur.substs, inner)
			appendStr(s[i:end], s[i:end])
			i = end

		case '<', '>':
			if i+1 < len(s) && s[i+1] == '(' {
				inner, end, err := scanBalanced(s, i+1)
				if err != nil {
					return nil, err
				}
				cur.substs = append(cur.substs, inner)
				cur.subshell = true
				appendStr(s[i:end], s[i:end])
				i = end
				continue
			}
			// 重定向。形如 2> 的前导数字属于 fd，不属于命令词。
			fd := ""
			if hasW && isAllDigits(string(textBuf)) && ch == '>' {
				fd = string(textBuf)
				resetWord()
			} else {
				finishWord()
			}
			op, next := parseRedirectOp(s, i)
			target, subs, end, err := readRedirectTarget(s, next)
			if err != nil {
				return nil, err
			}
			// <& 后必须是 fd 数字或 '-'（关闭 fd）；其他目标（如
			// cat<&rm -rf）是 bash 的歧义重定向错误，失败关闭为
			// OBFUSCATION，防止危险词藏进 fd 复用目标（fuzz 发现）。
			if op == "<&" {
				t := strings.TrimSpace(target)
				if t != "-" && !isAllDigits(t) {
					return nil, errUnbalanced
				}
			}
			cur.substs = append(cur.substs, subs...)
			cur.redirs = append(cur.redirs, redirect{op: fd + op, target: target})
			i = end

		case '&':
			if i+1 < len(s) && s[i+1] == '>' {
				finishWord()
				// &> / &>> 的操作符从 '>' 起解析；parseRedirectOp 只认 < / >
				// 开头，从 '&' 起解析会返回零消费位置，使 i 不再前进而
				// 死循环（7 字节输入 "&>>>0.." 即可挂死，fuzz 实测引发
				// worker 卡死）。op 补回 '&' 前缀，&> 重定向才能进入
				// classifyRedirect 的输出重定向分支（原先 op 丢失 '>'，
				// "&> file" 被当作输入重定向漏检）。
				inner, next := parseRedirectOp(s, i+1)
				target, subs, end, err := readRedirectTarget(s, next)
				if err != nil {
					return nil, err
				}
				cur.substs = append(cur.substs, subs...)
				cur.redirs = append(cur.redirs, redirect{op: "&" + inner, target: target})
				i = end
				continue
			}
			if i+1 < len(s) && s[i+1] == '&' {
				finishSegment(false)
				i += 2
				continue
			}
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

		case ';', '\n':
			finishSegment(false)
			i++

		case ' ', '\t', '\r':
			finishWord()
			i++

		case '(':
			if hasW {
				return nil, &riskError{"unexpected '(' inside word"}
			}
			inner, end, err := scanBalanced(s, i)
			if err != nil {
				return nil, err
			}
			cur.substs = append(cur.substs, inner)
			cur.subshell = true
			i = end

		case ')':
			return nil, &riskError{"unexpected ')'"}

		default:
			appendRune(ch)
			i++
		}
	}
	finishSegment(false)
	return segs, nil
}

// scanBalanced 从 s[start] == '(' 开始，返回括号内内容与结束位置（右括号之后）。
func scanBalanced(s string, start int) (string, int, error) {
	if start >= len(s) || s[start] != '(' {
		return "", start, errUnbalanced
	}
	depth := 0
	var quote byte
	for i := start; i < len(s); i++ {
		c := s[i]
		switch quote {
		case '\'':
			if c == '\'' {
				quote = 0
			}
			continue
		case '"':
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				quote = 0
			}
			continue
		}
		switch c {
		case '\\':
			i++
		case '\'':
			quote = '\''
		case '"':
			quote = '"'
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start+1 : i], i + 1, nil
			}
		}
	}
	return "", len(s), errUnbalanced
}

// scanBacktick 从 s[start] == '`' 开始扫描到配对反引号。
func scanBacktick(s string, start int) (string, int, error) {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '`' {
			return s[start+1 : i], i + 1, nil
		}
	}
	return "", len(s), &riskError{"unterminated command substitution"}
}

// parseRedirectOp 解析重定向操作符，返回操作符与下一个待读位置。
func parseRedirectOp(s string, start int) (string, int) {
	if start >= len(s) {
		return "", start
	}
	ch := s[start]
	if ch != '<' && ch != '>' {
		return "", start
	}
	i := start + 1
	if i >= len(s) {
		return s[start : start+1], i
	}
	switch {
	case s[i] == ch:
		i++
		if ch == '<' && i < len(s) && s[i] == '<' {
			i++ // <<< herestring
		}
	case s[i] == '&':
		i++
	case s[i] == '|' && ch == '>':
		i++
	case s[i] == '>' && ch == '<':
		i++ // <>
	}
	return s[start:i], i
}

// collectSubstitutions 扫描一段 shell 片段中的命令替换（$() 与反引号），
// 并递归进入嵌套的算术展开。算术展开本身不执行，但其中的 $(...) 会执行。
func collectSubstitutions(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\':
			i += 2
		case s[i] == '`':
			inner, end, err := scanBacktick(s, i)
			if err != nil {
				return nil, err
			}
			out = append(out, inner)
			i = end
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
			inner, end, err := scanBalanced(s, i+1)
			if err != nil {
				return nil, err
			}
			if isArithExpansion(s, i, end, inner) {
				nested, err := collectSubstitutions(inner)
				if err != nil {
					return nil, err
				}
				out = append(out, nested...)
			} else {
				out = append(out, inner)
			}
			i = end
		default:
			i++
		}
	}
	return out, nil
}

// readRedirectTarget 读取重定向目标词（允许引号与反斜杠），同时收集目标中的
// 命令替换：here-string / 输入重定向的目标同样会被 shell 求值。
func readRedirectTarget(s string, start int) (string, []string, int, error) {
	i := start
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var (
		b    strings.Builder
		subs []string
	)
	appendSub := func(inner string, end int) {
		subs = append(subs, inner)
		b.WriteString(s[i:end])
		i = end
	}
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '|' || c == '&' || c == '<' || c == '>' || c == '(' || c == ')' {
			// 零消费保护：目标位置就是分隔符/EOF 时，返回的位置必须严格大于
			// 调用方传入的 start，否则 parseCommand 的 i = end 不再前进，
			// 微型输入即可挂死分类器。此类病态形态失败关闭为 OBFUSCATION。
			if i == start {
				return "", nil, i, errUnbalanced
			}
			break
		}
		switch c {
		case '\'':
			i++
			for i < len(s) && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
		case '"':
			i++
			for i < len(s) && s[i] != '"' {
				switch {
				case s[i] == '\\' && i+1 < len(s):
					i++
					b.WriteByte(s[i])
					i++
				case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
					inner, end, err := scanBalanced(s, i+1)
					if err != nil {
						return "", nil, i, err
					}
					if isArithExpansion(s, i, end, inner) {
						nested, err := collectSubstitutions(inner)
						if err != nil {
							return "", nil, i, err
						}
						subs = append(subs, nested...)
						b.WriteString(s[i:end])
					} else {
						appendSub(inner, end)
						continue
					}
					i = end
				case s[i] == '`':
					inner, end, err := scanBacktick(s, i)
					if err != nil {
						return "", nil, i, err
					}
					appendSub(inner, end)
				default:
					b.WriteByte(s[i])
					i++
				}
			}
			if i < len(s) {
				i++
			}
		case '\\':
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				inner, end, err := scanBalanced(s, i+1)
				if err != nil {
					return "", nil, i, err
				}
				if isArithExpansion(s, i, end, inner) {
					nested, err := collectSubstitutions(inner)
					if err != nil {
						return "", nil, i, err
					}
					subs = append(subs, nested...)
					b.WriteString(s[i:end])
				} else {
					appendSub(inner, end)
					continue
				}
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case '`':
			inner, end, err := scanBacktick(s, i)
			if err != nil {
				return "", nil, i, err
			}
			appendSub(inner, end)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), subs, i, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 分段分类
// ---------------------------------------------------------------------------

func classifySegment(seg segment, depth int, acc *accumulator) {
	if depth > maxDepth {
		acc.add(LevelForbidden, "OBFUSCATION", "nesting depth exceeds 4")
		return
	}
	for _, sub := range seg.substs {
		classifyScript(sub, depth+1, acc)
	}
	// 命令替换是外层 shell 先求值的动态执行路径，静态分类无法证明替换
	// 结果安全：即使替换内容本身只读（fuzz 实测 "sh -c ps `ps`" 与
	// "systemctl status `ps`" 均因主命令与替换内容双双 SAFE_READ 而漏判
	// safe），含替换的段也必须至少 needs_approval（失败关闭；与
	// safeReadCommands 的替换降级同一语义，只收紧不放宽）。
	if len(seg.substs) > 0 {
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "command substitution requires approval")
	}
	// 无命令词但含替换的段（如 `<`id``、<(cmd)）：重定向目标/进程替换
	// 已被 shell 求值执行，而段内没有可分类的主命令，替换内容的 SAFE_READ
	// 不能代表整段——至少 needs_approval（失败关闭；fuzz 发现）。
	if len(seg.words) == 0 && len(seg.substs) > 0 {
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "substitution evaluated without a command")
	}
	for _, r := range seg.redirs {
		classifyRedirect(r, acc)
	}
	classifyWords(seg, depth, acc)
}

// classifyScript 递归分类命令替换 / shell -c / ssh 远端命令等内部脚本。
func classifyScript(script string, depth int, acc *accumulator) {
	if depth > maxDepth {
		acc.add(LevelForbidden, "OBFUSCATION", "nesting depth exceeds 4")
		return
	}
	norm, err := normalize(script)
	if err != nil {
		acc.add(LevelForbidden, "OBFUSCATION", err.Error())
		return
	}
	if norm == "" {
		// 空命令替换（如 sh -c ps ``）也是被 shell 求值的动态命令语法：
		// 递归分类拿不到任何可判 safe 的证据，静默返回会让外层的 SAFE_READ
		// 成为最高级别而整体判 safe（fuzz 不变式 2 实测击穿）。失败关闭为
		// needs_approval，绝不允许空替换稀释安全语义。
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "empty command substitution evaluated")
		return
	}
	if hit, ok := globalForbidden(norm); ok {
		acc.add(LevelForbidden, hit.rule, hit.reason)
	}
	segs, err := parseCommand(norm)
	if err != nil {
		acc.add(LevelForbidden, "OBFUSCATION", err.Error())
		return
	}
	for i := range segs {
		classifySegment(segs[i], depth, acc)
	}
	classifyPipelines(segs, acc)
}

// structuralKeywords 是复合命令分段后落在段首的 shell 结构关键字。
// if true; then rm -rf /etc; fi 分段后 [then rm -rf /etc] 的段首词是 then，
// time rm -rf /etc 的段首词是 time——它们不在规则表与包装器列表中，会让
// 段内危险命令全部落入 DEFAULT_ASK 兜底（forbidden 降级，评审实测）。
// 跳过这些前缀后再取真正的主命令词送规则表；跳过不会放过任何形态，
// 只是把更多真实命令送进判定。
var structuralKeywords = map[string]bool{
	"then": true, "else": true, "fi": true, "do": true, "done": true,
	"in": true, "time": true, "{": true, "}": true, "!": true,
}

func classifyWords(seg segment, depth int, acc *accumulator) {
	words := seg.words
	i := 0
	for i < len(words) && isAssignment(words[i].text) {
		name, val, _ := strings.Cut(words[i].text, "=")
		switch name {
		case "HISTFILE":
			if val == "/dev/null" {
				acc.add(LevelForbidden, "HISTORY_CLEAR", "HISTFILE=/dev/null")
			}
		case "HISTSIZE":
			if val == "0" {
				acc.add(LevelForbidden, "HISTORY_CLEAR", "HISTSIZE=0")
			}
		case "DOCKER_HOST":
			// 四期 C1（契约 7.3D CONTAINER_REMOTE）：环境变量赋值指向
			// 远程 daemon，绕过本机资产与审计面。
			acc.add(LevelNeedsApproval, "CONTAINER_REMOTE", "DOCKER_HOST assignment targets a remote daemon")
		}
		i++
	}
	// env DOCKER_HOST=... docker ps：赋值藏在 env 包装器参数里，剥壳前
	// 先按词扫描兜底（含前缀赋值位置之外的出现，方向保守只升不降）。
	for _, w := range words {
		if strings.HasPrefix(w.text, "DOCKER_HOST=") {
			acc.add(LevelNeedsApproval, "CONTAINER_REMOTE", "DOCKER_HOST assignment targets a remote daemon")
			break
		}
	}
	for i < len(words) && structuralKeywords[words[i].text] {
		i++
	}
	if i >= len(words) {
		// 纯赋值段（如 A=`pwd`）：反引号/$() 替换已被 shell 求值执行，
		// 不能因赋值词被跳过而整体静默 safe（fuzz 发现）。
		if len(seg.substs) > 0 {
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", "assignment evaluates command substitution")
			return
		}
		// 纯结构段（如单独的 fi）：不静默，维持结构词本身的 DEFAULT_ASK 语义。
		if len(words) > 0 {
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", "compound command structure without a runnable command")
		}
		return
	}

	rest, viaXargs := stripWrappers(words[i:])
	if len(rest) == 0 {
		return
	}
	name := rest[0].text
	if name == "" {
		// 首词为空（如 '' `id`）：替换仍被求值执行，不能静默返回
		// 整体 safe（fuzz 发现）。
		if len(seg.substs) > 0 {
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", "substitution evaluated without a command")
		}
		return
	}
	if hasUnresolvedVar(name) || strings.Contains(name, "`") || strings.Contains(name, "$(") {
		acc.add(LevelForbidden, "OBFUSCATION", "dynamic command name")
		return
	}
	cmd := path.Base(strings.TrimRight(name, "/"))
	// 路径调用拦截：命令词带目录前缀（/tmp/ls、./ls、~/ls、$HOME/ls）时，
	// 路径解析发生在远端 shell，静态分类无法证明该路径下的二进制就是
	// basename 对应的系统命令——可写目录（/tmp、/dev/shm、$HOME）预放置的
	// 同名二进制可冒充只读命令，在 auto_exec_enabled 服务器上经免审通道
	// 执行任意代码（评审 verified high，实测 7 形态全部漏判 safe）。任何
	// 路径调用一律至少 needs_approval；basename 仍送规则表，危险命令照常
	// 升级 forbidden（只收紧不放宽）。
	if name != cmd || strings.ContainsAny(name, "~$") {
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "command invoked by path requires approval")
	}
	args := make([]string, 0, len(rest)-1)
	for _, w := range rest[1:] {
		args = append(args, w.text)
	}

	// 下载内容直接喂给 shell（进程替换形式）。
	if isShell(cmd) || cmd == "source" || cmd == "." {
		for _, sub := range seg.substs {
			if startsWithDownload(sub) {
				acc.add(LevelForbidden, "CURL_PIPE_SHELL", "remote script fed to shell via process substitution")
				break
			}
		}
	}

	if viaXargs && destructiveWithoutTargets(cmd, args) {
		acc.add(LevelForbidden, "OBFUSCATION", "xargs feeds a destructive command with implicit targets")
	}

	classifyCommand(cmd, args, seg, depth, acc)
}

// isAssignment 判断词是否是 VAR=... 形式的赋值前缀（^[A-Za-z_][A-Za-z0-9_]*=）。
//
// 手写字节扫描替代正则：Go regexp 对该模式走 onepass/backtrack 引擎，
// bitState 位图按「指令数 × 输入长度」分配，且 -race 下分配被进一步放大
// ——fuzz 守卫实测 5000 字节的反引号轰炸（1000 个替换词逐个 MatchString）
// 在 -race 下分配 19MB（预算 8MB），其中 73% 是 regexp.(*bitState).reset。
// 前缀匹配无需正则，逐字节扫描零分配。
func isAssignment(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '_' || isAlphaByte(s[i])) {
		i++
	} else {
		return false
	}
	for i < len(s) {
		c := s[i]
		if c == '_' || isAlphaByte(c) || (c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return i < len(s) && s[i] == '='
}

func isAlphaByte(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// stripWrappers 递归剥离 sudo/env/timeout/xargs 等包装器，返回真正的命令词。
//
// 若包装器后没有真正的命令（例如单独的 `env`、`sudo`），保留原词，让
// classifyCommand 把 `env` 本身识别为 ENV_DUMP 等规则，而不是静默丢弃。
func stripWrappers(words []word) ([]word, bool) {
	viaXargs := false
	for depth := 0; depth < 8 && len(words) > 0; depth++ {
		cmd := path.Base(strings.TrimRight(words[0].text, "/"))
		var next []word
		switch cmd {
		case "sudo":
			next = skipOptions(words, map[string]bool{
				"-u": true, "--user": true, "-g": true, "--group": true,
				"-C": true, "--close-from": true, "-p": true, "--prompt": true,
				"-h": true, "--host": true, "-r": true, "--role": true,
				"-t": true, "--type": true, "-T": true, "--command-timeout": true,
				"-U": true, "--other-user": true, "-D": true, "--chdir": true,
			})
		case "env":
			next = skipEnv(words)
		case "nohup", "setsid", "command", "builtin":
			next = skipOptions(words, nil)
		case "exec":
			next = skipOptions(words, map[string]bool{"-a": true})
		case "timeout":
			next = skipTimeout(words)
		case "nice":
			next = skipNice(words)
		case "ionice":
			next = skipOptions(words, map[string]bool{"-c": true, "-n": true, "-p": true})
		case "stdbuf":
			next = skipOptions(words, map[string]bool{"-i": true, "-o": true, "-e": true})
		case "xargs":
			viaXargs = true
			next = skipOptions(words, map[string]bool{
				"-n": true, "-I": true, "-d": true, "-P": true, "-L": true,
				"-s": true, "-E": true, "-a": true, "-e": true,
			})
		case "watch":
			next = skipOptions(words, map[string]bool{"-n": true, "--interval": true})
		case "parallel":
			next = skipOptions(words, nil)
			for len(next) > 0 && next[0].text == ":::" {
				next = next[1:]
			}
		default:
			return words, viaXargs
		}
		if len(next) == 0 {
			return words, viaXargs
		}
		words = next
	}
	return words, viaXargs
}

func skipOptions(words []word, takesArg map[string]bool) []word {
	i := 1
	for i < len(words) {
		s := words[i].text
		if s == "--" {
			i++
			break
		}
		if strings.HasPrefix(s, "-") && s != "-" {
			if strings.Contains(s, "=") {
				i++
				continue
			}
			if takesArg[s] {
				i += 2
				continue
			}
			i++
			continue
		}
		break
	}
	if i > len(words) {
		i = len(words)
	}
	return words[i:]
}

func skipEnv(words []word) []word {
	i := 1
	for i < len(words) {
		s := words[i].text
		if s == "--" {
			i++
			break
		}
		if strings.HasPrefix(s, "-") && s != "-" {
			switch s {
			case "-u", "--unset", "-C", "--chdir", "-S", "--split-string":
				i += 2
			default:
				i++
			}
			continue
		}
		if isAssignment(s) {
			i++
			continue
		}
		break
	}
	if i > len(words) {
		i = len(words)
	}
	return words[i:]
}

func skipTimeout(words []word) []word {
	i := 1
	for i < len(words) {
		s := words[i].text
		if s == "--" {
			i++
			break
		}
		if strings.HasPrefix(s, "-") && s != "-" {
			switch s {
			case "-s", "--signal", "-k", "--kill-after":
				i += 2
			default:
				i++
			}
			continue
		}
		break
	}
	if i < len(words) {
		i++ // duration
	}
	if i > len(words) {
		i = len(words)
	}
	return words[i:]
}

func skipNice(words []word) []word {
	i := 1
	for i < len(words) {
		s := words[i].text
		if s == "--" {
			i++
			break
		}
		if strings.HasPrefix(s, "-") && s != "-" {
			if s == "-n" || s == "--adjustment" {
				i += 2
			} else {
				i++
			}
			continue
		}
		break
	}
	if i > len(words) {
		i = len(words)
	}
	return words[i:]
}

func destructiveWithoutTargets(cmd string, args []string) bool {
	switch cmd {
	case "rm":
		for _, a := range args {
			if strings.HasPrefix(a, "-") && (strings.ContainsAny(a, "rRf") || a == "--recursive" || a == "--force") {
				return true
			}
			if a == "--recursive" || a == "--force" {
				return true
			}
		}
	case "mkfs", "mkswap", "wipefs", "fdisk", "parted", "sgdisk", "gdisk", "cfdisk",
		"dd", "chmod", "chown", "kill", "pkill", "killall", "systemctl",
		"iptables", "nft", "ufw", "truncate", "shred", "userdel", "crontab":
		return true
	}
	if strings.HasPrefix(cmd, "mkfs.") {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 规则表
// ---------------------------------------------------------------------------

var safeReadCommands = map[string]bool{
	"ls": true, "pwd": true, "whoami": true, "id": true, "hostname": true,
	"uname": true, "uptime": true, "date": true, "df": true, "du": true,
	"free": true, "ps": true, "ss": true, "netstat": true, "cat": true,
	"head": true, "tail": true, "grep": true, "rg": true, "wc": true,
	"stat": true, "file": true, "which": true,
}

// unsafeSafeReadArgs 对名为只读、但参数可以改变系统状态的命令做参数级校验。
// 返回非空字符串表示必须人工审批（不改变原命令的风险等级语义）。
func unsafeSafeReadArgs(cmd string, args []string) string {
	switch cmd {
	case "date":
		for _, a := range args {
			if a == "-s" || a == "--set" || strings.HasPrefix(a, "--set=") {
				return "date -s modifies the system clock"
			}
			// 非选项、非 +format 的位置参数同样用于设置时间。
			if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
				return "date with a time operand modifies the system clock"
			}
		}
	case "hostname":
		for _, a := range args {
			switch {
			case a == "-F", a == "--file", strings.HasPrefix(a, "--file="),
				a == "-b", a == "--boot":
				return "hostname can change the system hostname"
			case !strings.HasPrefix(a, "-"):
				return "hostname with a name argument changes the system hostname"
			}
		}
	case "ss":
		for _, a := range args {
			if a == "--kill" {
				return "ss --kill terminates sockets"
			}
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") &&
				strings.Contains(a[1:], "K") {
				return "ss -K terminates sockets"
			}
		}
	}
	return ""
}

var shellCommands = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "ash": true, "ksh": true,
}

func isShell(cmd string) bool { return shellCommands[cmd] }

var criticalServices = map[string]bool{
	"ssh": true, "sshd": true, "systemd-networkd": true, "NetworkManager": true,
	"networking": true, "firewalld": true, "ufw": true, "iptables": true,
	"nftables": true, "auditd": true, "systemd-journald": true,
}

func normalizeUnit(s string) string {
	s = strings.TrimSuffix(s, ".service")
	return strings.TrimSuffix(s, ".socket")
}

func classifyCommand(cmd string, args []string, seg segment, depth int, acc *accumulator) {
	switch {
	case safeReadCommands[cmd]:
		if hasSubstitution(seg) || hasFileRedirect(seg) {
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", "read-only command with redirection or substitution")
			return
		}
		if reason := unsafeSafeReadArgs(cmd, args); reason != "" {
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", reason)
			return
		}
		acc.add(LevelSafe, "SAFE_READ", "read-only inspection command")
		return

	case cmd == "systemctl":
		classifySystemctl(args, acc)
		return

	case cmd == "journalctl":
		for _, a := range args {
			if strings.HasPrefix(a, "--vacuum") || a == "--rotate" {
				acc.add(LevelForbidden, "LOG_TAMPER", "journalctl destroys audit/log history")
				return
			}
		}
		acc.add(LevelSafe, "SAFE_SYSTEM_STATUS", "journalctl read-only query")
		return

	case cmd == "service":
		classifyService(args, acc)
		return

	case cmd == "reboot", cmd == "poweroff", cmd == "halt", cmd == "shutdown":
		acc.add(LevelForbidden, "POWER", "power state change")
		return

	case cmd == "init", cmd == "telinit":
		if len(args) > 0 && (args[0] == "0" || args[0] == "6") {
			acc.add(LevelForbidden, "POWER", "init to runlevel 0/6")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "init/telinit with unknown runlevel")
		return

	case cmd == "mkfs", strings.HasPrefix(cmd, "mkfs."), cmd == "mkswap", cmd == "wipefs",
		cmd == "fdisk", cmd == "parted", cmd == "sgdisk", cmd == "gdisk", cmd == "cfdisk":
		acc.add(LevelForbidden, "MKFS", "filesystem or partition table destruction")
		return

	case cmd == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=/dev/") && isBlockDevice(strings.TrimPrefix(a, "of=")) {
				acc.add(LevelForbidden, "DD_DEVICE", "dd writes to a block device")
				return
			}
		}
		for _, a := range args {
			if strings.HasPrefix(a, "of=") {
				acc.add(LevelNeedsApproval, "FILE_WRITE", "dd writes to a file")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "dd without a device target")
		return

	case cmd == "rm":
		classifyRM(args, acc)
		return

	case cmd == "shred":
		for _, a := range args {
			t := cleanTarget(a)
			if isHistoryPath(t) {
				acc.add(LevelForbidden, "HISTORY_CLEAR", "shred shell history")
				return
			}
			if underAny(t, "/var/log") {
				acc.add(LevelForbidden, "LOG_TAMPER", "shred log file")
				return
			}
		}
		acc.add(LevelNeedsApproval, "FILE_WRITE", "shred overwrites file contents")
		return

	case cmd == "truncate":
		for _, a := range args {
			t := cleanTarget(a)
			if isHistoryPath(t) {
				acc.add(LevelForbidden, "HISTORY_CLEAR", "truncate shell history")
				return
			}
			if underAny(t, "/var/log") {
				acc.add(LevelForbidden, "LOG_TAMPER", "truncate log file")
				return
			}
		}
		acc.add(LevelNeedsApproval, "FILE_WRITE", "truncate file")
		return

	case cmd == "chmod", cmd == "chown", cmd == "chgrp":
		classifyPermission(cmd, args, acc)
		return

	case cmd == "setfacl":
		acc.add(LevelNeedsApproval, "PERMISSION_CHANGE", "ACL change")
		return

	case cmd == "kill", cmd == "pkill", cmd == "killall":
		classifyKill(cmd, args, acc)
		return

	case cmd == "history":
		for _, a := range args {
			if strings.HasPrefix(a, "-") && (strings.Contains(a, "c") || strings.Contains(a, "d")) {
				acc.add(LevelForbidden, "HISTORY_CLEAR", "history -c/-d clears shell history")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "history command")
		return

	case cmd == "unset":
		for _, a := range args {
			if a == "HISTFILE" || a == "HISTSIZE" {
				acc.add(LevelForbidden, "HISTORY_CLEAR", "unset history variables")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "unset variables")
		return

	case cmd == "set":
		if len(args) >= 2 && args[0] == "+o" && args[1] == "history" {
			acc.add(LevelForbidden, "HISTORY_CLEAR", "set +o history disables history")
			return
		}
		if len(args) == 0 {
			acc.add(LevelNeedsApproval, "ENV_DUMP", "set dumps shell variables")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "set command")
		return

	case cmd == "export":
		if len(args) == 0 || contains(args, "-p") {
			acc.add(LevelNeedsApproval, "ENV_DUMP", "export -p dumps environment")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "export command")
		return

	case cmd == "env", cmd == "printenv":
		acc.add(LevelNeedsApproval, "ENV_DUMP", "dumps environment variables")
		return

	case cmd == "find":
		classifyFind(args, acc)
		return

	case cmd == "sed":
		for _, a := range args {
			if a == "-i" || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") {
				acc.add(LevelNeedsApproval, "FILE_WRITE", "in-place file edit")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "sed command")
		return

	case cmd == "tee":
		acc.add(LevelNeedsApproval, "FILE_WRITE", "tee writes files")
		return

	case cmd == "mv", cmd == "cp", cmd == "install":
		if cmd == "install" {
			acc.add(LevelNeedsApproval, "FILE_WRITE", "install writes files")
			return
		}
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if underAny(cleanTarget(a), "/etc", "/usr", "/var", "/boot") {
				acc.add(LevelNeedsApproval, "FILE_WRITE", "copy/move into system directory")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "copy/move command")
		return

	case cmd == "rsync":
		if contains(args, "--delete") || contains(args, "--delete-after") || contains(args, "--delete-before") {
			acc.add(LevelNeedsApproval, "FILE_WRITE", "rsync --delete")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "rsync command")
		return

	case cmd == "mount", cmd == "umount", cmd == "swapoff", cmd == "lvextend",
		cmd == "lvresize", cmd == "resize2fs", cmd == "xfs_growfs":
		acc.add(LevelNeedsApproval, "STORAGE_CHANGE", "storage layout change")
		return

	case cmd == "useradd", cmd == "usermod", cmd == "userdel", cmd == "passwd",
		cmd == "chpasswd", cmd == "groupadd", cmd == "groupdel":
		acc.add(LevelNeedsApproval, "ACCOUNT_CHANGE", "account change")
		return

	case cmd == "crontab":
		for _, a := range args {
			if strings.HasPrefix(a, "-") && (strings.Contains(a, "e") || strings.Contains(a, "r")) {
				acc.add(LevelNeedsApproval, "CRON_CHANGE", "crontab edit/remove")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "crontab command")
		return

	case cmd == "apt", cmd == "apt-get", cmd == "dnf", cmd == "yum", cmd == "zypper":
		if containsAny(args, "install", "remove", "purge", "upgrade", "update", "dist-upgrade", "autoremove") {
			acc.add(LevelNeedsApproval, "PACKAGE_CHANGE", "package state change")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "package manager query")
		return

	case cmd == "pacman":
		for _, a := range args {
			if strings.HasPrefix(a, "-") && strings.ContainsAny(a, "SRU") {
				acc.add(LevelNeedsApproval, "PACKAGE_CHANGE", "pacman package change")
				return
			}
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "pacman command")
		return

	case cmd == "snap":
		if containsAny(args, "install", "remove", "refresh") {
			acc.add(LevelNeedsApproval, "PACKAGE_CHANGE", "snap package change")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "snap command")
		return

	case cmd == "iptables", cmd == "ip6tables":
		for _, a := range args {
			switch a {
			case "-F", "--flush", "-X", "--delete-chain":
				acc.add(LevelForbidden, "FIREWALL_FLUSH", "flush firewall rules")
				return
			}
		}
		acc.add(LevelNeedsApproval, "NETWORK_CHANGE", "firewall rule change")
		return

	case cmd == "nft":
		if strings.Contains(strings.Join(args, " "), "flush ruleset") {
			acc.add(LevelForbidden, "FIREWALL_FLUSH", "nft flush ruleset")
			return
		}
		acc.add(LevelNeedsApproval, "NETWORK_CHANGE", "nftables change")
		return

	case cmd == "ufw":
		if len(args) > 0 && args[0] == "disable" {
			acc.add(LevelForbidden, "FIREWALL_FLUSH", "ufw disable")
			return
		}
		acc.add(LevelNeedsApproval, "NETWORK_CHANGE", "ufw change")
		return

	case cmd == "firewall-cmd":
		acc.add(LevelNeedsApproval, "NETWORK_CHANGE", "firewalld change")
		return

	case cmd == "ip":
		classifyIP(args, acc)
		return

	case cmd == "curl", cmd == "wget", cmd == "fetch":
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "network download command")
		return

	case cmd == "base64", cmd == "xxd", cmd == "openssl", cmd == "printf":
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "content transformation command")
		return

	case cmd == "eval":
		acc.add(LevelForbidden, "DYNAMIC_EVAL", "eval executes dynamic code")
		return

	case isShell(cmd):
		classifyShell(cmd, args, depth, seg, acc)
		return

	case cmd == "source", cmd == ".":
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "sourcing a shell script")
		return

	case cmd == "ssh":
		classifySSH(args, depth, acc)
		return

	case cmd == "docker", cmd == "podman", cmd == "nerdctl", cmd == "compose":
		classifyContainer(cmd, args, depth, acc)
		return

	case cmd == "kubectl":
		classifyKubectl(args, depth, acc)
		return

	case cmd == "psql", cmd == "mysql", cmd == "mariadb", cmd == "sqlite3":
		classifySQLCLI(args, acc)
		return
	}

	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "unrecognized command; approval required")
}

// factsTemplates 是 server_facts 允许使用的固定只读命令（精确匹配，不含参数）。
var factsTemplates = map[string]bool{
	"hostname":                            true,
	"uname -a":                            true,
	"uname -m":                            true,
	"nproc":                               true,
	"uptime":                              true,
	"cat /etc/os-release":                 true,
	"cat /proc/loadavg":                   true,
	"cat /proc/meminfo":                   true,
	"cat /proc/cpuinfo":                   true,
	"free -b":                             true,
	"df -B1":                              true,
	"df -B1T":                             true,
	"ip -o addr":                          true,
	"ip -o -4 addr":                       true,
	"hostname -f":                         true,
	"cat /proc/uptime":                    true,
	"getconf _NPROCESSORS_ONLN":           true,
	"lsblk -b -J -o NAME,MOUNTPOINT,SIZE": true,
	// 四期 C1（契约 7.5）：容器运行时探测命令整串登记进 SAFE_FACTS
	// 匹配集；server_facts 模板追加执行，输出写入 facts.container_runtime
	// 并回写 servers.container_runtime。
	containerDetectFactsCommand: true,
}

// containerDetectFactsCommand 是 Linux server_facts 模板四期追加的容器
// 探测命令（契约 5.2C/7.5 写死；与 containerx 的 Linux 探测模板同文）。
const containerDetectFactsCommand = `if command -v docker >/dev/null 2>&1 && docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then echo docker; elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then echo podman; elif command -v ctr >/dev/null 2>&1 && ctr version >/dev/null 2>&1; then echo containerd; else echo none; fi`

// ContainerDetectFactsCommand 返回 Linux 探测模板命令（agent 的
// server_facts 追加执行同一条命令，保证与 SAFE_FACTS 匹配集逐字节一致）。
func ContainerDetectFactsCommand() string { return containerDetectFactsCommand }

func isFactsTemplate(normalized string) bool { return factsTemplates[normalized] }

func classifySystemctl(args []string, acc *accumulator) {
	sub := ""
	operands := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if sub == "" {
			sub = a
			continue
		}
		operands = append(operands, a)
	}
	switch sub {
	case "status", "is-active", "is-enabled", "list-units", "list-unit-files", "show":
		acc.add(LevelSafe, "SAFE_SYSTEM_STATUS", "systemd status query")
	case "reboot", "poweroff", "halt", "kexec":
		acc.add(LevelForbidden, "POWER", "systemd power operation")
	case "stop", "disable", "mask":
		for _, o := range operands {
			if criticalServices[normalizeUnit(o)] {
				acc.add(LevelForbidden, "CRITICAL_SERVICE_STOP", "stop/disable critical service")
				return
			}
		}
		if (sub == "disable" || sub == "mask") && anySuffix(operands, ".timer") {
			acc.add(LevelNeedsApproval, "CRON_CHANGE", "disable timer unit")
			return
		}
		acc.add(LevelNeedsApproval, "SERVICE_CHANGE", "service state change")
	case "start", "restart", "reload", "try-restart", "enable":
		if sub == "enable" && anySuffix(operands, ".timer") {
			acc.add(LevelNeedsApproval, "CRON_CHANGE", "enable timer unit")
			return
		}
		acc.add(LevelNeedsApproval, "SERVICE_CHANGE", "service state change")
	default:
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "systemctl command")
	}
}

func classifyService(args []string, acc *accumulator) {
	if len(args) < 2 {
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "service command")
		return
	}
	// Debian 为 `service <name> <action>`；同时兼容 `service <action> <name>`。
	action, name := args[0], normalizeUnit(args[1])
	if !isServiceAction(action) && isServiceAction(args[1]) {
		action, name = args[1], normalizeUnit(args[0])
	}
	switch action {
	case "status":
		acc.add(LevelSafe, "SAFE_SYSTEM_STATUS", "service status query")
	case "stop", "disable", "mask":
		if criticalServices[name] {
			acc.add(LevelForbidden, "CRITICAL_SERVICE_STOP", "stop/disable critical service")
			return
		}
		acc.add(LevelNeedsApproval, "SERVICE_CHANGE", "service state change")
	case "start", "restart", "reload":
		acc.add(LevelNeedsApproval, "SERVICE_CHANGE", "service state change")
	default:
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "service command")
	}
}

func isServiceAction(s string) bool {
	switch s {
	case "start", "stop", "restart", "reload", "status", "enable", "disable", "mask":
		return true
	}
	return false
}

func classifyRM(args []string, acc *accumulator) {
	recursive, force, noPreserve := false, false, false
	targets := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a == "--":
			continue
		case a == "--recursive":
			recursive = true
		case a == "--force":
			force = true
		case a == "--no-preserve-root":
			noPreserve = true
		case strings.HasPrefix(a, "--"):
			continue
		case strings.HasPrefix(a, "-") && a != "-":
			for _, c := range a[1:] {
				if c == 'r' || c == 'R' {
					recursive = true
				}
				if c == 'f' {
					force = true
				}
			}
		default:
			targets = append(targets, a)
		}
	}

	if len(targets) == 0 {
		if recursive || force {
			acc.add(LevelForbidden, "RM_PROTECTED", "recursive delete with implicit targets")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "rm without target")
		return
	}

	for _, t := range targets {
		clean := cleanTarget(t)
		if hasUnresolvedVar(clean) {
			if recursive || force {
				acc.add(LevelForbidden, "RM_PROTECTED", "recursive delete of an unresolved path")
				return
			}
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", "delete of an unresolved path")
			return
		}
		// 含 ".." 的目标（含 $HOME/~ 白名单变量形式，如 $HOME/../../etc、
		// ~/../etc——root 下即 /etc）经 cleanTarget 原样保留，isProtectedRM
		// 的字面量/前缀匹配对它们失明，曾被降级为 RM_ANY 仅需审批即可执行
		// （评审实测）。历史文件/日志路径先按既有精确规则判；其后含 .. 的
		// 递归/强制删除按最坏情况保护，普通删除保持审批语义；无 .. 的 ~/x、
		// $HOME/x 白名单目标维持原有 RM_ANY 审批。
		if isProtectedRM(clean, noPreserve) {
			acc.add(LevelForbidden, "RM_PROTECTED", "delete of system root or critical directory")
			return
		}
		if isHistoryPath(clean) {
			acc.add(LevelForbidden, "HISTORY_CLEAR", "delete shell history file")
			return
		}
		if underAny(clean, "/var/log") {
			acc.add(LevelForbidden, "LOG_TAMPER", "delete log files")
			return
		}
		if strings.Contains(clean, "..") {
			if recursive || force {
				acc.add(LevelForbidden, "RM_PROTECTED", "recursive delete of a path containing .. may traverse to a protected root")
				return
			}
			acc.add(LevelNeedsApproval, "RM_ANY", "delete of a parent-relative path")
			return
		}
	}

	if recursive || force {
		acc.add(LevelNeedsApproval, "RM_ANY", "recursive/forced delete")
		return
	}
	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "rm command")
}

// rmProtectedRoots 是 rm 规则保护的系统顶级目录（CONTRACT.md 7.3 RM_PROTECTED）。
var rmProtectedRoots = []string{
	"/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/usr", "/etc", "/var", "/boot", "/dev", "/proc", "/sys", "/home", "/root",
}

func isProtectedRM(p string, noPreserve bool) bool {
	switch p {
	case "/", "/*", "/.*", "/home", "/root", "~", "$HOME", "${HOME}", ".", "..":
		return true
	}
	if noPreserve && p == "/" {
		return true
	}
	if underAny(p, rmProtectedRoots...) {
		return true
	}
	// 顶级 glob 目标（如 /etc*、/e??、/u*）经 shell 展开后可能命中保护目录
	// 本身；其子目录形式（如 /e*/x）展开后位于保护目录之下。用 path.Match
	// 对路径首段做 glob 测试，任一保护目录可被匹配即视为删除保护目标。
	// 仅处理绝对路径：相对路径目标沿用既有的不保护语义（与 underAny 一致）。
	if strings.HasPrefix(p, "/") && strings.ContainsAny(p, "*?[") {
		first := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)[0]
		if first != "" {
			for _, root := range rmProtectedRoots {
				if matched, err := path.Match(first, strings.TrimPrefix(root, "/")); err == nil && matched {
					return true
				}
			}
		}
	}
	return false
}

func classifyPermission(cmd string, args []string, acc *accumulator) {
	recursive := false
	operands := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a == "--recursive":
			recursive = true
		case strings.HasPrefix(a, "--"):
			continue
		case strings.HasPrefix(a, "-") && a != "-":
			// chmod/chown 的递归短选项只有大写 R；小写 r 是权限位。
			if strings.ContainsRune(a, 'R') {
				recursive = true
			}
		default:
			operands = append(operands, a)
		}
	}

	targets := operands
	if cmd == "chmod" && len(operands) > 0 {
		mode := operands[0]
		targets = operands[1:]
		if mode == "777" || mode == "0777" || mode == "a+rwx" {
			for _, t := range targets {
				if cleanTarget(t) == "/" {
					acc.add(LevelForbidden, "CHMOD_SYSTEM", "chmod 777 on /")
					return
				}
			}
		}
	}

	protected := []string{"/etc", "/usr", "/var", "/boot"}
	for _, t := range targets {
		clean := cleanTarget(t)
		if recursive {
			if clean == "/" || underAny(clean, protected...) {
				acc.add(LevelForbidden, "CHMOD_SYSTEM", "recursive permission change on system directory")
				return
			}
		}
	}
	acc.add(LevelNeedsApproval, "PERMISSION_CHANGE", "permission change")
}

func classifyKill(cmd string, args []string, acc *accumulator) {
	targets := make([]string, 0, len(args))
	sigKill := false
	for _, a := range args {
		switch {
		case a == "-9" || a == "-KILL" || a == "--signal=9" || a == "-s9" || strings.EqualFold(a, "-sKILL"):
			sigKill = true
		case strings.HasPrefix(a, "-"):
			continue
		default:
			targets = append(targets, a)
		}
	}
	if cmd == "kill" {
		for _, t := range targets {
			if t == "1" && sigKill {
				acc.add(LevelForbidden, "KILL_INIT", "kill -9 1")
				return
			}
		}
		acc.add(LevelNeedsApproval, "PROCESS_SIGNAL", "signal to process")
		return
	}
	for _, t := range targets {
		switch t {
		case "systemd", "init", "sshd", "systemd-journald", "1":
			acc.add(LevelForbidden, "KILL_INIT", "kill system daemon")
			return
		}
	}
	acc.add(LevelNeedsApproval, "PROCESS_SIGNAL", "signal to processes")
}

func classifyFind(args []string, acc *accumulator) {
	for i, a := range args {
		switch a {
		case "-delete":
			acc.add(LevelForbidden, "RM_PROTECTED", "find -delete removes files")
			return
		case "-exec", "-execdir", "-ok", "-okdir":
			if i+1 < len(args) {
				switch args[i+1] {
				case "rm", "shred", "unlink", "truncate":
					acc.add(LevelForbidden, "RM_PROTECTED", "find -exec deletes files")
					return
				}
			}
			acc.add(LevelNeedsApproval, "DEFAULT_ASK", "find -exec")
			return
		}
	}
	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "find command")
}

func classifyIP(args []string, acc *accumulator) {
	if len(args) == 0 {
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "ip command")
		return
	}
	object := args[0]
	if object == "a" {
		object = "addr"
	}
	if object == "r" {
		object = "route"
	}
	switch object {
	case "addr", "address", "route", "link", "neigh", "n":
		for _, a := range args[1:] {
			switch a {
			case "add", "del", "delete", "set", "change", "replace", "flush", "append":
				acc.add(LevelNeedsApproval, "NETWORK_CHANGE", "network configuration change")
				return
			}
		}
		acc.add(LevelSafe, "SAFE_SYSTEM_STATUS", "ip query")
	default:
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "ip command")
	}
}

func classifyShell(cmd string, args []string, depth int, seg segment, acc *accumulator) {
	for i, a := range args {
		if a == "-c" {
			if i+1 >= len(args) {
				acc.add(LevelNeedsApproval, "DEFAULT_ASK", "shell -c without script")
				return
			}
			script := args[i+1]
			if isVariableOnly(script) {
				acc.add(LevelForbidden, "DYNAMIC_EVAL", "shell -c with a variable script")
				return
			}
			if containsDownloadSubst(script) {
				acc.add(LevelForbidden, "CURL_PIPE_SHELL", "remote script executed by shell -c")
				return
			}
			classifyScript(script, depth+1, acc)
			return
		}
	}
	for _, sub := range seg.substs {
		if startsWithDownload(sub) {
			acc.add(LevelForbidden, "CURL_PIPE_SHELL", "remote script fed to shell")
			return
		}
	}
	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "shell invocation")
}

func classifySSH(args []string, depth int, acc *accumulator) {
	optsWithArg := map[string]bool{
		"-p": true, "-i": true, "-o": true, "-l": true, "-J": true, "-F": true,
		"-L": true, "-R": true, "-D": true, "-W": true, "-b": true, "-c": true,
		"-m": true, "-O": true, "-Q": true, "-S": true, "-E": true, "-e": true,
	}
	remote := make([]string, 0, len(args))
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if optsWithArg[args[i]] {
			i += 2
		} else {
			i++
		}
	}
	if i >= len(args) {
		acc.add(LevelNeedsApproval, "NESTED_SSH", "ssh without destination")
		return
	}
	i++ // host
	remote = append(remote, args[i:]...)
	if len(remote) == 0 {
		acc.add(LevelNeedsApproval, "NESTED_SSH", "interactive ssh session")
		return
	}
	classifyScript(strings.Join(remote, " "), depth+1, acc)
	acc.add(LevelNeedsApproval, "NESTED_SSH", "remote command via ssh")
}

// classifyContainer 按契约 7.3D 分级 docker/podman/nerdctl（含 compose
// 子命令树与独立 compose 二进制）。判定顺序（写死）：全局选项检查
// （CONTAINER_REMOTE）→ 别名前缀归一 → exec/run 载荷递归 → 逃逸判定 →
// 子命令分级表 → DEFAULT_ASK。判定从左到右首个命中生效，accumulator
// 取最高级别（forbidden 覆盖 needs_approval 覆盖 safe）。
//
// 四期 C1 修正（对照旧实现）：①logs/compose logs 由 safe 改
// needs_approval（CONTAINER_LOGS）；②只读白名单扩充并加全局选项前置
// 检查（旧实现跳过 - 前缀参数，docker -H tcp://x ps 误判 safe）；
// ③container/image 前缀别名与 nerdctl 归一（旧实现落 DEFAULT_ASK）；
// ④run 增加逃逸判定与载荷递归（旧实现仅 needs_approval 不看载荷）；
// ⑤network/volume/context/login/save/load/cp 等子命令显式分级。
func classifyContainer(cmd string, args []string, depth int, acc *accumulator) {
	// 独立 compose 二进制等价 `docker compose`：参数整体走 compose 分级表。
	if cmd == "compose" {
		classifyComposeArgs(args, depth, acc)
		return
	}

	// 1. 全局选项检查（CONTAINER_REMOTE，契约 7.4.21）：选项出现在
	// 子命令前 → 整条至少 needs_approval，不得因选项被剥离而按子命令
	// 判 safe（accumulator 取最高保证）。
	idx := containerSubIndex(args)
	if hasContainerRemoteOption(args, idx) {
		acc.add(LevelNeedsApproval, "CONTAINER_REMOTE", "global option targets a remote daemon or named context")
	}
	if idx < 0 {
		// 只有全局选项没有子命令：无法证明安全。
		acc.add(LevelNeedsApproval, "CONTAINER_REMOTE", "container command without a subcommand")
		return
	}
	rest := args[idx:] // 含子命令本身，交别名归一

	sub, rest := normalizeContainerSub(rest)
	switch sub {
	case "compose":
		classifyComposeArgs(rest, depth, acc)
		return
	case "exec":
		// 契约 7.3D：exec 即容器内命令执行。载荷含 shell -c 时递归分类
		// 内部命令取最高级别（载荷 safe 则整条 safe）；无 -c 载荷（如
		// docker exec c ping x）→ CONTAINER_CHANGE。
		if script, ok := extractShellC(rest); ok {
			classifyScript(script, depth+1, acc)
			return
		}
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "container exec")
		return
	case "run":
		// 契约 7.3D：run 与 exec 承载同一载荷，须走完整命令分类链——
		// 逃逸判定、CONTAINER_CHANGE 与镜像名后 shell -c 载荷递归取最高。
		classifyContainerEscape(rest, acc)
		if script, ok := extractShellC(rest); ok {
			classifyScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "container run")
		return
	case "context":
		if containsAny(rest, "create", "use", "rm", "remove") {
			acc.add(LevelNeedsApproval, "CONTAINER_REMOTE", "context management may redirect the daemon endpoint")
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "container context command")
		return
	case "logs":
		// 四期修正：容器日志可能含凭据/令牌进入 LLM 上下文（与三期
		// show log 归 CONFIG_DUMP 同一裁决）；专用 container_logs 工具
		// 为受控模板另行恒 safe（契约 7.2），手工 ssh_exec 不豁免。
		acc.add(LevelNeedsApproval, "CONTAINER_LOGS", "container logs may contain credentials")
		return
	}

	if isContainerSafeSub(sub, rest) {
		acc.add(LevelSafe, "SAFE_SYSTEM_STATUS", "container status query")
		return
	}
	if isContainerChangeSub(sub, rest) {
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "container state change")
		return
	}
	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "container command")
}

// classifyComposeArgs 分级 `docker compose <sub>`（契约 7.3D 末行）：
// ps/config/version/ls safe；logs 同 CONTAINER_LOGS；exec/run 同顶层
// 语义（载荷递归与逃逸判定）；变更子命令 CONTAINER_CHANGE；未列
// DEFAULT_ASK。
func classifyComposeArgs(args []string, depth int, acc *accumulator) {
	sub, rest := normalizeContainerSub(args)
	switch sub {
	case "ps", "config", "version", "ls":
		acc.add(LevelSafe, "SAFE_SYSTEM_STATUS", "compose status query")
		return
	case "logs":
		acc.add(LevelNeedsApproval, "CONTAINER_LOGS", "compose logs may contain credentials")
		return
	case "exec":
		if script, ok := extractShellC(rest); ok {
			classifyScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "compose exec")
		return
	case "run":
		classifyContainerEscape(rest, acc)
		if script, ok := extractShellC(rest); ok {
			classifyScript(script, depth+1, acc)
		}
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "compose run")
		return
	case "":
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "compose command")
		return
	}
	if composeChangeSubs[sub] {
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "compose state change")
		return
	}
	if isContainerChangeSub(sub, rest) {
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "compose state change")
		return
	}
	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "compose command")
}

// composeChangeSubs 是 compose 独有的变更动词（契约 7.3D needs_approval
// 行 compose 清单；up -d/down --volumes 变体含于动词本身）。
var composeChangeSubs = map[string]bool{
	"up": true, "down": true, "restart": true, "start": true, "stop": true,
	"rm": true, "pull": true, "build": true, "create": true, "run": true,
	"kill": true, "pause": true, "unpause": true, "push": true,
}

// containerGlobalOptions 是指向远程 daemon/命名上下文的全局选项（契约
// 7.3D CONTAINER_REMOTE 行）；--tls* 按前缀匹配。
var containerRemoteOptionPrefixes = []string{"--tls"}

var containerRemoteOptions = map[string]bool{
	"-h": true, "--host": true, "--context": true, "-c": true,
	// podman 远程访问旗标（契约 7.3D 清单同步补齐，评审发现遗漏）。
	"--remote": true, "--url": true,
}

// containerGlobalTakesValue 是子命令前出现且带独立值的常见全局选项——
// 其值不是子命令，扫描时跳过。键为小写：Linux 分册原文可为 -H、
// Windows 分册 args 已小写化，匹配统一 strings.ToLower。
var containerGlobalTakesValue = map[string]bool{
	"-h": true, "--host": true, "--context": true, "-c": true, "--config": true,
	"--log-level": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
	"--url": true,
	// compose 全局选项（评审 verified：--profile ps 的 ps 曾冒充子命令）。
	"--profile": true, "-p": true, "--project-name": true, "-f": true, "--file": true,
	"--env-file": true,
}

// containerSubIndex 返回子命令（第一个非选项参数）的下标；-1 表示没有。
func containerSubIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if !strings.Contains(a, "=") && containerGlobalTakesValue[strings.ToLower(a)] && i+1 < len(args) {
				i++ // 带独立值的全局选项：值不是子命令。
			}
			continue
		}
		return i
	}
	return -1
}

// hasContainerRemoteOption 检查 args[:idx]（子命令前）是否出现远程/
// 上下文全局选项。
func hasContainerRemoteOption(args []string, subIdx int) bool {
	if subIdx < 0 {
		subIdx = len(args)
	}
	for i := 0; i < subIdx && i < len(args); i++ {
		a := args[i]
		if a == "--" {
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		lower := strings.ToLower(a)
		if name, _, _ := strings.Cut(lower, "="); containerRemoteOptions[name] {
			return true
		}
		// pflag 拼接短旗标形态（-farg，如 docker CLI 接受的 -Htcp://x）：
		// 无 = 无空格，值直接拼在单字母短旗标后。两字符以上且首两位是
		// 远程短旗标即命中（-h/-c 已在上表，拼接形态长度 >2）。
		if len(lower) > 2 && lower[0] == '-' && containerRemoteOptions[lower[:2]] {
			return true
		}
		for _, p := range containerRemoteOptionPrefixes {
			if strings.HasPrefix(strings.ToLower(a), p) {
				return true
			}
		}
		// 带独立值的远程选项：其值同样跳过。
		if containerGlobalTakesValue[strings.ToLower(a)] {
			i++
		}
	}
	return false
}

// normalizeContainerSub 做别名前缀归一（契约 7.3D/7.4.23，分词后、规则
// 匹配前完成）：container ls|ps → ps；image ls|list → images；container
// <verb>/image <verb> 剥前缀按同名动词分级。返回归一后的子命令与
// 其余参数。
func normalizeContainerSub(args []string) (string, []string) {
	sub := firstOperand(args)
	if sub == "" {
		return "", args
	}
	// firstOperand 跳过选项；前缀别名判定要求别名词就是子命令本身，
	// 这里对 container/image 前缀只接受其紧跟动词的形式（docker 的
	// container/image 是子命令组，不接全局式选项）。
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		i++
	}
	if i >= len(args) || args[i] != sub {
		return sub, args
	}
	rest := args[i+1:]
	switch sub {
	case "container":
		if len(rest) == 0 {
			return "container", rest
		}
		switch rest[0] {
		case "ls", "ps":
			return "ps", rest[1:]
		default:
			return rest[0], rest[1:] // container <verb> 剥前缀按同名动词分级
		}
	case "image":
		if len(rest) == 0 {
			return "image", rest
		}
		switch rest[0] {
		case "ls", "list":
			return "images", rest[1:]
		default:
			return rest[0], rest[1:] // image <verb> 剥前缀按同名动词分级
		}
	}
	return sub, args[i+1:]
}

// isContainerSafeSub 判定归一化后的子命令是否 7.3D safe 行（不含 logs）。
func isContainerSafeSub(sub string, rest []string) bool {
	switch sub {
	case "ps", "images", "inspect", "version", "info", "top":
		return true
	case "stats":
		return contains(rest, "--no-stream")
	case "system":
		return firstOperand(rest) == "df"
	case "network", "volume":
		op := firstOperand(rest)
		return op == "ls" || op == "inspect"
	case "history":
		// image history（image 前缀已剥成 history）。
		return true
	}
	return false
}

// containerChangeSubs 是 7.3D needs_approval 行的变更子命令清单（save/
// export/cp 数据外带与 load/import 注入、login/logout 仓库凭据同列，
// 契约 7.4.24）。
var containerChangeSubs = map[string]bool{
	"run": true, "rm": true, "rmi": true, "stop": true, "kill": true,
	"start": true, "restart": true, "pause": true, "unpause": true,
	"rename": true, "update": true, "create": true, "build": true,
	"pull": true, "push": true, "load": true, "import": true, "save": true,
	"export": true, "cp": true, "tag": true, "commit": true, "wait": true,
	"attach": true, "login": true, "logout": true,
}

// isContainerChangeSub 判定归一化后的子命令是否变更/外带/注入类
// （CONTAINER_CHANGE）。
func isContainerChangeSub(sub string, rest []string) bool {
	if containerChangeSubs[sub] {
		return true
	}
	switch sub {
	case "network":
		op := firstOperand(rest)
		return op == "create" || op == "connect" || op == "disconnect" || op == "rm" || op == "prune"
	case "volume":
		op := firstOperand(rest)
		return op == "create" || op == "rm" || op == "prune"
	case "system":
		return firstOperand(rest) == "prune"
	case "image", "container":
		// image prune / container prune（无动词形态，prune 直接跟在前缀后
		// 已由归一剥成 prune∈containerChangeSubs）；此处防御未剥净形态。
		return firstOperand(rest) == "prune"
	case "prune":
		return true
	case "swarm":
		op := firstOperand(rest)
		return op == "join" || op == "leave"
	}
	return false
}

// containerEscapeHostMounts 是 CONTAINER_ESCAPE 的宿主敏感挂载根
// （契约 7.3D：按 path.Clean 前缀判定；宿主根 / 单独精确匹配，见
// classifyContainerEscape）。
var containerEscapeHostMounts = []string{
	"/etc", "/proc", "/sys", "/dev",
	"/var/run/docker.sock", "/run/docker.sock",
}

// classifyContainerEscape 判定 run（含 compose run）的逃逸面（契约
// 7.3D forbidden 行）：特权 flag 或宿主敏感路径挂载等同宿主 root；
// 挂载目标含未解析变量 → forbidden（无法静态证明非敏感路径）。
func classifyContainerEscape(args []string, acc *accumulator) {
	for _, a := range args {
		name, value, hasValue := strings.Cut(a, "=")
		switch name {
		case "--privileged":
			acc.add(LevelForbidden, "CONTAINER_ESCAPE", "privileged container is equivalent to host root")
		case "--pid", "--ipc", "--userns", "--network", "--net":
			if hasValue && value == "host" {
				acc.add(LevelForbidden, "CONTAINER_ESCAPE", "host namespace sharing is a container escape")
			}
		}
	}
	// 空格分离形态：--pid host / --network host。
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pid", "--ipc", "--userns", "--network", "--net":
			if i+1 < len(args) && args[i+1] == "host" {
				acc.add(LevelForbidden, "CONTAINER_ESCAPE", "host namespace sharing is a container escape")
			}
		}
	}
	// 挂载源判定：-v <src>:<dst>[:opts]、--volume ...、--mount source/src=<src>
	//（按 path.Clean 前缀判定；宿主敏感路径挂载即逃逸面）。
	for i := 0; i < len(args); i++ {
		var src string
		switch {
		case args[i] == "-v" || args[i] == "--volume":
			if i+1 >= len(args) {
				continue
			}
			parts := strings.SplitN(args[i+1], ":", 2)
			src = parts[0]
			i++
		case strings.HasPrefix(args[i], "--volume="):
			parts := strings.SplitN(strings.TrimPrefix(args[i], "--volume="), ":", 2)
			src = parts[0]
		case args[i] == "--mount" || strings.HasPrefix(args[i], "--mount="):
			src = mountSource(args[i], args, &i)
		default:
			continue
		}
		if src == "" {
			continue
		}
		if hasUnresolvedVar(src) {
			// 目标含未解析变量 → forbidden（契约 7.3D forbidden 行）。
			acc.add(LevelForbidden, "CONTAINER_ESCAPE", "mount source contains an unresolved variable")
			continue
		}
		clean := cleanTarget(src)
		// 宿主根本身单独精确匹配（挂的就是 / 或 /*）：若按前缀判定，
		// 任意绝对路径挂载都会命中，违背 7.3D 敏感路径清单语义。
		if clean == "/" || strings.HasPrefix(clean, "/*") {
			acc.add(LevelForbidden, "CONTAINER_ESCAPE", "host root mounted into container is an escape")
			continue
		}
		for _, root := range containerEscapeHostMounts {
			if clean == root || strings.HasPrefix(clean, root+"/") {
				acc.add(LevelForbidden, "CONTAINER_ESCAPE", "host path mounted into container is an escape")
				break
			}
		}
	}
}

// mountSource 提取 --mount 的 source/src 值（type=bind 的宿主路径）。
func mountSource(arg string, args []string, i *int) string {
	extract := func(s string) string {
		for _, kv := range strings.Split(s, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if k == "source" || k == "src" {
				return v
			}
		}
		return ""
	}
	if s, ok := strings.CutPrefix(arg, "--mount="); ok {
		return extract(s)
	}
	if arg == "--mount" && *i+1 < len(args) {
		*i++
		return extract(args[*i])
	}
	return ""
}

func classifyKubectl(args []string, depth int, acc *accumulator) {
	sub := firstOperand(args)
	switch sub {
	case "delete", "apply", "scale", "rollout":
		acc.add(LevelNeedsApproval, "CONTAINER_CHANGE", "kubernetes resource change")
	case "exec":
		if script, ok := extractShellC(args); ok {
			classifyScript(script, depth+1, acc)
			return
		}
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "kubectl exec")
	default:
		acc.add(LevelNeedsApproval, "DEFAULT_ASK", "kubectl command")
	}
}

var (
	sqlWhereRe = regexp.MustCompile(`\bWHERE\b`)
	sqlDropRe  = regexp.MustCompile(`\bDROP\b`)
)

func classifySQLCLI(args []string, acc *accumulator) {
	sql := strings.ToUpper(strings.Join(args, " "))
	hasWhere := sqlWhereRe.MatchString(sql)
	if sqlDropRe.MatchString(sql) || strings.Contains(sql, "TRUNCATE") {
		acc.add(LevelForbidden, "SQL_DESTRUCTIVE", "DROP/TRUNCATE via database CLI")
		return
	}
	if strings.Contains(sql, "DELETE") || strings.Contains(sql, "UPDATE") {
		if !hasWhere {
			acc.add(LevelForbidden, "SQL_DESTRUCTIVE", "DELETE/UPDATE without WHERE")
			return
		}
		acc.add(LevelNeedsApproval, "SQL_WRITE", "database write statement")
		return
	}
	for _, kw := range []string{"INSERT", "CREATE", "ALTER", "GRANT", "REVOKE", "CALL",
		"COPY", "LOAD", "REPLACE", "VACUUM", "PRAGMA", "FOR UPDATE", "LOCK IN SHARE MODE"} {
		if strings.Contains(sql, kw) {
			acc.add(LevelNeedsApproval, "SQL_WRITE", "database write statement")
			return
		}
	}
	acc.add(LevelNeedsApproval, "DEFAULT_ASK", "database CLI command")
}

// ---------------------------------------------------------------------------
// 重定向与管道
// ---------------------------------------------------------------------------

func classifyRedirect(r redirect, acc *accumulator) {
	if !strings.Contains(r.op, ">") {
		return // 输入重定向不写文件
	}
	target := strings.TrimSpace(r.target)
	if target == "" {
		return
	}
	if strings.HasSuffix(r.op, "&") && isAllDigits(target) {
		return // 2>&1 之类的 fd 复制
	}
	t := cleanTarget(target)
	switch t {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin",
		"/dev/zero", "/dev/random", "/dev/urandom":
		return
	}
	if strings.HasPrefix(t, "/dev/fd/") {
		return
	}
	if isBlockDevice(t) {
		acc.add(LevelForbidden, "DEVICE_WRITE", "redirection to a block device")
		return
	}
	if isHistoryPath(t) {
		acc.add(LevelForbidden, "HISTORY_CLEAR", "redirection to shell history file")
		return
	}
	if underAny(t, "/var/log") {
		acc.add(LevelForbidden, "LOG_TAMPER", "redirection truncates a log file")
		return
	}
	acc.add(LevelNeedsApproval, "FILE_WRITE", "output redirection to a file")
}

func isBlockDevice(p string) bool {
	return strings.HasPrefix(p, "/dev/sd") || strings.HasPrefix(p, "/dev/nvme") ||
		strings.HasPrefix(p, "/dev/vd") || strings.HasPrefix(p, "/dev/hd") ||
		strings.HasPrefix(p, "/dev/dm-") || strings.HasPrefix(p, "/dev/mapper/")
}

func isHistoryPath(p string) bool {
	p = strings.TrimSpace(p)
	switch p {
	case "~/.bash_history", "~/.zsh_history", "~/.history",
		"$HOME/.bash_history", "${HOME}/.bash_history",
		"$HOME/.zsh_history", "${HOME}/.zsh_history",
		"/root/.bash_history", "/root/.zsh_history":
		return true
	}
	switch path.Base(p) {
	case ".bash_history", ".zsh_history", "fish_history", ".history":
		return true
	}
	return false
}

func hasSubstitution(seg segment) bool { return len(seg.substs) > 0 }

func hasFileRedirect(seg segment) bool {
	for _, r := range seg.redirs {
		if !strings.Contains(r.op, ">") {
			continue
		}
		t := cleanTarget(r.target)
		if t == "/dev/null" || strings.HasSuffix(r.op, "&") && isAllDigits(t) {
			continue
		}
		return true
	}
	return false
}

// classifyPipelines 检查 | 连接的命令链，识别下载/解码内容直接喂给 shell，
// 并在所有段都是只读命令时给出 SAFE_PIPE。
func classifyPipelines(segs []segment, acc *accumulator) {
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
				c, ok := primaryCommand(segs[k])
				if !ok {
					allSafe = false
					continue
				}
				cmds = append(cmds, c)
				if !pipelineSafeCommands[c] {
					allSafe = false
				}
			}
			if allSafe && len(cmds) >= 2 {
				acc.add(LevelSafe, "SAFE_PIPE", "pipeline of read-only commands")
			}
			if len(cmds) >= 2 && isShell(cmds[len(cmds)-1]) {
				// 下载命令藏在某段的命令/进程替换里时（如 cat <(curl …) | bash、
				// echo $(curl …) | bash），主命令名比对看不到它；按段内 substs
				// 等价判定为下载段，同样按远程脚本喂 shell 禁止。
				for k := i; k < j; k++ {
					for _, sub := range segs[k].substs {
						if startsWithDownload(sub) {
							acc.add(LevelForbidden, "CURL_PIPE_SHELL", "download in substitution piped to shell")
							break
						}
					}
				}
				for _, c := range cmds[:len(cmds)-1] {
					switch c {
					case "curl", "wget", "fetch":
						acc.add(LevelForbidden, "CURL_PIPE_SHELL", "download piped to shell")
					case "base64":
						acc.add(LevelForbidden, "CURL_PIPE_SHELL", "base64 decoded output piped to shell")
					case "xxd", "openssl", "printf":
						acc.add(LevelForbidden, "DYNAMIC_EVAL", "generated content piped to shell")
					}
				}
			}
		}
		i = j + 1
	}
}

// pipelineSafeCommands 用于 SAFE_PIPE 判定；只是“不降低级别”的附加标记，
// 逐段规则仍然独立生效，因此这里宁可少列。
var pipelineSafeCommands = map[string]bool{
	"ls": true, "pwd": true, "whoami": true, "id": true, "hostname": true,
	"uname": true, "uptime": true, "date": true, "df": true, "du": true,
	"free": true, "ps": true, "ss": true, "netstat": true, "cat": true,
	"head": true, "tail": true, "grep": true, "rg": true, "wc": true,
	"stat": true, "file": true, "which": true, "sort": true, "uniq": true,
	"awk": true, "cut": true, "tr": true, "column": true, "journalctl": true,
	"systemctl": true, "docker": true, "podman": true, "ip": true,
}

// primaryCommand 返回一段的主命令名（剥离赋值与包装器）。
func primaryCommand(seg segment) (string, bool) {
	i := 0
	for i < len(seg.words) && isAssignment(seg.words[i].text) {
		i++
	}
	if i >= len(seg.words) {
		return "", false
	}
	rest, _ := stripWrappers(seg.words[i:])
	if len(rest) == 0 {
		return "", false
	}
	return path.Base(strings.TrimRight(rest[0].text, "/")), true
}

func startsWithDownload(s string) bool {
	norm, err := normalize(s)
	if err != nil {
		return false
	}
	segs, err := parseCommand(norm)
	if err != nil || len(segs) == 0 {
		return false
	}
	cmd, ok := primaryCommand(segs[0])
	if !ok {
		return false
	}
	return cmd == "curl" || cmd == "wget" || cmd == "fetch"
}

var downloadSubstRe = regexp.MustCompile(`(?i)\$\([^)]*\b(curl|wget|fetch)\b|` + "`" + `[^` + "`" + `]*\b(curl|wget|fetch)\b`)

func containsDownloadSubst(script string) bool { return downloadSubstRe.MatchString(script) }

func isVariableOnly(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "$(") && strings.HasSuffix(s, ")") {
		return true
	}
	if strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") && len(s) > 1 {
		return true
	}
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		return true
	}
	if strings.HasPrefix(s, "$") && !strings.ContainsAny(s, " \t") {
		return true
	}
	return false
}

// firstOperand 返回第一个非选项参数。带独立值的选项（containerGlobalTakesValue，
// 如 --profile ps、-c ctx、--log-level debug）其取值不是操作数，必须跳过——
// 否则选项值冒充子命令（评审 verified：`docker compose --profile ps up` 的
// ps 冒充子命令判 safe，真实 up/run/exec 从未分级）。
func firstOperand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if !strings.Contains(a, "=") && containerGlobalTakesValue[strings.ToLower(a)] && i+1 < len(args) {
				i++ // 选项的独立取值不是操作数。
			}
			continue
		}
		return a
	}
	return ""
}

func extractShellC(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" {
			continue
		}
		if isShell(a) {
			for j := i + 1; j < len(args); j++ {
				if args[j] == "-c" && j+1 < len(args) {
					return args[j+1], true
				}
				if strings.HasPrefix(args[j], "-") {
					continue
				}
				break
			}
		}
	}
	return "", false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsAny(list []string, wants ...string) bool {
	for _, w := range wants {
		if contains(list, w) {
			return true
		}
	}
	return false
}

func anySuffix(list []string, suffix string) bool {
	for _, s := range list {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}
