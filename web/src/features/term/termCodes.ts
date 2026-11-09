// F1：终端/回放/RDP 前端错误码中文映射（CONTRACT.md 9.6）。
// FE2 的 CODE_MESSAGES（components/admin/apiError.ts）未覆盖二期交互会话
// 错误码，且该文件不归 F1；这里叠加一层 F1 映射，未命中时回落通用
// describeFailure（契约 9.6：未知 code 直接显示 message）。
// 三期（N3）：补 NETCLI_*（契约 9.6 错误码表）——终端页与设备命令执行
// 相关场景在此映射；服务器连接测试弹窗的映射见 servers/ServersPage。

import { describeFailure, type ApiFailure } from '../../components/admin/apiError';

const F1_CODE_MESSAGES: Record<string, string> = {
  TERM_SESSION_NOT_FOUND: '终端会话不存在',
  TERM_SESSION_EXISTS: '该服务器已有活动终端会话',
  TERM_SESSION_LIMIT: '活动终端会话数达到上限',
  TERM_CONNECT_FAILED: '终端连接建立失败',
  TERM_UNSUPPORTED: '该服务器不支持交互终端（WinRM 认证无 PTY；Telnet 接入的网络设备不支持终端）',
  TERM_WS_PROTOCOL: '终端 WebSocket 子协议不匹配',
  RECORDING_NOT_FOUND: '该会话没有录制文件',
  RECORDING_UNAVAILABLE: '录制文件缺失或不可读',
  RDP_SESSION_NOT_FOUND: 'RDP 会话不存在',
  RDP_SESSION_EXISTS: '该服务器已有活动 RDP 会话',
  RDP_SESSION_LIMIT: '活动 RDP 会话数达到上限',
  RDP_CONNECT_FAILED: 'RDP 网关连接失败',
  RDP_UNSUPPORTED: '仅 Windows 服务器支持 RDP',
  RDP_NO_CREDENTIALS: '服务器未保存可用的密码凭据',
  GUACD_UNAVAILABLE: 'RDP 网关（guacd）未配置或不可达',
  // 三期（N3，契约 9.6）。
  NETCLI_LOGIN_FAILED: '网络设备登录失败（凭据错误或提示符未出现）',
  NETCLI_EXEC_FAILED: '网络设备命令执行失败（expect 超时或通道断开）',
};

export function describeTermFailure(failure: ApiFailure): string {
  const localized = F1_CODE_MESSAGES[failure.code];
  if (localized) {
    const message = failure.message.trim();
    // 与 FE2 describeFailure 同规则：通用兜底 message 不重复拼接。
    return message && message.toLowerCase() !== failure.code.toLowerCase()
      ? `${localized}（${message}）`
      : localized;
  }
  return describeFailure(failure);
}

/** closed 帧 reason → 中文（契约 6.4 枚举）。 */
const CLOSED_REASON_TEXT: Record<string, string> = {
  client_closed: '客户端主动断开',
  idle_timeout: '会话空闲超时（10 分钟无活动）',
  max_duration: '会话达到最大时长（8 小时）',
  server_shutdown: '服务端关闭',
  error: '会话因错误结束',
};

export function describeClosedReason(reason: string): string {
  return CLOSED_REASON_TEXT[reason] ?? '连接已关闭';
}

/**
 * 「连接异常断开」提示的可见性（契约 6.4：WS 意外断开必须提示）。
 * 走查修复：TermLive 曾用 `!ended && abnormalClose === true` 判断，
 * 而 ended 的定义是 closedReason !== null || abnormalClose——
 * abnormalClose 为 true 时 ended 必为 true，条件恒假，提示永不显示
 * （后端崩溃/断网时页面只静默变「已结束」）。正确语义：WS 意外断开
 * （abnormalClose）且未收到服务端 closed 帧（closedReason 为 null）
 * 时提示；收到 closed 帧或用户主动断开的路径不提示。
 */
export function shouldShowAbnormalCloseHint(
  state: { closedReason: string | null; abnormalClose: boolean } | null,
): boolean {
  return state !== null && state.abnormalClose === true && state.closedReason === null;
}
