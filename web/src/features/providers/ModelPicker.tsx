import { AutoComplete, Space, Typography } from 'antd';
import type { ReactNode } from 'react';

import CopyButton from '../../components/CopyButton';

interface ModelPickerProps {
  value?: string;
  onChange?: (value: string) => void;
  options: string[];
  placeholder?: string;
  disabled?: boolean;
}

/**
 * 下拉项内容：模型名 + 复制按钮。
 *
 * 复制按钮上的 mousedown/click 必须 stopPropagation——否则 antd 的选项
 * 点击会把「复制」当成选中该模型（下拉关闭、值被改写）。CopyButton 内部
 * 的异步剪贴板写入不受影响（事件只在 DOM 冒泡层拦截）。
 */
function ModelOption({ model }: { model: string }): ReactNode {
  return (
    <Space
      size={6}
      style={{ width: '100%', justifyContent: 'space-between' }}
      onMouseDown={(event) => event.stopPropagation()}
      onClick={(event) => event.stopPropagation()}
    >
      <Typography.Text ellipsis style={{ maxWidth: 360 }}>
        {model}
      </Typography.Text>
      <CopyButton text={model} label="复制模型名" />
    </Space>
  );
}

// 默认模型选择器：可选已拉取的模型，也允许手动输入。
//
// 六期打磨 sprint 1（契约 8.7 ③「模型名（下拉选项与详情）」）：下拉选项
// 与当前值回显都带模型名复制——模型名常被粘到别处（.env、其他工具配置），
// 此前只有列表页 default_model 一处可复制，下拉里选完即无处可取。
//
// 实现说明：antd AutoComplete 的 options 渲染走 `label`（不像 Select 那样
// 支持 optionRender——实测 optionRender 在 AutoComplete 上不生效，e2e 断言
// 下拉里找不到按钮），故把 label 直接写成 ReactNode；过滤仍按 option.value
// （filterOption 读 value，不受 label 结构影响）。
export default function ModelPicker({
  value,
  onChange,
  options,
  placeholder = '选择或输入模型名',
  disabled = false,
}: ModelPickerProps) {
  const modelNames = [...new Set(options)];
  return (
    <Space.Compact style={{ width: '100%', alignItems: 'center' }} block>
      <AutoComplete
        style={{ flex: 1 }}
        value={value ?? null}
        options={modelNames.map((model) => ({
          value: model,
          label: <ModelOption model={model} />,
        }))}
        onChange={(next) => onChange?.(next ?? '')}
        placeholder={placeholder}
        disabled={disabled}
        allowClear
        filterOption={(input, option) =>
          String(option?.value ?? '')
            .toLowerCase()
            .includes(input.toLowerCase())
        }
      />
      {/* 回显详情：当前选中的模型名（含手输值）可直接复制。 */}
      {value ? <CopyButton text={value} label="复制当前模型名" /> : null}
    </Space.Compact>
  );
}
