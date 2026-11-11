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

// handleListReplays 返回「可回放会话」的摘要页（契约 2.12 约定 4 / 5.19.1）。
//
// 权限同 GET /api/conversations（登录态；v1 单管理员，无额外 ACL 维度）。
// 有界性：limit 1..200（缺省 50）、单条聚合 SQL、多查一条判 has_more；11 个
// 动作类工具名以参数传入（不在 SQL 复制名单）；不读 messages、不读 result。
func (d *Deps) handleListReplays(c *gin.Context) {
	if !d.requireStore(c) {
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

	items, hasMore, err := d.Store.Conversations().ListReplayCatalog(c.Request.Context(),
		store.ListReplayCatalogParams{
			Query:       q,
			HasActions:  hasActions,
			Cursor:      cursor,
			Limit:       limit,
			ActionTools: agent.ReplayActionTools(),
		})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.ReplayCatalogItem{}
	}
	var next *conversationCursor
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = &conversationCursor{LastMessageAt: last.LastMessageAt, ID: last.ConversationID}
	}
	c.JSON(http.StatusOK, replayCatalogPage{Items: items, HasMore: hasMore, NextCursor: next})
}
