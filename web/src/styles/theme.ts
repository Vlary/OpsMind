import { theme, type ThemeConfig } from 'antd';

/**
 * 生效主题（已解析的 light / dark）。用户的**偏好**是三态
 * `light | dark | system`，见 `styles/themePreference.ts`。
 */
export type ThemeMode = 'light' | 'dark';

/** 契约 8.3.8 的命名形态（`themeConfig(mode)` 的入参类型别名）。 */
export type themeMode = ThemeMode;

/**
 * 语义令牌的 TS 侧唯一副本（CONTRACT.md 8.3.2 / 8.3.3 / 8.3.4 / 8.3.5 /
 * 8.3.7）。键名与 `tokens.css` 的 CSS 变量**去掉 `--` 前缀后逐字相同**，
 * 由 `styles/themeTokens.test.ts` 断言两者一致——改一处漏另一处会立刻红。
 *
 * 为什么此处必须是具体色值、不能写 `var(--color-primary)`：antd 的
 * `getDesignToken` 无法解析 CSS 变量，会返回 `#000000`，派生色随之崩坏
 * （8.3.15 实测）。
 */
export const SEMANTIC_TOKENS = {
  light: {
    'color-bg-base': '#f0f3f8',
    'color-bg-container': '#ffffff',
    'color-bg-elevated': '#ffffff',
    'color-bg-shell': '#ffffff',
    'color-bg-subtle': '#e9edf4',
    'color-bg-hover': '#e9eef7',
    'color-bg-active': '#e4e9f2',
    'color-bg-selected': '#e3edfd',
    'color-bg-code': '#f6f8fa',

    'color-text': '#1f2430',
    'color-text-secondary': '#4a5163',
    'color-text-tertiary': '#5b6373',
    'color-text-placeholder': '#5b6373',
    'color-text-disabled': '#767e91',
    'color-text-inverse': '#ffffff',

    'color-primary': '#1a56c4',
    'color-primary-hover': '#14479f',
    'color-primary-active': '#0d3b9e',
    'color-info': '#0b6e7f',
    'color-success': '#146c2e',
    'color-warning': '#8a5a00',
    'color-danger': '#c0272d',
    'color-accent': '#6d28d9',
    'color-focus-ring': '#1a56c4',

    'color-link': '#0b6e7f',
    'color-link-hover': '#0a6373',
    'color-link-active': '#08505c',

    'color-border-strong': '#767e91',
    'color-border': '#c8d0dd',
    'color-border-subtle': '#e1e7f0',

    'color-primary-soft': '#e8effc',
    'color-info-soft': '#e8fafd',
    'color-success-soft': '#e9fbef',
    'color-warning-soft': '#fff6e5',
    'color-danger-soft': '#fbeaea',
    'color-accent-soft': '#f0e9fb',

    'color-chart-1': '#1a56c4',
    'color-chart-2': '#146c2e',
    'color-chart-3': '#8a5a00',
    'color-chart-4': '#9d174d',
    'color-chart-5': '#0b6e7f',
    'color-chart-6': '#6d28d9',

    'color-code-keyword': '#6d28d9',
    'color-code-string': '#146c2e',
    'color-code-number': '#a35200',
    'color-code-comment': '#5b6373',
    'color-code-function': '#1a56c4',
    'color-code-variable': '#8a5a00',
  },
  dark: {
    'color-bg-base': '#1e1e2e',
    'color-bg-container': '#1e1e2e',
    'color-bg-elevated': '#313244',
    'color-bg-shell': '#181825',
    'color-bg-subtle': '#313244',
    'color-bg-hover': '#313244',
    'color-bg-active': '#383b4d',
    'color-bg-selected': '#383b4d',
    'color-bg-code': '#11111b',

    'color-text': '#cdd6f4',
    'color-text-secondary': '#bac2de',
    'color-text-tertiary': '#a6adc8',
    'color-text-placeholder': '#a6adc8',
    'color-text-disabled': '#8b90a6',
    'color-text-inverse': '#1e1e2e',

    'color-primary': '#89b4fa',
    'color-primary-hover': '#a8c9fc',
    'color-primary-active': '#82adf2',
    'color-info': '#74c7ec',
    'color-success': '#a6e3a1',
    'color-warning': '#f9e2af',
    'color-danger': '#f38ba8',
    'color-accent': '#cba6f7',
    'color-focus-ring': '#89b4fa',

    'color-link': '#74c7ec',
    'color-link-hover': '#9fd8f2',
    'color-link-active': '#5aa6c6',

    'color-border-strong': '#9399b2',
    'color-border': '#45475a',
    'color-border-subtle': '#313244',

    'color-primary-soft': '#222c3f',
    'color-info-soft': '#1f3038',
    'color-success-soft': '#243027',
    'color-warning-soft': '#38331f',
    'color-danger-soft': '#3a232c',
    'color-accent-soft': '#2e2740',

    'color-chart-1': '#89b4fa',
    'color-chart-2': '#a6e3a1',
    'color-chart-3': '#f9e2af',
    'color-chart-4': '#fab387',
    'color-chart-5': '#74c7ec',
    'color-chart-6': '#cba6f7',

    'color-code-keyword': '#cba6f7',
    'color-code-string': '#a6e3a1',
    'color-code-number': '#fab387',
    'color-code-comment': '#9399b2',
    'color-code-function': '#89b4fa',
    'color-code-variable': '#f9e2af',
  },
} as const;

