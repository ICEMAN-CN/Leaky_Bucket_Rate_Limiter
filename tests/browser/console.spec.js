import { test, expect } from "@playwright/test";
import { randomUUID } from "node:crypto";

const outage = process.env.OUTAGE === "1";
const external = process.env.EXTERNAL_STANDARD_URL || "http://127.0.0.1:8081";
const freshUser = () => "test-" + randomUUID();

class ConsolePage {
  constructor(page) {
    this.page = page;
    this.monitor = page.getByLabel("监控目标");
    this.target = page.getByLabel("请求目标");
    this.user = page.getByLabel("用户 ID");
    this.log = page.getByRole("table", { name: "请求记录", exact: true });
    this.health = page.getByRole("list", { name: "实例列表" });
  }
  async open() {
    await this.page.goto("/");
    await expect(this.page.getByRole("heading", { name: "限流操作台" })).toBeVisible();
  }
  button(name) {
    return this.page.getByRole("button", { name, exact: true });
  }
  metric(name) {
    return this.page
      .getByRole("article")
      .filter({ has: this.page.getByText(name, { exact: true }) })
      .locator("strong");
  }
}

test.describe("操作台", () => {
  test.skip(outage, "常规验收依赖健康的 Redis");

  test("查询未知用户、发送突发、读取快照并清空记录", async ({ page }) => {
    const ui = new ConsolePage(page);
    const user = freshUser();
    await ui.open();
    await ui.user.fill(user);
    await ui.button("查询水位").click();
    await expect(ui.log.getByRole("row").filter({ hasText: "404" })).toContainText("无桶记录");
    await ui.button("突发 6 次").click();
    await expect(ui.log.getByRole("cell", { name: "放行", exact: true })).toHaveCount(5);
    await expect(ui.log.getByRole("cell", { name: "限流", exact: true })).toHaveCount(1);
    await expect(ui.log.getByRole("row").filter({ hasText: "429" })).toContainText(user);
    await ui.button("查询水位").click();
    await expect(ui.log.getByRole("cell", { name: "已查询", exact: true })).toHaveCount(1);
    await expect(page.getByRole("meter")).toHaveAttribute("aria-valuemax", "5");
    await ui.user.fill(freshUser());
    await expect(page.getByRole("meter")).toHaveAttribute("aria-valuetext", "暂无快照");
    await ui.button("发送一次").click();
    await expect(ui.log.getByRole("cell", { name: "放行", exact: true })).toHaveCount(6);
    await ui.button("清空记录").click();
    await expect(ui.log.getByRole("row")).toHaveCount(1);
  });

  for (const name of ["漏水恢复", "用户隔离", "跨实例共享"]) {
    test("真实限流实验 · " + name, async ({ page }) => {
      const ui = new ConsolePage(page);
      await ui.open();
      await ui.button("验证" + name).click();
      await expect(page.getByRole("status").filter({ hasText: name + " · 验证通过" })).toBeVisible();
      await expect(ui.log.getByRole("cell", { name: "限流", exact: true })).toHaveCount(1);
      const rows = ui.log.getByRole("row");
      if (name === "跨实例共享") {
        await expect(rows.filter({ hasText: "go-a" }).first()).toBeVisible();
        await expect(rows.filter({ hasText: "go-b" }).first()).toBeVisible();
        await expect(rows.filter({ hasText: "已查询" })).toHaveCount(1);
      }
      if (name === "用户隔离") await expect(rows.last()).toContainText(/demo-\w+-b/);
      if (name === "漏水恢复") await expect(rows.last()).toContainText("放行");
    });
  }

  test("外部客户端产生流量，浏览器重开后仍读到服务端历史", async ({ page, request }) => {
    const ui = new ConsolePage(page);
    await ui.open();
    await ui.monitor.selectOption("clojure");
    await expect(ui.metric("放行请求")).toHaveText(/^[\d,]+$/);
    const before = Number((await ui.metric("放行请求").innerText()).replaceAll(",", ""));
    await page.goto("about:blank");
    for (let i = 0; i < 3; i++) {
      const result = await request.post(external + "/v1/check", { data: { user_id: freshUser() } });
      expect(result.status()).toBe(200);
    }
    await expect
      .poll(async () => {
        const result = await request.get("/api/history?target=clojure");
        return (await result.json()).points.reduce((sum, point) => sum + (point.allowed ?? 0), 0);
      })
      .toBeGreaterThanOrEqual(before + 3);
    await ui.open();
    await ui.monitor.selectOption("clojure");
    await expect
      .poll(async () => Number((await ui.metric("放行请求").innerText()).replaceAll(",", "")))
      .toBeGreaterThanOrEqual(before + 3);
    await page.getByLabel("时间范围").selectOption("300");
    await page.getByText("最近采样明细", { exact: true }).click();
    await expect(page.getByRole("table", { name: "最近采样明细" }).getByRole("row").nth(1)).toBeVisible();
    await ui.monitor.selectOption("go-a");
    await expect(ui.health.getByRole("listitem")).toHaveCount(1);
    await expect(ui.health).toContainText("Go A");
    await expect(ui.health).toContainText("就绪");
  });

  test("缺失采样、延迟上界与采样中断分别呈现", async ({ page }) => {
    const ui = new ConsolePage(page);
    const now = Math.floor(Date.now() / 1000);
    let unavailable = false;
    await page.route("**/api/history?*", (route) => {
      if (unavailable) return route.fulfill({ status: 503 });
      return route.fulfill({
        json: {
          interval_seconds: 2,
          window_seconds: 1800,
          instances: [],
          points: [
            {
              at: now - 2,
              allowed: null,
              limited: null,
              errors: null,
              rps: null,
              p95_ms: null,
              partial: true,
            },
            { at: now, allowed: 3, limited: 1, errors: 1, rps: 2.5, p95_ms: -1, partial: false },
          ],
        },
      });
    });
    await ui.open();
    await expect(ui.metric("P95 耗时")).toHaveText(">500");
    await expect(ui.metric("放行请求")).toHaveText("3");
    await expect(page.getByText("统计仅包含已采集的请求", { exact: false })).toBeVisible();
    await page.getByText("最近采样明细", { exact: true }).click();
    const missing = page
      .getByRole("table", { name: "最近采样明细" })
      .getByRole("row")
      .filter({ hasText: "缺失" });
    await expect(missing.getByRole("cell", { name: "—", exact: true })).toHaveCount(4);
    unavailable = true;
    await expect(page.getByText("采样连接中断")).toBeVisible();
    await expect(ui.metric("放行请求")).toHaveText("3");
  });

  test("窄屏可切换实验目标与操作，无横向页面溢出", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 780 });
    const ui = new ConsolePage(page);
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await ui.open();
    await page.getByRole("link", { name: "请求实验" }).click();
    await ui.target.selectOption("go-b");
    await ui.user.fill(freshUser());
    await ui.button("发送一次").click();
    await expect(ui.log.getByRole("row").filter({ hasText: "go-b" })).toContainText("放行");
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
    expect(overflow).toBeLessThanOrEqual(1);
    expect(errors).toEqual([]);
  });
});

test.describe("Redis 故障", () => {
  test.skip(!outage, "独立故障演练由 OUTAGE=1 启用");
  test("Redis 停止时页面显示 HTTP 503、系统错误和未就绪实例", async ({ page }) => {
    const ui = new ConsolePage(page);
    await ui.open();
    // 故障演练可能也停掉了 go-a，直接请求仍在运行的 go-b。
    await ui.monitor.selectOption("go-b");
    await expect(ui.metric("系统错误")).toHaveText(/^[\d,]+$/);
    const baseline = Number((await ui.metric("系统错误").innerText()).replaceAll(",", ""));
    await ui.target.selectOption("go-b");
    await ui.user.fill(freshUser());
    await ui.button("发送一次").click();
    await expect(page.getByText("限流服务暂不可用", { exact: false })).toBeVisible();
    await expect(ui.log.getByRole("row").filter({ hasText: "503" })).toContainText("失败");
    await expect(ui.log.getByRole("cell", { name: "限流", exact: true })).toHaveCount(0);
    await expect(ui.health).toContainText("未就绪");
    await expect
      .poll(async () => Number((await ui.metric("系统错误").innerText()).replaceAll(",", "")))
      .toBeGreaterThan(baseline);
  });
});
