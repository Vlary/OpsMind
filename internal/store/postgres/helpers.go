package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aiops/internal/store"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// pageLimit 默认 50，合法范围 1..200；越界返回 store.ErrInvalid。
func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultPageLimit, nil
	}
	if limit < 1 || limit > maxPageLimit {
		return 0, store.ErrInvalid
	}
	return limit, nil
}

// fixedPageLimit 用于固定上限的列表（会话列表）：非正值取最大值，超出则截断。
func fixedPageLimit(limit int) int {
	if limit <= 0 || limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// nullStr 把空字符串转为 SQL NULL。
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nonNil 保证 text[] 参数是空数组而不是 NULL。
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// utc 统一返回 UTC 时间。
func utc(t time.Time) time.Time {
	return t.UTC()
}

// optionalID 把空字符串转换为 nil（NULL::uuid 语义）。
func optionalID(id string) *string {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	return &id
}

// nullableString 把数据库 COALESCE 出来的空串还原为 nil。
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ptrOrNil 把 nil/空指针或空串统一转成 SQL NULL，避免 ::uuid 收到空串。
// 四期 C1 起也用于 approvals 的可空 conversation_id/tool_call_id（NULL=console 审批）。
func ptrOrNil(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// deleteByID 删除一行；不存在时返回 store.ErrNotFound。
// table 只由本包常量传入，绝不来自请求。
func deleteByID(ctx context.Context, q querier, table, id string) error {
	tag, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1::uuid`, table), id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
