import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import App, { ClientKeysPanel, CompressionPage, Models, Proxies, Usage } from "./main";

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const summary = {
  requests: 100,
  counted_requests: 90,
  external_requests: 10,
  success: 9,
  success_rate: 0.1,
  prompt_tokens: 1200,
  completion_tokens: 800,
  total_tokens: 2000,
  free_models: 1,
  active_proxies: 2,
};

const publicStatus = {
  generated_at: "2026-08-11T07:00:00Z",
  timezone: "Asia/Shanghai",
  window_start: "2026-08-10T07:00:00Z",
  window_end: "2026-08-11T07:00:00Z",
  bucket_hours: 24,
  summary: {
    models: 1,
    requests_24h: 24,
    recent_requests_15m: 2,
    success_rate: 0.95,
    status: "available",
  },
  models: [
    {
      model_id: "alpha:free",
      display_name: "Alpha",
      admin_enabled: true,
      requests_24h: 24,
      recent_requests_15m: 2,
      success_24h: 23,
      external_errors_24h: 0,
      success_rate: 0.9583,
      avg_latency_ms: 33900,
      avg_first_token_latency_ms: 21600,
      status: "available",
      buckets: Array.from({ length: 24 }, (_, index) => ({
        start: new Date(Date.UTC(2026, 7, 10, 7 + index)).toISOString(),
        requests: index === 23 ? 2 : 1,
        success: 1,
        external_errors: 0,
        success_rate: 1,
        status: "available",
      })),
    },
  ],
};

const model = {
  id: 7,
  model_id: "alpha:free",
  display_name: "Alpha",
  is_free: true,
  free_reason: "test",
  refreshed_at: "2026-01-01T00:00:00Z",
  admin_enabled: true,
};

const adminResponse = (input: RequestInfo | URL, init?: RequestInit) => {
  const path = String(input);
  if (path === "/api/auth/me") return json({ ok: true });
  if (path === "/api/stats/summary") return json(summary);
  if (path === "/api/models/free") return json([model]);
  if (path === "/api/public/status") return json(publicStatus);
  if (path === "/api/settings/upstream") return json({});
  if (path === "/api/settings/maintenance") return json({ enabled: false });
  if (path.startsWith("/api/stats/models?")) return json([]);
  if (path.startsWith("/api/stats/proxies?")) return json([]);
  if (path.startsWith("/api/proxies?")) {
    return json({ items: [], page: 1, page_size: 50, total: 0, total_pages: 1 });
  }
  if (path === "/api/settings/proxy-engine") {
    return json({
      engine: "builtin",
      effective_engine: "builtin",
      resin_gateway_url: "",
      resin_platform: "Default",
      resin_configured: false,
      has_resin_proxy_token: false,
      resin_fallback_active: false,
    });
  }
  if (path === "/api/model-aliases") return json([]);
  if (path === "/api/model-overrides/free") return json([]);
  if (path === "/api/client-keys") return json([]);
  if (path.startsWith("/api/usage/requests?")) {
    const page = Number(new URL(path, "http://relay.test").searchParams.get("page") || 1);
    return json({ items: [], page, page_size: 25, total: 0, total_pages: 1, models: [] });
  }
  if (path === "/api/usage/rates") {
    return json({ window_seconds: 60, rpm: 0, tpm: 0, measured_at: "" });
  }
  if (path === "/api/stats/timeseries") return json([]);
  if (init?.method === "POST" || init?.method === "PUT" || init?.method === "PATCH") return json({});
  return json({});
};

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn(adminResponse));
  vi.stubGlobal("confirm", vi.fn(() => true));
  history.replaceState(null, "", "/");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  document.body.style.overflow = "";
});

