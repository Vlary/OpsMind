// 纯 CSS 加载指示器：不依赖 antd（Spin）与图标 chunk。
// 路由 fallback / 认证门等首屏链上的加载态必须用它——antd Spin 会把
// icons-vendor（~54KB）拉进登录首屏（分包注释宣称壳层图标不进首屏链）。
import './spinner.css';

export default function Spinner({ size = 24 }: { size?: number }) {
  return (
    <span
      className="opsmind-spinner"
      style={{ width: size, height: size }}
      role="status"
      aria-label="加载中"
    />
  );
}
