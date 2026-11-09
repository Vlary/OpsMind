import { defineConfig } from '@playwright/test';

// 端到端测试配置：globalSetup 拉起 postgres(aiops_e2e) + 后端(18080) +
// 假 LLM(18081) + vite preview(4173)，teardown 全量回收。
// 环境细节见 e2e/global-setup.ts。
//
// 五个 project：
// - chromium：全部常规用例（不含 auth-password.spec.ts 与三个走查 spec）。
// - walkthrough-p5round4 / round5 / round6：五期走查 spec，链式依赖
//   （chromium → round4 → round5 → round6）。round5 的 monitor 空态用例
//   独占干净库（直插 server_metrics/latest 前先清场），而 round4 三个
//   用例直插 server_metrics_latest 且不回收；round6 的 monitor 用例同样
//   直插指标/事件并新建第二台服务器——文件间并行（workers:2）必然污染
//   空态前置，故三个走查文件必须先于彼此顺序执行（全量跑实证 round5
//   空态断言 20s 超时红）。
// - auth-password：修改密码全链路，依赖链尾（walkthrough-p5round6）全部
//   结束后才启动。改密会吊销 admin 的全部会话；若与其他用例并行，它们
//   持有的 storageState Cookie 会立即失效导致连锁失败，必须串行在最后。
export default defineConfig({
  testDir: './e2e',
  // 最多 2 个 worker 并行；浏览器实例按 worker 复用（Playwright 默认
  // worker 级 browser fixture，不为每个测试新起浏览器）。
  // 登录限流为按 IP 令牌桶 5 次/12s（cmd/aiops/main.go:79）且后端每轮
  // 重建重启（桶随之重置）；一轮内登录调用仅 4 次（setup 1 + auth.spec 2
  // + auth-password.spec 1），并行不会触顶。auth.spec 与 auth-password.spec
  // 内部保持串行（fullyParallel: false），文件之间并行。
  workers: 2,
  fullyParallel: false,
  timeout: 90_000,
  expect: { timeout: 20_000 },
  retries: 0,
  reporter: [['list']],
  globalSetup: './e2e/global-setup.ts',
  outputDir: './e2e/.test-results',
  use: {
    baseURL: 'http://localhost:4173',
    // headless 默认；不录视频（无 video 配置）。
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [
    {
      name: 'chromium',
      testIgnore: /auth-password\.spec\.ts|walkthrough-p5round[456]\.spec\.ts/,
    },
    {
      name: 'walkthrough-p5round4',
      testMatch: /walkthrough-p5round4\.spec\.ts/,
      dependencies: ['chromium'],
    },
    {
      name: 'walkthrough-p5round5',
      testMatch: /walkthrough-p5round5\.spec\.ts/,
      dependencies: ['walkthrough-p5round4'],
    },
    {
      name: 'walkthrough-p5round6',
      testMatch: /walkthrough-p5round6\.spec\.ts/,
      dependencies: ['walkthrough-p5round5'],
    },
    {
      name: 'auth-password',
      testMatch: /auth-password\.spec\.ts/,
      dependencies: ['walkthrough-p5round6'],
    },
  ],
});
