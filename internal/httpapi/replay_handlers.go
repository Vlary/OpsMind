// replay_handlers.go 实现八期回放目录接口（契约 2.12 约定 4 / 5.19.1）。
//
// 为什么是后端接口而不是前端对 /api/conversations 聚合（契约 5.19.1 的表）：
// 现有会话列表没有「工具数 / 审批数 / 是否有动作 / 每会话 truncated 预判」；
// 前端若为每个会话各调一次 /replay（20000 步、8 MiB），100 个会话就是 100 次
// 三表全量翻页，不可接受。
package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/agent"
	"aiops/internal/apierr"
	"aiops/internal/authz"
	"aiops/internal/store"
)

const (
	// replayCatalogDefaultLimit 是目录页缺省页大小（契约 5.19.1：比聊天侧栏
	// 更窄，缺省 50——不得复用 queryConversationLimit 的 200）。
	replayCatalogDefaultLimit = 50
	// replayCatalogMaxLimit 是目录页大小上限（契约 11 节第 22 条）。
	replayCatalogMaxLimit = 200
	// replayCatalogQueryMaxRunes 是 q 的长度上限（契约 5.19.1：1..200 字符）。
	replayCatalogQueryMaxRunes = 200
)

// replayCatalogPage 是目录页响应（openapi ReplayCatalogPage）。单行直接复用
// store.ReplayCatalogItem 的 JSON tag——它是契约 3.2 的逐字定义。
type replayCatalogPage struct {
	Items      []store.ReplayCatalogItem `json:"items"`
	HasMore    bool                      `json:"has_more"`
	NextCursor *conversationCursor       `json:"next_cursor"`
}

// queryReplayCatalogLimit 解析目录 limit：缺省 50，合法范围 1..200（与
// queryLimit 同校验口径，但缺省更小——契约 5.19.1 明确禁止复用
// queryConversationLimit 的 200 缺省）。
func queryReplayCatalogLimit(c *gin.Context) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return replayCatalogDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > replayCatalogMaxLimit {
		writeError(c, apierr.ValidationFailed, "limit must be between 1 and 200")
		return 0, false
	}
	return n, true
}

// queryReplayCatalogHasActions 解析 has_actions：缺省 false。
func queryReplayCatalogHasActions(c *gin.Context) (bool, bool) {
	raw := strings.TrimSpace(c.Query("has_actions"))
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		writeError(c, apierr.ValidationFailed, "has_actions must be a boolean")
		return false, false
	}
	return v, true
}

// replayCatalogAuditIDLimit 是目录级 replay.viewed 审计里明细 ID 的上限
// （防 OOM 铁律：写死数值；完整条数由 detail.count 表达）。
const replayCatalogAuditIDLimit = 20

// catalogAuditSummary 是目录级审计的统计结果。
type catalogAuditSummary struct {
	count     int
	ids       []string
	truncated bool
}

// crossOwnerCatalogAudit 统计本页中「不属于当前用户」的回放会话。
//
// audit:read 的目录没有 SQL 归属过滤，逐行读会话拿 owner（与修复前逐条
// 写审计时的查询次数相同，单页最多 replayCatalogMaxLimit=200 次按主键
// SELECT）；区别是审计只写一条而不是 200 条。
func (d *Deps) crossOwnerCatalogAudit(c *gin.Context, items []store.ReplayCatalogItem, me string) catalogAuditSummary {
	var sum catalogAuditSummary
	if d.Store == nil || d.Store.Conversations() == nil {
		return sum
	}
	if sum.ids == nil {
		sum.ids = make([]string, 0, replayCatalogAuditIDLimit)
	}
	for _, it := range items {
		conv, err := d.Store.Conversations().Get(c.Request.Context(), it.ConversationID)
		if err != nil || conv.UserID == "" || conv.UserID == me {
			continue
		}
		sum.count++
		if len(sum.ids) < replayCatalogAuditIDLimit {
			sum.ids = append(sum.ids, conv.ID)
		} else {
			sum.truncated = true
		}
	}
	return sum
}

