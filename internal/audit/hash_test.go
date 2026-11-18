package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

// 走查第 4 轮（high）：audit_logs.created_at 是 timestamptz，PostgreSQL 只
// 保留微秒；哈希输入若用纳秒，写回读出的值必然不同，全新区块链会被
// /api/audit/verify 误判断裂。
func TestPrepareTruncatesCreatedAtToDatabasePrecision(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 8, 7, 123456789, time.FixedZone("CST", 8*3600))
	p := Prepare(Event{ActorType: "user", Action: "test"}, now)
	// UTC + 微秒截断（时区信息不落哈希输入，避免同一时刻两串哈希）。
	if p.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt location = %v, want UTC", p.CreatedAt.Location())
	}
	if got := p.CreatedAt.Nanosecond(); got != 123456000 {
		t.Fatalf("CreatedAt nanos = %d, want 123456000", got)
	}

	payload, err := OutboxPayload(Event{ActorType: "user", Action: "test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// outbox payload 也必须带微秒值，worker 复算时才与落库行一致。
	if strings.Contains(string(payload), "123456789") {
		t.Fatalf("outbox payload kept nanoseconds: %s", payload)
	}
	var decoded store.CreateAuditParams
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("payload CreatedAt = %s, want %s", decoded.CreatedAt, p.CreatedAt)
	}
}

func TestComputeHashIgnoresSubMicrosecondPrecision(t *testing.T) {
	base := time.Date(2026, 11, 12, 9, 8, 7, 123456000, time.UTC)
	withNanos := base.Add(789 * time.Nanosecond)
	p := store.CreateAuditParams{
		ActorType: "user", ActorID: "u1", Action: "test.action",
		ResourceType: "test", ResourceID: "r1", IP: "127.0.0.1",
		Detail: json.RawMessage(`{"k":"v"}`),
	}
	p.CreatedAt = withNanos
	nanosHash := computeHash(p)
	p.CreatedAt = base
	if microHash := computeHash(p); microHash != nanosHash {
		t.Fatalf("hash differs across sub-microsecond input: %q vs %q", nanosHash, microHash)
	}
}

// VerifyRecord 必须能校验真实 Postgres 读回的行（微秒），而 legacy 行只
// 校验链接、跳过不可恢复的哈希复算。
func TestVerifyRecordPrecisionAndLegacy(t *testing.T) {
	p := Prepare(Event{ActorType: "user", ActorID: "u1", Action: "user.create",
		ResourceType: "user", ResourceID: "u2", Detail: map[string]any{"role": "auditor"}},
		time.Date(2026, 11, 12, 9, 8, 7, 123456789, time.UTC))
	p.PrevHash = zeroHash
	p.Hash = computeHash(p)

	// 模拟 Postgres 读回：时间只有微秒（Prepare 已截断），链接与哈希一致。
	row := store.AuditLog{
		ID: 1, CreatedAt: p.CreatedAt, ActorType: p.ActorType, ActorID: p.ActorID,
		Action: p.Action, ResourceType: p.ResourceType, ResourceID: p.ResourceID,
		Detail: p.Detail, PrevHash: p.PrevHash, Hash: p.Hash,
	}
	if _, ok := VerifyRecord(zeroHash, row); !ok {
		t.Fatal("microsecond row should verify")
	}
	// 链接被改：两条路径都必须失败。
	row.PrevHash = strings.Repeat("a", 64)
	if _, ok := VerifyRecord(zeroHash, row); ok {
		t.Fatal("wrong prev_hash should fail")
	}
	row.PrevHash = zeroHash
	row.Hash = strings.Repeat("b", 64)
	if _, ok := VerifyRecord(zeroHash, row); ok {
		t.Fatal("tampered hash should fail")
	}

	// legacy 行：纳秒哈希已不可复算，但链接正确时按 legacy 规则通过；
	// 链接错误仍然失败（legacy 不是免检）。
	legacyRow := row
	legacyRow.LegacyHash = true
	legacyRow.Hash = strings.Repeat("c", 64)
	if _, ok := VerifyRecord(zeroHash, legacyRow); !ok {
		t.Fatal("legacy row with valid link should pass")
	}
	legacyRow.PrevHash = strings.Repeat("d", 64)
	if _, ok := VerifyRecord(zeroHash, legacyRow); ok {
		t.Fatal("legacy row with broken link should fail")
	}
}