describe("仪表盘", () => {
  it("使用公开状态页的 24 小时成功率且不请求最新记录", async () => {
    render(<App />);

    expect(await screen.findByText("95.00%")).toBeInTheDocument();
    expect(screen.getByText("23 / 24 有效请求")).toBeInTheDocument();
    expect(screen.queryByText("最新请求")).not.toBeInTheDocument();
    expect(screen.queryByText("10.0%")).not.toBeInTheDocument();
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/public/status",
      expect.objectContaining({ cache: "no-store", credentials: "omit" }),
    );
    expect(vi.mocked(fetch).mock.calls.some(([input]) =>
      String(input).includes("/api/usage/requests"),
    )).toBe(false);
  });

  it.each([
    {
      name: "全部是外部异常",
      requests: 5,
      externalErrors: 5,
    },
    {
      name: "没有请求",
      requests: 0,
      externalErrors: 0,
    },
  ])("$name 时显示无有效请求", async ({ requests, externalErrors }) => {
    const noEffectiveRequests = {
      ...publicStatus,
      summary: {
        ...publicStatus.summary,
        requests_24h: requests,
        success_rate: null,
        status: "no_request",
      },
      models: publicStatus.models.map((entry) => ({
        ...entry,
        requests_24h: requests,
        success_24h: 0,
        external_errors_24h: externalErrors,
        success_rate: null,
        status: "no_request",
      })),
    };
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input) === "/api/public/status"
        ? Promise.resolve(json(noEffectiveRequests))
        : Promise.resolve(adminResponse(input, init)),
    );

    render(<App />);

    expect(await screen.findByText("暂无有效请求")).toBeInTheDocument();
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
  });

  it("部分初始请求失败时显示告警提示", async () => {
    vi.mocked(fetch).mockImplementation((input, init) =>
      String(input) === "/api/stats/summary"
        ? Promise.resolve(json({ error: "summary unavailable" }, 500))
        : Promise.resolve(adminResponse(input, init)),
    );

    render(<App />);

    const toast = await screen.findByText("部分控制台数据未能刷新");
    expect(toast.closest(".toast")).toHaveClass("warning");
  });

  it("会话过期后返回登录页", async () => {
    render(<App />);
    await screen.findByRole("heading", { name: "仪表盘" });

    window.dispatchEvent(new Event("relaydesk:auth-expired"));

    expect(await screen.findByRole("button", { name: "进入控制台" })).toBeInTheDocument();
  });
});

describe("公开模型状态", () => {
  it("无需管理员会话并渲染每个小时状态", async () => {
    vi.mocked(fetch).mockImplementation(async (input) =>
      String(input) === "/api/public/status"
        ? json(publicStatus)
        : json({ error: "unexpected request" }, 500),
    );
    history.replaceState(null, "", "/status");

    render(<App />);

    expect(await screen.findByRole("heading", { name: "全部模型" })).toBeInTheDocument();
    expect(screen.getByText("Alpha")).toBeInTheDocument();
    expect(screen.getAllByRole("listitem")).toHaveLength(24);
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/public/status",
      expect.objectContaining({ credentials: "omit" }),
    );
    expect(vi.mocked(fetch)).not.toHaveBeenCalledWith("/api/auth/me", expect.anything());
  });
});

