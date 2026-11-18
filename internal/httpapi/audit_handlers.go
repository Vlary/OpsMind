package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
	"aiops/internal/audit"
	"aiops/internal/store"
)

// auditItem 是审计查询响应项：在存储模型基础上装配 actor_name，
// 供前端主行直接显示「管理员 <username>」而不是裸 UUID。
type auditItem struct {
	store.AuditLog
	ActorName *string `json:"actor_name"`
}

type auditPageResponse struct {
	Items        []auditItem `json:"items"`
	HasMore      bool        `json:"has_more"`
	NextBeforeID *int64      `json:"next_before_id"`
}

func (d *Deps) handleListAudit(c *gin.Context) {
	beforeID, ok := queryBeforeID(c)
	if !ok {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}

	filter := store.AuditFilter{
		BeforeID:     beforeID,
		Limit:        limit,
		Action:       c.Query("action"),
		ResourceType: c.Query("resource_type"),
		ResourceID:   c.Query("resource_id"),
	}
	if raw := c.Query("conversation_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			writeError(c, apierr.ValidationFailed, "conversation_id must be a valid uuid")
			return
		}
		filter.ConversationID = raw
	}
	if raw := c.Query("server_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			writeError(c, apierr.ValidationFailed, "server_id must be a valid uuid")
			return
		}
		filter.ServerID = raw
	}

	items, hasMore, err := d.Store.AuditLogs().List(c.Request.Context(), filter)
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	if items == nil {
		items = []store.AuditLog{}
	}

	// 装配 actor_name：仅 actor_type=user 的记录按 actor_id 去重批量查用户名；
	// 查询失败降级为 null（前端退回显示 actor_id），审计页本身不因此失败。
	names, err := d.actorNames(c.Request.Context(), items)
	if err != nil {
		if d.Log != nil {
			d.Log.WarnContext(c.Request.Context(), "audit_actor_names_failed", "error", err.Error())
		}
		names = nil
	}
	out := make([]auditItem, len(items))
	for i := range items {
		out[i] = auditItem{AuditLog: items[i]}
		if name, ok := names[items[i].ActorID]; ok {
			n := name
			out[i].ActorName = &n
		}
	}
	var next *int64
	if hasMore && len(out) > 0 {
		id := out[len(out)-1].ID
		next = &id
	}
	c.JSON(http.StatusOK, auditPageResponse{Items: out, HasMore: hasMore, NextBeforeID: next})
}

