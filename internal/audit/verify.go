package audit

import "aiops/internal/store"

// ZeroHash 是哈希链首行的 prev_hash（与写入路径同一常量语义）。
const ZeroHash = zeroHash

// VerifyRecord 复算一条审计记录的哈希并校验前向链接。
//
// prev 是调用方维护的上一条记录哈希（首条为 ZeroHash）。返回复算值与是否
// 一致：要求 log.PrevHash == prev 且 log.Hash == computeHash(该行)。
//
// legacy 行（00014 之前的存量）例外：其哈希输入含纳秒，数据库只保留
// 微秒、纳秒已不可恢复，因此只校验 prev_hash 链接、跳过哈希复算；调用方
// 应把这类行计入 legacy 并如实展示（不能声称已证明未被篡改）。
func VerifyRecord(prev string, log store.AuditLog) (string, bool) {
	p := store.CreateAuditParams{
		CreatedAt:      log.CreatedAt,
		ActorType:      log.ActorType,
		ActorID:        log.ActorID,
		Action:         log.Action,
		ResourceType:   log.ResourceType,
		ResourceID:     log.ResourceID,
		RequestID:      log.RequestID,
		ConversationID: log.ConversationID,
		ServerID:       log.ServerID,
		IP:             log.IP,
		Detail:         log.Detail,
		PrevHash:       log.PrevHash,
	}
	expected := computeHash(p)
	if log.LegacyHash {
		return expected, log.PrevHash == prev
	}
	return expected, log.PrevHash == prev && log.Hash == expected
}
