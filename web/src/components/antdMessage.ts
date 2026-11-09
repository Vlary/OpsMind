import { message as staticMessage } from 'antd';
import type { App } from 'antd';

// antd v6 的静态 message 只读 globalConfig()，不消费组件树内
// ConfigProvider 的主题：在暗色应用（Cattpuccin Mocha，契约 8.3）里
// 直接 `import { message } from 'antd'` 会弹默认亮色白底 toast。
//
// 应用根部 AntdAppRoot 通过 App.useApp() 拿到主题化的 message 实例并
// bindMessage 注入；注入完成前（极早期调用或单测环境）回退静态实现。
type MessageInstance = ReturnType<typeof App.useApp>['message'];

let instance: MessageInstance = staticMessage as MessageInstance;

export function bindMessage(api: MessageInstance): void {
  instance = api;
}

/** 主题化 message：用法与 antd 静态 message 一致（success/error/warning…）。 */
export const message = {
  success: (...args: Parameters<MessageInstance['success']>) => instance.success(...args),
  error: (...args: Parameters<MessageInstance['error']>) => instance.error(...args),
  warning: (...args: Parameters<MessageInstance['warning']>) => instance.warning(...args),
  info: (...args: Parameters<MessageInstance['info']>) => instance.info(...args),
  loading: (...args: Parameters<MessageInstance['loading']>) => instance.loading(...args),
  open: (...args: Parameters<MessageInstance['open']>) => instance.open(...args),
  destroy: (...args: Parameters<MessageInstance['destroy']>) => instance.destroy(...args),
};
