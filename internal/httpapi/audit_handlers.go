package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/apierr"
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
