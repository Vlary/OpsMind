// Package authz 是 RBAC 角色到权限映射的唯一实现（docs/CONTRACT.md 12.1/12.2）。
//
// 权限串与契约逐字一致；角色固定四类，每用户恰一个角色。客户端提交的
// role/permissions 一律不采信——服务端只用本包的映射计算。
package authz

import "aiops/internal/store"

// Permission 是 resource:action 形式的权限串（契约 12.1，共 19 条）。
type Permission string

const (
	// PermChat 发起对话与管理自己的会话。
	PermChat Permission = "chat:use"
	// PermAssetsRead 查看资产元数据（不含凭据）。
	PermAssetsRead Permission = "asset:read"
	// PermAssetsWrite 创建/修改/删除资产（含写入密文凭据）。
	PermAssetsWrite Permission = "asset:write"
	// PermConnectionTest 使用已存凭据主动连接目标。
	PermConnectionTest Permission = "asset:test"
	// PermExecuteReadonly 只读执行与查询（server_facts/容器只读/db_query/诊断）。
	PermExecuteReadonly Permission = "exec:read"
	// PermExecuteWrite 发起命令与写操作（是否直执行仍由 NeedsApproval 硬闸决定）。
	PermExecuteWrite Permission = "exec:write"
	// PermMetricsRead 指标与告警只读。
	PermMetricsRead Permission = "monitor:read"
	// PermAlertConfig 告警规则/渠道/采集器配置。
	PermAlertConfig Permission = "monitor:manage"
	// PermInspection 巡检任务与报告。
	PermInspection Permission = "inspection:manage"
	// PermTerminal JIT 终端/RDP 与自己录制。
	PermTerminal Permission = "terminal:use"
	// PermApprovalRead 审批列表/详情/有界上下文。
	PermApprovalRead Permission = "approval:read"
	// PermApprovalDecide 批准/拒绝审批与策略变更。
	PermApprovalDecide Permission = "approval:decide"
	// PermPolicyRequest 请求审批策略变更。
	PermPolicyRequest Permission = "approval:policy"
	// PermAudit 审计/回放/录制跨用户只读。
	PermAudit Permission = "audit:read"
	// PermProviders 供应商与 API Key 管理。
	PermProviders Permission = "provider:manage"
	// PermUsersRead 查看用户。
	PermUsersRead Permission = "user:read"
	// PermUsersWrite 创建用户/改角色/启停/吊销会话。
	PermUsersWrite Permission = "user:write"
	// PermCredentialReset 签发一次性密码重置令牌。
	PermCredentialReset Permission = "user:credential_reset"
	// PermPprof 读取 pprof。
	PermPprof Permission = "debug:read"
)

// AllPermissions 是契约 12.1 的权限全集（顺序与契约表格一致）。
var AllPermissions = []Permission{
	PermChat,
	PermAssetsRead,
	PermAssetsWrite,
	PermConnectionTest,
	PermExecuteReadonly,
	PermExecuteWrite,
	PermMetricsRead,
	PermAlertConfig,
	PermInspection,
	PermTerminal,
	PermApprovalRead,
	PermApprovalDecide,
	PermPolicyRequest,
	PermAudit,
	PermProviders,
	PermUsersRead,
	PermUsersWrite,
	PermCredentialReset,
	PermPprof,
}

// rolePermissions 是契约 12.2 的权限矩阵（唯一事实源）。
var rolePermissions = map[store.Role][]Permission{
	store.RoleAdmin: {
		PermChat, PermAssetsRead, PermAssetsWrite, PermConnectionTest,
		PermExecuteReadonly, PermExecuteWrite, PermMetricsRead, PermAlertConfig,
		PermInspection, PermTerminal, PermApprovalRead, PermApprovalDecide,
		PermPolicyRequest, PermAudit, PermProviders, PermUsersRead,
		PermUsersWrite, PermCredentialReset, PermPprof,
	},
	store.RoleOperator: {
		PermChat, PermAssetsRead, PermConnectionTest, PermExecuteReadonly,
		PermExecuteWrite, PermMetricsRead, PermInspection, PermTerminal,
	},
	store.RoleApprover: {
		PermAssetsRead, PermApprovalRead, PermApprovalDecide,
	},
	store.RoleAuditor: {
		PermAudit,
	},
}

// PermissionsFor 返回该角色的权限副本；未知角色返回空集合（失败关闭）。
func PermissionsFor(role store.Role) []Permission {
	src := rolePermissions[role]
	out := make([]Permission, len(src))
	copy(out, src)
	return out
}

// PermissionStrings 返回该角色的权限字符串（供 /auth/me 与登录响应）。
func PermissionStrings(role store.Role) []string {
	src := rolePermissions[role]
	out := make([]string, len(src))
	for i, p := range src {
		out[i] = string(p)
	}
	return out
}

// Has 报告 role 是否拥有 perm。未知角色一律 false。
func Has(role store.Role, perm Permission) bool {
	for _, p := range rolePermissions[role] {
		if p == perm {
			return true
		}
	}
	return false
}

// ValidRole 报告 role 是否为契约固定的四类。
func ValidRole(role store.Role) bool { return role.Valid() }
