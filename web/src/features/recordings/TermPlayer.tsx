// F1：终端录制回放播放器（xterm 只读渲染 + castParser/castScheduler）。
//
// - .cast 经 loadCastFromResponse 流式解析（不整块载入）；
// - 播放/暂停、1x/2x/4x 倍速、进度条拖拽（seek：清屏后同步快进重放）、
//   总时长显示（契约 8.7：进度条基于最大事件时间戳）；
// - 回放忠实还原录制尺寸（header width/height + r 事件），不 fit、不接输入；
// - 卸载：调度器 dispose、终端 dispose、下载中止（AbortController）。

import { CaretRightOutlined, PauseOutlined } from '@ant-design/icons';
import { Button, Segmented, Slider, Spin, Tag, Typography } from 'antd';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { terminalTheme } from '../../styles/palette';
import { useTheme } from '../../styles/themePreference';
import { formatClockSeconds } from '../term/format';
import { loadCastFromResponse, type CastParseResult } from './castParser';
import { CastScheduler } from './castScheduler';
import '../../styles/term.css';

interface TermPlayerProps {
  /** 下载地址：/api/term/recordings/{id}。 */
  src: string;
  title: string;
  onClose: () => void;
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'ready'; result: CastParseResult }
  | { phase: 'error'; message: string };

const SPEED_OPTIONS = [
  { label: '1x', value: 1 },
  { label: '2x', value: 2 },
  { label: '4x', value: 4 },
];

/** 浏览器内回放的 .cast 大小上限（64MB）：事件数组全量常驻内存的防护。 */
const MAX_CAST_BYTES = 64 * 1024 * 1024;

/** 进度条步长（秒）；时长不足一步的录制视为退化录制。 */
export const PLAYER_SLIDER_STEP_SECONDS = 0.1;

