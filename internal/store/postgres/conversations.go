package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

// replayCatalogStepLimit 是回放步数上限（契约 11 节第 17/21 条写死）；
// 目录只做步数预判 ToolCount+ApprovalCount，不读 result、不判 payload。
const replayCatalogStepLimit = 20000

// replayCatalogQueryMaxRunes 是回放目录 q 的长度上限（契约 5.19.1：1..200）。
const replayCatalogQueryMaxRunes = 200

type conversationStore struct{ q querier }

const conversationColumns = `id::text, title, coalesce(provider_id::text, ''), model,
	approval_mode, created_at, updated_at, last_message_at`

func scanConversation(row pgx.Row) (store.Conversation, error) {
	var c store.Conversation
	var providerID string
	var approvalMode *string
	err := row.Scan(&c.ID, &c.Title, &providerID, &c.Model,
		&approvalMode, &c.CreatedAt, &c.UpdatedAt, &c.LastMessageAt)
	if approvalMode != nil {
		mode := store.ApprovalMode(*approvalMode)
		c.ApprovalMode = &mode
	}
	c.ProviderID = nullableString(providerID)
	c.CreatedAt = utc(c.CreatedAt)
	c.UpdatedAt = utc(c.UpdatedAt)
	c.LastMessageAt = utc(c.LastMessageAt)
	return c, mapError(err)
}

