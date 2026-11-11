import { useId } from 'react';

/**
 * 枢弧品牌 mark（内联 SVG）。
 *
 * 为什么必须内联：`<img src>` 与 favicon 是独立文档，不会继承宿主 CSS 的
 * `color`；只有内联 SVG 的 `currentColor` 才能跟随 tokens.css 的
 * `--color-primary`（浅/深两套主题各有一个取值）。需要 `<img>` 时用
 * public/logo-mark.svg（浅色固定）或 public/logo-mark-dark.svg（深色固定）。
 *
 * 描边档位：全套资产统一轴 8 / 弧 6 / hub r=8（轴宽 2×）。弧不再分 5/6
 * 两档——5 档与 favicon、横版、文档相差 20%，也和 logo.svg 的“定稿几何
 * 轴 8 / 弧 6”矛盾。16px 的弧可读性由 favicon-16.svg 的 crispEdges 像素
 * 吸附解决，不靠改描边宽度。hub r=8：16px 中心行把 x4/x7 推到约 227/255，
 * 非中心行收窄，读成点；r=6 时 x4/x7 只有 92/255（36% 边），会读成加粗轴。
 * 不要把轴降到 5，也不要把弧删成闭合方框——闭合后方框在 16px 会读成
 * Layout/Sidebar 类 UI 图标。
 *
 * 光学补偿：mark 的 alpha 质心天然偏左（轴 + hub 的质量集中在 x=24，弧很轻）。
 * 这里把几何整体 translateX(4)：20px 下 +1.25px、24px 下 +1.5px、32px 下
 * +2px，质心回到容器中线，与 logo-mark.svg / favicon 的 x=28 同源。
 * 不要再在 CSS 里叠加 translateX，否则会重复补偿。
 */
export interface BrandMarkProps {
  /** 渲染边长（px）。侧栏 20、登录页 24、独立底座 40。 */
  size?: number;
  /** 与字标并排时置 true：对读屏隐藏，避免与字标重复播报。 */
  decorative?: boolean;
  className?: string;
}

export default function BrandMark({ size = 24, decorative = false, className }: BrandMarkProps) {
  const titleId = useId();
  const a11y = decorative
    ? { 'aria-hidden': true as const }
    : { role: 'img' as const, 'aria-labelledby': titleId };

  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width={size}
      height={size}
      viewBox="0 0 64 64"
      focusable="false"
      className={className}
      {...a11y}
    >
      {decorative ? null : <title id={titleId}>云枢 OpsMind</title>}
      <g transform="translate(4 0)">
        <g fill="none" stroke="currentColor" strokeLinecap="round">
          <path d="M24 8V56" strokeWidth={8} />
          <path d="M36 16A20 20 0 0 1 36 48" strokeWidth={6} />
        </g>
        <circle cx="24" cy="32" r="8" fill="currentColor" />
      </g>
    </svg>
  );
}
