// cli_test.go 覆盖主机侧离线子命令的参数解析与拒绝语义（契约 12.3 第 11/25
// 条）。评审 high：此前 `aiops break-glass ...` 与 `aiops admin reset ...`
// 会当成普通启动再起一个服务进程（不写审计、不改状态）。
package main

import (
	"strings"
	"testing"
)

func TestParseOfflineCommand(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		want    *offlineCommand
		wantErr string
	}{
		{"no args starts server", nil, nil, ""},
		{"single arg starts server", []string{"server"}, nil, ""},
		{
			"admin reset",
			[]string{"admin", "reset", "--user", "admin", "--reason", "locked out", "--confirm"},
			&offlineCommand{kind: "admin-reset", username: "admin", reason: "locked out", confirm: true},
			"",
		},
		{"admin reset without confirm still parses", []string{"admin", "reset", "--user", "admin"},
			&offlineCommand{kind: "admin-reset", username: "admin", reason: "offline admin reset", confirm: false}, ""},
		{"admin reset without user", []string{"admin", "reset", "--confirm"}, nil, "--user"},
		{"unknown admin subcommand", []string{"admin", "nuke"}, nil, "unknown admin subcommand"},
		{
			"break-glass",
			[]string{"break-glass", "--action", "approve", "--approval-id", "ap-1", "--reason", "recover", "--confirm"},
			&offlineCommand{kind: "break-glass", approvalID: "ap-1", reason: "recover", confirm: true},
			"",
		},
		{"break-glass unknown action", []string{"break-glass", "--action", "deny", "--approval-id", "ap-1", "--reason", "x"},
			nil, "unsupported action"},
		{"break-glass missing approval id", []string{"break-glass", "--reason", "x", "--confirm"}, nil, "--approval-id"},
		{"break-glass missing reason", []string{"break-glass", "--approval-id", "ap-1", "--confirm"}, nil, "--reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOfflineCommand(tc.argv)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("cmd = %+v, want %+v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("cmd = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestOfflineCommandRequiresConfirm：缺 --confirm 时不得改变任何状态。
func TestOfflineCommandRequiresConfirm(t *testing.T) {
	var out strings.Builder
	code := runOfflineCommand(&offlineCommand{kind: "admin-reset", username: "admin"}, nil, &out)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "--confirm") {
		t.Fatalf("output = %q", out.String())
	}
}

// TestOfflineCommandUnknownKind：内部 kind 非法时退出码 2。
func TestOfflineCommandUnknownKind(t *testing.T) {
	var out strings.Builder
	code := runOfflineCommand(&offlineCommand{kind: "bogus", confirm: true}, nil, &out)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestParseAuditAnchor：audit anchor 子命令解析（外部 anchor 导出入口）。
func TestParseAuditAnchor(t *testing.T) {
	got, err := parseOfflineCommand([]string{"audit", "anchor", "--out", "/tmp/a.jsonl"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got == nil || got.kind != "audit-anchor" || got.out != "/tmp/a.jsonl" {
		t.Fatalf("cmd = %+v", got)
	}
	if _, err := parseOfflineCommand([]string{"audit", "anchor"}); err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("missing --out err = %v", err)
	}
	if _, err := parseOfflineCommand([]string{"audit", "verify"}); err == nil {
		t.Fatal("unknown audit subcommand must fail")
	}
}