/** 阴影/遮罩等非纯色令牌（`--color-shadow` 是完整 box-shadow 值，不入 antd）。 */
export const EFFECT_TOKENS = {
  light: {
    'color-shadow': '0 4px 16px rgb(15 23 42 / 12%)',
    'color-scrim': 'rgb(15 23 42 / 45%)',
    'color-overlay-media-bg': 'rgb(15 23 42 / 72%)',
    'color-overlay-media-text': '#f8fafc',
    'color-primary-glow': 'rgb(26 86 196 / 12%)',
    'color-accent-glow': 'rgb(109 40 217 / 10%)',
  },
  dark: {
    'color-shadow': '0 4px 16px rgb(0 0 0 / 35%)',
    'color-scrim': 'rgb(0 0 0 / 45%)',
    'color-overlay-media-bg': 'rgb(15 23 42 / 72%)',
    'color-overlay-media-text': '#f8fafc',
    'color-primary-glow': 'rgb(137 180 250 / 14%)',
    'color-accent-glow': 'rgb(203 166 247 / 10%)',
  },
} as const;

/** 字体/形状沿用现行取值（8.3.8 末段）。 */
const SHAPE = {
  borderRadius: 8,
  controlHeight: 36,
  fontFamily: "'Inter','Noto Sans CJK SC','Microsoft YaHei',sans-serif",
  fontFamilyCode: "'JetBrains Mono','Fira Code',monospace",
} as const;

/**
 * CONTRACT.md 8.3.8：`themeConfig(mode)` 是全站唯一的 antd `ThemeConfig` 来源。
 *
 * 两条硬规则（均为契约记录的本机实测结论）：
 * 1. **两个主题都用 `theme.defaultAlgorithm`**。`theme.darkAlgorithm` 会把
 *    seed 重映射（主色 `#89b4fa` → `#779cd8`，登录页主按钮白字只有
 *    2.79:1），深色也必须 defaultAlgorithm + 全量钉死。
 * 2. **每个键显式给出**。算法不会覆盖显式给出的 map token，但未钉的
 *    `colorPrimaryHover/Active` 会被推导（深色 `#6a8fd4` 上白字 3.23:1）。
 *
 * 组件级 token 里 `Menu` 的 `darkItem*` 必须逐键钉死：antd 默认
 * `darkItemColor = colorTextLightSolid @ 65% alpha`，深色下会合成
 * `#1c1c2b`（侧栏 `#181825` 上 1.05:1，文字几乎不可见）。
 */
