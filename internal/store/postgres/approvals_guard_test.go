package postgres

import (
	"strings"
	"testing"
)

// TestDecideSQLEnforcesSoD 契约 12.3 第 6 条：审批决策的 UPDATE 必须自带
// requested_by <> decided_by（legacy 历史行豁免、Cancel 的空 decided_by
// 豁免）。这是 DB 层的第二道防线，不能只靠应用层先读后写。
func TestDecideSQLEnforcesSoD(t *testing.T) {
	clause := strings.Join(strings.Fields(decideSoDClause), " ")
	for _, want := range []string{
		"legacy OR",
		"$3::uuid IS NULL OR",
		"requested_by <> $3::uuid",
	} {
		if !strings.Contains(clause, want) {
			t.Fatalf("decide SoD clause missing %q: %s", want, clause)
		}
	}
}

// TestDecideSQLEnforcesExpiry 第 11 轮走查 high：decide 的 UPDATE 必须自带
// expires_at > 决策时刻（$5=decided_at）的 TTL 兜底，覆盖「服务层读到未
// 过期、写入时已过期」的竞态；系统 expired 迁移（Cancel/重启收敛）豁免。
func TestDecideSQLEnforcesExpiry(t *testing.T) {
	clause := strings.Join(strings.Fields(decideExpiryClause), " ")
	for _, want := range []string{
		"expires_at > $5",
		"$2 = 'expired'",
	} {
		if !strings.Contains(clause, want) {
			t.Fatalf("decide expiry clause missing %q: %s", want, clause)
		}
	}
}
