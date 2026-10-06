import { defineConfig } from "@playwright/test";

// 控制台 e2e（P2-2 人控闭环）：webServer 只起 web（dev 模式，rewrites 指向
// API_URL，默认 localhost:8080）；平台 API 由测试前置/外部栈保证。
export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  use: {
    baseURL: "http://localhost:3000",
    trace: "retain-on-failure",
  },
  webServer: {
    command: "pnpm dev",
    url: "http://localhost:3000",
    reuseExistingServer: true,
    timeout: 60_000,
  },
});