export function themeConfig(mode: ThemeMode): ThemeConfig {
  const c = SEMANTIC_TOKENS[mode];
  const isDark = mode === 'dark';

  // 深色 Tooltip 底：antd 的 Tooltip 没有独立背景 token（用 colorBgSpotlight
  // 作背景、colorTextLightSolid 作文字）。深色下 colorTextLightSolid 必须是
  // `#1e1e2e`（实心按钮需要），若 colorBgSpotlight 仍取深色浮层色，Tooltip
  // 文字对比度只有 1.30:1；改浅底后 13.51:1。
  const spotlight = isDark ? '#e6e9f2' : '#313244';

  return {
    algorithm: theme.defaultAlgorithm,
    token: {
      // ---- 语义色 ----
      colorPrimary: c['color-primary'],
      colorPrimaryHover: c['color-primary-hover'],
      colorPrimaryActive: c['color-primary-active'],
      colorInfo: c['color-info'],
      colorSuccess: c['color-success'],
      colorWarning: c['color-warning'],
      colorError: c['color-danger'],

      // ---- 链接 ----
      colorLink: c['color-link'],
      colorLinkHover: c['color-link-hover'],
      colorLinkActive: c['color-link-active'],

      // ---- 背景 ----
      colorBgBase: c['color-bg-base'],
      colorBgLayout: c['color-bg-base'],
      colorBgContainer: c['color-bg-container'],
      colorBgElevated: c['color-bg-elevated'],
      colorBgSpotlight: spotlight,
      colorBgMask: isDark ? 'rgba(0,0,0,.45)' : 'rgba(15,23,42,.45)',
      colorBgContainerDisabled: isDark ? 'rgba(255,255,255,.06)' : 'rgba(0,0,0,.04)',

      // ---- 文本 ----
      colorText: c['color-text'],
      colorTextSecondary: c['color-text-secondary'],
      colorTextTertiary: c['color-text-tertiary'],
      colorTextQuaternary: c['color-text-tertiary'],
      colorTextPlaceholder: c['color-text-placeholder'],
      colorTextDisabled: c['color-text-disabled'],
      colorTextLightSolid: c['color-text-inverse'],
      colorTextHeading: c['color-text'],
      colorTextLabel: c['color-text-secondary'],
      colorTextDescription: c['color-text-tertiary'],

      // ---- 边框 ----
      colorBorder: c['color-border'],
      colorBorderSecondary: c['color-border-subtle'],
      colorBorderDisabled: c['color-border-subtle'],

      // ---- 填充 ----
      colorFill: isDark ? 'rgba(255,255,255,.12)' : 'rgba(0,0,0,.15)',
      colorFillSecondary: isDark ? 'rgba(255,255,255,.08)' : 'rgba(0,0,0,.06)',
      colorFillTertiary: isDark ? 'rgba(255,255,255,.06)' : 'rgba(0,0,0,.04)',
      colorFillQuaternary: isDark ? 'rgba(255,255,255,.03)' : 'rgba(0,0,0,.02)',
      colorFillAlter: isDark ? 'rgba(255,255,255,.02)' : 'rgba(0,0,0,.02)',
      colorFillContent: isDark ? 'rgba(255,255,255,.06)' : 'rgba(0,0,0,.06)',
      colorSplit: isDark ? 'rgba(203,212,246,.11)' : 'rgba(5,55,130,.12)',

      // ---- 受选项/文本按钮底 ----
      controlItemBgActive: c['color-bg-selected'],
      // 深色 activeHover 与 active 同值：任何提亮都会把三级文本与危险色压到
      // 4.5 以下，选中项悬停反馈改为「底色不变 + 文字保持高对比」。
      controlItemBgActiveHover: isDark ? c['color-bg-selected'] : '#d3e3fb',
      controlItemBgHover: isDark ? 'rgba(255,255,255,.06)' : 'rgba(0,0,0,.04)',
      colorBgTextHover: isDark ? 'rgba(255,255,255,.06)' : 'rgba(0,0,0,.04)',
      colorBgTextActive: isDark ? 'rgba(255,255,255,.12)' : 'rgba(0,0,0,.12)',

      // ---- 图标 ----
      colorIcon: c['color-text-tertiary'],
      colorIconHover: c['color-text'],

      // ---- 状态派生色：Alert / Message / Notification 的底 + 边 ----
      // 不钉的话算法会从 seed 派生：深色 colorErrorBg 算出 #fff0f1（近白），
      // 配 --color-text（#cdd6f4）后文字与底几乎同亮度 → Alert 完全不可读
      // （Playwright 截图实测）。这里按 8.3.4 的柔和底 + 同语义前景钉死：
      // 浅色 / 深色各一套，标题正文仍走 colorTextHeading（≥ 4.5）。
      colorErrorBg: c['color-danger-soft'],
      colorErrorBgHover: c['color-danger-soft'],
      colorErrorBgActive: c['color-danger-soft'],
      colorErrorBorder: c['color-danger'],
      colorErrorBorderHover: c['color-danger'],
      colorErrorText: c['color-danger'],
      colorErrorTextHover: c['color-danger'],
      colorErrorTextActive: c['color-danger'],
      colorWarningBg: c['color-warning-soft'],
      colorWarningBgHover: c['color-warning-soft'],
      colorWarningBorder: c['color-warning'],
      colorWarningText: c['color-warning'],
      colorWarningTextHover: c['color-warning'],
      colorSuccessBg: c['color-success-soft'],
      colorSuccessBgHover: c['color-success-soft'],
      colorSuccessBorder: c['color-success'],
      colorSuccessText: c['color-success'],
      colorSuccessTextHover: c['color-success'],
      colorInfoBg: c['color-info-soft'],
      colorInfoBgHover: c['color-info-soft'],
      colorInfoBorder: c['color-info'],
      colorInfoText: c['color-info'],
      colorInfoTextHover: c['color-info'],
      colorPrimaryBg: c['color-primary-soft'],
      colorPrimaryBorder: c['color-primary'],
      colorPrimaryBorderHover: c['color-primary-hover'],
      colorPrimaryText: c['color-primary'],

      // ---- 预设色阶（Tag filled 取 c1 底 + c7 字）----
      // 注意：antd 的 defaultAlgorithm 会用 seed 色板重新生成 green1..10 /
      // gold1..10 等彩色阶梯并覆盖此处（themes/default/index.js:13-30），
      // 因此**不在此钉这些键**——写了不生效，反而让 themeTokens 测试误判。
      // 实际取值由 tokens.css 的 `.css-var-*` 桥接覆盖 antd 自己生成的
      // `--ant-green-1/3/6/7` 等变量（见该文件注释）。

      ...SHAPE,
    },
    components: {
      Layout: {
        headerBg: c['color-bg-shell'],
        siderBg: c['color-bg-shell'],
        bodyBg: c['color-bg-base'],
        // antd 默认 triggerBg '#002140' + triggerColor = colorTextLightSolid，
        // 深色下合成 1.01:1（折叠条文字不可见），必须钉死。
        triggerBg: c['color-bg-subtle'],
        triggerColor: c['color-text-secondary'],
      },
      Menu: {
        // theme="light" 分支（浅色模式使用）
        itemColor: c['color-text-secondary'],
        itemHoverColor: c['color-text'],
        itemBg: c['color-bg-shell'],
        itemHoverBg: c['color-bg-hover'],
        itemSelectedBg: c['color-bg-selected'],
        itemSelectedColor: c['color-primary'],
        // theme="dark" 分支：两主题都给值，保证 `theme={mode}` 之外误用也不会塌陷。
        darkItemColor: '#bac2de',
        darkItemHoverColor: '#cdd6f4',
        darkItemBg: '#181825',
        darkItemHoverBg: '#313244',
        darkItemSelectedBg: '#383b4d',
        darkItemSelectedColor: '#89b4fa',
        darkPopupBg: '#313244',
        darkSubMenuItemBg: '#181825',
        darkGroupTitleColor: '#a6adc8',
        darkItemDisabledColor: '#8b90a6',
      },
      Card: { colorBgContainer: c['color-bg-container'] },
      Table: {
        headerBg: c['color-bg-subtle'],
        rowHoverBg: c['color-bg-hover'],
        // 七期走查（8.3.11）：antd 的表格空态（`table/style/empty.js`）直接用
        // `colorTextDisabled` 渲染「暂无…」提示——浅色 #767e91 on #fff 仅
        // 4.07:1（< 4.5），而契约明令禁用色不得用于提示文案。组件级覆盖该
        // alias token，空态落到三级文本（浅 6.04 / 深 7.37）。它同时影响
        // 表头筛选下拉的「Not Found」与搜索图标（同属提示，一并改善）。
        colorTextDisabled: c['color-text-tertiary'],
      },
      Modal: { contentBg: c['color-bg-container'], headerBg: c['color-bg-container'] },
      Drawer: { colorBgElevated: c['color-bg-container'] },
      Button: { primaryShadow: 'none' },
      // ---- 可操作边界（8.3.11 第 4 条：用户必须看见才能操作的边界 ≥ 3:1）----
      // 全局 `colorBorder` 保持装饰级弱线（表格/卡片分隔线 1.55 / 1.80 是
      // 有意为之）；输入类控件的外框是「必须看见才能操作」的边界，必须在
      // 组件级钉为 `--color-border-strong`（浅 4.07 / 深 5.81 on container）。
      // 覆盖的是组件作用域内的 alias token，不外溢到其他组件；hover/active
      // 仍走 antd 的 primary 系（≥ 5.4），状态反馈不变。
      Input: { colorBorder: c['color-border-strong'] },
      InputNumber: { colorBorder: c['color-border-strong'] },
      Select: { colorBorder: c['color-border-strong'] },
      DatePicker: { colorBorder: c['color-border-strong'] },
      // 七期走查：回放播放器进度条的未播放轨道默认取 `colorFillTertiary`
      // （浅色 rgba(0,0,0,.04) 压白 ≈ 1.07:1），用户看不出可拖范围与总长
      // 度。轨道属于「必须看见才能操作」的边界，钉成 border-strong；已播放
      // 段与滑块用 primary（浅 6.62 / 深 7.79），两者与 rail 的区分也足够。
      Slider: {
        railBg: c['color-border-strong'],
        railHoverBg: c['color-border-strong'],
        trackBg: c['color-primary'],
        trackHoverBg: c['color-primary-hover'],
        handleColor: c['color-primary'],
        handleActiveColor: c['color-primary'],
      },
    },
  };
}