// actorNames 汇总一页审计记录中 user 操作者的 id 并批量查用户名。
func (d *Deps) actorNames(ctx context.Context, items []store.AuditLog) (map[string]string, error) {
	seen := make(map[string]struct{}, 4)
	ids := make([]string, 0, 4)
	for _, it := range items {
		if it.ActorType != "user" || it.ActorID == "" {
			continue
		}
		if _, dup := seen[it.ActorID]; dup {
			continue
		}
		seen[it.ActorID] = struct{}{}
		ids = append(ids, it.ActorID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return d.Store.Users().UsernamesByIDs(ctx, ids)
}

// 审计哈希链校验（契约 12.3 第 19/20 条、openapi GET /api/audit/verify）。
const (
	auditVerifyDefaultWindow = 200
	auditVerifyMaxWindow     = 200
)

type auditVerifyResponse struct {
	OK      bool `json:"ok"`
	Checked int  `json:"checked"`
	// FirstID 是本次校验区间覆盖的最小审计 id（空区间为 0）。页面据此
	// 展示「#first–#head」并声明更早记录未被本次校验覆盖（第 2 轮走查
	// low：此前只报 checked 条数，499 行库查 200 条却像是全链通过）。
	FirstID int64 `json:"first_id"`
	// Window 是本次实际生效的窗口上限（缺省 200）。前端据此说明这是
	// 有界窗口而非全量校验。
	Window int `json:"window"`
	// Legacy 是本次校验区间内按「仅链接」处理的 00014 前存量行数：
	// 其纳秒哈希输入已不可恢复，不能声称已复算通过。
	Legacy   int    `json:"legacy"`
	HeadID   int64  `json:"head_id"`
	HeadHash string `json:"head_hash"`
	BrokenAt *int64 `json:"broken_at"`
}

// auditVerifyBrokenDetails 把 409 的校验结果放进 error.details（统一错误
// 信封不变）：审计页据此展示断点 id 与已校验条数。
func auditVerifyBrokenDetails(resp auditVerifyResponse) map[string]any {
	details := map[string]any{
		"checked":   resp.Checked,
		"first_id":  resp.FirstID,
		"window":    resp.Window,
		"legacy":    resp.Legacy,
		"head_id":   resp.HeadID,
		"head_hash": resp.HeadHash,
	}
	if resp.BrokenAt != nil {
		details["broken_at"] = *resp.BrokenAt
	}
	return details
}

// handleVerifyAudit 重算指定区间（缺省最近 200 条）的 prev_hash/hash。
//
// 有界读取：窗口上限 200（store 单页上限也是 200），经 List 只取窗口 +
// 一条锚点，不把全表装进内存。链完整 200；断裂 409 AUDIT_CHAIN_BROKEN；
// 读取本身写 audit.verify 审计。
func (d *Deps) handleVerifyAudit(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	fromID, ok := queryPositiveID(c, "from_id")
	if !ok {
		return
	}
	toID, ok := queryPositiveID(c, "to_id")
	if !ok {
		return
	}
	if fromID > 0 && toID > 0 && fromID > toID {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "from_id must not exceed to_id")
		return
	}
	window := auditVerifyDefaultWindow
	if toID > 0 && fromID > 0 {
		if span := toID - fromID + 1; span < int64(window) {
			window = int(span)
		}
	}
	if window <= 0 || window > auditVerifyMaxWindow {
		window = auditVerifyDefaultWindow
	}

	// 取 [.., to_id] 的最新 window 条（store 单页上限 200）；窗口前一条
	// 单独查作链锚点，避免请求超过分页上限。
	beforeID := int64(0)
	if toID > 0 {
		beforeID = toID + 1
	}
	items, _, err := d.Store.AuditLogs().List(c.Request.Context(), store.AuditFilter{
		BeforeID: beforeID,
		Limit:    window,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.NotFound)
		return
	}
	// List 是 id DESC；翻转为 ASC。
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	if len(items) > 0 && fromID > 0 {
		start := 0
		for start < len(items) && items[start].ID < fromID {
			start++
		}
		items = items[start:]
	}
	prevHash := audit.ZeroHash
	if len(items) > 0 && items[0].ID > 1 {
		anchor, _, aerr := d.Store.AuditLogs().List(c.Request.Context(), store.AuditFilter{
			BeforeID: items[0].ID,
			Limit:    1,
		})
		if aerr != nil {
			d.handleStoreError(c, aerr, apierr.NotFound)
			return
		}
		if len(anchor) > 0 {
			prevHash = anchor[0].Hash
		}
	}

	resp := auditVerifyResponse{OK: true, HeadHash: audit.ZeroHash, Window: window}
	for _, item := range items {
		if resp.FirstID == 0 {
			resp.FirstID = item.ID
		}
		resp.Checked++
		if item.LegacyHash {
			resp.Legacy++
		}
		_, ok := audit.VerifyRecord(prevHash, item)
		resp.HeadID = item.ID
		resp.HeadHash = item.Hash
		if !ok {
			resp.OK = false
			brokenAt := item.ID
			resp.BrokenAt = &brokenAt
			break
		}
		prevHash = item.Hash
	}
	if len(items) == 0 {
		// 空区间：链尾即当前链头。
		if last, err := d.Store.AuditLogs().LastHash(c.Request.Context()); err == nil {
			resp.HeadHash = last
		}
	}

	d.auditEvent(c, "audit.verify", "audit_log", "", map[string]any{
		"from_id":   fromID,
		"to_id":     toID,
		"first_id":  resp.FirstID,
		"window":    resp.Window,
		"ok":        resp.OK,
		"checked":   resp.Checked,
		"legacy":    resp.Legacy,
		"broken_at": resp.BrokenAt,
	})
	if !resp.OK {
		writeAPIErrorDetail(c, http.StatusConflict, "AUDIT_CHAIN_BROKEN",
			"audit hash chain is broken", auditVerifyBrokenDetails(resp))
		return
	}
	c.JSON(http.StatusOK, resp)
}

// queryPositiveID 解析正整数查询参数（缺省 0）。
func queryPositiveID(c *gin.Context, name string) (int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 1 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, name+" must be a positive integer")
		return 0, false
	}
	return v, true
}