/**
 * 退化录制判定（p2 sprint 5）：时长小于进度条步长（0.1s）意味着全部
 * 事件集中在开头几毫秒（实测 429B/0:05 的短录制 maxTime≈0.002）——
 * step=0.1 下任何拖拽都只会提交 seek(0)，reset 后 t≤0 无事件可重放，
 * 屏幕永久空白（实测 nonEmpty 7→0 不恢复）。这类录制按空录制处理：
 * 时长显示「—」、禁用拖拽（播放仍可用，事件立即重放）。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function isDegenerateRecording(durationSeconds: number): boolean {
  return durationSeconds < PLAYER_SLIDER_STEP_SECONDS;
}

export default function TermPlayer({ src, title, onClose }: TermPlayerProps) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const schedulerRef = useRef<CastScheduler | null>(null);
  // 主题热更新需要终端实例（契约 8.3.6：只改 options.theme，不重建）。
  const terminalRef = useRef<Terminal | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const [load, setLoad] = useState<LoadState>({ phase: 'loading' });
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(1);
  const [position, setPosition] = useState(0);
  const [ended, setEnded] = useState(false);
  // 契约 8.3.6：终端色板随主题走；用 ref 供建终端时取初始值，mode 变化
  // 时只做 options.theme 热更新（重建终端会丢已重放的内容与进度）。
  const { mode } = useTheme();
  const modeRef = useRef(mode);
  // 更新当前模式供「建终端」effect 读取；ref 写入放到 effect 里（渲染期写
  // ref 会被 react-hooks/refs 判为错误，且并发渲染下不安全）。首帧的值由
  // useRef 的初始参数给出，建终端时读到的仍是最新值。
  useEffect(() => {
    modeRef.current = mode;
  }, [mode]);

  useEffect(() => {
    const abort = new AbortController();
    abortRef.current = abort;
    let cancelled = false;

    (async () => {
      try {
        const response = await fetch(src, { credentials: 'same-origin', signal: abort.signal });
        // 大小预检：.cast 事件数组全量常驻内存（设计权衡），超大录制直接
        // 打满标签页内存——给出明确上限提示并引导改走「下载 .cast」，
        // 而不是无提示地解析（评审发现）。
        const contentLength = Number(response.headers.get('Content-Length') ?? '0');
        if (contentLength > MAX_CAST_BYTES) {
          setLoad({
            phase: 'error',
            message: `录制文件过大（${(contentLength / 1024 / 1024).toFixed(1)}MB，上限 ${MAX_CAST_BYTES / 1024 / 1024}MB），请改用「下载 .cast」查看完整内容`,
          });
          return;
        }
        const result = await loadCastFromResponse(response, { signal: abort.signal });
        if (!cancelled) {
          setLoad({ phase: 'ready', result });
        }
      } catch (error) {
        if (cancelled || abort.signal.aborted) {
          return;
        }
        const message = error instanceof Error ? error.message : String(error);
        setLoad({ phase: 'error', message });
      }
    })();

    return () => {
      cancelled = true;
      abort.abort();
      abortRef.current = null;
    };
  }, [src]);

  const duration = load.phase === 'ready' ? load.result.maxTime : 0;
  // 时长不足进度条一步（含 0）：拖拽无意义且 seek(0) 会把屏幕永久拖空，
  // 视为空录制——时长显示「—」、禁用拖拽（见 isDegenerateRecording）。
  const degenerate = isDegenerateRecording(duration);

  // 调度器在 result 与终端就绪后创建；position/playing 状态经 onTick 回推。
  useEffect(() => {
    if (load.phase !== 'ready') {
      return undefined;
    }
    const viewport = viewportRef.current;
    const header = load.result.header;
    if (!viewport) {
      return undefined;
    }
    const terminal = new Terminal({
      fontSize: 13,
      fontFamily: "'JetBrains Mono','Fira Code',monospace",
      // 回放只读：禁用光标闪烁与输入指示。
      cursorBlink: false,
      disableStdin: true,
      cols: header?.width ?? 120,
      rows: header?.height ?? 40,
      theme: terminalTheme(modeRef.current),
    });
    terminal.open(viewport);
    terminalRef.current = terminal;

    const scheduler = new CastScheduler({
      events: load.result.events,
      sink: {
        write: (data) => terminal.write(data),
        resize: (cols, rows) => terminal.resize(cols, rows),
        reset: () => terminal.reset(),
      },
      onTick: (current, isEnded) => {
        setPosition(current);
        setEnded(isEnded);
        setPlaying(schedulerRef.current?.isPlaying ?? false);
      },
    });
    schedulerRef.current = scheduler;

    return () => {
      scheduler.dispose();
      terminal.dispose();
      terminalRef.current = null;
      schedulerRef.current = null;
    };
  }, [load]);

  // 契约 8.3.6 / 8.3.15：切换主题只改 xterm 的 options.theme，不重建终端。
  // 回放终端无 PTY，但重建会清屏并让播放进度与画面不一致，故同样只热更新。
  useEffect(() => {
    const terminal = terminalRef.current;
    if (!terminal) {
      return;
    }
    terminal.options.theme = terminalTheme(mode);
  }, [mode]);

  const handleToggle = useCallback(() => {
    const scheduler = schedulerRef.current;
    if (!scheduler) {
      return;
    }
    if (scheduler.isPlaying) {
      scheduler.pause();
      setPlaying(false);
      return;
    }
    if (scheduler.currentPosition >= scheduler.duration) {
      // 播完后再点播放：从头重放。
      scheduler.seek(0);
      setEnded(false);
    }
    scheduler.play();
    setPlaying(true);
  }, []);

  const handleSeek = useCallback((value: number | null) => {
    const scheduler = schedulerRef.current;
    if (!scheduler || value === null) {
      return;
    }
    scheduler.seek(value);
    setPosition(value);
    setEnded(scheduler.currentPosition >= scheduler.duration);
    setPlaying(scheduler.isPlaying);
  }, []);

  const handleSpeed = useCallback((value: number | string) => {
    const scheduler = schedulerRef.current;
    const next = Number(value);
    setSpeed(next);
    scheduler?.setSpeed(next);
  }, []);

  const headerLabel = useMemo(() => {
    if (load.phase !== 'ready' || !load.result.header) {
      return null;
    }
    const { width, height } = load.result.header;
    return `${width}×${height}`;
  }, [load]);

  return (
    <div className="opsmind-player-shell" aria-label="终端回放">
      <div className="opsmind-term-header">
        <div className="opsmind-term-header-meta">
          <Typography.Text strong>{title}</Typography.Text>
          {headerLabel ? <Tag>录制尺寸 {headerLabel}</Tag> : null}
          {load.phase === 'ready' && load.result.badLines > 0 ? (
            <Tag color="warning">{load.result.badLines} 行无法解析</Tag>
          ) : null}
          <Button type="link" size="small" onClick={onClose}>
            关闭回放
          </Button>
        </div>
      </div>

      {load.phase === 'loading' ? (
        <div className="opsmind-term-status" role="status">
          <Spin size="small" /> 正在加载录制文件…
        </div>
      ) : null}
      {load.phase === 'error' ? (
        <div className="opsmind-term-status opsmind-term-status--alert" role="alert">
          录制加载失败：{load.message}
        </div>
      ) : null}

      <div ref={viewportRef} className="opsmind-player-viewport" data-testid="player-viewport" />

      {load.phase === 'ready' ? (
        <div className="opsmind-player-controls">
          <Button
            type="primary"
            shape="circle"
            icon={playing ? <PauseOutlined /> : <CaretRightOutlined />}
            onClick={handleToggle}
            aria-label={playing ? '暂停' : '播放'}
          />
          <Segmented options={SPEED_OPTIONS} value={speed} onChange={handleSpeed} />
          <Slider
            className="opsmind-player-slider"
            min={0}
            max={Math.max(duration, 0.001)}
            step={0.1}
            value={Math.min(position, duration)}
            /* 拖拽期间 onChange 只更新进度显示（轻量 setPosition）；
               松手/键盘提交后才 onChangeComplete 执行真正的 seek——
               seek 是 reset + O(n) 同步快进重放，拖拽的每个中间值都
               触发会让大 .cast 反复全量重放卡死主线程（评审发现）。 */
            onChange={(value) => {
              if (value !== null) {
                setPosition(value);
              }
            }}
            onChangeComplete={handleSeek}
            disabled={degenerate}
            tooltip={{ formatter: (value) => formatClockSeconds(value ?? 0) }}
          />
          <span className="opsmind-player-time">
            {degenerate ? '—' : `${formatClockSeconds(position)} / ${formatClockSeconds(duration)}`}
          </span>
          {ended && !playing ? <Tag>播放完毕</Tag> : null}
        </div>
      ) : null}
    </div>
  );
}
