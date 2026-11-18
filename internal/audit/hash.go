package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// truncateToDBTime 把时间截断到 audit_logs.created_at（timestamptz）能精确
// 落库的微秒精度。哈希输入必须与读回值同精度，否则全新库也会被
// GET /api/audit/verify 判成「链已破坏」（走查第 4 轮 high）。
func truncateToDBTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// Prepare 把事件转换为可入队/可持久化的审计参数（detail 已脱敏截断）。
// 供 Writer.Enqueue 与事务内 audit_outbox 共用，保证两条路径逐字节一致。
func Prepare(e Event, now time.Time) store.CreateAuditParams {
	return store.CreateAuditParams{
		CreatedAt:      truncateToDBTime(now),
		ActorType:      e.ActorType,
		ActorID:        e.ActorID,
		Action:         e.Action,
		ResourceType:   e.ResourceType,
		ResourceID:     e.ResourceID,
		RequestID:      e.RequestID,
		ConversationID: e.ConversationID,
		ServerID:       e.ServerID,
		IP:             e.IP,
		Detail:         sanitizeDetail(e.Detail),
	}
}

// OutboxPayload 序列化一条事件为 audit_outbox.payload（JSON 为
// store.CreateAuditParams；哈希在 worker 追加时计算）。
func OutboxPayload(e Event, now time.Time) ([]byte, error) {
	return json.Marshal(Prepare(e, now))
}

// computeHash 按契约固定输入计算审计哈希：
//
//	prev_hash \x1f created_at(RFC3339Nano, UTC, 微秒精度) \x1f actor_type \x1f actor_id
//	\x1f action \x1f resource_type \x1f resource_id \x1f request_id
//	\x1f conversation_id(空为 "") \x1f server_id(空为 "") \x1f ip
//	\x1f canonical_json(detail)
//
// created_at 统一截断到微秒（与 timestamptz 落库精度一致）；校验侧用库内
// 读回值复算时逐字节自洽。
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
		truncateToDBTime(p.CreatedAt).Format(time.RFC3339Nano),
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

// AppendOnce 在调用方事务内把一条事件直接追加到 audit_logs 哈希链。
//
// 供「没有常驻 worker 的进程」使用：主机侧离线恢复 CLI（aiops admin reset /
// aiops break-glass）与 anchor 导出。它做与 worker 相同的事——取链锁、
// 读链尾、按库内 jsonb 规范形态复算 detail、追加——因此链保持连续，
// 且审计在命令返回时已可查（评审实测要求 audit_logs 立刻可见）。
//
// 若同一事务里已有 audit_outbox 待处理行，它们仍会在之后由 worker 追加，
// 顺序变为「CLI 行 → 旧 outbox 行」，链依然自洽（各行 prev_hash 均指向
// 写入时的链尾）。
func AppendOnce(ctx context.Context, st store.Store, e Event, now time.Time) error {
	if st == nil {
		return errors.New("audit: nil store")
	}
	p := Prepare(e, now)
	if outbox := store.OutboxOf(st); outbox != nil {
		if err := outbox.LockAuditChain(ctx); err != nil {
			return err
		}
	}
	prev, err := st.AuditLogs().LastHash(ctx)
	if err != nil {
		return err
	}
	p.PrevHash = prev
	if canon := store.AuditDetailCanonicalizerOf(st); canon != nil {
		detail, cerr := canon.CanonicalAuditDetail(ctx, p.Detail)
		if cerr != nil {
			return cerr
		}
		p.Detail = detail
	}
	p.Hash = computeHash(p)
	return st.AuditLogs().Append(ctx, p)
}
