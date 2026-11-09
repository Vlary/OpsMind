// 剪贴板写入的小工具：优先使用异步 Clipboard API，仅在非安全上下文
// （如 http://内网IP 部署）navigator.clipboard 不可用时降级为
// textarea + execCommand。返回是否复制成功，由调用方负责提示。
//
// 六期（FE-P2）拆出两级单独导出：CopyButton 需要区分「clipboard 成功」
// 「execCommand 降级成功」与「两级全失败（可选中文本降级）」三种终态
// （契约 8.2/8.7）；copyText 保持原有组合语义，既有调用方（CodeBlock
// 等）不变。

/** 第一级：异步 Clipboard API（仅安全上下文可用）。 */
export async function copyViaClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 权限被拒或文档失焦：继续尝试降级方案。
    }
  }
  return false;
}

/** 第二级：textarea + execCommand 降级。 */
export function copyViaExecCommand(text: string): boolean {
  try {
    const textarea = document.createElement('textarea');
    textarea.value = text;
    textarea.setAttribute('readonly', '');
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    document.body.appendChild(textarea);
    textarea.select();
    const ok = document.execCommand('copy');
    textarea.remove();
    return ok;
  } catch {
    return false;
  }
}

export async function copyText(text: string): Promise<boolean> {
  if (await copyViaClipboard(text)) {
    return true;
  }
  return copyViaExecCommand(text);
}
