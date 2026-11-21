// 图表交互元素的键盘激活：Enter / Space 触发与鼠标点击相同的动作，
// Space 阻止页面滚动。SVG 图元本身可 tabIndex=0，辅助技术按 role/aria-label
// 读取（role="img" 只放在 svg 根上，交互子元素各自带 role="button"）。

import type { KeyboardEvent as ReactKeyboardEvent } from 'react';

export function activateOnKey(event: ReactKeyboardEvent<SVGElement>, action: () => void): void {
  if (event.key === 'Enter' || event.key === ' ' || event.key === 'Spacebar') {
    event.preventDefault();
    action();
  }
}
