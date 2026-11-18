package store

import (
	"context"
	"encoding/json"
)

// AuditDetailCanonicalizer 是审计明细的规范化能力（PostgreSQL 专用）。
//
// audit_logs.detail 是 jsonb：落库会重排对象键、规范空白与数字文本。写入侧
// 若直接对 json.Marshal 的结果算哈希，数据库读回的文本必然不同，
// GET /api/audit/verify 会误判链断裂（走查第 4 轮）。实现方在 append 前把
// 明细转换成与读回值逐字节一致的文本；不实现该接口的存储（内存 fake）
// 视为无需规范化——写入与校验使用同一份字节。
type AuditDetailCanonicalizer interface {
	CanonicalAuditDetail(ctx context.Context, raw json.RawMessage) (json.RawMessage, error)
}

// AuditDetailCanonicalizerOf 返回审计仓储的明细规范化能力；未实现返回 nil。
func AuditDetailCanonicalizerOf(st Store) AuditDetailCanonicalizer {
	if st == nil || st.AuditLogs() == nil {
		return nil
	}
	c, _ := st.AuditLogs().(AuditDetailCanonicalizer)
	return c
}