// ListPage 按 last_message_at DESC, id DESC 游标分页返回会话。
// cursor.ID 为空表示第一页；否则返回排序严格排在 (cursor.LastMessageAt,
// cursor.ID) 之后的更早会话。多查一条判定 has_more。
func (s conversationStore) ListPage(ctx context.Context, cursor store.ConversationListCursor, limit int) ([]store.Conversation, bool, error) {
	limit = fixedPageLimit(limit)
	var (
		rows pgx.Rows
		err  error
	)
	if cursor.ID == "" {
		rows, err = s.q.Query(ctx, `
			SELECT `+conversationColumns+`
			FROM conversations
			ORDER BY last_message_at DESC, id DESC
			LIMIT $1`, limit+1)
	} else {
		rows, err = s.q.Query(ctx, `
			SELECT `+conversationColumns+`
			FROM conversations
			WHERE (last_message_at, id) < ($1, $2::uuid)
			ORDER BY last_message_at DESC, id DESC
			LIMIT $3`, utc(cursor.LastMessageAt), cursor.ID, limit+1)
	}
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Conversation, 0)
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := mapError(rows.Err()); err != nil {
		return nil, false, err
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

func (s conversationStore) Get(ctx context.Context, id string) (store.Conversation, error) {
	return scanConversation(s.q.QueryRow(ctx,
		`SELECT `+conversationColumns+` FROM conversations WHERE id = $1::uuid`, id))
}

func (s conversationStore) Create(ctx context.Context, p store.CreateConversationParams) (store.Conversation, error) {
	title := p.Title
	if title == "" {
		title = "新会话"
	}
	return scanConversation(s.q.QueryRow(ctx, `
		INSERT INTO conversations (title)
		VALUES ($1)
		RETURNING `+conversationColumns, title))
}

func (s conversationStore) UpdateTitle(ctx context.Context, id, title string, at time.Time) (store.Conversation, error) {
	return scanConversation(s.q.QueryRow(ctx, `
		UPDATE conversations SET title = $2, updated_at = $3
		WHERE id = $1::uuid
		RETURNING `+conversationColumns, id, title, utc(at)))
}

func (s conversationStore) BindModel(ctx context.Context, id, providerID, model string, at time.Time) (store.Conversation, error) {
	return scanConversation(s.q.QueryRow(ctx, `
		UPDATE conversations
		SET provider_id = NULLIF($2, '')::uuid, model = $3, updated_at = $4
		WHERE id = $1::uuid
		RETURNING `+conversationColumns, id, providerID, model, utc(at)))
}

func (s conversationStore) SetApprovalMode(ctx context.Context, id string, mode *store.ApprovalMode, at time.Time) (store.Conversation, error) {
	// 六期 BE-P（契约 3.4）：mode=nil 清除覆盖（NULL=跟随全局）；非法值由
	// 列 CHECK 兜底（驱动错误经 mapError 归 ErrInvalid 语义）。
	var modeValue any
	if mode != nil {
		modeValue = string(*mode)
	}
	return scanConversation(s.q.QueryRow(ctx, `
		UPDATE conversations SET approval_mode = $2, updated_at = $3
		WHERE id = $1::uuid
		RETURNING `+conversationColumns, id, modeValue, utc(at)))
}

func (s conversationStore) Touch(ctx context.Context, id string, at time.Time) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE conversations SET last_message_at = $2, updated_at = $2
		WHERE id = $1::uuid`, id, utc(at))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s conversationStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "conversations", id)
}

// ensureConversation 用于分页查询前的存在性判断；不存在返回 store.ErrNotFound。
func ensureConversation(ctx context.Context, q querier, id string) error {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM conversations WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		return mapError(err)
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

// replayCatalogColumns 是回放目录的单行聚合（契约 5.19.1 的 SQL 逐字形状）：
// 两个 LATERAL 子查询各算一次（tool/action/approval 计数），不读 messages、
// 不读 result、不新增索引。action_tools 由调用方以参数传入（agent.
// ReplayActionTools 的同一份名单，不在 SQL 里复制字面量）。
const replayCatalogColumns = `c.id::text, c.title, c.last_message_at,
	coalesce(tc.tool_count, 0), coalesce(tc.action_count, 0),
	coalesce(ap.approval_count, 0)`

const replayCatalogFrom = `
	FROM conversations c
	LEFT JOIN LATERAL (
		SELECT count(*) AS tool_count,
		       count(*) FILTER (WHERE t.name = ANY($1::text[])) AS action_count
		FROM tool_calls t WHERE t.conversation_id = c.id
	) tc ON TRUE
	LEFT JOIN LATERAL (
		SELECT count(*) AS approval_count
		FROM approvals a WHERE a.conversation_id = c.id
	) ap ON TRUE`

// replayCatalogEscape 转义 ILIKE 模式元字符：反斜杠先替（否则会把后面的
// 转义再转一遍），再替 % 与 _。SQL 侧显式 ESCAPE '\'（契约 5.19.1：防
// 通配符放大扫描，q='%' 只命中标题含字面 % 的会话）。
var replayCatalogEscape = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func scanReplayCatalogItem(row pgx.Row) (store.ReplayCatalogItem, error) {
	var it store.ReplayCatalogItem
	err := row.Scan(&it.ConversationID, &it.Title, &it.LastMessageAt,
		&it.ToolCount, &it.ActionCount, &it.ApprovalCount)
	if err != nil {
		return store.ReplayCatalogItem{}, mapError(err)
	}
	it.LastMessageAt = utc(it.LastMessageAt)
	it.HasActions = it.ActionCount > 0
	// 步数上限预判写死（契约 5.19.1）：tool_count + approval_count > 20000。
	it.Truncated = it.ToolCount+it.ApprovalCount > replayCatalogStepLimit
	return it, nil
}

// ListReplayCatalog 是回放目录聚合查询（八期 BE-R，契约 5.19.1）：单条 SQL
// + 两个 LATERAL 子查询，多查一条判定 has_more；排序与游口语义与 ListPage
// 完全一致（last_message_at DESC, id DESC）。
func (s conversationStore) ListReplayCatalog(ctx context.Context, p store.ListReplayCatalogParams) ([]store.ReplayCatalogItem, bool, error) {
	if p.Limit < 1 || p.Limit > maxPageLimit {
		return nil, false, store.ErrInvalid
	}
	if utf8.RuneCountInString(p.Query) > replayCatalogQueryMaxRunes {
		return nil, false, store.ErrInvalid
	}
	actionTools := nonNil(p.ActionTools)
	pattern := ""
	if p.Query != "" {
		pattern = replayCatalogEscape.Replace(p.Query)
	}

	where := `WHERE ($2 = '' OR c.title ILIKE '%' || $2 || '%' ESCAPE '\')
		AND (NOT $3::boolean OR coalesce(tc.action_count, 0) > 0)`
	args := []any{actionTools, pattern, p.HasActions}
	if p.Cursor.ID != "" {
		args = append(args, utc(p.Cursor.LastMessageAt), p.Cursor.ID, p.Limit+1)
		where += `
		AND (c.last_message_at, c.id) < ($4, $5::uuid)`
	} else {
		args = append(args, p.Limit+1)
	}
	query := `SELECT ` + replayCatalogColumns + replayCatalogFrom + `
		` + where + `
		ORDER BY c.last_message_at DESC, c.id DESC
		LIMIT $` + fmt.Sprintf("%d", len(args))

	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer rows.Close()

	items := make([]store.ReplayCatalogItem, 0, p.Limit)
	for rows.Next() {
		item, err := scanReplayCatalogItem(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapError(err)
	}
	if len(items) > p.Limit {
		return items[:p.Limit], true, nil
	}
	return items, false, nil
}