describe("页面刷新", () => {
  it("相同筛选和页码下仍重新请求记录、速率和每日汇总", async () => {
    history.replaceState(null, "", "/?usage_time=24h&usage_page=3");
    let refreshPage: (() => Promise<boolean>) | undefined;
    const registerRefresh = (_page: string, handler: () => Promise<boolean>) => {
      refreshPage = handler;
      return () => {};
    };

    render(<Usage notify={vi.fn()} onLogout={vi.fn()} registerRefresh={registerRefresh} />);

    await waitFor(() => expect(refreshPage).toBeDefined());
    await waitFor(() => {
      expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input).startsWith("/api/usage/requests?")).length).toBe(1);
      expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === "/api/usage/rates").length).toBe(1);
      expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === "/api/stats/timeseries").length).toBe(1);
    });

    await act(async () => {
      expect(await refreshPage!()).toBe(true);
    });

    expect(location.search).toContain("usage_page=3");
    expect(location.search).toContain("usage_time=24h");
    expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input).startsWith("/api/usage/requests?")).length).toBe(2);
    expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === "/api/usage/rates").length).toBe(2);
    expect(vi.mocked(fetch).mock.calls.filter(([input]) => String(input) === "/api/stats/timeseries").length).toBe(2);
  });

  it("保留已应用的使用记录筛选", async () => {
    render(<Usage notify={vi.fn()} onLogout={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("button", { name: "应用筛选" })).not.toBeDisabled());

    fireEvent.click(screen.getByRole("button", { name: "近 24 小时" }));
    fireEvent.click(screen.getByRole("button", { name: "应用筛选" }));

    await waitFor(() => expect(location.search).toContain("usage_time=24h"));
  });
});

describe("移动导航抽屉", () => {
  it("支持按钮、遮罩、Escape 和导航关闭并维护 ARIA", async () => {
    render(<App />);
    await screen.findByRole("heading", { name: "仪表盘" });
    const menu = screen.getByLabelText("打开导航");
    const navigation = screen.getByLabelText("控制台导航");

    expect(menu).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(menu);
    expect(menu).toHaveAttribute("aria-expanded", "true");
    expect(navigation).toHaveClass("is-open");
    expect(document.body.style.overflow).toBe("hidden");

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(menu).toHaveAttribute("aria-expanded", "false"));

    fireEvent.click(menu);
    fireEvent.click(document.querySelector(".drawer-backdrop")!);
    expect(menu).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(menu);
    fireEvent.click(navigation.querySelector("button[aria-label='关闭导航']")!);
    expect(menu).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(menu);
    fireEvent.click(screen.getByRole("button", { name: "代理池Proxy pool" }));
    expect(await screen.findByRole("heading", { name: "代理池" })).toBeInTheDocument();
    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(location.search).toContain("view=proxies");
  });
});

