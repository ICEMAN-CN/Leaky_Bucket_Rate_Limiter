import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  testMatch: /console\.spec\.js/,
  workers: 1,
  timeout: 45000,
  expect: { timeout: 10000 },
  reporter: "list",
  use: {
    baseURL: process.env.BASE_URL || "http://127.0.0.1:8080",
    locale: "en-HK",
    reducedMotion: "reduce",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    viewport: { width: 1280, height: 860 },
  },
});
