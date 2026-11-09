package logx

import (
	neturl "net/url"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var sensitiveKeyParts = []string{
	"password", "passphrase", "private_key", "api_key", "token", "authorization", "cookie", "secret",
}

// inlineSecret 匹配「键名[=:]值」形态的内联密钥；键名允许复合前缀/后缀
// （DB_PASSWORD、MYSQL_TOKEN、my-api-key 等，评审实测复合键曾被 \b 边界
// 放过），值取到下一个空白。
var inlineSecret = regexp.MustCompile(`(?i)([a-z0-9_-]*(?:password|passphrase|token|api[_-]?key)[a-z0-9_-]*)\s*[:=]\s*\S+`)

// inlineAuth 单独处理 Authorization 头：值是「scheme token」两段形态
// （Bearer abc123），只吃一段会把 token 残留在日志里。
var inlineAuth = regexp.MustCompile(`(?i)(authorization[a-z0-9_-]*)\s*[:=]\s*\S+(?:\s+[A-Za-z0-9._~+/=;-]+)?`)

// Redact 对审计 detail 做浅层脱敏：敏感键名整体替换，字符串值与字符串
// 数组（如终端输入摘要 session.input_summary 的 lines）中的
// password=/token=/Authorization: 片段替换。
//
// TODO(BE1)：按 CONTRACT.md 5.9 补齐 8KB 单字段与 32KB 总量截断。
func Redact(detail map[string]any) map[string]any {
	if detail == nil {
		return nil
	}
	out := make(map[string]any, len(detail))
	for k, v := range detail {
		if isSensitiveKey(k) {
			out[k] = redacted
			continue
		}
		switch val := v.(type) {
		case string:
			out[k] = RedactString(val)
		case []string:
			// 字符串数组逐元素做同样的内联密钥替换：用户在终端敲出的
			// export DB_PASSWORD=…、Authorization: Bearer … 等行不能
			// 因值类型是数组而绕过脱敏（评审实测击穿）。
			redacted2 := make([]string, len(val))
			for i, item := range val {
				redacted2[i] = RedactString(item)
			}
			out[k] = redacted2
		default:
			out[k] = v
		}
	}
	return out
}

// RedactURLInError 对错误串中的完整 URL 做脱敏：*url.Error 的 Error()
// 输出形如 `Post "https://host/path/<token>": ...`，webhook URL 内嵌
// access_token 等同凭据，不得整串落日志——保留 host 与错误原因，
// path/query 掩码（五期评审：通知重试日志曾泄漏完整 webhook URL）。
func RedactURLInError(msg string) string {
	// 匹配 "..." 或裸 https?://... 到空白/引号为止的 URL 片段。
	re := regexp.MustCompile(`https?://[^\s"']+`)
	return re.ReplaceAllStringFunc(msg, func(u string) string {
		parsed, err := neturl.Parse(u)
		if err != nil {
			return "[URL_REDACTED]"
		}
		return parsed.Scheme + "://" + parsed.Host + "/[REDACTED]"
	})
}

// RedactString 替换字符串中的内联密钥。
func RedactString(s string) string {
	s = inlineAuth.ReplaceAllString(s, "$1="+redacted)
	return inlineSecret.ReplaceAllString(s, "$1="+redacted)
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}