describe("运维操作", () => {
  it("显示逐行代理导入结果", async () => {
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path.startsWith("/api/proxies?")) {
        return json({ items: [], page: 1, page_size: 50, total: 0, total_pages: 1 });
      }
      if (path === "/api/settings/proxy-engine") return adminResponse(input, init);
      if (path === "/api/proxies/import" && init?.method === "POST") {
        return json({
          results: [
            { line: 1, uri: "http://one.example:8080", status: "imported" },
            { line: 2, uri: "bad-proxy", status: "invalid", error: "unsupported proxy URI" },
          ],
        });
      }
      return json({});
    });
    render(<Proxies upstream={{}} reload={async () => true} notify={vi.fn()} />);

    fireEvent.change(document.querySelector("textarea")!, {
      target: { value: "http://one.example:8080\nbad-proxy" },
    });
    fireEvent.click(screen.getByRole("button", { name: "导入代理" }));

    expect(await screen.findByText("第 2 行")).toBeInTheDocument();
    expect(screen.getByText("unsupported proxy URI")).toBeInTheDocument();
  });

  it("删除指定的客户端 Key", async () => {
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path === "/api/client-keys" && (!init?.method || init.method === "GET")) {
        return json([
          { id: 1, name: "Legacy", hint: "legacy", recoverable: false, enabled: true, rpm_limit: 0, tpm_limit: 0 },
          { id: 2, name: "CI", hint: "ocp...test", recoverable: true, enabled: true, rpm_limit: 10, tpm_limit: 0 },
        ]);
      }
      if (path === "/api/client-keys/2" && init?.method === "DELETE") return json({ ok: true });
      return json({});
    });
    render(<ClientKeysPanel notify={vi.fn()} />);
    await screen.findByText("CI");

    fireEvent.click(screen.getByText("CI").closest(".key-row")!.querySelector("button[title='删除 Key']")!);

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/client-keys/2",
      expect.objectContaining({ method: "DELETE" }),
    ));
  });

  it("按需显示、隐藏并复制可恢复的客户端 Key", async () => {
    const clipboard = { writeText: vi.fn(async () => {}) };
    Object.assign(navigator, { clipboard });
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path === "/api/client-keys" && (!init?.method || init.method === "GET")) {
        return json([
          { id: 2, name: "Deploy", hint: "ocp...test", recoverable: true, enabled: true, maintenance_allowed: false, rpm_limit: 0, tpm_limit: 0 },
          { id: 3, name: "Legacy", hint: "legacy", recoverable: false, enabled: true, maintenance_allowed: false, rpm_limit: 0, tpm_limit: 0 },
        ]);
      }
      if (path === "/api/client-keys/2/secret") return json({ client_key: "ocp-visible-secret" });
      return json({});
    });
    render(<ClientKeysPanel notify={vi.fn()} />);
    const deploy = await screen.findByText("Deploy");
    const row = deploy.closest(".key-row")!;

    expect(screen.queryByText("ocp-visible-secret")).not.toBeInTheDocument();
    fireEvent.click(row.querySelector("button[title='显示 Key']")!);
    expect(await screen.findByText("ocp-visible-secret")).toBeInTheDocument();
    expect(vi.mocked(fetch)).toHaveBeenCalledWith("/api/client-keys/2/secret", expect.anything());

    fireEvent.click(row.querySelector("button[title='复制 Key']")!);
    await waitFor(() => expect(clipboard.writeText).toHaveBeenCalledWith("ocp-visible-secret"));

    fireEvent.click(row.querySelector("button[title='隐藏 Key']")!);
    expect(screen.queryByText("ocp-visible-secret")).not.toBeInTheDocument();
    expect(screen.getByText("旧 Key 无法查看")).toBeInTheDocument();
  });

  it("创建和轮换后在对应 Key 行展示新密钥", async () => {
    let keys = [{ id: 7, name: "CI", hint: "ocp...old", recoverable: true, enabled: true, maintenance_allowed: false, rpm_limit: 0, tpm_limit: 0 }];
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path === "/api/client-keys" && (!init?.method || init.method === "GET")) return json(keys);
      if (path === "/api/client-keys" && init?.method === "POST") {
        keys = [...keys, { id: 8, name: "New", hint: "ocp...new", recoverable: true, enabled: true, maintenance_allowed: false, rpm_limit: 0, tpm_limit: 0 }];
        return json({ id: 8, client_key: "ocp-new-secret" }, 201);
      }
      if (path === "/api/client-keys/7/rotate" && init?.method === "POST") return json({ client_key: "ocp-rotated-secret" });
      return json({});
    });
    render(<ClientKeysPanel notify={vi.fn()} />);
    await screen.findByText("CI");

    fireEvent.change(screen.getByPlaceholderText("例如 CI"), { target: { value: "New" } });
    fireEvent.click(screen.getByRole("button", { name: "创建 Key" }));
    expect(await screen.findByText("ocp-new-secret")).toBeInTheDocument();

    fireEvent.click(screen.getByText("CI").closest(".key-row")!.querySelector("button[title='轮换 Key']")!);
    expect(await screen.findByText("ocp-rotated-secret")).toBeInTheDocument();
  });

  it("网关卡片仅提供地址和客户端 Key 管理入口", async () => {
    render(<Models models={[]} upstream={{}} reload={async () => true} notify={vi.fn()} />);
    const gateway = document.querySelector(".gateway-card")!;

    expect(gateway).toHaveTextContent(`${location.origin}/v1`);
    expect(gateway).toHaveTextContent("管理客户端 Key");
    expect(gateway.querySelector("button[title='轮换 Key']")).toBeNull();
    expect(vi.mocked(fetch).mock.calls.some(([input]) => String(input) === "/api/settings/client-key/rotate")).toBe(false);
  });

  it("从模型状态控件更新模型策略", async () => {
    const reload = vi.fn(async () => true);
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path === "/api/model-aliases" || path === "/api/client-keys") return json([]);
      if (path === "/api/models/7/policy" && init?.method === "PATCH") {
        return json({ model_id: "alpha:free", enabled: false });
      }
      return json({});
    });
    render(<Models models={[model]} upstream={{}} reload={reload} notify={vi.fn()} />);

    fireEvent.click(await screen.findByTitle("停用模型"));

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/models/7/policy",
      expect.objectContaining({ method: "PATCH" }),
    ));
  });

  it("为没有 free 后缀的模型添加持久免费标记", async () => {
    const reload = vi.fn(async () => true);
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path === "/api/model-aliases" || path === "/api/client-keys") return json([]);
      if (path === "/api/model-overrides/free" && (!init?.method || init.method === "GET")) return json([]);
      if (path === "/api/model-overrides/free" && init?.method === "POST") {
        return json({ model_id: "provider/model", created_at: "2026-08-12T00:00:00Z" }, 201);
      }
      return json({});
    });
    render(<Models models={[]} upstream={{}} reload={reload} notify={vi.fn()} />);

    fireEvent.change(await screen.findByPlaceholderText("provider/model-name"), { target: { value: "provider/model" } });
    fireEvent.click(screen.getByRole("button", { name: "添加免费模型" }));

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/model-overrides/free",
      expect.objectContaining({ method: "POST", body: JSON.stringify({ model_id: "provider/model" }) }),
    ));
    expect(reload).toHaveBeenCalled();
  });

  it("保存维护模式并只对标记的 Client Key 放行", async () => {
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const path = String(input);
      if (path === "/api/settings/probes") return json({ enabled: true, exit_minutes: 15, upstream_minutes: 60 });
      if (path === "/api/settings/alerts") return json({ enabled: false, webhook_url: "", has_webhook_secret: false, events: [], low_proxy_threshold: 3, success_rate_percent: 80 });
      if (path === "/api/settings/model-refresh") return json({ refresh_minutes: 360 });
      if (path === "/api/settings/maintenance" && (!init?.method || init.method === "GET")) return json({ enabled: false });
      if (path === "/api/settings/maintenance" && init?.method === "PUT") return json({ enabled: true });
      if (path === "/api/models/free") return json([model]);
      if (path === "/api/model-aliases" || path === "/api/model-overrides/free") return json([]);
      if (path === "/api/client-keys" && (!init?.method || init.method === "GET")) return json([{ id: 2, name: "Deploy", hint: "ocp...test", enabled: true, maintenance_allowed: false, rpm_limit: 0, tpm_limit: 0 }]);
      if (path === "/api/client-keys/2" && init?.method === "PATCH") return json({});
      return json({});
    });
    render(<App />);
    await screen.findByRole("heading", { name: "仪表盘" });

    fireEvent.click(screen.getByRole("button", { name: /设置Settings/ }));
    const maintenanceToggle = await screen.findByRole("checkbox", { name: /维护模式/ });
    fireEvent.click(maintenanceToggle);
    fireEvent.click(screen.getByRole("button", { name: "保存运行设置" }));

    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/settings/maintenance",
      expect.objectContaining({ method: "PUT", body: JSON.stringify({ enabled: true }) }),
    ));

    fireEvent.click(screen.getByRole("button", { name: /模型与网关Models/ }));
    const allowDuringMaintenance = await screen.findByRole("checkbox", { name: "维护放行" });
    fireEvent.click(allowDuringMaintenance);
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      "/api/client-keys/2",
      expect.objectContaining({ method: "PATCH", body: JSON.stringify({ maintenance_allowed: true }) }),
    ));
  });
});

