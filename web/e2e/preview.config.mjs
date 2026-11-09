// vite preview 专用配置：仅用于 Playwright 端到端测试。
// 与 web/vite.config.ts 独立，不修改产品配置。
//
// - 端口固定 4173（strictPort），产物沿用 web/dist。
// - /api 与 /healthz 反向代理到 127.0.0.1:18080 的 aiops 后端。
//
// 键序即匹配序（vite proxy 按 Object.keys 顺序 startsWith 命中）：
// '/api/rdp' 与 '/api/term' 必须声明在 '/api' 之前，否则二者的 WS 升级
// 会落进 '/api' 的 ws:false。
export default {
  preview: {
    // host:true 让 preview 同时监听 v4/v6：Node 17+ 把 'localhost' 解析为
    // 仅 ::1，会导致 127.0.0.1 探活与浏览器 v4 回环同时连不上。
    host: true,
    port: 4173,
    strictPort: true,
    proxy: {
      // RDP WS 真链路（p2 sprint 3）：guacamole WebSocketTunnel 的升级
      // 请求经 preview 代理到后端（子协议 guacamole 透传，指令帧为文本
      // 帧）——修复前 /api 统一 ws:false 使全部 e2e 的 WS 必然握手失败，
      // RDP 链路从未被 e2e 覆盖（二进制帧 bug 即由此漏网）。REST 请求
      // （/api/rdp/sessions 等）经同一条转发，行为与 '/api' 一致。
      '/api/rdp': {
        target: 'http://127.0.0.1:18080',
        changeOrigin: false,
        ws: true,
      },
      // 终端 WS 真链路（p2 sprint 5）：与 RDP 同理放行 /api/term 的 WS
      // 升级，终端 e2e 从此可走真后端 + 假 sshd（cmd/e2esshd）的完整 PTY
      // 链路。此前 ws:false 使终端用例只能 mock REST 且 WS 必然失败，
      // PTY 尺寸同步缺陷（挂载即上报的 resize 在 CONNECTING 期被静默
      // 丢弃，服务端永远停在创建时的 120×40）从未被 e2e 暴露；依赖
      // 「WS 必然失败」语义的存量 mock 用例已在 p2 sprint 5 改为真链路。
      '/api/term': {
        target: 'http://127.0.0.1:18080',
        changeOrigin: false,
        ws: true,
      },
      '/api': {
        target: 'http://127.0.0.1:18080',
        changeOrigin: false,
        // SSE：http-proxy 按流式响应直接透传，无需（也没有）缓冲开关；
        // ws:false 只表示不代理 WebSocket 升级，与缓冲无关。上游若经
        // 反向代理发版，缓冲由服务端 X-Accel-Buffering:no 响应头关闭
        // （internal/httpapi/chat.go），e2e preview 不引入额外链路。
        // 终端/RDP 的 WS 已由上面的长前缀规则单独放行。
        ws: false,
      },
      '/healthz': {
        target: 'http://127.0.0.1:18080',
        changeOrigin: false,
      },
    },
  },
};
