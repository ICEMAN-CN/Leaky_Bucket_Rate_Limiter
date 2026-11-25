import { test, expect } from "@playwright/test";
import { randomUUID } from "node:crypto";

const outage = process.env.OUTAGE === "1";
const external = process.env.EXTERNAL_STANDARD_URL || "http://127.0.0.1:8081";
const freshUser = () => "test-" + randomUUID();

class ConsolePage {
  constructor(page) {
    this.page = page;
    this.monitor = page.getByLabel("Monitor target");
    this.target = page.getByLabel("Request target");
    this.user = page.getByLabel("User ID");
    this.log = page.getByRole("table", { name: "Request log", exact: true });
    this.health = page.getByRole("list", { name: "Instance list" });
  }
  async open() {
    await this.page.goto("/");
    await expect(this.page.getByRole("heading", { name: "Rate limiting console" })).toBeVisible();
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

test.describe("Console", () => {
  test.skip(outage, "The normal suite requires a healthy Redis server");

  test("looks up an unknown user, sends a burst, reads the snapshot, and clears the log", async ({ page }) => {
    const ui = new ConsolePage(page);
    const user = freshUser();
    await ui.open();
    await ui.user.fill(user);
    await ui.button("Look up bucket").click();
    await expect(ui.log.getByRole("row").filter({ hasText: "404" })).toContainText("No bucket");
    await ui.button("Send burst of 6").click();
    await expect(ui.log.getByRole("cell", { name: "Allowed", exact: true })).toHaveCount(5);
    await expect(ui.log.getByRole("cell", { name: "Rate limited", exact: true })).toHaveCount(1);
    await expect(ui.log.getByRole("row").filter({ hasText: "429" })).toContainText(user);
    await ui.button("Look up bucket").click();
    await expect(ui.log.getByRole("cell", { name: "Read", exact: true })).toHaveCount(1);
    await expect(page.getByRole("meter")).toHaveAttribute("aria-valuemax", "5");
    await ui.user.fill(freshUser());
    await expect(page.getByRole("meter")).toHaveAttribute("aria-valuetext", "No snapshot yet");
    await ui.button("Send one request").click();
    await expect(ui.log.getByRole("cell", { name: "Allowed", exact: true })).toHaveCount(6);
    await ui.button("Clear log").click();
    await expect(ui.log.getByRole("row")).toHaveCount(1);
  });

  test("manual lookups preserve dot segments, special characters, and long user IDs", async ({ page }) => {
    const ui = new ConsolePage(page);
    await ui.open();
    for (const user of [".", "..", "\u7532 /+?%#", "long-" + "x".repeat(9000)]) {
      await ui.user.fill(user);
      const sent = page.waitForResponse((response) => response.url().endsWith("/v1/check"));
      await ui.button("Send one request").click();
      const admitted = await (await sent).json();
      const read = page.waitForResponse((response) => response.url().endsWith("/v1/bucket-state"));
      await ui.button("Look up bucket").click();
      const response = await read;
      expect(response.request().method()).toBe("POST");
      expect(response.request().postDataJSON()).toEqual({ user_id: user });
      expect(response.status()).toBe(200);
      expect((await response.json()).bucket).toEqual(admitted.bucket);
      await expect(page.getByText("Read the last settled bucket snapshot.")).toBeVisible();
    }
  });

  for (const name of ["Quota recovery", "User isolation", "Shared quota"]) {
    test("Live rate-limit scenario · " + name, async ({ page }) => {
      const ui = new ConsolePage(page);
      await ui.open();
      await ui.button("Test " + name.toLowerCase()).click();
      await expect(page.getByRole("status").filter({ hasText: name + " · Passed" })).toBeVisible();
      await expect(ui.log.getByRole("cell", { name: "Rate limited", exact: true })).toHaveCount(1);
      const rows = ui.log.locator("tbody tr");
      if (name === "Shared quota") {
        await expect(rows.filter({ hasText: "go-a" }).first()).toBeVisible();
        await expect(rows.filter({ hasText: "go-b" }).first()).toBeVisible();
        await expect(rows.filter({ hasText: "Read" })).toHaveCount(1);
      }
      if (name === "User isolation" || name === "Quota recovery") {
        await expect(rows.first()).toContainText("Allowed");
        await expect(rows.nth(1)).toContainText("Rate limited");
      }
      if (name === "User isolation") await expect(rows.first()).toContainText(/demo-\w+-b/);
    });
  }

  test("tracks external traffic and preserves server-side history when the page is reopened", async ({ page, request }) => {
    const ui = new ConsolePage(page);
    await ui.open();
    await ui.monitor.selectOption("clojure");
    await expect(ui.metric("Allowed requests")).toHaveText(/^[\d,]+$/);
    const before = Number((await ui.metric("Allowed requests").innerText()).replaceAll(",", ""));
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
      .poll(async () => Number((await ui.metric("Allowed requests").innerText()).replaceAll(",", "")))
      .toBeGreaterThanOrEqual(before + 3);
    await page.getByLabel("Time range").selectOption("300");
    await page.getByText("Recent samples", { exact: true }).click();
    await expect(page.getByRole("table", { name: "Recent samples" }).getByRole("row").nth(1)).toBeVisible();
    await ui.monitor.selectOption("go-a");
    await expect(ui.health.getByRole("listitem")).toHaveCount(1);
    await expect(ui.health).toContainText("Go A");
    await expect(ui.health).toContainText("Ready");
  });

  test("shows sampling gaps, latency bounds, and sampler disconnection separately", async ({ page }) => {
    const ui = new ConsolePage(page);
    const now = Math.floor(Date.now() / 1000);
    let unavailable = false;
    await page.route("**/api/history?*", (route) => {
      if (unavailable) return route.fulfill({ status: 503 });
      return route.fulfill({
        json: {
          interval_seconds: 2,
          window_seconds: 1800,
          instances: [{ id: "go-b", at: now, up: false, ready: true }],
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
            { at: now, allowed: 3, limited: 1, errors: 1, rps: 2.5, p95_ms: -1, partial: true },
          ],
        },
      });
    });
    await ui.open();
    await expect(ui.metric("p95 latency")).toHaveText(">500");
    await expect(ui.metric("Allowed requests")).toHaveText("3");
    await expect(ui.health).toContainText("Ready · Metrics missing");
    await expect(page.getByText("1 / 1 ready", {exact: true})).toBeVisible();
    await expect(page.getByText("req/s · Partial", { exact: true })).toBeVisible();
    await expect(page.getByText("ms · Partial upper bound", { exact: true })).toBeVisible();
    await expect(page.getByText("Totals include only sampled requests", { exact: false })).toBeVisible();
    await page.getByText("Recent samples", { exact: true }).click();
    const missing = page
      .getByRole("table", { name: "Recent samples" })
      .getByRole("row")
      .filter({ hasText: "Missing" });
    await expect(missing.getByRole("cell", { name: "—", exact: true })).toHaveCount(4);
    unavailable = true;
    await expect(page.getByText("Sampling disconnected")).toBeVisible();
    await expect(ui.metric("Allowed requests")).toHaveText("3");
  });

  test("supports target selection and requests on narrow screens without horizontal page overflow", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 780 });
    const ui = new ConsolePage(page);
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await ui.open();
    await page.getByRole("link", { name: "Request testing" }).click();
    await ui.target.selectOption("go-b");
    await ui.user.fill(freshUser());
    await ui.button("Send one request").click();
    await expect(ui.log.getByRole("row").filter({ hasText: "go-b" })).toContainText("Allowed");
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
    expect(overflow).toBeLessThanOrEqual(1);
    expect(errors).toEqual([]);
  });
});

test.describe("Redis outage", () => {
  test.skip(!outage, "The isolated outage scenario runs with OUTAGE=1");
  test("shows HTTP 503, service errors, and unready instances when Redis is down", async ({ page }) => {
    const ui = new ConsolePage(page);
    await ui.open();
    // The outage scenario can also stop go-a. Send requests directly to the surviving go-b.
    await ui.monitor.selectOption("go-b");
    await expect(ui.metric("Service errors")).toHaveText(/^[\d,]+$/);
    const baseline = Number((await ui.metric("Service errors").innerText()).replaceAll(",", ""));
    await ui.target.selectOption("go-b");
    await ui.user.fill(freshUser());
    await ui.button("Send one request").click();
    await expect(page.getByText("Rate limiter unavailable", { exact: false })).toBeVisible();
    await expect(ui.log.getByRole("row").filter({ hasText: "503" })).toContainText("Failed");
    await expect(ui.log.getByRole("cell", { name: "Rate limited", exact: true })).toHaveCount(0);
    await expect(ui.health).toContainText("Not ready");
    await expect
      .poll(async () => Number((await ui.metric("Service errors").innerText()).replaceAll(",", "")))
      .toBeGreaterThan(baseline);
  });
});