const compressionSettings = {
  enabled: true,
  api_style: "openai",
  base_url: "https://summary.example.com",
  model: "summary-model",
  use_proxy: false,
  trigger_ratio: 0.75,
  context_window: 200000,
  keep_recent_messages: 8,
  min_fold_messages: 4,
  max_summary_tokens: 1200,
  temperature: 0.2,
  timeout_seconds: 90,
  summary_role: "system",
  marker: "📦 已折叠 {count} 条较早消息",
  prompt: "把较早的对话压缩成一份摘要。",
  fail_open: true,
  has_api_key: true,
  configured: true,
  default_marker: "📦 [Relay Desk 压缩摘要] 已折叠 {count} 条较早消息",
  default_prompt: "默认压缩提示词",
};

const compressionStats = {
  days: 7,
  compressions: 3,
  failures: 1,
  tokens_before: 9000,
  tokens_after: 3000,
  saved_tokens: 6000,
  summary_tokens: 1200,
  average_latency_ms: 640,
  sessions: 1,
  daily: [{ date: "2026-10-20", compressions: 3, saved_tokens: 6000, failures: 1 }],
  recent: [
    {
      id: 12,
      created_at: "2026-10-20T08:00:00Z",
      session_key: "session-alpha",
      model: "alpha:free",
      api_style: "openai",
      summary_model: "summary-model",
      folded_messages: 6,
      tokens_before: 9000,
      tokens_after: 3000,
      saved_tokens: 6000,
      summary_tokens: 1200,
      latency_ms: 640,
      status: "success",
      error_message: "",
    },
  ],
};

