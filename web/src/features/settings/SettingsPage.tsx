import { Alert, Card, Space, Spin, Typography } from 'antd';

import ErrorBanner from '../../components/ErrorBanner';
import PageHeader from '../../components/PageHeader';
import ApprovalModeSelect from '../../components/ApprovalModeSelect';
import {
  APPROVAL_MODE_META,
  APPROVAL_MODES,
  normalizeApprovalMode,
} from '../../components/approvalMode';
import { describeError, toApiErrorInfo } from '../chat/useChatStream';
import { useGlobalApprovalMode, useUpdateGlobalApprovalMode } from './useApprovalSettings';

// 设置页（六期 FE-P1，契约 8.1：/settings——全局审批门限选择与说明）。
// 保存即时生效（onChange 即 PUT /api/settings）并 toast；已 pending 的审批
// 不迁移，按创建时快照展示（2.11 约定 1）。

export default function SettingsPage() {
  const settingsQuery = useGlobalApprovalMode();
  const updateMutation = useUpdateGlobalApprovalMode();

  // 读取兜底 standard（后端同样对缺失键兜底；前端防御非法值）。
  const mode = normalizeApprovalMode(settingsQuery.data?.approval_mode);
  const errorInfo = settingsQuery.error ? toApiErrorInfo(settingsQuery.error) : null;

  return (
    <div className="opsmind-admin-page">
      <PageHeader title="设置" description="系统级配置；修改保存即时生效。" />

      {errorInfo ? (
        <ErrorBanner
          code={errorInfo.code}
          message={describeError(errorInfo.code, errorInfo.message)}
          requestId={errorInfo.requestId}
          onRetry={() => void settingsQuery.refetch()}
        />
      ) : (
        <Card
          size="small"
          title="审批门限"
          extra={
            settingsQuery.isLoading ? (
              <Spin size="small" />
            ) : (
              <ApprovalModeSelect
                value={mode}
                ariaLabel="审批门限"
                disabled={updateMutation.isPending}
                onChange={(next) => {
                  // 全局侧无「跟随全局」项（allowFollowGlobal=false），
                  // null 分支只为满足联合类型签名，不应当出现。
                  if (next !== null) {
                    updateMutation.mutate(next);
                  }
                }}
              />
            )
          }
        >
          <Space direction="vertical" size={10} style={{ width: '100%' }}>
            <Typography.Text type="secondary">
              决定模型与控制台命令「是否需要人工批准」的全局档位；各会话可在对话页头部
              单独覆盖（会话覆盖优先于全局，2.11 约定 1）。变更对之后新建的审批即时生效，
              已等待中的审批不受影响。
            </Typography.Text>

            {APPROVAL_MODES.map((value) => {
              const meta = APPROVAL_MODE_META[value];
              const active = mode === value;
              return (
                <div key={value} className="settings-approval-mode-item">
                  <Typography.Text
                    strong={active}
                    {...(meta.danger ? { className: 'approval-mode-danger-text' } : {})}
                  >
                    {active ? '当前 · ' : ''}
                    {meta.label}
                  </Typography.Text>
                  <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
                    {meta.description}
                  </Typography.Paragraph>
                </div>
              );
            })}

            {/* 契约 8.7：none 档不可跳过警示（当前生效为 none 时展示）。
                六期打磨 sprint 5（走查 low）：句子里的 `auto_exec_enabled=false`
                是数据库字段名，用户看不懂；改写为服务器页的开关名「自动执行」，
                字段名只留代码注释（approvalMode.ts 同款两处同步）。 */}
            {mode === 'none' ? (
              <Alert
                type="warning"
                showIcon
                message="所有命令将自动执行（含高危），仅剩审计留痕；关闭了「自动执行」开关的服务器仍会拦截非只读命令"
              />
            ) : null}

            {updateMutation.isError ? (
              <Alert type="error" showIcon message="全局审批门限保存失败，请重试。" />
            ) : null}
          </Space>
        </Card>
      )}
    </div>
  );
}