// handleListReplays 返回「可回放会话」的摘要页（契约 2.12 约定 4 / 5.19.1）。
//
// 权限同 GET /api/conversations（登录态；非 audit:read 按 user_id 过滤，audit:read 全量只读）。
// 有界性：limit 1..200（缺省 50）、单条聚合 SQL、多查一条判 has_more；11 个
// 动作类工具名以参数传入（不在 SQL 复制名单）；不读 messages、不读 result。
func (d *Deps) handleListReplays(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	// 会话自有回放或 audit:read 跨用户目录；其他角色（含 approver）403。
	if !authz.AuthorizeAny(c, d.authzDenied, authz.PermChat, authz.PermAudit) {
		return
	}
	limit, ok := queryReplayCatalogLimit(c)
	if !ok {
		return
	}
	cursor, ok := queryConversationCursor(c)
	if !ok {
		return
	}
	hasActions, ok := queryReplayCatalogHasActions(c)
	if !ok {
		return
	}
	// q：1..200 字符（空=不过滤）；转义在 store 内完成（ILIKE ESCAPE，
	// 防通配符放大扫描——契约 5.19.1）。
	q := c.Query("q")
	if utf8.RuneCountInString(q) > replayCatalogQueryMaxRunes {
		writeError(c, apierr.ValidationFailed, "q must be between 1 and 200 characters")
		return
	}

	// 契约 12.3 第 3 条：非 audit:read 的目录在 SQL 内强制 user_id=自己
	//（他人会话占满 limit 窗口会让目录为空且无法翻页）。audit:read 传空串
	// 拿全量只读目录；跨用户读取仍需写 replay.viewed（第 20 条）。
	me := c.GetString("user_id")
	ownerFilter := me
	if authz.Allowed(c, authz.PermAudit) {
		ownerFilter = ""
	}
	items, hasMore, err := d.Store.Conversations().ListReplayCatalog(c.Request.Context(),
		store.ListReplayCatalogParams{
			Query:       q,
			HasActions:  hasActions,
			Cursor:      cursor,
			Limit:       limit,
			ActionTools: agent.ReplayActionTools(),
			UserID:      ownerFilter,
		})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.ReplayCatalogItem{}
	}
	if ownerFilter == "" {
		// 契约 12.3 第 20 条：audit:read 跨用户读取回放目录写
		// replay.viewed（自己的不审计）。
		//
		// 走查第 3 轮 low：此前对目录里每个非本人会话逐条写 replay.viewed
		//（单页最多 200 行）——审计员每打开一次目录就刷 200 条，浏览行为把
		//「查看他人回放」这条治理信号淹没（实测 admin 201 行、au1 55 行，
		// 而会话只有 76 个）。这里合并成**一条**目录级审计：detail 带跨用户
		// 条数、本页条数与 conversation_id 摘要（有界 20 个 + truncated 标记）；
		// 真正打开单个回放仍由 replayConversation 逐条写。
		catalogAudit := d.crossOwnerCatalogAudit(c, items, me)
		if catalogAudit.count > 0 {
			detail := map[string]any{
				"count":            catalogAudit.count,
				"page_count":       len(items),
				"limit":            limit,
				"has_more":         hasMore,
				"query":            q,
				"has_actions":      hasActions,
				"conversation_ids": catalogAudit.ids,
			}
			if catalogAudit.truncated {
				detail["conversation_ids_truncated"] = true
			}
			d.auditEvent(c, "replay.viewed", "replay_catalog", "", detail)
		}
	}
	var next *conversationCursor
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = &conversationCursor{LastMessageAt: last.LastMessageAt, ID: last.ConversationID}
	}
	c.JSON(http.StatusOK, replayCatalogPage{Items: items, HasMore: hasMore, NextCursor: next})
}