const compressionSessions = {
  sessions: [
    {
      session_key: "session-alpha",
      model: "alpha:free",
      preview: "摘要预览文本",
      folded_messages: 6,
      folded_tokens: 9000,
      compressions: 1,
      tokens_before: 9000,
      tokens_after: 3000,
      created_at: "2026-10-20T08:00:00Z",
      updated_at: "2026-10-20T08:00:00Z",
    },
  ],
};

const compressionResponse = (input: RequestInfo | URL, init?: RequestInit) => {
  const path = String(input);
  if (path === "/api/settings/compression" && (!init?.method || init.method === "GET")) {
    return json(compressionSettings);
  }
  if (path === "/api/settings/compression" && init?.method === "PUT") {
    return json({ ...compressionSettings, ...JSON.parse(String(init.body)) });
  }
  if (path === "/api/settings/compression/test") {
    return json({
      ok: true,
      summary: "接口返回的摘要",
      summary_tokens: 12,
      prompt_tokens: 40,
      completion_tokens: 12,
      latency_ms: 88,
      api_style: "openai",
      model: "summary-model",
      endpoint: "https://summary.example.com/v1/chat/completions",
    });
  }
  if (path.startsWith("/api/compression/stats")) return json(compressionStats);
  if (path.startsWith("/api/compression/sessions?")) return json(compressionSessions);
  if (path === "/api/compression/preview") {
    return json({
      applied: true,
      window: 200000,
      folded_messages: 3,
      kept_messages: 2,
      tokens_before: 1000,
      tokens_after: 400,
      saved_tokens: 600,
      summary: "预览生成的摘要",
      summary_tokens: 50,
      latency_ms: 42,
    });
  }
  if (path.startsWith("/api/compression/sessions/")) return json({ ok: true, removed: 1 });
  return json({});
};

