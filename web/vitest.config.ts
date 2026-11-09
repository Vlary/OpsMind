import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    // 骨架阶段尚未有测试文件；FE1 加入 SSE/缓冲测试后此开关应删除。
    passWithNoTests: true,
  },
});
