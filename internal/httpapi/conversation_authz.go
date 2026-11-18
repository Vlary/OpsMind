// conversation_authz.go 是会话私有语义的归属守卫（docs/CONTRACT.md
// 12.3 第 3 条 / 12.4 第 2 条）。
//
// 会话按 conversations.user_id 隔离：非 owner 对 GET/PATCH/DELETE/
// messages/tool-calls/replay/POST chat 一律 404（不泄露存在性）；admin
// 不例外。回放额外允许 audit:read 跨用户只读，跨用户读取写
// replay.viewed 审计（自己的回放不审计）。
package httpapi

import (
	"errors"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/authz"
	"aiops/internal/store"
)

// requireChatPermission 是会话端点的第一道权限守卫（router.go 注册的
// 路由无法在路由层挂中间件，故在 handler 内联）。
func (d *Deps) requireChatPermission(c *gin.Context) bool {
	return authz.Authorize(c, authz.PermChat, d.authzDenied)
}

// conversationNotFound 统一 404，缺失与无权访问使用同一响应。
func conversationNotFound(c *gin.Context) {
	writeError(c, apierr.ConversationNotFound, "conversation not found")
}

// ownedConversation 加载会话并校验 owner；非 owner/不存在均 404。
// 仓储故障是服务端错误（500），不能伪装成「不存在」——否则 DB 故障在
// 前端表现为会话被删，且丢失可观测信号。
func (d *Deps) ownedConversation(c *gin.Context, id string) (store.Conversation, bool) {
	conv, err := d.Store.Conversations().Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			conversationNotFound(c)
			return store.Conversation{}, false
		}
		d.handleStoreError(c, err, apierr.ConversationNotFound)
		return store.Conversation{}, false
	}
	if conv.UserID != c.GetString("user_id") {
		conversationNotFound(c)
		return store.Conversation{}, false
	}
	return conv, true
}

// replayConversation 加载回放目标：owner 或 audit:read；其他人
// 404。跨用户读取写 replay.viewed（自己的不审计）。
func (d *Deps) replayConversation(c *gin.Context, id string) (store.Conversation, bool) {
	conv, err := d.Store.Conversations().Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			conversationNotFound(c)
			return store.Conversation{}, false
		}
		d.handleStoreError(c, err, apierr.ConversationNotFound)
		return store.Conversation{}, false
	}
	me := c.GetString("user_id")
	if conv.UserID == me {
		return conv, true
	}
	if !authz.Allowed(c, authz.PermAudit) {
		conversationNotFound(c)
		return store.Conversation{}, false
	}
	d.auditEvent(c, "replay.viewed", "conversation", conv.ID, map[string]any{
		"owner_id": conv.UserID,
	})
	return conv, true
}

// 会话列表的归属过滤不在本文件：它必须下沉进 SQL
//（postgres.conversationStore.ListPage 的 userID 参数），否则他人会话会占满
// 分页窗口，让当前用户自己的列表为空且无法翻页（第 2 轮走查 high）。Go 侧
// 逐行过滤（filterOwnedConversations）已删除，避免两套语义并存。
