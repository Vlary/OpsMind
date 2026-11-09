// F1：实时终端视图（xterm.js + fit + 终端 WS）。
//
// 生命周期（CONTRACT.md 6.4 / 8.7）：
// - 挂载即 openTermSocket（子协议 opsmind-term-v1）并渲染 xterm；
// - ResizeObserver 驱动 fit，尺寸变化经 controller 节流上报 resize 帧；
// - 收到服务端 closed 帧后提示原因并引导「新建会话」，不自动重连；
// - WS 意外断开（未收 closed）提示异常断开；
// - 卸载时 WS / xterm / fit / ResizeObserver / 时长定时器全量 dispose。

import { DisconnectOutlined, ReloadOutlined } from '@ant-design/icons';
import { Button, Space, Tag, Typography } from 'antd';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { useCallback, useEffect, useRef, useState } from 'react';
import type { components } from '../../api/schema';
import { openTermSocket } from '../../api/ws';
import CopyButton from '../../components/CopyButton';
import { terminalTheme } from '../../styles/palette';
import { useTheme } from '../../styles/themePreference';
import { TermSessionController, type TermSessionState } from './termSession';
import { describeClosedReason, shouldShowAbnormalCloseHint } from './termCodes';
import { formatElapsed } from './format';
// FE-P2（六期，契约 8.7 ⑩）：当前输入行命令复制。
import { cursorLineText, type TerminalBufferLike } from './termLine';

type TerminalSession = components['schemas']['TerminalSession'];

interface TermLiveProps {
  session: TerminalSession;
  /** 用户点击「新建会话」（closed/断开后重建）。 */
  onNewSession: () => void;
  /** 会话终止（closed 帧或异常断开）：父级乐观置该行已结束并延迟失效列表。 */
  onSessionEnded: () => void;
}

