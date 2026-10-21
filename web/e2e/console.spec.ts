import { expect, test, type Page, type TestInfo } from "@playwright/test";

const consolePages = [
  { view: "overview", title: "仪表盘" },
  { view: "proxies", title: "代理池" },
  { view: "models", title: "模型与网关" },
  { view: "usage", title: "使用记录" },
  { view: "settings", title: "设置" },
] as const;

async function login(page: Page) {
  await page.goto("/");
  await page.getByLabel("管理员密码").fill("relaydesk-playwright-password");
  await page.getByRole("button", { name: "进入控制台" }).click();
  await expect(page.getByRole("heading", { name: "仪表盘" })).toBeVisible();
}

async function expectPageFrame(page: Page, title: string) {
  await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();
  await expect(page.locator("h1")).toHaveCount(1);
  await expect.poll(() => page.evaluate(() => {
    const viewport = document.documentElement.clientWidth;
    return Array.from(document.querySelectorAll<HTMLElement>("body *"))
      .map((node) => {
        const rect = node.getBoundingClientRect();
        return { tag: node.tagName, className: node.className, right: Math.round(rect.right), width: Math.round(rect.width) };
      })
      .filter((node) => node.right > viewport + 1)
      .slice(0, 12);
  })).toEqual([]);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  await page.waitForTimeout(220);
  const path = testInfo.outputPath(`${name}.png`);
  await page.screenshot({ path, fullPage: true });
  await testInfo.attach(name, { path, contentType: "image/png" });
}

async function openConsolePage(page: Page, title: string, mobile: boolean) {
  if (title === "仪表盘") return;
  if (mobile) {
    await page.getByRole("button", { name: "打开导航" }).click();
  }
  await page.locator("nav button").filter({ hasText: title }).click();
  if (mobile) {
    await expect(page.getByRole("button", { name: "打开导航" })).toHaveAttribute("aria-expanded", "false");
  }
}

test("1440x900 控制台五个页面保持统一结构", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);

  for (const entry of consolePages) {
    await openConsolePage(page, entry.title, false);
    await expectPageFrame(page, entry.title);
    await capture(page, testInfo, `desktop-${entry.view}`);
  }
});

test("390x844 抽屉和五个页面适配移动端", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route("**/api/usage/requests?**", async (route) => {
    const url = new URL(route.request().url());
    const currentPage = Number(url.searchParams.get("page") || 1);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [{
          id: 501,
          created_at: "2026-08-11T07:00:00Z",
          request_kind: "chat",
          model: "provider/alpha-free-model-with-a-very-long-production-name",
          client_user_agent: "opencode/1.18.12 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13 long-client-suffix",
          stream: true,
          proxy_uri: "socks5://long-proxy-hostname.example.internal:1080",
          status: "error",
          status_code: 429,
          error_origin: "external",
          latency_ms: 33900,
          first_token_latency_ms: 21600,
          retry_count: 1,
          prompt_tokens: 123456,
          completion_tokens: 7890,
          total_tokens: 131346,
          error_message: "upstream rate limit response with a deliberately long diagnostic detail",
          route_engine: "builtin",
        }],
        page: currentPage,
        page_size: 25,
        total: 1,
        total_pages: 1,
        models: ["provider/alpha-free-model-with-a-very-long-production-name"],
      }),
    });
  });
  await login(page);

  const menu = page.getByRole("button", { name: "打开导航" });
  await menu.click();
  await expect(menu).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByLabel("控制台导航")).toHaveClass(/is-open/);
  await expect.poll(() => page.evaluate(() => document.body.style.overflow)).toBe("hidden");
  await page.keyboard.press("Escape");
  await expect(menu).toHaveAttribute("aria-expanded", "false");

  await menu.click();
  await page.locator(".drawer-backdrop").click({ position: { x: 380, y: 420 } });
  await expect(menu).toHaveAttribute("aria-expanded", "false");

  await menu.click();
  await page.getByLabel("控制台导航").getByRole("button", { name: "关闭导航" }).click();
  await expect(menu).toHaveAttribute("aria-expanded", "false");

  for (const entry of consolePages) {
    await openConsolePage(page, entry.title, true);
    await expectPageFrame(page, entry.title);
    if (entry.view === "usage") {
      await expect(page.getByText("Token 131,346")).toBeVisible();
      await page.getByRole("button", { name: "展开请求详情" }).click();
      const details = page.locator(".usage-mobile-detail");
      await expect(details.getByText(/long-client-suffix/)).toBeVisible();
      await expect(details.getByText(/upstream rate limit response/)).toBeVisible();
    }
    await capture(page, testInfo, `mobile-${entry.view}`);
  }
});

