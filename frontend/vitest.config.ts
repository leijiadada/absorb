import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// 组件测试独立配置（不影响 wails build 走的主 vite.config.ts）
export default defineConfig({
  // 测试环境关闭模板资产转换：plugin-vue 默认把 <img src="/x.svg"> 编译成 import，
  // Windows 下 vitest 解析 public 资产的 file URL（无盘符）会抛
  // "must be a file URL object... Received 'file:///logo-white.svg'"，整套件加载失败。
  // src 保持纯字符串即可，组件测试不需要真实资源解析。
  plugins: [vue({ template: { transformAssetUrls: false } })],
  // 缓存固定在本包 node_modules 内：测试文件跨 frontend/ 与 extension/ 两处，
  // vitest 解析出的工作区根会落到仓库根，不固定则缓存散落污染 git status
  cacheDir: 'node_modules/.vitest',
  test: {
    // jsdom：DOMPurify 需要（happy-dom 下静默失效不清脚本）
    environment: 'jsdom',
    setupFiles: ['tests/setup.ts'],
    include: ['tests/**/*.spec.ts'],
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      // extension/*.js 经 createRequire 原生加载，绕过 vite 管线无法插桩——
      // 其质量由 golden/语料回归保障；此处只量化桌面端 src/**。
      include: ['src/**'],
      // main.ts / router.ts 为无分支的启动装配代码，不参与门槛
      exclude: ['src/main.ts', 'src/router.ts', 'src/wailsjs/**', 'src/**/*.d.ts', 'src/**/*.css'],
      thresholds: {
        global: {
          statements: 70,
          branches: 60,
          functions: 70,
          lines: 70,
        },
      },
    },
  },
})