export default function TermLive({ session, onNewSession, onSessionEnded }: TermLiveProps) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const controllerRef = useRef<TermSessionController | null>(null);
  // FE-P2（六期）：当前命令复制在点击时读光标行，terminal 实例经 ref 暴露。
  const terminalRef = useRef<Terminal | null>(null);
  const endedNotifiedRef = useRef(false);
  const endedAtRef = useRef<number | null>(null);
  const [state, setState] = useState<TermSessionState | null>(null);
  const [elapsedNow, setElapsedNow] = useState(() => Date.now());
  // 契约 8.3.6：xterm 只接受具体色值，取 palette.ts 的两套终端色板。
  // 用 ref 暂存当前模式，避免把 mode 塞进「建终端」effect 的依赖里——
  // 切换主题必须只改 options.theme 热更新，销毁重建会丢 PTY 尺寸与输入。
  const { mode } = useTheme();
  const modeRef = useRef(mode);
  // 更新当前模式供「建终端」effect 读取；ref 写入放到 effect 里（渲染期写
  // ref 会被 react-hooks/refs 判为错误，且并发渲染下不安全）。首帧的值由
  // useRef 的初始参数给出，建终端时读到的仍是最新值。
  useEffect(() => {
    modeRef.current = mode;
  }, [mode]);

  // 控制器与 xterm 的状态桥：controller 构造时可能同步回调（subscribe 内
  // 首次推送），此时 React 尚未提交——用 ref 暂存，useEffect 内统一同步。
  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return undefined;
    }

    const terminal = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: "'JetBrains Mono','Fira Code',monospace",
      // 契约 8.3.6：16 色 ANSI 全量显式给出（不得依赖 xterm 默认调色板），
      // 两主题各一套由 palette.ts 的 terminalTheme(mode) 提供。
      theme: terminalTheme(modeRef.current),
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(viewport);
    terminalRef.current = terminal;

    const controller = new TermSessionController({
      terminal,
      connect: (handlers) => openTermSocket(session.id, handlers),
    });
    controllerRef.current = controller;
    const unsubscribe = controller.subscribe(setState);

    // 初始 fit：按容器实际尺寸对齐一次并上报（会话创建默认 120x40，
    // 这里把真实视口尺寸同步给服务端 PTY）。
    const applyFit = () => {
      try {
        fitAddon.fit();
      } catch {
        return; // 容器不可见（如抽屉动画中）时 fit 抛错，忽略等待下一帧
      }
      controller.reportResize(terminal.cols, terminal.rows);
    };
    applyFit();

    const observer = new ResizeObserver(() => applyFit());
    observer.observe(viewport);

    // 粘贴：xterm 原生处理浏览器 paste（Ctrl/Cmd+V → onData）；这里补
    // 终端惯例的 Ctrl+Shift+V 与右键粘贴（读剪贴板为文本发送）。
    const pasteFromClipboard = async () => {
      try {
        const text = await navigator.clipboard.readText();
        if (text) {
          controller.sendInput(text);
        }
      } catch {
        // 剪贴板权限被拒（非安全上下文/未授权）：静默，用户仍可 Ctrl+V。
      }
    };
    terminal.attachCustomKeyEventHandler((event) => {
      if (
        event.type === 'keydown' &&
        event.ctrlKey &&
        event.shiftKey &&
        event.key.toLowerCase() === 'v'
      ) {
        void pasteFromClipboard();
        return false;
      }
      return true;
    });
    const handleContextMenu = (event: MouseEvent) => {
      event.preventDefault();
      void pasteFromClipboard();
    };
    viewport.addEventListener('contextmenu', handleContextMenu);

    // 页头会话时长：1s 心跳更新（契约：页头显示会话时长）；会话结束后
    // 停止心跳——时长显示固定在实际结束时刻。
    const elapsedTimer = window.setInterval(() => {
      if (endedNotifiedRef.current) {
        return;
      }
      setElapsedNow(Date.now());
    }, 1000);

    return () => {
      window.clearInterval(elapsedTimer);
      observer.disconnect();
      viewport.removeEventListener('contextmenu', handleContextMenu);
      unsubscribe();
      controller.dispose();
      fitAddon.dispose();
      terminal.dispose();
      terminalRef.current = null;
      controllerRef.current = null;
    };
    // 会话 ID 唯一确定一条连接：父级用 key=<session.id> 保证重开时重挂载。
  }, [session.id]);

  // 契约 8.3.6 / 8.3.15：主题切换只改 options.theme（实测即时生效），
  // 不销毁重建终端——会话、PTY 尺寸与未提交输入都不能丢。
  useEffect(() => {
    const terminal = terminalRef.current;
    if (!terminal) {
      return;
    }
    terminal.options.theme = terminalTheme(mode);
  }, [mode]);

  // 会话终止通知（closed 帧或异常断开）：结束回调只发一次；并记录结束
  // 时刻，页头时长停在结束点而非继续增长（评审发现）。
  useEffect(() => {
    if (!state) {
      return;
    }
    const ended = state.closedReason !== null || state.abnormalClose;
    if (ended && !endedNotifiedRef.current) {
      endedNotifiedRef.current = true;
      endedAtRef.current = Date.now();
      setElapsedNow(endedAtRef.current);
      onSessionEnded();
    }
  }, [state, onSessionEnded]);

  const handleDisconnect = useCallback(() => {
    // 手动断开也置一次性标记：closeByUser 同步置 closedReason，随后的
    // state effect 不再重复 onSessionEnded（评审发现：列表失效双发）。
    endedNotifiedRef.current = true;
    controllerRef.current?.closeByUser();
    endedAtRef.current = Date.now();
    onSessionEnded();
  }, [onSessionEnded]);

  // FE-P2（六期，契约 8.7 ⑩）：点击时读 xterm 光标行（当前输入回显命令）；
  // 选区复制由 xterm 自带（不重复做）。
  const currentLineText = useCallback((): string => {
    const terminal = terminalRef.current;
    if (!terminal) {
      return '';
    }
    return cursorLineText(terminal.buffer.active as unknown as TerminalBufferLike);
  }, []);

  const ended = state !== null && (state.closedReason !== null || state.abnormalClose === true);
  const showClosedHint = state !== null && state.closedReason !== null;
  const showErrorFrame = state !== null && state.errorMessage !== null;

  return (
    <div className="opsmind-term-shell" aria-label="交互终端">
      <div className="opsmind-term-header">
        <div className="opsmind-term-header-meta">
          <Typography.Text strong>{session.server_name || '终端会话'}</Typography.Text>
          {ended ? <Tag>已结束</Tag> : <Tag color="green">● 录制中</Tag>}
          <span className="opsmind-term-header-meta-code">
            时长 {formatElapsed(session.started_at, elapsedNow)}
          </span>
          <span className="opsmind-term-header-meta-code">
            {/* 三期（N3）：os=network 的会话是网络设备 CLI 终端（SSH 通道）。 */}
            {session.os === 'network'
              ? '网络设备 CLI'
              : session.os === 'windows'
                ? 'PowerShell'
                : 'SSH'}
          </span>
        </div>
        <Space>
          {/* 契约 8.7 ⑩（六期打磨 sprint 1）：当前输入行复制始终存在——
              此前仅 !ended 时渲染，会话建立失败/已结束（dashboard 首屏
              即 TERM_CONNECT_FAILED）时走 ended 分支，按钮整个消失（实机
              开终端后 getByLabel('复制当前命令') 计数 = 0）。保留按钮后，
              已结束时点击命中「没有可复制的内容」提示（CopyButton 的
              延迟求值空值分支），不误导也不丢入口。 */}
          <CopyButton text={currentLineText} label="复制当前命令" />
          {ended ? (
            <Button type="primary" icon={<ReloadOutlined />} onClick={onNewSession}>
              新建会话
            </Button>
          ) : (
            <Button danger icon={<DisconnectOutlined />} onClick={handleDisconnect}>
              断开
            </Button>
          )}
        </Space>
      </div>

      {showErrorFrame ? (
        <div className="opsmind-term-status opsmind-term-status--alert" role="alert">
          服务端错误 {state?.errorCode ?? ''}：{state?.errorMessage ?? ''}
        </div>
      ) : null}
      {showClosedHint ? (
        <div className="opsmind-term-status opsmind-term-status--alert" role="status">
          会话已结束：{describeClosedReason(state?.closedReason ?? '')}
          <span>录制将出现在「回放」页；如需继续操作请新建会话。</span>
        </div>
      ) : null}
      {shouldShowAbnormalCloseHint(state) ? (
        <div className="opsmind-term-status opsmind-term-status--alert" role="status">
          连接异常断开（服务端未发送结束原因）。录制已封盘；如需继续操作请新建会话。
        </div>
      ) : null}
      {!ended && state?.socket !== 'open' ? (
        <div className="opsmind-term-status" role="status">
          正在建立终端连接…
        </div>
      ) : null}

      <div
        ref={viewportRef}
        className="opsmind-term-viewport"
        data-testid="term-viewport"
        aria-label="终端输出"
      />
    </div>
  );
}
