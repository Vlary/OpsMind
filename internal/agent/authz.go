// authz.go 是 AI 工具层的权限双重保险（docs/CONTRACT.md 12.4 第 3 条）。
//
//  1. 构造发给模型的工具列表时按当前用户权限裁剪——模型看不到无权工具；
//  2. executeToolCall 分发前再按同一张 map 校验一次——即使模型硬调也拒绝，
//     且与普通工具错误一样落 tool_calls 与 tool.error 审计。
//
// 映射逐字取自契约 12.4 第 3 条；缺失映射（未知工具）失败关闭。
package agent

import (
	"context"

	"aiops/internal/authz"
	"aiops/internal/llm"
	"aiops/internal/store"
)

// toolPermissionMap 是工具名→权限的唯一映射（契约 12.4 第 3 条）。
var toolPermissionMap = map[string]authz.Permission{
	ToolListServers:         authz.PermAssetsRead,
	ToolListDatabases:       authz.PermAssetsRead,
	ToolServerFacts:         authz.PermExecuteReadonly,
	ToolDBQuery:             authz.PermExecuteReadonly,
	ToolContainerList:       authz.PermExecuteReadonly,
	ToolContainerInspect:    authz.PermExecuteReadonly,
	ToolContainerLogs:       authz.PermExecuteReadonly,
	ToolImageList:           authz.PermExecuteReadonly,
	ToolRunDiagnosis:        authz.PermExecuteReadonly,
	ToolSSHExec:             authz.PermExecuteWrite,
	ToolContainerAction:     authz.PermExecuteWrite,
	ToolBatchExecute:        authz.PermExecuteWrite,
	ToolGetMetrics:          authz.PermMetricsRead,
	ToolListAlerts:          authz.PermMetricsRead,
	ToolAuditSearch:         authz.PermAudit,
	ToolRunInspection:       authz.PermInspection,
	ToolGetInspectionReport: authz.PermInspection,
	ToolAskUser:             authz.PermChat,
}

// ToolPermission 返回工具的权限映射；第二返回值 false 表示缺失映射
// （调用方必须拒绝，不得放行）。
func ToolPermission(name string) (authz.Permission, bool) {
	perm, ok := toolPermissionMap[name]
	return perm, ok
}

// toolAllowed 判定当前用户能否使用某工具。permissions 是登录/请求时由
// 服务端计算的权限串（RunInput.Permissions）；与 role→权限映射任一命中
// 即可（两者都来自服务端，客户端无法注入）。
func toolAllowed(role store.Role, permissions []string, name string) bool {
	perm, ok := toolPermissionMap[name]
	if !ok {
		return false
	}
	for _, p := range permissions {
		if p == string(perm) {
			return true
		}
	}
	return authz.Has(role, perm)
}

// ToolDefinitionsForRole 返回按权限裁剪后的工具定义列表；未知角色/空角色
// 返回空列表（fail-closed）。顺序与 ToolDefinitions 一致。
func ToolDefinitionsForRole(role store.Role, permissions []string) []llm.Tool {
	all := ToolDefinitions()
	out := make([]llm.Tool, 0, len(all))
	for _, def := range all {
		if toolAllowed(role, permissions, def.Name) {
			out = append(out, def)
		}
	}
	return out
}

// FilterToolNames 返回当前角色可见的工具名（测试与排查用）。
func FilterToolNames(role store.Role, permissions []string) []string {
	out := make([]string, 0, len(toolPermissionMap))
	for _, name := range ToolNames() {
		if toolAllowed(role, permissions, name) {
			out = append(out, name)
		}
	}
	return out
}

// UserAuthStateLookup 在每次工具调用前复读用户状态（契约 12.4 第 3 条：
// 停用/降权后不执行）。由装配层注入；nil 时只依赖 Run 入口的角色快照
// （单测环境）。
type UserAuthStateLookup interface {
	UserAuthState(ctx context.Context, userID string) (store.Role, bool, error)
}