describe("压缩网关", () => {
  it("加载配置、统计与会话摘要", async () => {
    vi.stubGlobal("fetch", vi.fn(compressionResponse));
    render(
      <CompressionPage
        notify={vi.fn()}
        onLogout={vi.fn()}
        registerRefresh={() => () => {}}
      />,
    );

    expect(await screen.findByRole("heading", { level: 3, name: "摘要接口与折叠策略" })).toBeInTheDocument();
    expect(await screen.findByDisplayValue("https://summary.example.com")).toBeInTheDocument();
    expect(await screen.findByDisplayValue("summary-model")).toBeInTheDocument();
    expect((await screen.findAllByText("session-alpha")).length).toBeGreaterThan(0);
    expect(await screen.findByText(/近 7 天折叠次数/)).toBeInTheDocument();
    expect(screen.getByText("6,000")).toBeInTheDocument();
  });

  it("保存配置时提交可配置字段且不回传密钥", async () => {
    vi.stubGlobal("fetch", vi.fn(compressionResponse));
    render(
      <CompressionPage notify={vi.fn()} onLogout={vi.fn()} registerRefresh={() => () => {}} />,
    );

    await screen.findByDisplayValue("https://summary.example.com");
    fireEvent.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() =>
      expect(vi.mocked(fetch)).toHaveBeenCalledWith(
        "/api/settings/compression",
        expect.objectContaining({ method: "PUT" }),
      ),
    );
    const call = vi
      .mocked(fetch)
      .mock.calls.find(([input, init]) => String(input) === "/api/settings/compression" && init?.method === "PUT");
    const body = JSON.parse(String(call?.[1]?.body));
    expect(body).toMatchObject({
      enabled: true,
      api_style: "openai",
      base_url: "https://summary.example.com",
      model: "summary-model",
      trigger_ratio: 0.75,
      context_window: 200000,
      keep_recent_messages: 8,
      min_fold_messages: 4,
      max_summary_tokens: 1200,
      summary_role: "system",
      fail_open: true,
    });
    expect(body.api_key).toBeUndefined();
    expect(body.has_api_key).toBeUndefined();
  });

  it("预览压缩会提交示例消息并展示折叠结果", async () => {
    vi.stubGlobal("fetch", vi.fn(compressionResponse));
    render(
      <CompressionPage notify={vi.fn()} onLogout={vi.fn()} registerRefresh={() => () => {}} />,
    );

    await screen.findByRole("heading", { level: 3, name: "摘要接口与折叠策略" });
    fireEvent.click(screen.getByRole("button", { name: "预览压缩" }));

    expect(await screen.findByText(/已折叠 3 条 · 保留 2 条/)).toBeInTheDocument();
    const call = vi
      .mocked(fetch)
      .mock.calls.find(([input]) => String(input) === "/api/compression/preview");
    const body = JSON.parse(String(call?.[1]?.body));
    expect(Array.isArray(body.messages)).toBe(true);
    expect(body.messages.length).toBeGreaterThan(0);
    expect(body.force).toBe(true);
  });

  it("测试摘要接口后展示返回的摘要", async () => {
    vi.stubGlobal("fetch", vi.fn(compressionResponse));
    render(
      <CompressionPage notify={vi.fn()} onLogout={vi.fn()} registerRefresh={() => () => {}} />,
    );

    await screen.findByDisplayValue("summary-model");
    fireEvent.click(screen.getByRole("button", { name: "测试摘要接口" }));

    expect(await screen.findByText(/接口返回的摘要/)).toBeInTheDocument();
    expect(await screen.findByText(/88 ms/)).toBeInTheDocument();
  });

  it("从导航进入压缩网关页", async () => {
    vi.mocked(fetch).mockImplementation((input, init) => {
      const compression = compressionResponse(input, init);
      const path = String(input);
      if (path.startsWith("/api/compression") || path.startsWith("/api/settings/compression")) {
        return Promise.resolve(compression);
      }
      return Promise.resolve(adminResponse(input, init));
    });
    render(<App />);
    await screen.findByRole("heading", { name: "仪表盘" });

    fireEvent.click(screen.getByRole("button", { name: /压缩网关Compression/ }));

    expect(await screen.findByRole("heading", { level: 1, name: "压缩网关" })).toBeInTheDocument();
    await waitFor(() =>
      expect(vi.mocked(fetch)).toHaveBeenCalledWith(
        "/api/settings/compression",
        expect.anything(),
      ),
    );
  });
});
