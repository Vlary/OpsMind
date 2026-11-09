import { describe, expect, it } from 'vitest';

// p2 sprint 5 走查缺陷锁定：建会话后立即断开的短录制（cast 事件全部在
// 开头几毫秒，maxTime≈0.002）在播放器里显示「0:00 / 0:00」且进度条可拖
// ——duration≠0 所以 Slider 不禁用，step=0.1 下任何拖拽都提交 seek(0)，
// reset 后 t≤0 无事件可重放，屏幕永久空白（实测 nonEmpty 7→0 不恢复）。
// 判定逻辑抽为纯函数 isDegenerateRecording：时长小于步长即视为空录制
// （时长显示「—」、禁用拖拽）。
import { isDegenerateRecording, PLAYER_SLIDER_STEP_SECONDS } from '../TermPlayer';

describe('isDegenerateRecording（短录制视为空录制）', () => {
  it('时长小于进度条步长（0.1s）视为退化录制', () => {
    expect(PLAYER_SLIDER_STEP_SECONDS).toBe(0.1);
    expect(isDegenerateRecording(0.002)).toBe(true); // 走查实测形态（429B/0:05）
    expect(isDegenerateRecording(0.09)).toBe(true);
  });

  it('时长为 0（无事件的空录制）同样视为退化（禁用拖拽的既有语义合并）', () => {
    expect(isDegenerateRecording(0)).toBe(true);
  });

  it('正常录制（≥0.1s）不视为退化', () => {
    expect(isDegenerateRecording(0.1)).toBe(false);
    expect(isDegenerateRecording(10.1)).toBe(false);
  });
});
