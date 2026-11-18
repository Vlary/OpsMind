// 审计员只读说明（RBAC 走查 sprint 1 low）。
//
// 审计员（role=auditor）只有 audit:read：可看审计、跨用户回放与录制，但
// 任何写接口都会被服务端 403。页头只写「只追加审计日志」不足以说明当前
// 账号是只读的，这里在 /audit 与 /replays 顶部给出明确提示；其他角色不渲染。

import { Alert } from 'antd';

import { useMe } from '../features/auth/useMe';

export default function AuditorReadOnlyNotice() {
  const meQuery = useMe();
  if (meQuery.data?.user.role !== 'auditor') {
    return null;
  }
  return (
    <Alert
      type="info"
      showIcon
      message="审计员为只读角色"
      description="可查看审计、回放与录制；任何写操作都会被服务端拒绝。"
    />
  );
}
