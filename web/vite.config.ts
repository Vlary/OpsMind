import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// CONTRACT.md 9.1：/api 与 /healthz 代理到 127.0.0.1:8080，
// changeOrigin:false 保留浏览器 Origin 供 CSRF 校验。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
      '/healthz': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    rolldownOptions: {
      output: {
        // vendor 分层只捕获“全应用共用、闭包稳定”的框架层，
        // 业务代码迭代不失效这些 chunk 的浏览器缓存。
        //
        // 用 rolldown 原生 codeSplitting.groups（按声明顺序 test 互斥匹配），
        // 不用函数式 manualChunks：后者会被迁移成“单 group 多名字”，
        // 跨 vendor 的依赖模块（react/jsx-runtime 等）会被复制进依赖方的
        // vendor chunk，实测导致 react 并入 icons-vendor、markdown-vendor
        // 被首屏连带加载。groups 互斥匹配则 react 恒归 react-vendor。
        //
        // antd / rc-* / @rc-component / dayjs 刻意不捕获：AppShell 只用
        // Layout/Menu/Dropdown 等壳组件，Table/Tree/Form/Select 等重组件
        // 仅被懒加载路由使用，由 rolldown 按使用方分块（整包归入单个
        // vendor 实测 1.07MB 且首屏全量下载）。
        //
        // markdown 渲染链（react-markdown/remark/micromark）同样交给
        // rolldown 按使用方分块：它只被懒加载的 MessageList/MarkdownView
        // 引用，天然不进首屏，比固定 vendor 捕获更贴合首屏预算。
        codeSplitting: {
          groups: [
            {
              // react 系框架：必须声明在最前，react 恒定归属此 chunk。
              name: 'react-vendor',
              test: /[\\/]node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom|@tanstack[\\/]react-query|@tanstack[\\/]query-core|zustand)[\\/]/,
            },
            {
              // 图标为具名 ESM 引入，tree-shaking 后只含实际用到的图标；
              // 独立成 chunk 使图标迭代不失效 react-vendor 缓存。
              name: 'icons-vendor',
              test: /[\\/]node_modules[\\/]@ant-design[\\/]icons(-svg)?[\\/]/,
            },
            {
              // Markdown 渲染链（react-markdown/remark/micromark/hast 等）。
              // 仅被懒加载的 MessageList/MarkdownView 使用；react-vendor
              // 组声明在前，jsx-runtime 等依赖不会被拖进本 chunk。
              name: 'markdown-vendor',
              test: /[\\/]node_modules[\\/](react-markdown|remark-[^\\/]+|micromark[^\\/]*|mdast-[^\\/]+|unified|unist-[^\\/]+|vfile[^\\/]*|hast-[^\\/]+|property-information|html-url-attributes|space-separated-tokens|comma-separated-tokens|decode-named-character-reference|character-entities[^\\/]*|trim-lines|zwitch|bail|trough|devlop|is-plain-obj|kind-of|@ungap[\\/]structured-clone|markdown-table|inline-style-parser|style-to-object|extend)[\\/]/,
            },
          ],
        },
      },
    },
  },
});
