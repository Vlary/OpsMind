import { CheckOutlined, EditOutlined } from '@ant-design/icons';
import { useQuery } from '@tanstack/react-query';
import { Alert, Button, Input, Space, Tag, Typography } from 'antd';
import { useState } from 'react';

import { api } from '../api/client';
import { qk } from '../api/queryKeys';
import {
  buildDecideBody,
  canSubmitEditedCommand,
  describeRiskChange,
  EDITED_COMMAND_MAX_LENGTH,
  hasEditedChange,
  validateEditedCommand,
} from './approvalEdit';
import { describeRiskRule } from './riskRules';
import RiskBadge from './RiskBadge';

export interface ApprovalCommandEditorProps {
  /** 审批原命令（编辑态初值；未改动时不携带 edited_command）。 */
  command: string;
  /** 审批落库的风险级别（改写前后对照的基线）。 */
  riskLevel: string;
  /**
   * 审批目标服务器（preview-risk 的上下文；服务端按服务器实际 os /
   * netdev_brand 覆盖显式分册参数，与 decide 重分类一致）。为空时不做
   * 改写——与契约 2.11 约定 3「server_id 为空的审批不允许改写（400）」同口径。
   */
  serverId: string | null;
  /** 「保存并批准」提交（body 已按契约 6.3 构造：仅改写时携带 edited_command）。 */
  onSubmit: (body: { decision: 'approve' | 'deny'; edited_command?: string }) => void;
  /** 禁用（提交中/已过期/无 server_id）。 */
  disabled?: boolean;
  /** 提交中（按钮 loading）。 */
  pending?: boolean;
}

/**
 * 审批「编辑命令」面板（六期 FE-P1 的能力，六期打磨 sprint 3 抽出复用）。
 *
 * 抽出的原因：六期改写能力原先只在对话审批卡（components/ApprovalCard.tsx）
 * 可用——容器操作、诊断修复、批量详情三个 console/batch 审批入口都只能
 * 「批准/拒绝」，无法改写。后端早已支持（`POST /api/approvals/:id/decide`
 * 携带 `edited_command` 会重新分类并执行改写后命令；实测批量审批返回
 * `edited_command=uptime` 且执行输出为改写命令的结果），缺的只是入口。
 *
 * 校验/预览/请求体构造全部复用 approvalEdit.ts 的纯函数（与对话审批卡
 * 同一口径）：空草稿不可提交（六期打磨 sprint 2 high 的回归护栏）、
 * `preview-risk` 实时预览改写后风险、forbidden 时禁用提交。
 */
export default function ApprovalCommandEditor({
  command,
  riskLevel,
  serverId,
  onSubmit,
  disabled = false,
  pending = false,
}: ApprovalCommandEditorProps) {
  const [draft, setDraft] = useState<string>(command);
  const changed = hasEditedChange(command, draft);
  const error = validateEditedCommand(draft);
  // 契约 2.11 约定 3：server_id 为空的审批不允许改写（服务端 400）。
  const editable = serverId !== null && serverId !== '';

  const previewQuery = useQuery({
    queryKey: qk.approvalRiskPreview(draft),
    queryFn: async () => {
      const { data, error: apiError } = await api.POST('/api/ops/preview-risk', {
        params: { header: { Origin: window.location.origin } },
        body: {
          command: draft,
          os: 'linux',
          netdev_brand: 'generic',
          ...(editable ? { server_id: serverId } : {}),
        },
      });
      if (apiError) {
        throw apiError;
      }
      return data;
    },
    enabled: editable && draft.trim() !== '' && changed,
    staleTime: 15_000,
    retry: false,
  });

  if (!editable) {
    return (
      <Typography.Text type="secondary">
        该审批未绑定目标服务器，不支持编辑命令。
      </Typography.Text>
    );
  }

  const preview = previewQuery.data ?? null;
  const previewLevel = preview?.risk_level ?? null;
  const riskChange =
    changed && previewLevel ? describeRiskChange(riskLevel, previewLevel) : null;
  const canSubmit = !disabled && !pending && canSubmitEditedCommand(draft, previewLevel);

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <Typography.Text type="secondary">
        改写命令（保存并批准时提交，服务端将重新分类；原命令不会执行）
      </Typography.Text>
      <Input.TextArea
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
        rows={3}
        maxLength={EDITED_COMMAND_MAX_LENGTH}
        showCount
        autoSize={{ minRows: 2, maxRows: 6 }}
        aria-label="改写命令"
      />
      {error ? (
        <Alert type="warning" showIcon message={error} />
      ) : changed && previewQuery.isFetching ? (
        <Typography.Text type="secondary">正在预览改写后的风险…</Typography.Text>
      ) : changed && preview ? (
        <Space size={8} wrap>
          <Typography.Text type="secondary">改写后风险：</Typography.Text>
          <RiskBadge level={preview.risk_level} />
          {riskChange ? (
            <Tag
              color={
                riskChange.kind === 'forbidden'
                  ? 'red'
                  : riskChange.kind === 'up'
                    ? 'volcano'
                    : riskChange.kind === 'down'
                      ? 'green'
                      : 'default'
              }
            >
              {riskChange.label}
            </Tag>
          ) : null}
          <Typography.Text type="secondary">
            {describeRiskRule(preview.rule_id) ?? preview.reason}
          </Typography.Text>
        </Space>
      ) : changed && previewQuery.isError ? (
        <Typography.Text type="secondary">
          风险预览暂不可用，提交时服务端仍会重新分类。
        </Typography.Text>
      ) : null}
      {previewLevel === 'forbidden' ? (
        <Alert
          type="error"
          showIcon
          message={`改写后的命令被风险规则禁止（${preview?.rule_id ?? '未知规则'}），无法批准`}
        />
      ) : null}
      <Space wrap>
        <Button
          type="primary"
          icon={<CheckOutlined />}
          disabled={!canSubmit}
          loading={pending}
          onClick={() => {
            // 空草稿再兜一层（按钮已禁用；键盘回车/程序化触发同样不得按
            // 原命令批准执行——六期打磨 sprint 2 high）。
            if (draft.trim() === '') {
              return;
            }
            onSubmit(buildDecideBody('approve', command, draft));
          }}
        >
          保存并批准
        </Button>
        <Button
          icon={<EditOutlined />}
          disabled={disabled || pending}
          onClick={() => setDraft(command)}
        >
          重置为原命令
        </Button>
      </Space>
    </Space>
  );
}
