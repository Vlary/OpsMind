import { SendOutlined, StopOutlined } from '@ant-design/icons';
import { Button, Flex, Input, Modal, Typography, type GetRef } from 'antd';
import { message } from '../../components/antdMessage';
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from 'react';

import { useComposerStore } from '../../stores/composerStore';

type TextAreaRef = GetRef<typeof Input.TextArea>;

export interface ComposerProps {
  conversationId: string;
  streaming: boolean;
  disabled?: boolean;
  disabledHint?: ReactNode;
  /** 文本长度上限；调用方按上下文前缀扣减后传入。 */
  maxLength?: number;
  /** 返回 true 表示消息已被接受，此时才清空草稿。 */
  onSend: (content: string) => boolean | Promise<boolean>;
  onStop: () => void;
  /** 存在 pending 审批时停止会作废审批（不可恢复），需要二次确认。 */
  stopRequiresConfirm?: boolean;
  placeholder?: string;
}

const DEFAULT_MAX_LENGTH = 16000;

export default function Composer({
  conversationId,
  streaming,
  disabled = false,
  disabledHint,
  maxLength = DEFAULT_MAX_LENGTH,
  onSend,
  onStop,
  stopRequiresConfirm = false,
  placeholder = '描述你的运维需求，或直接粘贴命令…',
}: ComposerProps) {
  const draft = useComposerStore((state) => state.drafts[conversationId] ?? '');
  const setDraft = useComposerStore((state) => state.setDraft);
  const clearDraft = useComposerStore((state) => state.clearDraft);
  const [confirmStop, setConfirmStop] = useState(false);
  const [sending, setSending] = useState(false);
  const inputRef = useRef<TextAreaRef>(null);

  // 挂载与会话切换后自动聚焦：登录跳转 /chat、侧栏切回对话、新建会话后
  // activeElement 一直是 BODY/菜单项，键盘用户每次都要先点一下输入框。
  // Composer 不随 /c/a → /c/b 切换 remount，故按 conversationId 驱动；
  // disabled 期间（供应商/会话加载中）跳过，就绪后一并聚焦。
  useEffect(() => {
    if (!disabled) {
      inputRef.current?.focus();
    }
  }, [conversationId, disabled]);

  const submit = useCallback(async () => {
    const content = draft.trim();
    if (!content || disabled || sending) {
      return;
    }
    if (streaming) {
      // 流式（含审批等待）期间输入框保持可打字以便预输入下一条指令，
      // 但不允许并发发起新一轮；草稿留在 composerStore，回答结束后再发送。
      message.warning('回答生成中，已输入的内容已保留，请回答结束后再发送。');
      return;
    }
    setSending(true);
    try {
      const accepted = await onSend(content);
      // 仅当调用方确认已接受（会话创建成功或流已启动）才清空，避免失败丢稿。
      if (accepted) {
        clearDraft(conversationId);
      }
    } finally {
      setSending(false);
    }
  }, [clearDraft, conversationId, disabled, draft, onSend, sending, streaming]);

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLTextAreaElement>) => {
      if (event.key !== 'Enter' || event.shiftKey) {
        return;
      }
      if (event.nativeEvent.isComposing) {
        return;
      }
      event.preventDefault();
      void submit();
    },
    [submit],
  );

  return (
    <div className="chat-composer">
      {disabled && disabledHint ? <div className="chat-composer-hint">{disabledHint}</div> : null}
      <Input.TextArea
        ref={inputRef}
        value={draft}
        onChange={(event) => setDraft(conversationId, event.target.value)}
        onKeyDown={handleKeyDown}
        placeholder={placeholder}
        autoSize={{ minRows: 2, maxRows: 10 }}
        maxLength={maxLength}
        showCount
        disabled={disabled}
        aria-label="消息输入框"
      />
      <Flex align="center" justify="space-between" style={{ marginTop: 8 }}>
        <Typography.Text type="secondary" className="chat-composer-tip">
          Enter 发送 · Shift+Enter 换行
        </Typography.Text>
        {streaming ? (
          <Button
            danger
            icon={<StopOutlined />}
            onClick={() => {
              // 停止是高频低风险操作（有重新生成与悬挂轮重发兜底），纯流式
              // 单击直停；仅在存在 pending 审批时二次确认——那会作废审批。
              if (stopRequiresConfirm) {
                setConfirmStop(true);
                return;
              }
              onStop();
            }}
          >
            停止
          </Button>
        ) : (
          <Button
            type="primary"
            icon={<SendOutlined />}
            disabled={disabled || draft.trim().length === 0}
            onClick={() => void submit()}
          >
            发送
          </Button>
        )}
      </Flex>

      <Modal
        open={confirmStop}
        title="停止当前回答？"
        okText="停止"
        cancelText="继续"
        okButtonProps={{ danger: true }}
        onOk={() => {
          setConfirmStop(false);
          onStop();
        }}
        onCancel={() => setConfirmStop(false)}
      >
        当前回答将停止，未处理的审批将失效。
      </Modal>
    </div>
  );
}
