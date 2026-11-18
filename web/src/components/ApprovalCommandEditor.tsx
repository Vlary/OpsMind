import { CheckOutlined } from '@ant-design/icons';
import { Button, Space, Typography } from 'antd';

export interface ApprovalCommandEditorProps {
  /** 审批原命令（只读展示）。 */
  command: string;
  /** 审批落库的风险级别（保留 props 兼容既有调用方）。 */
  riskLevel: string;
  /** 审批目标服务器（保留 props 兼容既有调用方）。 */
  serverId: string | null;
  /** 提交批准；00012 起请求体只含 decision。 */
  onSubmit: (body: { decision: 'approve' | 'deny' }) => void;
  /** 禁用（提交中/已过期）。 */
  disabled?: boolean;
  /** 提交中（按钮 loading）。 */
  pending?: boolean;
  /**
   * 是否渲染批准入口（RBAC 00012 / 第 7 轮走查）。
   *
   * 调用方必须同时满足「当前账号有 approval:decide」与「requested_by 不是
   * 当前用户」；否则只展示只读命令，批准按钮隐藏并给出等待说明。缺省
   * true 保持既有调用方语义（服务端仍是唯一授权点）。
   */
  canApprove?: boolean;
}

/**
 * 审批命令展示与批准入口（00012 RBAC 定稿）。
 *
 * 六期曾支持「审批时改写命令」，但改写会让审批者成为动作作者，绕过
 * `requested_by != decided_by` 的职责分离；00012 起后端拒绝 edited_command。
 * 本组件只展示原命令并批准；需要改命令时必须拒绝后由发起人重新发起。
 */
export default function ApprovalCommandEditor(props: ApprovalCommandEditorProps) {
  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      <Typography.Text type="secondary">
        本版本不支持审批时改写命令。如需修改，请拒绝后由发起人重新发起（重新走审批与门限）。
      </Typography.Text>
      <Space size={8} wrap>
        <Typography.Text type="secondary">命令：</Typography.Text>
        <span className="opsmind-admin-mono">{props.command}</span>
      </Space>
      {props.canApprove === false ? (
        <Typography.Text type="secondary">等待审批者处理</Typography.Text>
      ) : (
        <Button
          type="primary"
          icon={<CheckOutlined />}
          disabled={props.disabled ?? false}
          loading={props.pending ?? false}
          onClick={() => props.onSubmit({ decision: 'approve' })}
        >
          批准原命令
        </Button>
      )}
    </Space>
  );
}