test("Client Key 行在桌面和移动端可显示且无溢出", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  await openConsolePage(page, "模型与网关", false);

  const name = `Layout Key ${Date.now()}`;
  await page.getByPlaceholder("例如 CI").fill(name);
  await page.getByRole("button", { name: "创建 Key" }).click();
  const entry = page.locator(".key-entry", { hasText: name });
  await expect(entry).toBeVisible();
  await expect(entry.locator(".key-secret code")).toHaveText(/ocp-/);
  await expect.poll(() => page.evaluate(() => ({
    body: document.body.scrollWidth - document.body.clientWidth,
    document: document.documentElement.scrollWidth - document.documentElement.clientWidth,
  }))).toEqual({ body: 0, document: 0 });
  await capture(page, testInfo, "desktop-client-key");

  await page.setViewportSize({ width: 390, height: 844 });
  await entry.scrollIntoViewIfNeeded();
  await expect(entry).toBeVisible();
  await expect(entry.locator(".key-secret code")).toHaveText(/ocp-/);
  await expect.poll(() => page.evaluate(() => {
    const viewport = document.documentElement.clientWidth;
    return Array.from(document.querySelectorAll<HTMLElement>("body *"))
      .map((node) => {
        const rect = node.getBoundingClientRect();
        return { tag: node.tagName, className: node.className, right: Math.round(rect.right), width: Math.round(rect.width) };
      })
      .filter((node) => node.right > viewport + 1)
      .slice(0, 12);
  })).toEqual([]);
  await capture(page, testInfo, "mobile-client-key");
});

test("公开状态页在桌面和移动端均无溢出", async ({ page }, testInfo) => {
  const buckets = Array.from({ length: 24 }, (_, index) => ({
    start: new Date(Date.UTC(2026, 7, 10, 7 + index)).toISOString(),
    requests: 1,
    success: 1,
    external_errors: 0,
    success_rate: 1,
    status: "available",
  }));
  await page.route("**/api/public/status", (route) => route.fulfill({
    status: 200,
    contentType: "application/json",
    body: JSON.stringify({
      generated_at: "2026-08-11T07:00:00Z",
      timezone: "Asia/Shanghai",
      window_start: "2026-08-10T07:00:00Z",
      window_end: "2026-08-11T07:00:00Z",
      bucket_hours: 24,
      summary: { models: 1, requests_24h: 24, recent_requests_15m: 2, success_rate: 1, status: "available" },
      models: [{
        model_id: "provider/alpha-free-model-with-a-very-long-production-name",
        display_name: "Alpha Free Production Model With Long Display Name",
        admin_enabled: true,
        requests_24h: 24,
        recent_requests_15m: 2,
        success_24h: 24,
        external_errors_24h: 0,
        success_rate: 1,
        avg_latency_ms: 1200,
        avg_first_token_latency_ms: 400,
        status: "available",
        buckets,
      }],
    }),
  }));

  for (const viewport of [
    { name: "desktop", width: 1440, height: 900 },
    { name: "mobile", width: 390, height: 844 },
  ]) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    await page.goto("/status");
    await expectPageFrame(page, "全部模型");
    await expect(page.getByText("Alpha Free Production Model With Long Display Name")).toBeVisible();
    await expect(page.locator(".public-status-strip .public-status-cell")).toHaveCount(24);
    await expect(page.getByRole("textbox")).toHaveCount(0);
    await capture(page, testInfo, `${viewport.name}-status`);
  }
});
