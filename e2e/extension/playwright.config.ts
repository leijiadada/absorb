import { defineConfig } from '@playwright/test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  testDir: path.join(here, 'specs'),
  timeout: 60_000,
  expect: { timeout: 10_000 },
  // scope-isolation 等用例依赖固定端口与顺序状态，单 worker 顺序执行
  workers: 1,
  fullyParallel: false,
  // CI runner 慢且偶发环境级 flake（如 #askBtn click 挂满超时），CI 允许重试一次；本地保持 0 便于调试
  retries: process.env.CI ? 1 : 0,
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: path.join(here, 'playwright-report') }],
  ],
  use: {
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  outputDir: path.join(here, 'test-results'),
  webServer: {
    command: 'node fixtures/servers.mjs',
    url: 'http://127.0.0.1:8899',
    cwd: here,
    reuseExistingServer: !process.env.CI,
    timeout: 15_000,
  },
});
