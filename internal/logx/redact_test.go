package logx

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactStringInline(t *testing.T) {
	cases := map[string]string{
		"export DB_PASSWORD=hunter2":               "export DB_PASSWORD=[REDACTED]",
		`curl -H "Authorization: Bearer abc" http`: `curl -H "Authorization=[REDACTED]" http`,
		"token=abc123 continue":                    "token=[REDACTED] continue",
		"normal text":                              "normal text",
	}
	for in, want := range cases {
		if got := RedactString(in); got != want {
			t.Errorf("RedactString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactSensitiveKeys(t *testing.T) {
	out := Redact(map[string]any{
		"password":    "x",
		"api_key":     "y",
		"description": "keep me",
	})
	if out["password"] != redacted || out["api_key"] != redacted {
		t.Fatalf("sensitive keys not redacted: %v", out)
	}
	if out["description"] != "keep me" {
		t.Fatalf("normal value altered: %v", out)
	}
}

// 字符串数组（终端输入摘要 session.input_summary 的 lines）必须逐元素
// 脱敏：用户敲出的 export DB_PASSWORD=…、Authorization: Bearer … 行不能
// 因值类型是数组而原样落入审计（评审实测击穿）。
func TestRedactStringArrayElements(t *testing.T) {
	in := map[string]any{
		"lines": []string{
			"export DB_PASSWORD=hunter2",
			`curl -H "Authorization: Bearer abc123" http://x`,
			"mysql -uroot -pS3cret -e 'select 1'",
			"ls -la",
		},
		"truncated": false,
		"count":     4,
	}
	out := Redact(in)
	lines, ok := out["lines"].([]string)
	if !ok {
		t.Fatalf("lines type = %T, want []string", out["lines"])
	}
	want := []string{
		"export DB_PASSWORD=[REDACTED]",
		`curl -H "Authorization=[REDACTED]" http://x`,
		"mysql -uroot -pS3cret -e 'select 1'", // -pS3cret 无 key= 形态，不在内联正则范围
		"ls -la",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	for _, l := range lines {
		if contains(l, "hunter2") || contains(l, "abc123") {
			t.Fatalf("secret leaked in %q", l)
		}
	}
	// 非字符串类型原样保留。
	if out["truncated"] != false || out["count"] != 4 {
		t.Fatalf("non-string values altered: %v", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// RedactURLInError：*url.Error 串中的完整 webhook URL（含 token 路径）
// 不得落日志——保留 host 掩码 path（五期评审：通知重试日志泄漏凭据）。
func TestRedactURLInError(t *testing.T) {
	in := `Post "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef-token-123": dial tcp: lookup fail`
	out := RedactURLInError(in)
	if strings.Contains(out, "abcdef-token-123") {
		t.Fatalf("token leaked: %s", out)
	}
	if !strings.Contains(out, "open.feishu.cn") {
		t.Fatalf("host lost: %s", out)
	}
	if !strings.Contains(out, "dial tcp") {
		t.Fatalf("error cause lost: %s", out)
	}
	// 无 URL 的错误串原样保留。
	plain := "dial tcp 127.0.0.1:23: connection refused"
	if RedactURLInError(plain) != plain {
		t.Fatalf("plain error altered: %s", RedactURLInError(plain))
	}
}
