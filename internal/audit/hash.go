package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"aiops/internal/logx"
	"aiops/internal/store"
)

const (
	zeroHash       = "0000000000000000000000000000000000000000000000000000000000000000"
	maxDetailBytes = 32 * 1024
	maxStringBytes = 8 * 1024
)

// computeHash 按契约固定输入计算审计哈希：
//
//	prev_hash \x1f created_at(RFC3339Nano, UTC) \x1f actor_type \x1f actor_id
//	\x1f action \x1f resource_type \x1f resource_id \x1f request_id
//	\x1f conversation_id(空为 "") \x1f server_id(空为 "") \x1f ip
//	\x1f canonical_json(detail)
func computeHash(p store.CreateAuditParams) string {
	conversationID := ""
	if p.ConversationID != nil {
		conversationID = *p.ConversationID
	}
	serverID := ""
	if p.ServerID != nil {
		serverID = *p.ServerID
	}
	detail := "{}"
	if len(p.Detail) > 0 {
		detail = string(p.Detail)
	}
	input := strings.Join([]string{
		p.PrevHash,
		p.CreatedAt.UTC().Format(time.RFC3339Nano),
		p.ActorType,
		p.ActorID,
		p.Action,
		p.ResourceType,
		p.ResourceID,
		p.RequestID,
		conversationID,
		serverID,
		p.IP,
		detail,
	}, "\x1f")
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// sanitizeDetail 先经 logx.Redact，再递归处理嵌套值、截断单字符串 8KB，
// 最后保证序列化后不超过 32KB。
func sanitizeDetail(detail map[string]any) json.RawMessage {
	if detail == nil {
		return json.RawMessage("{}")
	}
	clean := sanitizeMap(detail)
	encoded, err := json.Marshal(clean)
	if err != nil {
		return json.RawMessage(`{"_error":"detail_encode_failed"}`)
	}
	if len(encoded) <= maxDetailBytes {
		return json.RawMessage(encoded)
	}
	// 截断后必须仍是合法 JSON；保留大小信息，避免静默丢字段。
	replacement, _ := json.Marshal(map[string]any{
		"_truncated": true,
		"bytes":      len(encoded),
	})
	return json.RawMessage(replacement)
}

func sanitizeMap(in map[string]any) map[string]any {
	out := logx.Redact(in)
	if out == nil {
		return map[string]any{}
	}
	for key, value := range out {
		out[key] = sanitizeValue(value)
	}
	return out
}

func sanitizeValue(value any) any {
	switch v := value.(type) {
	case string:
		return truncateString(v)
	case map[string]any:
		return sanitizeMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeValue(item)
		}
		return out
	case []string:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = truncateString(item)
		}
		return out
	default:
		return value
	}
}

func truncateString(s string) string {
	if len(s) <= maxStringBytes {
		return s
	}
	cut := s[:maxStringBytes]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
