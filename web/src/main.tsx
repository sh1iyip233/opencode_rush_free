import { createRoot } from "react-dom/client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Activity,
  ArrowUpRight,
  CalendarRange,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  CircleGauge,
  Copy,
  Database,
  Eye,
  EyeOff,
  Filter,
  FilterX,
  ImageIcon,
  KeyRound,
  LogOut,
  Menu,
  Network,
  Radio,
  RefreshCw,
  Server,
  Settings2,
  ShieldCheck,
  TriangleAlert,
  Trash2,
  Upload,
  UsersRound,
  X,
} from "lucide-react";
import "./styles.css";

type Summary = {
  requests: number;
  counted_requests: number;
  external_requests: number;
  success: number;
  success_rate: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  free_models: number;
  active_proxies: number;
  proxy_engine?: "builtin" | "resin";
  effective_proxy_engine?: "builtin" | "resin";
  resin_fallback_active?: boolean;
  resin_fallback_since?: string;
  resin_fallback_reason?: string;
};
type Proxy = {
  id: number;
  uri: string;
  scheme: string;
  host: string;
  port: number;
  username?: string;
  enabled: boolean;
  health_status: string;
  failure_count: number;
  usage_state?: "unused" | "in_use" | "cooldown";
  cooldown_until?: string;
  expires_at?: string;
  last_probe_at?: string;
  last_probe_latency_ms?: number;
  last_exit_ip?: string;
  last_probe_error?: string;
  upstream_probe_at?: string;
  upstream_probe_status?: string;
};
type ProxyPage = {
  items: Proxy[];
  page: number;
  page_size: number;
  total: number;
  total_pages: number;
};
type Model = {
  id: number;
  model_id: string;
  display_name: string;
  is_free: boolean;
  free_reason: string;
  refreshed_at: string;
  admin_enabled?: boolean;
};
type ForceFreeModel = {
  model_id: string;
  created_at: string;
};
type RequestRow = {
  id: number;
  created_at: string;
  request_kind?: string;
  model: string;
  client_user_agent?: string;
  stream?: boolean;
  proxy_uri?: string;
  status: string;
  status_code: number;
  error_origin?: "none" | "user" | "external" | "internal";
  latency_ms: number;
  first_token_latency_ms?: number;
  retry_count: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  error_message?: string;
  route_engine?: "builtin" | "resin" | "direct";
};
type ProxyEngine = {
  engine: "builtin" | "resin";
  effective_engine: "builtin" | "resin";
  resin_gateway_url: string;
  resin_platform: string;
  has_resin_proxy_token: boolean;
  resin_configured: boolean;
  resin_fallback_active: boolean;
  resin_fallback_since?: string;
  resin_fallback_reason?: string;
  resin_last_checked_at?: string;
  resin_last_check_error?: string;
  resin_health?: "unknown" | "healthy" | "degraded";
};
type ToastType = "success" | "warning" | "error";
type Toast = { message: string; type: ToastType };
type ImportResult = { line: number; uri: string; status: string; error?: string };
type ClientKey = {
  id: number;
  name: string;
  hint: string;
  recoverable: boolean;
  enabled: boolean;
  maintenance_allowed: boolean;
  expires_at?: string;
  rpm_limit: number;
  tpm_limit: number;
  last_used_at?: string;
};
type ModelAlias = {
  id: number;
  alias: string;
  target_model_id: string;
  enabled: boolean;
};
type ProbeJob = {
  id: string;
  status: string;
  total: number;
  completed: number;
  results: Array<{ proxy_id: number; uri: string; exit_ok: boolean; exit_ip?: string; error?: string }>;
};
type AlertSettings = {
  enabled: boolean;
  webhook_url: string;
  has_webhook_secret: boolean;
  events: string[];
  low_proxy_threshold: number;
  success_rate_percent: number;
};
type ProbeSettings = { enabled: boolean; exit_minutes: number; upstream_minutes: number };
type MaintenanceSettings = { enabled: boolean };
type UsagePage = {
  items: RequestRow[];
  page: number;
  page_size: number;
  total: number;
  total_pages: number;
  models: string[];
};
type UsageRates = {
  window_seconds: number;
  rpm: number;
  tpm: number;
  measured_at: string;
};
type DailyUsage = {
  day: string;
  requests: number;
  tokens: number;
};
type DimensionStat = {
  name: string;
  requests: number;
  success: number;
  external_errors: number;
  user_errors: number;
  tokens: number;
  success_rate: number;
  p50_latency_ms: number;
  p95_latency_ms: number;
  p50_first_token_latency_ms: number;
  p95_first_token_latency_ms: number;
};
type PublicStatusState = "available" | "degraded" | "outage" | "no_request";
type PublicStatusBucket = {
  start: string;
  requests: number;
  success: number;
  external_errors: number;
  success_rate: number | null;
  status: PublicStatusState;
};
type PublicModelStatus = {
  model_id: string;
  display_name: string;
  admin_enabled: boolean;
  requests_24h: number;
  recent_requests_15m: number;
  success_24h: number;
  external_errors_24h: number;
  success_rate: number | null;
  avg_latency_ms: number;
  avg_first_token_latency_ms: number;
  status: PublicStatusState;
  buckets: PublicStatusBucket[];
};
type PublicStatusResponse = {
  generated_at: string;
  timezone: string;
  window_start: string;
  window_end: string;
  bucket_hours: number;
  maintenance_mode?: boolean;
  summary: {
    models: number;
    requests_24h: number;
    recent_requests_15m: number;
    success_rate: number | null;
    status: PublicStatusState;
  };
  models: PublicModelStatus[];
};
type ProxyFilterState = "all" | "unused" | "in_use" | "cooldown";
type UsageTimePreset = "all" | "1h" | "24h" | "7d" | "30d" | "custom";
type StatsWindow = "1h" | "24h" | "7d" | "30d";
type UsageFilters = {
  time: UsageTimePreset;
  model: string;
  status: "" | "success" | "error" | "external";
  customFrom: string;
  customTo: string;
};
type RefreshHandler = () => Promise<boolean>;
type RegisterRefresh = (page: string, handler: RefreshHandler) => () => void;
type RegisterChildRefresh = (handler: RefreshHandler) => () => void;

const noopRegisterRefresh: RegisterRefresh = () => () => {};

const alertEventOptions = [
  { value: "resin_unavailable", label: "Resin 不可达" },
  { value: "proxy_availability_low", label: "可用代理低于阈值" },
  { value: "proxy_pool_empty", label: "代理池为空" },
  { value: "success_rate_low", label: "5 分钟成功率过低" },
  { value: "model_refresh_failed", label: "模型刷新失败" },
  { value: "client_key_rate_limited", label: "客户端 Key 限流" },
];

const api = async (path: string, init: RequestInit = {}) => {
  const r = await fetch(path, {
    credentials: "include",
    headers: { "Content-Type": "application/json", ...(init.headers || {}) },
    ...init,
  });
  if (r.status === 401) {
    window.dispatchEvent(new Event("relaydesk:auth-expired"));
    throw new Error("AUTH");
  }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || "请求失败");
  return data;
};
const publicApi = async (path: string, init: RequestInit = {}) => {
  const r = await fetch(path, {
    ...init,
    credentials: "omit",
    headers: { "Content-Type": "application/json", ...(init.headers || {}) },
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || "状态数据暂时不可用");
  return data;
};
const fmt = (n: number) => new Intl.NumberFormat("zh-CN").format(n || 0);
const freeReasonText = (reason: string) =>
  reason === "manual_override" ? "手动指定" : reason.replaceAll("_", " ");
function Login({ onLogin }: { onLogin: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    try {
      await api("/api/auth/login", {
        method: "POST",
        body: JSON.stringify({ password }),
      });
      onLogin();
    } catch (err) {
      setError((err as Error).message);
    }
  };
  return (
    <main className="login-shell">
      <div className="login-mark">
        <span>R</span>
        <div>
          <b>Relay Desk</b>
          <small>Free model gateway</small>
        </div>
      </div>
      <form className="login-card" onSubmit={submit}>
        <div className="eyebrow">PRIVATE CONTROL PLANE</div>
        <h1>
          把请求，交给
          <br />
          <em>更好的路径。</em>
        </h1>
        <p>管理模型、代理与使用记录。所有上游凭证只在服务器侧保存。</p>
        <label>
          管理员密码
          <input
            autoFocus
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="输入密码"
          />
        </label>
        {error && <div className="error-line">{error}</div>}
        <button className="primary wide" type="submit">
          <ShieldCheck size={17} />
          进入控制台
        </button>
      </form>
      <div className="login-foot">OpenAI-compatible relay · v0.1</div>
    </main>
  );
}

const publicStatusLabels: Record<PublicStatusState, string> = {
  available: "可用",
  degraded: "降级",
  outage: "异常",
  no_request: "无请求",
};

const publicStatusTime = new Intl.DateTimeFormat("zh-CN", {
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
  timeZone: "Asia/Shanghai",
});

const publicStatusRate = (value: number | null) =>
  value === null || value === undefined ? "—" : `${(value * 100).toFixed(2)}%`;

const publicStatusDuration = (value: number) =>
  value > 0 ? formatDuration(value) : "—";

function PublicStatusPage() {
  const [data, setData] = useState<PublicStatusResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    const previousTitle = document.title;
    document.title = "模型状态 · Relay Desk";
    return () => {
      document.title = previousTitle;
    };
  }, []);
  const load = useCallback(async () => {
    setRefreshing(true);
    try {
      const next = (await publicApi("/api/public/status")) as PublicStatusResponse;
      setData(next);
      setError("");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void load();
    const refreshWhenVisible = () => {
      if (document.visibilityState === "visible") void load();
    };
    const timer = window.setInterval(refreshWhenVisible, 60_000);
    document.addEventListener("visibilitychange", refreshWhenVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refreshWhenVisible);
    };
  }, [load]);

  const statusClass = (status: PublicStatusState) =>
    `public-status-state ${status}`;
  const statusText = (status: PublicStatusState) =>
    publicStatusLabels[status] || publicStatusLabels.no_request;
  const timeText = (raw: string) => {
    const date = new Date(raw);
    return Number.isNaN(date.getTime()) ? "—" : publicStatusTime.format(date);
  };

  return (
    <div className="public-status-shell">
      <header className="public-status-header">
        <a className="public-status-brand" href="/" aria-label="Relay Desk 控制台">
          <span className="public-status-brand-icon">R</span>
          <span>
            <strong>Relay Desk</strong>
            <small>MODEL AVAILABILITY</small>
          </span>
        </a>
        <div className="public-status-header-actions">
          <span className={`public-status-live ${data?.maintenance_mode ? "maintenance" : ""}`}><i />{data?.maintenance_mode ? "维护中" : "实时数据"}</span>
          <button
            className="icon-btn"
            type="button"
            title="刷新状态"
            aria-label="刷新状态"
            onClick={() => void load()}
            disabled={refreshing}
          >
            <RefreshCw size={16} className={refreshing ? "spin" : ""} />
          </button>
          <a className="public-status-console-link" href="/">进入控制台 <ArrowUpRight size={14} /></a>
        </div>
      </header>
      <main className="public-status-main">
        <div className="public-status-intro">
          <div>
            <span className="eyebrow">MODEL STATUS</span>
            <h1>全部模型</h1>
            <p>近 24 小时每小时状态</p>
          </div>
          {data && (
            <div className="public-status-summary">
              <span>{data.summary.models} 个 Free 模型</span>
              <strong>{publicStatusRate(data.summary.success_rate)}</strong>
              <small>24h 整体成功率</small>
            </div>
          )}
        </div>
        {error && (
          <div className="public-status-error" role="alert">
            <TriangleAlert size={16} />
            <span>{error}</span>
            <button type="button" onClick={() => void load()}>重试</button>
          </div>
        )}
        {data?.maintenance_mode && (
          <div className="public-status-maintenance" role="status">
            <TriangleAlert size={16} />
            <span>服务正在维护，新的模型请求暂时不可用。</span>
          </div>
        )}
        {loading && !data ? (
          <div className="public-status-loading"><CircleGauge className="spin" size={19} />正在读取模型状态</div>
        ) : data && data.models.length > 0 ? (
          <div className="public-status-list">
            {data.models.map((model) => (
              <article className="public-status-card" key={model.model_id}>
                <div className="public-status-card-head">
                  <div className="public-model-identity">
                    <span className={`public-status-mark ${model.status}`} aria-hidden="true" />
                    <div>
                      <div className="public-model-title">
                        <strong>{model.display_name}</strong>
                        {!model.admin_enabled && <span className="public-disabled-tag">已停用</span>}
                      </div>
                      <small>{model.model_id}</small>
                    </div>
                  </div>
                  <div className="public-model-overall">
                    <span className={statusClass(model.status)}><i />{statusText(model.status)}</span>
                    <strong>{publicStatusRate(model.success_rate)}</strong>
                    <small>24h 成功率</small>
                  </div>
                </div>
                <div className="public-status-strip-wrap">
                  <div className="public-status-strip" role="list" aria-label={`${model.display_name} 近 24 小时状态`}>
                    {model.buckets.map((bucket) => (
                      <span
                        key={bucket.start}
                        className={`public-status-cell ${bucket.status}`}
                        role="listitem"
                        title={`${timeText(bucket.start)} · ${statusText(bucket.status)} · ${fmt(bucket.requests)} 请求 · ${publicStatusRate(bucket.success_rate)}`}
                        aria-label={`${timeText(bucket.start)}，${statusText(bucket.status)}，${fmt(bucket.requests)} 请求，成功率 ${publicStatusRate(bucket.success_rate)}`}
                      />
                    ))}
                  </div>
                  <div className="public-status-axis">
                    <span>{timeText(model.buckets[0]?.start || data.window_start)}</span>
                    <span>近 24 小时</span>
                    <span>{timeText(model.buckets[model.buckets.length - 1]?.start || data.window_end)}</span>
                  </div>
                </div>
                <div className="public-status-metrics">
                  <div><span>24h 请求</span><strong>{fmt(model.requests_24h)}</strong></div>
                  <div><span>近 15 分钟</span><strong>{fmt(model.recent_requests_15m)}</strong></div>
                  <div><span>平均首字</span><strong>{publicStatusDuration(model.avg_first_token_latency_ms)}</strong></div>
                  <div><span>平均耗时</span><strong>{publicStatusDuration(model.avg_latency_ms)}</strong></div>
                </div>
              </article>
            ))}
          </div>
        ) : data ? (
          <div className="public-status-empty">暂时没有 Free 模型</div>
        ) : null}
        {data && (
          <footer className="public-status-footer">
            <div className="public-status-legend">
              {(["available", "degraded", "outage", "no_request"] as PublicStatusState[]).map((status) => (
                <span key={status}><i className={`public-status-cell ${status}`} />{statusText(status)} {status === "available" ? "≥95%" : status === "degraded" ? "80–95%" : status === "outage" ? "<80%" : ""}</span>
              ))}
            </div>
            <small>更新于 {timeText(data.generated_at)} · 数据时区 Asia/Shanghai</small>
          </footer>
        )}
      </main>
    </div>
  );
}

function App() {
  if (location.pathname === "/status") return <PublicStatusPage />;
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [page, setPage] = useState(() => {
    const view = new URLSearchParams(location.search).get("view");
    return ["overview", "proxies", "models", "usage", "settings"].includes(view || "") ? view! : "overview";
  });
  const [toast, setToast] = useState<Toast | null>(null);
  useEffect(() => {
    api("/api/auth/me")
      .then(() => setAuthed(true))
      .catch(() => setAuthed(false));
  }, []);
  useEffect(() => {
    const expire = () => setAuthed(false);
    window.addEventListener("relaydesk:auth-expired", expire);
    return () => window.removeEventListener("relaydesk:auth-expired", expire);
  }, []);
  useEffect(() => {
    const syncPageFromURL = () => {
      const view = new URLSearchParams(location.search).get("view");
      setPage(["overview", "proxies", "models", "usage", "settings"].includes(view || "") ? view! : "overview");
    };
    window.addEventListener("popstate", syncPageFromURL);
    return () => window.removeEventListener("popstate", syncPageFromURL);
  }, []);
  useEffect(() => {
    if (toast) {
      const t = setTimeout(() => setToast(null), 3000);
      return () => clearTimeout(t);
    }
  }, [toast]);
  if (authed === null)
    return (
      <div className="boot">
        <CircleGauge className="spin" />
        正在连接控制台
      </div>
    );
  if (!authed) return <Login onLogin={() => setAuthed(true)} />;
  const selectPage = (next: string) => {
    const url = new URL(location.href);
    url.searchParams.set("view", next);
    history.pushState(null, "", url);
    setPage(next);
  };
  return (
    <Console
      page={page}
      setPage={selectPage}
      notify={(message, type = "success") => setToast({ message, type })}
      onLogout={() => {
        api("/api/auth/logout", { method: "POST" }).finally(() =>
          setAuthed(false),
        );
      }}
      toast={toast}
    />
  );
}

function Console({
  page,
  setPage,
  notify,
  onLogout,
  toast,
}: {
  page: string;
  setPage: (v: string) => void;
  notify: (v: string, type?: ToastType) => void;
  onLogout: () => void;
  toast: Toast | null;
}) {
  const [summary, setSummary] = useState<Summary>({
    requests: 0,
    counted_requests: 0,
    external_requests: 0,
    success: 0,
    success_rate: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    free_models: 0,
    active_proxies: 0,
  });
  const [models, setModels] = useState<Model[]>([]);
  const [upstream, setUpstream] = useState<any>({});
  const [publicStatus, setPublicStatus] = useState<PublicStatusResponse | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const refreshHandlers = useRef(new Map<string, RefreshHandler>());
  const overviewStatisticsRefresh = useRef<RefreshHandler | null>(null);
  const menuButtonRef = useRef<HTMLButtonElement | null>(null);
  const drawerCloseRef = useRef<HTMLButtonElement | null>(null);
  const pageTitleRef = useRef<HTMLHeadingElement | null>(null);
  const load = useCallback(async () => {
    const results = await Promise.allSettled([
      api("/api/stats/summary"),
      api("/api/models/free"),
      publicApi("/api/public/status", { cache: "no-store" }),
      api("/api/settings/upstream"),
    ]);
    const [summaryResult, modelsResult, statusResult, upstreamResult] = results;
    if (summaryResult.status === "fulfilled") setSummary(summaryResult.value);
    if (modelsResult.status === "fulfilled") setModels(modelsResult.value);
    if (statusResult.status === "fulfilled") setPublicStatus(statusResult.value);
    if (upstreamResult.status === "fulfilled") setUpstream(upstreamResult.value);
    const errors = results.filter((result) => result.status === "rejected") as PromiseRejectedResult[];
    if (errors.some((result) => (result.reason as Error).message === "AUTH")) {
      onLogout();
      return false;
    }
    return errors.length === 0;
  }, [onLogout]);
  useEffect(() => {
    void load().then((ok) => {
      if (!ok) notify("部分控制台数据未能刷新", "warning");
    });
  }, [load, notify]);
  useEffect(() => {
    const refreshWhenVisible = () => {
      if (page === "overview" && document.visibilityState === "visible") void load();
    };
    const timer = window.setInterval(refreshWhenVisible, 10_000);
    document.addEventListener("visibilitychange", refreshWhenVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refreshWhenVisible);
    };
  }, [load, page]);
  useEffect(() => {
    if (!drawerOpen) return;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const focusFrame = window.requestAnimationFrame(() => drawerCloseRef.current?.focus());
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setDrawerOpen(false);
        window.requestAnimationFrame(() => menuButtonRef.current?.focus());
      }
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      window.cancelAnimationFrame(focusFrame);
      document.body.style.overflow = previousOverflow;
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [drawerOpen]);
  const registerRefresh = useCallback<RegisterRefresh>((pageName, handler) => {
    refreshHandlers.current.set(pageName, handler);
    return () => {
      if (refreshHandlers.current.get(pageName) === handler) {
        refreshHandlers.current.delete(pageName);
      }
    };
  }, []);
  const nav = [
    ["overview", "仪表盘", "Overview", "网关、模型与路径的实时运行状态", CircleGauge],
    ["proxies", "代理池", "Proxy pool", "管理出口路径、路由策略与健康状态", Network],
    ["models", "模型与网关", "Models", "配置上游、免费模型与客户端访问", Database],
    ["usage", "使用记录", "Usage log", "按时间、模型和结果审计每次调用", Activity],
    ["settings", "设置", "Settings", "管理安全、探测、告警与数据保留", Settings2],
  ] as const;
  const currentPage = nav.find((item) => item[0] === page) || nav[0];
  const navigate = (next: string) => {
    setDrawerOpen(false);
    setPage(next);
    window.requestAnimationFrame(() => pageTitleRef.current?.focus());
  };
  const closeDrawer = () => {
    setDrawerOpen(false);
    window.requestAnimationFrame(() => menuButtonRef.current?.focus());
  };
  const registerOverviewStatisticsRefresh = useCallback<RegisterChildRefresh>((handler) => {
    overviewStatisticsRefresh.current = handler;
    return () => {
      if (overviewStatisticsRefresh.current === handler) {
        overviewStatisticsRefresh.current = null;
      }
    };
  }, []);
  const refreshCurrentPage = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      let ok: boolean;
      if (page === "overview") {
        const [rootOK, statisticsOK] = await Promise.all([
          load(),
          overviewStatisticsRefresh.current?.() || Promise.resolve(true),
        ]);
        ok = rootOK && statisticsOK;
      } else {
        ok = await (refreshHandlers.current.get(page) || load)();
      }
      notify(ok ? "数据已刷新" : "部分数据未能刷新", ok ? "success" : "warning");
    } catch (error) {
      if ((error as Error).message === "AUTH") onLogout();
      else notify((error as Error).message, "error");
    } finally {
      setRefreshing(false);
    }
  };
  return (
    <div className="app-shell">
      <aside id="console-navigation" className={drawerOpen ? "is-open" : ""} aria-label="控制台导航">
        <div className="brand">
          <div className="brand-icon">R</div>
          <div>
            <strong>Relay Desk</strong>
            <small>FREE MODEL GATEWAY</small>
          </div>
          <button ref={drawerCloseRef} className="icon-btn drawer-close" type="button" aria-label="关闭导航" title="关闭导航" onClick={closeDrawer}>
            <X size={17} />
          </button>
        </div>
        <div className="nav-label">CONTROL ROOM</div>
        <nav>
          {nav.map(([id, zh, en, , Icon]) => (
            <button
              key={id}
              className={page === id ? "active" : ""}
              aria-current={page === id ? "page" : undefined}
              onClick={() => navigate(id)}
            >
              <Icon size={17} />
              <span>
                <b>{zh}</b>
                <small>{en}</small>
              </span>
              {page === id && <ChevronRight size={15} />}
            </button>
          ))}
        </nav>
        <div className="aside-note">
          <span className="pulse" />
          网关在线
          <br />
          <small>SQLite · 密钥加密存储</small>
        </div>
        <button className="logout" onClick={onLogout}>
          <LogOut size={16} />
          退出
        </button>
      </aside>
      {drawerOpen && <button className="drawer-backdrop" type="button" aria-label="关闭导航" onClick={closeDrawer} />}
      <main className="main">
        <header className="console-header">
          <div className="header-leading">
            <button
              ref={menuButtonRef}
              className="icon-btn menu-button"
              type="button"
              aria-label="打开导航"
              title="打开导航"
              aria-controls="console-navigation"
              aria-expanded={drawerOpen}
              onClick={() => setDrawerOpen(true)}
            >
              <Menu size={18} />
            </button>
            <div className="header-copy">
            <div className="crumb">
              CONTROL ROOM <ChevronRight size={13} />{" "}
              {currentPage[2].toUpperCase()}
            </div>
              <h1 ref={pageTitleRef} tabIndex={-1}>{currentPage[1]}</h1>
              <p>{currentPage[3]}</p>
            </div>
          </div>
          <div className="header-actions">
            <a className="console-status-link" href="/status" title="打开公开模型状态页">
              <Radio size={14} />
              <span>状态页</span>
            </a>
            <span className="endpoint">
              <span className="dot green" /> :8080 /v1
            </span>
            <button
              className="icon-btn"
              title="刷新数据"
              aria-label="刷新数据"
              disabled={refreshing}
              onClick={() => void refreshCurrentPage()}
            >
              <RefreshCw className={refreshing ? "spin" : ""} size={17} />
            </button>
          </div>
        </header>
        {page === "overview" && (
          <Overview
            summary={summary}
            models={models}
            status={publicStatus}
            registerStatisticsRefresh={registerOverviewStatisticsRefresh}
          />
        )}{" "}
        {page === "proxies" && (
          <Proxies
            upstream={upstream}
            reload={load}
            notify={notify}
            registerRefresh={registerRefresh}
          />
        )}{" "}
        {page === "models" && (
          <Models
            models={models}
            upstream={upstream}
            reload={load}
            notify={notify}
            registerRefresh={registerRefresh}
          />
        )}{" "}
        {page === "usage" && (
          <Usage
            notify={notify}
            onLogout={onLogout}
            registerRefresh={registerRefresh}
          />
        )}{" "}
        {page === "settings" && (
          <SettingsPage notify={notify} onLogout={onLogout} registerRefresh={registerRefresh} />
        )}{" "}
        {toast && (
          <div className={`toast ${toast.type}`} role={toast.type === "error" ? "alert" : "status"}>
            {toast.type === "success" ? <Check size={15} /> : <TriangleAlert size={15} />}
            {toast.message}
          </div>
        )}
      </main>
    </div>
  );
}

function Stat({
  label,
  value,
  detail,
  accent,
}: {
  label: string;
  value: string;
  detail: string;
  accent?: string;
}) {
  return (
    <div className="stat">
      <div className="stat-top">
        <span>{label}</span>
        <span className="stat-accent" style={{ color: accent }}>
          <ArrowUpRight size={14} />
        </span>
      </div>
      <strong>{value}</strong>
      <small>{detail}</small>
    </div>
  );
}
function Overview({
  summary,
  models,
  status,
  registerStatisticsRefresh,
}: {
  summary: Summary;
  models: Model[];
  status: PublicStatusResponse | null;
  registerStatisticsRefresh: RegisterChildRefresh;
}) {
  const enabledModels = models.filter((model) => model.admin_enabled !== false);
  const success24h = status?.models.reduce((total, model) => total + model.success_24h, 0) || 0;
  const external24h = status?.models.reduce((total, model) => total + model.external_errors_24h, 0) || 0;
  const counted24h = Math.max(0, (status?.summary.requests_24h || 0) - external24h);
  const gatewayState = status?.summary.status || "no_request";
  const gatewayLabel = status ? publicStatusLabels[gatewayState] : "状态未知";
  const successLabel = status ? publicStatusRate(status.summary.success_rate) : "—";
  return (
    <div className="page">
      <section className={`gateway-status-band ${gatewayState}`} aria-label="近 24 小时网关状态">
        <div className="gateway-status-context">
          <span className={`public-status-mark ${gatewayState}`} />
          <div>
            <span className="eyebrow">24H GATEWAY STATUS</span>
            <strong>{gatewayLabel}</strong>
            <small>
              {status
                ? `更新于 ${new Date(status.generated_at).toLocaleTimeString()}`
                : "状态数据暂时不可用"}
            </small>
          </div>
        </div>
        <div className="gateway-status-metrics">
          <div>
            <span>24h 成功率</span>
            <strong>{successLabel}</strong>
            <small>
              {!status
                ? "状态数据暂时不可用"
                : status.summary.success_rate === null
                ? "暂无有效请求"
                : `${fmt(success24h)} / ${fmt(counted24h)} 有效请求`}
            </small>
          </div>
          <div>
            <span>24h 请求</span>
            <strong>{status ? fmt(status.summary.requests_24h) : "—"}</strong>
            <small>{fmt(external24h)} 次外部异常</small>
          </div>
        </div>
      </section>
      <section className="stat-grid">
        <Stat
          label="总请求数"
          value={fmt(summary.requests)}
          detail="全历史累计"
          accent="#c9684a"
        />
        <Stat
          label="当日 Token"
          value={fmt(summary.total_tokens)}
          detail={`${fmt(summary.prompt_tokens)} 输入 · ${fmt(summary.completion_tokens)} 输出`}
          accent="#416b5a"
        />
        <Stat
          label="免费模型"
          value={fmt(summary.free_models)}
          detail="网关可用"
          accent="#c49b3a"
        />
        <Stat
          label="可用代理"
          value={summary.effective_proxy_engine === "resin" ? "RESIN" : fmt(summary.active_proxies)}
          detail="参与轮换"
          accent="#5d78a4"
        />
      </section>
      <DashboardStatistics registerRefresh={registerStatisticsRefresh} />
      <section className="panel free-models-panel">
        <div className="panel-head">
          <div>
            <span className="eyebrow">ROUTE DIRECTORY</span>
            <h3>免费模型</h3>
            <p className="muted">当前允许客户端调用的 Free 模型。</p>
          </div>
          <span className="count-chip">{enabledModels.length} 个可用</span>
        </div>
        {enabledModels.length === 0 ? (
          <Empty text="还没有可用的 Free 模型" />
        ) : (
          <div className="model-list model-list-wide">
            {enabledModels.slice(0, 8).map((model) => (
              <div className="model-row" key={model.model_id}>
                <span className="model-swatch">✦</span>
                <div>
                  <b>{model.model_id}</b>
                  <small>
                    {freeReasonText(model.free_reason)} · 更新于{" "}
                    {new Date(model.refreshed_at).toLocaleDateString()}
                  </small>
                </div>
                <span className="status-tag">可用</span>
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

const statsWindows: Array<{ value: StatsWindow; label: string }> = [
  { value: "1h", label: "1 小时" },
  { value: "24h", label: "24 小时" },
  { value: "7d", label: "7 天" },
  { value: "30d", label: "30 天" },
];

function DashboardStatistics({ registerRefresh }: { registerRefresh: RegisterChildRefresh }) {
  const initialWindow = new URLSearchParams(location.search).get("stats_window");
  const [windowName, setWindowName] = useState<StatsWindow>(
    statsWindows.some((option) => option.value === initialWindow) ? initialWindow as StatsWindow : "24h",
  );
  const [modelStats, setModelStats] = useState<DimensionStat[]>([]);
  const [proxyStats, setProxyStats] = useState<DimensionStat[]>([]);
  const [error, setError] = useState("");
  const load = useCallback(async () => {
    const [modelsResult, proxiesResult] = await Promise.allSettled([
      api(`/api/stats/models?window=${windowName}`),
      api(`/api/stats/proxies?window=${windowName}`),
    ]);
    const modelsOK = modelsResult.status === "fulfilled" && Array.isArray(modelsResult.value);
    const proxiesOK = proxiesResult.status === "fulfilled" && Array.isArray(proxiesResult.value);
    if (modelsOK) setModelStats(modelsResult.value as DimensionStat[]);
    if (proxiesOK) setProxyStats(proxiesResult.value as DimensionStat[]);
    const failed = [modelsOK, proxiesOK].filter((ok) => !ok);
    setError(failed.length ? "部分维度统计暂时不可用" : "");
    return failed.length === 0;
  }, [windowName]);
  useEffect(() => {
    const url = new URL(location.href);
    url.searchParams.set("stats_window", windowName);
    history.replaceState(null, "", url);
    void load();
  }, [load, windowName]);
  useEffect(() => registerRefresh(load), [load, registerRefresh]);
  useEffect(() => {
    const refreshWhenVisible = () => {
      if (document.visibilityState === "visible") void load();
    };
    const timer = window.setInterval(refreshWhenVisible, 10_000);
    document.addEventListener("visibilitychange", refreshWhenVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refreshWhenVisible);
    };
  }, [load]);
  const dimensionTable = (title: string, stats: DimensionStat[], empty: string) => (
    <div className="dimension-table-wrap">
      <h3>{title}</h3>
      <div className="dimension-table">
        <div className="dimension-head">
          <span>对象</span><span>请求 / 成功率</span><span>Token</span><span>总耗时 P50 / P95</span><span>首 Token P50 / P95</span>
        </div>
        {stats.slice(0, 8).map((stat) => (
          <div className="dimension-row" key={stat.name}>
            <b title={stat.name}>{stat.name}</b>
            <span>{fmt(stat.requests)} / {(stat.success_rate * 100).toFixed(1)}%</span>
            <span>{fmt(stat.tokens)}</span>
            <span>{formatDuration(stat.p50_latency_ms)} / {formatDuration(stat.p95_latency_ms)}</span>
            <span>{formatDuration(stat.p50_first_token_latency_ms)} / {formatDuration(stat.p95_first_token_latency_ms)}</span>
          </div>
        ))}
        {!stats.length && <Empty text={empty} />}
      </div>
    </div>
  );
  return (
    <section className="panel dashboard-stats-panel">
      <div className="panel-head">
        <div><span className="eyebrow">SERVICE WINDOWS</span><h3>路径与模型表现</h3><p className="muted">用户错误与外部异常分开计入成功率。</p></div>
        <div className="time-segments stats-window" role="group" aria-label="仪表盘时间窗口">
          {statsWindows.map((option) => <button key={option.value} type="button" className={windowName === option.value ? "active" : ""} onClick={() => setWindowName(option.value)}>{option.label}</button>)}
        </div>
      </div>
      {error && <p className="stats-warning">{error}</p>}
      <div className="dimension-scroll">{dimensionTable("按模型", modelStats, "所选时间窗口内没有模型请求")}</div>
      <div className="dimension-scroll">{dimensionTable("按代理", proxyStats, "所选时间窗口内没有代理请求")}</div>
    </section>
  );
}

function Proxies({
  upstream,
  reload,
  notify,
  registerRefresh = noopRegisterRefresh,
}: {
  upstream: any;
  reload: () => Promise<unknown>;
  notify: (v: string, type?: ToastType) => void;
  registerRefresh?: RegisterRefresh;
}) {
  const [text, setText] = useState("");
  const [expiry, setExpiry] = useState("0");
  const [customExpiry, setCustomExpiry] = useState("");
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const [proxyPage, setProxyPage] = useState(1);
  const [pageSize, setPageSize] = useState(50);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<number[]>([]);
  const [busy, setBusy] = useState(false);
  const [proxyState, setProxyState] = useState<ProxyFilterState>("all");
  const [sessionLimit, setSessionLimit] = useState(
    String(upstream.session_proxy_request_limit ?? 50),
  );
  const [engineConfig, setEngineConfig] = useState<ProxyEngine>({
    engine: "builtin",
    effective_engine: "builtin",
    resin_gateway_url: "",
    resin_platform: "Default",
    has_resin_proxy_token: false,
    resin_configured: false,
    resin_fallback_active: false,
  });
  const [resinGatewayURL, setResinGatewayURL] = useState("");
  const [resinPlatform, setResinPlatform] = useState("Default");
  const [resinToken, setResinToken] = useState("");
  const [resinTokenDirty, setResinTokenDirty] = useState(false);
  const [resinResult, setResinResult] = useState<any>(null);
  const [importResults, setImportResults] = useState<ImportResult[]>([]);
  const [probeJob, setProbeJob] = useState<ProbeJob | null>(null);
  const loadProxies = useCallback(
    async (requestedPage: number, requestedPageSize: number) => {
      setLoading(true);
      try {
        const params = new URLSearchParams({
          page: String(requestedPage),
          page_size: String(requestedPageSize),
          state: proxyState,
        });
        const data = (await api(
          `/api/proxies?${params.toString()}`,
        )) as ProxyPage;
        setProxies(data.items);
        setProxyPage(data.page);
        setPageSize(data.page_size);
        setTotal(data.total);
        setTotalPages(data.total_pages);
        setSelected((current) =>
          current.filter((id) => data.items.some((proxy) => proxy.id === id)),
        );
        return true;
      } catch (e) {
        notify((e as Error).message, "error");
        return false;
      } finally {
        setLoading(false);
      }
    },
    [notify, proxyState],
  );
  const loadEngine = useCallback(async () => {
    try {
      const next = (await api("/api/settings/proxy-engine")) as ProxyEngine;
      setEngineConfig(next);
      setResinGatewayURL(next.resin_gateway_url || "");
      setResinPlatform(next.resin_platform || "Default");
      setResinToken("");
      setResinTokenDirty(false);
      return true;
    } catch (e) {
      notify((e as Error).message, "error");
      return false;
    }
  }, [notify]);
  const refreshAll = useCallback(async () => {
    const results = await Promise.all([
      reload(),
      loadProxies(proxyPage, pageSize),
      loadEngine(),
    ]);
    return results[1] && results[2] && results[0] !== false;
  }, [loadEngine, loadProxies, pageSize, proxyPage, reload]);
  useEffect(() => registerRefresh("proxies", refreshAll), [refreshAll, registerRefresh]);
  useEffect(() => {
    void loadProxies(proxyPage, pageSize);
    void loadEngine();
  }, [loadEngine, loadProxies, pageSize, proxyPage]);
  useEffect(
    () => setSessionLimit(String(upstream.session_proxy_request_limit ?? 50)),
    [upstream.session_proxy_request_limit],
  );
  useEffect(() => {
    const refreshWhenVisible = () => {
      if (document.visibilityState !== "visible") return;
      void loadProxies(proxyPage, pageSize);
      void loadEngine();
    };
    const timer = window.setInterval(refreshWhenVisible, 30_000);
    document.addEventListener("visibilitychange", refreshWhenVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refreshWhenVisible);
    };
  }, [loadEngine, loadProxies, pageSize, proxyPage]);
  const refreshAfterMutation = async (requestedPage = proxyPage) => {
    await reload();
    if (requestedPage !== proxyPage) {
      setProxyPage(requestedPage);
      return;
    }
    await loadProxies(requestedPage, pageSize);
  };
  const importIt = async () => {
    if (!text.trim()) return;
    const payload: any = { text };
    if (expiry === "custom") {
      if (!customExpiry) {
        notify("请选择到期时间");
        return;
      }
      payload.expires_at = new Date(customExpiry).toISOString();
    } else if (expiry !== "0") {
      payload.expires_in_days = Number(expiry);
    }
    setBusy(true);
    try {
      const d = await api("/api/proxies/import", {
        method: "POST",
        body: JSON.stringify(payload),
      });
      const ok = d.results.filter((x: any) => x.status === "imported").length;
      setImportResults(d.results as ImportResult[]);
      const failed = d.results.length - ok;
      notify(
        `已导入 ${ok} 个代理${failed ? `，${failed} 行需处理` : ""}${expiry === "0" ? "" : "，已设置有效期"}`,
        failed ? "warning" : "success",
      );
      setText("");
      await refreshAfterMutation(1);
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const toggle = async (p: Proxy) => {
    setBusy(true);
    try {
      await api(`/api/proxies/${p.id}`, {
        method: "PATCH",
        body: JSON.stringify({ enabled: !p.enabled }),
      });
      await refreshAfterMutation();
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const remove = async (p: Proxy) => {
    if (!confirm(`删除 ${p.uri}？`)) return;
    setBusy(true);
    try {
      await api(`/api/proxies/${p.id}`, { method: "DELETE" });
      notify("代理已删除");
      await refreshAfterMutation();
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const toggleSelected = (id: number) =>
    setSelected((current) =>
      current.includes(id) ? current.filter((x) => x !== id) : [...current, id],
    );
  const allSelected =
    proxies.length > 0 && proxies.every((proxy) => selected.includes(proxy.id));
  const bulkDelete = async () => {
    if (!selected.length || !confirm(`删除选中的 ${selected.length} 个代理？`))
      return;
    setBusy(true);
    try {
      const d = await api("/api/proxies/bulk-delete", {
        method: "POST",
        body: JSON.stringify({ ids: selected }),
      });
      notify(`已删除 ${d.deleted} 个代理`);
      setSelected([]);
      await refreshAfterMutation();
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const probeCurrentPage = async () => {
    setBusy(true);
    try {
      const job = (await api("/api/proxy-probes", {
        method: "POST",
        body: JSON.stringify({ ids: proxies.map((proxy) => proxy.id), mode: "both" }),
      })) as ProbeJob;
      setProbeJob(job);
      notify("代理探测已开始");
    } catch (error) {
      notify((error as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  useEffect(() => {
    if (!probeJob || probeJob.status !== "running") return;
    const poll = async () => {
      if (document.visibilityState !== "visible") return;
      try {
        const next = (await api(`/api/proxy-probes/${probeJob.id}`)) as ProbeJob;
        setProbeJob(next);
        if (next.status === "completed") {
          notify("代理探测已完成", next.results.some((result) => !result.exit_ok) ? "warning" : "success");
          void loadProxies(proxyPage, pageSize);
        }
      } catch (error) {
        notify((error as Error).message, "error");
      }
    };
    const timer = window.setInterval(poll, 1200);
    document.addEventListener("visibilitychange", poll);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", poll);
    };
  }, [loadProxies, notify, pageSize, probeJob, proxyPage]);
  const saveEngine = async (nextEngine = engineConfig.engine) => {
    setBusy(true);
    try {
      const payload: Record<string, unknown> = {
        engine: nextEngine,
        resin_gateway_url: resinGatewayURL,
        resin_platform: resinPlatform,
      };
      if (resinTokenDirty) payload.resin_proxy_token = resinToken;
      const next = (await api("/api/settings/proxy-engine", {
        method: "PUT",
        body: JSON.stringify(payload),
      })) as ProxyEngine;
      setEngineConfig(next);
      setResinToken("");
      setResinTokenDirty(false);
      setResinResult(null);
      await reload();
      notify(nextEngine === "resin" ? "已启用 Resin 网关" : "已启用内置代理池");
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const testResin = async () => {
    setBusy(true);
    try {
      const result = await api("/api/settings/proxy-engine/resin/test", { method: "POST", body: "{}" });
      setResinResult(result);
      await loadEngine();
      await reload();
      notify("Resin 网关测试通过");
    } catch (e) {
      setResinResult({ ok: false, error: (e as Error).message });
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const proxyStatus = (proxy: Proxy) => {
    if (proxy.usage_state === "cooldown" || proxy.health_status === "cooldown")
      return { label: "冷却中", className: "cooldown" };
    if (proxy.usage_state === "in_use")
      return { label: "使用中", className: "in-use" };
    if (!proxy.enabled) return { label: "已禁用", className: "disabled" };
    return { label: "就绪", className: "healthy" };
  };
  return (
    <div className="page">
      <section className="panel engine-panel">
        <div className="panel-head">
          <div>
            <div className="eyebrow">ROUTE ENGINE</div>
            <h3>路由引擎</h3>
          </div>
          <Network size={20} />
        </div>
        <div className="segmented" role="group" aria-label="路由引擎">
          <button
            type="button"
            className={engineConfig.engine === "builtin" ? "active" : ""}
            disabled={busy}
            onClick={() => void saveEngine("builtin")}
          >
            内置代理池
          </button>
          <button
            type="button"
            className={engineConfig.engine === "resin" ? "active" : ""}
            disabled={busy}
            onClick={() => {
              if (engineConfig.engine === "resin") return;
              setEngineConfig((current) => ({ ...current, engine: "resin" }));
            }}
          >
            Resin 网关
          </button>
        </div>
        {engineConfig.engine === "resin" && (
          <div className="resin-config">
            <div className="config-grid">
              <label>
                Resin 网关 URL
                <input
                  value={resinGatewayURL}
                  onChange={(event) => setResinGatewayURL(event.target.value)}
                  placeholder="http://host.docker.internal:2260"
                  autoComplete="off"
                />
              </label>
              <label>
                平台
                <input
                  value={resinPlatform}
                  onChange={(event) => setResinPlatform(event.target.value)}
                  placeholder="Default"
                  autoComplete="off"
                />
              </label>
              <label className="resin-token-field">
                代理令牌
                <input
                  type="password"
                  value={resinToken}
                  onChange={(event) => {
                    setResinToken(event.target.value);
                    setResinTokenDirty(true);
                  }}
                  placeholder={engineConfig.has_resin_proxy_token ? "已安全保存" : "Resin 未启用认证时可留空"}
                  autoComplete="new-password"
                />
              </label>
            </div>
            <div className="config-actions">
              <button className="primary" type="button" disabled={busy} onClick={() => void saveEngine("resin")}>
                <Network size={16} />
                保存 Resin 配置
              </button>
              <button className="secondary" type="button" disabled={busy || !engineConfig.resin_configured} onClick={() => void testResin()}>
                测试网关
              </button>
            </div>
            <div className={`resin-state ${engineConfig.resin_health === "degraded" ? "error-line" : "muted"}`}>
              Resin 状态：{engineConfig.resin_health === "healthy" ? "健康" : engineConfig.resin_health === "degraded" ? "不可达或异常" : "尚未检测"}
              {engineConfig.resin_last_checked_at ? ` · ${new Date(engineConfig.resin_last_checked_at).toLocaleString()}` : ""}
              {engineConfig.resin_last_check_error ? ` · ${engineConfig.resin_last_check_error}` : ""}
            </div>
            {resinResult && (
              <div className={`test-result ${resinResult.ok ? "ok" : "bad"}`}>
                {resinResult.ok ? "网关可用" : resinResult.error || "网关测试失败"}
              </div>
            )}
          </div>
        )}
      </section>
      <section className="panel routing-panel">
        <div className="panel-head">
          <div>
            <h3>会话路由</h3>
            <p className="muted">
              同一会话固定使用一个代理；达到额度、收到上游 429
              或代理网络失败后才切换。
            </p>
          </div>
          <Network size={20} />
        </div>
        <div className="routing-controls">
          <label>
            单会话 / 单代理请求额度
            <input
              type="number"
              min="0"
              max="100000"
              value={sessionLimit}
              onChange={(e) => setSessionLimit(e.target.value)}
            />
            <small className="field-help">
              默认 50。设为 0 时不按请求次数切换，只在上游 429
              或代理故障时切换。
            </small>
          </label>
          <button
            className="secondary"
            disabled={busy}
            onClick={async () => {
              const limit = Number(sessionLimit);
              if (!Number.isInteger(limit) || limit < 0 || limit > 100000) {
                notify("请输入 0 到 100000 之间的整数");
                return;
              }
              setBusy(true);
              try {
                await api("/api/settings/routing", {
                  method: "PUT",
                  body: JSON.stringify({ session_proxy_request_limit: limit }),
                });
                notify("会话路由额度已保存");
                reload();
              } catch (e) {
                notify((e as Error).message, "error");
              } finally {
                setBusy(false);
              }
            }}
          >
            保存额度
          </button>
        </div>
      </section>
      <section className="panel import-panel">
        <div className="panel-head">
          <div>
            <h3>批量导入</h3>
            <p className="muted">
              每行一个代理 URI，例如 http://user:pass@host:8080
            </p>
          </div>
          <Upload size={20} />
        </div>
        <textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={"http://10.0.0.2:8080\nsocks5://user:pass@10.0.0.3:1080"}
        />
        <div className="expiry-controls">
          <label>
            有效期
            <select value={expiry} onChange={(e) => setExpiry(e.target.value)}>
              <option value="0">永久</option>
              <option value="1">1 天</option>
              <option value="7">7 天</option>
              <option value="30">30 天</option>
              <option value="90">90 天</option>
              <option value="custom">自定义到期时间</option>
            </select>
          </label>
          {expiry === "custom" && (
            <label>
              到期时间
              <input
                type="datetime-local"
                value={customExpiry}
                onChange={(e) => setCustomExpiry(e.target.value)}
                min={new Date().toISOString().slice(0, 16)}
              />
            </label>
          )}
        </div>
        <button className="primary" onClick={importIt} disabled={busy}>
          <Upload size={16} />
          {busy ? "导入中…" : "导入代理"}
        </button>
        {importResults.length > 0 && (
          <div className="import-report" aria-live="polite">
            {importResults.map((result) => (
              <div key={`${result.line}-${result.uri}`} className={result.status === "imported" ? "ok" : "bad"}>
                <b>第 {result.line} 行</b>
                <code>{result.uri}</code>
                <span>{result.status === "imported" ? "已导入" : result.status === "duplicate" ? "重复" : result.error || "导入失败"}</span>
                {result.status !== "imported" && <button className="icon-btn" title="复制失败行" onClick={async () => { try { await navigator.clipboard.writeText(result.uri); notify("失败行已复制"); } catch { notify("复制失败", "error"); } }}><Copy size={14} /></button>}
              </div>
            ))}
          </div>
        )}
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h3>当前路径</h3>
            <p className="muted">失败路径会短暂冷却；到期代理会自动删除。</p>
          </div>
          <div className="proxy-actions">
            <label className="proxy-state-filter">
              <span>筛选</span>
              <select
                value={proxyState}
                disabled={loading || busy}
                onChange={(event) => {
                  setProxyState(event.target.value as ProxyFilterState);
                  setProxyPage(1);
                  setSelected([]);
                }}
              >
                <option value="all">全部代理</option>
                <option value="unused">未使用</option>
                <option value="in_use">正在使用</option>
                <option value="cooldown">冷却中</option>
              </select>
            </label>
            <label className="select-all">
              <input
                type="checkbox"
                checked={allSelected}
                onChange={(e) =>
                  setSelected(e.target.checked ? proxies.map((p) => p.id) : [])
                }
                disabled={!proxies.length || loading || busy}
              />
              <span>全选本页</span>
            </label>
            <button
              className="secondary"
              onClick={probeCurrentPage}
              disabled={!proxies.length || busy || loading}
            >
              <Radio size={14} />
              {probeJob?.status === "running" ? `${probeJob.completed}/${probeJob.total}` : "检测本页"}
            </button>
            <button
              className="danger-button"
              onClick={bulkDelete}
              disabled={!selected.length || busy || loading}
            >
              <Trash2 size={14} />
              删除选中
            </button>
            <span className="count-chip">{fmt(total)} 条</span>
          </div>
        </div>
        <div className={`data-table ${loading ? "is-loading" : ""}`}>
          <div className="table-head">
            <span>URI</span>
            <span>状态</span>
            <span>失败次数</span>
            <span>操作</span>
          </div>
          {proxies.map((p) => {
            const status = proxyStatus(p);
            return (
            <div className="table-row" key={p.id}>
              <div className="proxy-uri-cell">
                <input
                  type="checkbox"
                  checked={selected.includes(p.id)}
                  onChange={() => toggleSelected(p.id)}
                  disabled={loading || busy}
                  aria-label={`选择 ${p.uri}`}
                />
                <div>
                  <b className="mono">{p.uri}</b>
                  <small>
                    {p.scheme.toUpperCase()} · {p.host}:{p.port}
                     {p.expires_at
                        ? ` · 到期 ${new Date(p.expires_at).toLocaleString()}`
                        : ""}
                    {p.last_probe_at
                      ? ` · 检测 ${new Date(p.last_probe_at).toLocaleString()}${p.last_exit_ip ? ` · ${p.last_exit_ip}` : ""}`
                      : " · 未检测"}
                    {p.last_probe_error ? ` · ${p.last_probe_error}` : ""}
                  </small>
                </div>
              </div>
              <span className={`health ${status.className}`}>
                <i />
                {status.label}
              </span>
              <span>{p.failure_count}</span>
              <div className="row-actions">
                <button
                  className="text-btn"
                  disabled={busy || loading}
                  onClick={() => toggle(p)}
                >
                  {p.enabled ? "禁用" : "启用"}
                </button>
                <button
                  className="danger-icon"
                  title="删除"
                  disabled={busy || loading}
                  onClick={() => remove(p)}
                >
                  <Trash2 size={15} />
                </button>
              </div>
            </div>
            );
          })}
          {loading && proxies.length === 0 && <Empty text="正在加载代理…" />}
          {!loading && proxies.length === 0 && (
            <Empty text="先导入第一条代理路径" />
          )}
        </div>
        <div className="proxy-pagination" aria-label="代理分页">
          <span className="pagination-range" aria-live="polite">
            {total === 0
              ? "0 / 0"
              : `${(proxyPage - 1) * pageSize + 1}-${Math.min(proxyPage * pageSize, total)} / ${fmt(total)}`}
          </span>
          <label className="page-size-control">
            <span>每页</span>
            <select
              value={pageSize}
              disabled={loading}
              onChange={(e) => {
                setPageSize(Number(e.target.value));
                setProxyPage(1);
              }}
            >
              <option value={50}>50</option>
              <option value={100}>100</option>
              <option value={200}>200</option>
            </select>
          </label>
          <div className="page-navigation">
            <button
              className="icon-btn"
              type="button"
              title="上一页"
              aria-label="上一页"
              disabled={loading || proxyPage <= 1}
              onClick={() =>
                setProxyPage((current) => Math.max(1, current - 1))
              }
            >
              <ChevronLeft size={16} />
            </button>
            <span className="page-indicator">
              第 {proxyPage} / {totalPages} 页
            </span>
            <button
              className="icon-btn"
              type="button"
              title="下一页"
              aria-label="下一页"
              disabled={loading || proxyPage >= totalPages}
              onClick={() =>
                setProxyPage((current) => Math.min(totalPages, current + 1))
              }
            >
              <ChevronRight size={16} />
            </button>
          </div>
        </div>
      </section>
    </div>
  );
}

function ClientKeysPanel({
  notify,
  registerRefresh,
}: {
  notify: (value: string, type?: ToastType) => void;
  registerRefresh?: RegisterChildRefresh;
}) {
  const [keys, setKeys] = useState<ClientKey[]>([]);
  const [name, setName] = useState("");
  const [rpm, setRPM] = useState("0");
  const [tpm, setTPM] = useState("0");
  const [expiresAt, setExpiresAt] = useState("");
  const [visibleKeys, setVisibleKeys] = useState<Record<number, string>>({});
  const [busy, setBusy] = useState(false);
  const load = useCallback(async () => {
    try {
      const next = (await api("/api/client-keys")) as ClientKey[];
      setKeys(next);
      setVisibleKeys((current) =>
        Object.fromEntries(
          Object.entries(current).filter(([id]) => next.some((key) => key.id === Number(id))),
        ),
      );
      return true;
    } catch (error) {
      notify((error as Error).message, "error");
      return false;
    }
  }, [notify]);
  useEffect(() => void load(), [load]);
  useEffect(() => registerRefresh?.(load), [load, registerRefresh]);
  const create = async () => {
    const rpmLimit = Number(rpm);
    const tpmLimit = Number(tpm);
    if (!name.trim() || !Number.isInteger(rpmLimit) || !Number.isInteger(tpmLimit)) {
      notify("请填写 Key 名称和整数限额", "warning");
      return;
    }
    setBusy(true);
    try {
      const data = await api("/api/client-keys", { method: "POST", body: JSON.stringify({ name, expires_at: expiresAt ? new Date(expiresAt).toISOString() : "", rpm_limit: rpmLimit, tpm_limit: tpmLimit }) });
      setName("");
		setExpiresAt("");
      await load();
		setVisibleKeys((current) => ({ ...current, [data.id]: data.client_key }));
      notify("客户端 Key 已创建");
    } catch (error) {
      notify((error as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const update = async (key: ClientKey, patch: Partial<ClientKey>) => {
    setBusy(true);
    try {
      await api(`/api/client-keys/${key.id}`, { method: "PATCH", body: JSON.stringify(patch) });
      await load();
    } catch (error) {
      notify((error as Error).message, "error");
    } finally { setBusy(false); }
  };
  const rotate = async (key: ClientKey) => {
    if (!confirm(`轮换 ${key.name} 后旧 Key 会失效，继续？`)) return;
	setBusy(true);
    try {
      const data = await api(`/api/client-keys/${key.id}/rotate`, { method: "POST" });
		setVisibleKeys((current) => ({ ...current, [key.id]: data.client_key }));
      notify("新 Key 已生成，请立即复制");
      await load();
	} catch (error) { notify((error as Error).message, "error"); } finally { setBusy(false); }
  };
  const reveal = async (key: ClientKey) => {
	if (visibleKeys[key.id]) {
		setVisibleKeys((current) => {
			const next = { ...current };
			delete next[key.id];
			return next;
		});
		return;
	}
	setBusy(true);
	try {
		const data = await api(`/api/client-keys/${key.id}/secret`);
		setVisibleKeys((current) => ({ ...current, [key.id]: data.client_key }));
	} catch (error) {
		notify((error as Error).message, "error");
	} finally {
		setBusy(false);
	}
  };
  const copySecret = async (key: ClientKey) => {
	setBusy(true);
	try {
		const secret = visibleKeys[key.id] || (await api(`/api/client-keys/${key.id}/secret`)).client_key;
		if (!navigator.clipboard) throw new Error("剪贴板不可用");
		await navigator.clipboard.writeText(secret);
		notify("客户端 Key 已复制");
	} catch (error) {
		notify((error as Error).message, "error");
	} finally {
		setBusy(false);
	}
  };
  const remove = async (key: ClientKey) => {
    if (!confirm(`删除 ${key.name} 后无法再使用该 Key，继续？`)) return;
    setBusy(true);
    try {
      await api(`/api/client-keys/${key.id}`, { method: "DELETE" });
      await load();
      notify("客户端 Key 已删除");
    } catch (error) {
      notify((error as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  return <section className="panel client-keys-panel" id="client-keys">
    <div className="panel-head"><div><span className="eyebrow">CLIENT ACCESS</span><h3>客户端 Key</h3><p className="muted">每个客户端可独立撤销、限流和审计。Key 以加密方式保存，仅管理员可按需查看。</p></div><UsersRound size={20} /></div>
    <div className="key-create-grid">
      <label>名称<input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如 CI" /></label>
      <label>RPM<input type="number" min="0" value={rpm} onChange={(event) => setRPM(event.target.value)} /></label>
      <label>TPM<input type="number" min="0" value={tpm} onChange={(event) => setTPM(event.target.value)} /></label>
		<label>到期时间<input type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} /></label>
      <button className="secondary" onClick={create} disabled={busy}>创建 Key</button>
    </div>
    <div className="key-list">
      {keys.map((key) => <div className="key-entry" key={key.id}>
        <div className="key-row">
          <div><b>{key.name}</b><small>{key.hint} · {key.rpm_limit || "∞"} RPM · {key.tpm_limit || "∞"} TPM{key.expires_at ? ` · 到期 ${new Date(key.expires_at).toLocaleString()}` : ""}{key.last_used_at ? ` · 最近使用 ${new Date(key.last_used_at).toLocaleString()}` : ""}</small></div>
          <span className={`health ${key.enabled ? "healthy" : "disabled"}`}><i />{key.enabled ? "启用" : "已停用"}</span>
          <span className={`reason key-recovery-state ${key.recoverable ? "recoverable" : "unrecoverable"}`}>{key.recoverable ? "可查看" : "旧 Key 无法查看"}</span>
          <label className="maintenance-key-toggle" title="Only allow this key through the gateway during maintenance"><input type="checkbox" checked={key.maintenance_allowed} onChange={(event) => void update(key, { maintenance_allowed: event.target.checked })} disabled={busy || !key.enabled} /><span>维护放行</span></label>
          <div className="row-actions"><button className="icon-btn" title={key.enabled ? "停用 Key" : "启用 Key"} onClick={() => void update(key, { enabled: !key.enabled })} disabled={busy}><KeyRound size={15} /></button><button className="icon-btn" title={key.recoverable ? (visibleKeys[key.id] ? "隐藏 Key" : "显示 Key") : "旧 Key 无法查看，请轮换"} onClick={() => void reveal(key)} disabled={busy || !key.recoverable}>{visibleKeys[key.id] ? <EyeOff size={15} /> : <Eye size={15} />}</button><button className="icon-btn" title={key.recoverable ? "复制 Key" : "旧 Key 无法复制，请轮换"} onClick={() => void copySecret(key)} disabled={busy || !key.recoverable}><Copy size={15} /></button><button className="icon-btn" title="轮换 Key" onClick={() => void rotate(key)} disabled={busy}><RefreshCw size={15} /></button>{keys.length > 1 && <button className="icon-btn" title="删除 Key" onClick={() => void remove(key)} disabled={busy}><Trash2 size={15} /></button>}</div>
        </div>
        {visibleKeys[key.id] && <div className="copy-field key-secret"><code>{visibleKeys[key.id]}</code><button className="icon-btn" title="复制 Key" onClick={() => void copySecret(key)} disabled={busy}><Copy size={15} /></button><button className="icon-btn" title="隐藏 Key" onClick={() => void reveal(key)} disabled={busy}><EyeOff size={15} /></button></div>}
      </div>)}
    </div>
  </section>;
}

function ModelAliasesPanel({
  models,
  notify,
  registerRefresh,
}: {
  models: Model[];
  notify: (value: string, type?: ToastType) => void;
  registerRefresh?: RegisterChildRefresh;
}) {
  const [aliases, setAliases] = useState<ModelAlias[]>([]);
  const [alias, setAlias] = useState("");
  const [target, setTarget] = useState("");
  const load = useCallback(async () => {
    try {
      setAliases((await api("/api/model-aliases")) as ModelAlias[]);
      return true;
    } catch (error) {
      notify((error as Error).message, "error");
      return false;
    }
  }, [notify]);
  useEffect(() => void load(), [load]);
  useEffect(() => registerRefresh?.(load), [load, registerRefresh]);
  const create = async () => {
    try { await api("/api/model-aliases", { method: "POST", body: JSON.stringify({ alias, target_model_id: target }) }); setAlias(""); await load(); notify("模型别名已保存"); } catch (error) { notify((error as Error).message, "error"); }
  };
  return <section className="panel alias-panel">
    <div className="panel-head"><div><span className="eyebrow">STABLE ROUTES</span><h3>模型别名</h3><p className="muted">别名始终映射到一个已启用的 Free 模型。</p></div><Database size={20} /></div>
    <div className="alias-create-grid"><label>别名<input value={alias} onChange={(event) => setAlias(event.target.value)} placeholder="free-fast" /></label><label>目标模型<select value={target} onChange={(event) => setTarget(event.target.value)}><option value="">选择模型</option>{models.filter((model) => model.admin_enabled !== false).map((model) => <option key={model.model_id} value={model.model_id}>{model.model_id}</option>)}</select></label><button className="secondary" onClick={create} disabled={!alias || !target}>添加别名</button></div>
    <div className="key-list">{aliases.map((item) => <div className="key-row" key={item.id}><div><b>{item.alias}</b><small>→ {item.target_model_id}</small></div><span className={`health ${item.enabled ? "healthy" : "disabled"}`}><i />{item.enabled ? "启用" : "已停用"}</span><div className="row-actions"><button className="icon-btn" title={item.enabled ? "停用别名" : "启用别名"} onClick={async () => { try { await api(`/api/model-aliases/${item.id}`, { method: "PATCH", body: JSON.stringify({ enabled: !item.enabled }) }); await load(); } catch (error) { notify((error as Error).message, "error"); } }}><KeyRound size={15} /></button><button className="icon-btn" title="删除别名" onClick={async () => { try { await api(`/api/model-aliases/${item.id}`, { method: "DELETE" }); await load(); } catch (error) { notify((error as Error).message, "error"); } }}><Trash2 size={15} /></button></div></div>)}</div>
  </section>;
}

function ForceFreeModelsPanel({
  models,
  notify,
  reload,
  registerRefresh,
}: {
  models: Model[];
  notify: (v: string, type?: ToastType) => void;
  reload: () => Promise<unknown>;
  registerRefresh?: RegisterChildRefresh;
}) {
  const [items, setItems] = useState<ForceFreeModel[]>([]);
  const [modelID, setModelID] = useState("");
  const [busy, setBusy] = useState(false);
  const load = useCallback(async () => {
    try {
      setItems((await api("/api/model-overrides/free")) as ForceFreeModel[]);
      return true;
    } catch (error) {
      notify((error as Error).message, "error");
      return false;
    }
  }, [notify]);
  useEffect(() => void load(), [load]);
  useEffect(() => registerRefresh?.(load), [load, registerRefresh]);
  const add = async () => {
    const value = modelID.trim();
    if (!value) return;
    setBusy(true);
    try {
      await api("/api/model-overrides/free", {
        method: "POST",
        body: JSON.stringify({ model_id: value }),
      });
      setModelID("");
      await Promise.all([load(), reload()]);
      notify("已将模型标记为免费");
    } catch (error) {
      notify((error as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const remove = async (value: string) => {
    setBusy(true);
    try {
      await api("/api/model-overrides/free", {
        method: "DELETE",
        body: JSON.stringify({ model_id: value }),
      });
      await Promise.all([load(), reload()]);
      notify("已恢复自动模型识别");
    } catch (error) {
      notify((error as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const available = new Set(models.map((model) => model.model_id));
  return <section className="panel force-free-panel">
    <div className="panel-head">
      <div>
        <span className="eyebrow">FREE MODEL OVERRIDES</span>
        <h3>强制免费模型</h3>
        <p className="muted">按完整模型 ID 固定标记为免费；模型重新被上游发现时自动生效。</p>
      </div>
      <Check size={20} />
    </div>
    <div className="force-free-create-grid">
      <label>
        模型 ID
        <input
          value={modelID}
          onChange={(event) => setModelID(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") void add();
          }}
          placeholder="provider/model-name"
          spellCheck={false}
        />
      </label>
      <button className="secondary" onClick={() => void add()} disabled={busy || !modelID.trim()}>
        添加免费模型
      </button>
    </div>
    {items.length > 0 && <div className="key-list force-free-list">
      {items.map((item) => <div className="key-row" key={item.model_id}>
        <div>
          <b>{item.model_id}</b>
          <small>{available.has(item.model_id) ? "上游已发现，当前可用" : "等待上游模型列表再次发现"}</small>
        </div>
        <span className="reason">手动指定</span>
        <button className="icon-btn" title="删除免费模型标记" onClick={() => void remove(item.model_id)} disabled={busy}>
          <Trash2 size={15} />
        </button>
      </div>)}
    </div>}
  </section>;
}

function Models({
  models,
  upstream,
  reload,
  notify,
  registerRefresh = noopRegisterRefresh,
}: {
  models: Model[];
  upstream: any;
  reload: () => Promise<unknown>;
  notify: (v: string, type?: ToastType) => void;
  registerRefresh?: RegisterRefresh;
}) {
  const [visionBase, setVisionBase] = useState(upstream.vision_base_url || "");
  const [visionKey, setVisionKey] = useState("");
  const [visionModel, setVisionModel] = useState(upstream.vision_model || "");
  const [visionUseProxy, setVisionUseProxy] = useState(
    upstream.vision_use_proxy !== false,
  );
  const [busy, setBusy] = useState(false);
  const [testResult, setTestResult] = useState<any>(null);
  const childRefreshers = useRef(new Set<RefreshHandler>());
  useEffect(() => {
    setVisionBase(upstream.vision_base_url || "");
    setVisionModel(upstream.vision_model || "");
    setVisionUseProxy(upstream.vision_use_proxy !== false);
  }, [
    upstream.vision_base_url,
    upstream.vision_model,
    upstream.vision_use_proxy,
  ]);
  const registerChildRefresh = useCallback<RegisterChildRefresh>((handler) => {
    childRefreshers.current.add(handler);
    return () => childRefreshers.current.delete(handler);
  }, []);
  const refreshSavedData = useCallback(async () => {
    const results = await Promise.all([
      reload(),
      ...Array.from(childRefreshers.current, (refresh) => refresh()),
    ]);
    return results.every((result) => result !== false);
  }, [reload]);
  useEffect(() => registerRefresh("models", refreshSavedData), [refreshSavedData, registerRefresh]);
  const save = async () => {
    setBusy(true);
    try {
      await api("/api/settings/upstream", {
        method: "PUT",
        body: JSON.stringify({
          vision_base_url: visionBase,
          vision_api_key: visionKey,
          vision_model: visionModel,
          vision_use_proxy: visionUseProxy,
        }),
      });
      notify("辅助模型配置已保存");
      setVisionKey("");
      reload();
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const refresh = async () => {
    setBusy(true);
    try {
      const d = await api("/api/settings/models/refresh", { method: "POST" });
      notify(d.warning || `已刷新 ${d.free_model_count} 个 Free 模型`);
      reload();
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const testKey = async () => {
    setBusy(true);
    setTestResult(null);
    try {
      const d = await api("/api/settings/upstream/test", { method: "POST" });
      setTestResult(d);
      notify("上游 Key 测试完成");
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  const copy = async () => {
    try {
      if (!navigator.clipboard) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(`${location.origin}/v1`);
      notify("网关地址已复制");
    } catch {
      notify("复制失败", "error");
    }
  };
  const setModelEnabled = async (model: Model) => {
    try {
      await api(`/api/models/${model.id}/policy`, { method: "PATCH", body: JSON.stringify({ enabled: model.admin_enabled === false }) });
      await reload();
      notify(model.admin_enabled === false ? "模型已启用" : "模型已停用");
    } catch (error) {
      notify((error as Error).message, "error");
    }
  };
  return (
    <div className="page">
      <div className="page-actions">
        <button className="primary" onClick={refresh} disabled={busy}>
          <RefreshCw size={16} />
          {busy ? "刷新中…" : "刷新模型"}
        </button>
      </div>
      <section className="config-grid">
        <div className="panel">
          <div className="panel-head">
            <div>
              <h3>上游通道</h3>
              <p className="muted">固定为 OpenCode Zen 匿名免费通道</p>
            </div>
            <Server size={20} />
          </div>
          <div className="lane-facts">
            <div>
              <span>通道</span>
              <code>{upstream.upstream_lane || "zen-anonymous"}</code>
            </div>
            <div>
              <span>聊天端点</span>
              <code>
                {upstream.upstream_endpoint ||
                  "https://opencode.ai/zen/v1/chat/completions"}
              </code>
            </div>
            <div>
              <span>模型目录</span>
              <code>
                {upstream.upstream_models_endpoint ||
                  "https://opencode.ai/zen/v1/models"}
              </code>
            </div>
            <div>
              <span>认证</span>
              <code>{upstream.upstream_authorization || "Bearer public"}</code>
            </div>
          </div>
          <p className="muted header-note">
            地址与认证写死在程序里，不需要也不能填写。请求按 OpenCode CLI
            的形态发出（含固定 User-Agent、<code>x-opencode-client: cli</code>
            和每段会话固定的 <code>ses_</code> 会话号），免费额度按会话与出口 IP
            计算，因此会话号会在整段对话中保持不变。
          </p>
          <div className="config-actions">
            <button className="secondary" onClick={testKey} disabled={busy}>
              测试上游连通性
            </button>
          </div>
          {testResult && (
            <div className="test-result">
              <b>测试模型：{testResult.model}</b>
              <span>
                直连：
                {testResult.direct?.status_code ||
                  testResult.direct?.status}{" "}
                {testResult.direct?.message || ""}
              </span>
              <span>
                代理：
                {testResult.proxy?.status_code || testResult.proxy?.status}{" "}
                {testResult.proxy?.message || ""}
              </span>
            </div>
          )}
          {upstream.last_model_refresh_error && (
            <div className="error-line">
              {upstream.last_model_refresh_error}
            </div>
          )}
        </div>
        <div className="panel gateway-card">
          <div className="eyebrow">CLIENT GATEWAY</div>
          <h3>给 OpenCode 的地址</h3>
          <div className="copy-field">
            <code>{location.origin}/v1</code>
            <button className="icon-btn" title="复制地址" onClick={copy}>
              <Copy size={15} />
            </button>
          </div>
          <p className="muted small">
            将此地址和下方创建的 Client Key 填入 OpenCode。上游由服务器内置，客户端只需要这对地址和 Key。
          </p>
          <button className="text-btn gateway-key-link" type="button" onClick={() => document.getElementById("client-keys")?.scrollIntoView({ behavior: "smooth", block: "start" })}>
            管理客户端 Key
            <ArrowUpRight size={14} />
          </button>
        </div>
      </section>
      <section className="panel vision-helper-panel">
        <div className="panel-head">
          <div>
            <span className="eyebrow">OPTIONAL VISION BRIDGE</span>
            <h3>图片辅助模型</h3>
            <p className="muted">
              使用独立供应商的多模态模型先描述图片，再交给纯文本模型处理。
            </p>
          </div>
          <ImageIcon size={20} />
        </div>
        <div className="vision-grid">
          <label>
            辅助 Base URL
            <input
              value={visionBase}
              onChange={(e) => setVisionBase(e.target.value)}
              placeholder="https://vision-provider.example.com"
            />
          </label>
          <label>
            辅助模型 ID
            <input
              value={visionModel}
              onChange={(e) => setVisionModel(e.target.value)}
              placeholder="provider/vision-model"
            />
          </label>
          <label>
            辅助 API Key
            <input
              type="password"
              value={visionKey}
              onChange={(e) => setVisionKey(e.target.value)}
              placeholder={
                upstream.has_vision_api_key
                  ? "已保存 · 留空保持不变"
                  : "输入辅助供应商 API Key"
              }
            />
          </label>
        </div>
        <label className="vision-proxy-toggle">
          <input
            type="checkbox"
            checked={visionUseProxy}
            onChange={(e) => setVisionUseProxy(e.target.checked)}
          />
          <span>
            <b>使用代理池</b>
            <small>
              {visionUseProxy
                ? "图片辅助请求通过当前选中的代理发送。"
                : "图片辅助请求直接连接供应商；主模型请求仍使用代理池。"}
            </small>
          </span>
        </label>
        <p className="muted vision-helper-note">
          三项同时填写后启用。辅助请求走这里填写的独立供应商，不会使用 Zen
          通道，也不会把图片内容写入使用记录。
        </p>
        <div className="config-actions">
          <button className="secondary" onClick={save} disabled={busy}>
            保存配置
          </button>
        </div>
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h3>免费模型</h3>
            <p className="muted">
              {upstream.last_model_refresh_at
                ? `最近刷新 ${new Date(upstream.last_model_refresh_at).toLocaleString()}`
                : "尚未刷新"}
            </p>
          </div>
          <span className="count-chip">{models.length} 个模型</span>
        </div>
        <div className="model-grid">
          {models.map((m) => (
            <div className="model-card" key={m.model_id}>
              <div className="model-card-top">
                <span className="model-swatch">✦</span>
                <button className={m.admin_enabled === false ? "status-tag muted-tag" : "status-tag"} onClick={() => void setModelEnabled(m)} title={m.admin_enabled === false ? "启用模型" : "停用模型"}>{m.admin_enabled === false ? "已停用" : "可用"}</button>
              </div>
              <b>{m.model_id}</b>
              <small>{m.display_name}</small>
              <span className="reason">
                {freeReasonText(m.free_reason)}
              </span>
            </div>
          ))}
          {models.length === 0 && <Empty text="刷新上游以发现 Free 模型" />}
        </div>
      </section>
      <ForceFreeModelsPanel models={models} notify={notify} reload={reload} registerRefresh={registerChildRefresh} />
      <ModelAliasesPanel models={models} notify={notify} registerRefresh={registerChildRefresh} />
      <ClientKeysPanel notify={notify} registerRefresh={registerChildRefresh} />
    </div>
  );
}

function PasswordCard({ notify }: { notify: (v: string, type?: ToastType) => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    if (next.length < 8) {
      notify("新密码至少需要 8 个字符");
      return;
    }
    setBusy(true);
    try {
      await api("/api/auth/password", {
        method: "POST",
        body: JSON.stringify({ current, new: next }),
      });
      setCurrent("");
      setNext("");
      notify("管理员密码已更新，请重新登录");
      window.setTimeout(() => window.location.reload(), 500);
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="panel security-panel">
      <div className="panel-head">
        <div>
          <span className="eyebrow">SECURITY</span>
          <h3>管理员密码</h3>
          <p className="muted">修改后会撤销所有控制台会话并要求重新登录。</p>
        </div>
        <KeyRound size={20} />
      </div>
      <div className="password-grid">
        <label>
          当前密码
          <input
            type="password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </label>
        <label>
          新密码
          <input
            type="password"
            value={next}
            maxLength={72}
            onChange={(e) => setNext(e.target.value)}
            placeholder="至少 8 个字符"
          />
        </label>
        <button className="secondary" onClick={submit} disabled={busy}>
          更新密码
        </button>
      </div>
    </section>
  );
}

function OperationsSettings({
  notify,
  registerRefresh,
}: {
  notify: (value: string, type?: ToastType) => void;
  registerRefresh: RegisterChildRefresh;
}) {
  const [probes, setProbes] = useState<ProbeSettings>({ enabled: true, exit_minutes: 15, upstream_minutes: 60 });
  const [alerts, setAlerts] = useState<AlertSettings>({ enabled: false, webhook_url: "", has_webhook_secret: false, events: alertEventOptions.map((event) => event.value), low_proxy_threshold: 3, success_rate_percent: 80 });
  const [refreshMinutes, setRefreshMinutes] = useState(360);
  const [maintenance, setMaintenance] = useState<MaintenanceSettings>({ enabled: false });
  const [secret, setSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const load = useCallback(async () => {
    const results = await Promise.allSettled([
      api("/api/settings/probes"),
      api("/api/settings/alerts"),
      api("/api/settings/model-refresh"),
      api("/api/settings/maintenance"),
    ]);
    const [probeResult, alertResult, refreshResult, maintenanceResult] = results;
      if (probeResult.status === "fulfilled") setProbes(probeResult.value as ProbeSettings);
      if (alertResult.status === "fulfilled") setAlerts(alertResult.value as AlertSettings);
      if (refreshResult.status === "fulfilled") setRefreshMinutes(refreshResult.value.refresh_minutes);
      if (maintenanceResult.status === "fulfilled") setMaintenance(maintenanceResult.value as MaintenanceSettings);
    return results.every((result) => result.status === "fulfilled");
  }, []);
  useEffect(() => void load(), [load]);
  useEffect(() => registerRefresh(load), [load, registerRefresh]);
  const save = async () => {
    setBusy(true);
    try {
      await Promise.all([
        api("/api/settings/probes", { method: "PUT", body: JSON.stringify(probes) }),
        api("/api/settings/model-refresh", { method: "PUT", body: JSON.stringify({ refresh_minutes: refreshMinutes }) }),
        api("/api/settings/alerts", { method: "PUT", body: JSON.stringify({ ...alerts, webhook_secret: secret || undefined }) }),
        api("/api/settings/maintenance", { method: "PUT", body: JSON.stringify(maintenance) }),
      ]);
      setSecret("");
      notify("运行与告警设置已保存");
    } catch (error) {
      notify((error as Error).message, "error");
    } finally { setBusy(false); }
  };
  return <section className="panel operations-panel">
    <div className="panel-head"><div><span className="eyebrow">OPERATIONS</span><h3>探测、刷新与告警</h3><p className="muted">探测默认开启，告警仅在配置 Webhook 后启用。</p></div><Radio size={20} /></div>
    <div className="operations-grid">
      <label className="vision-proxy-toggle maintenance-mode-toggle"><input type="checkbox" checked={maintenance.enabled} onChange={(event) => setMaintenance({ enabled: event.target.checked })} /><span><b>维护模式</b><small>{maintenance.enabled ? "仅维护白名单 Key 可调用网关" : "所有启用的 Client Key 可调用网关"}</small></span></label>
      <label className="vision-proxy-toggle"><input type="checkbox" checked={probes.enabled} onChange={(event) => setProbes((current) => ({ ...current, enabled: event.target.checked }))} /><span><b>后台代理探测</b><small>出口与上游路径定时复测</small></span></label>
      <label>出口探测（分钟）<input type="number" min="5" value={probes.exit_minutes} onChange={(event) => setProbes((current) => ({ ...current, exit_minutes: Number(event.target.value) }))} /></label>
      <label>上游探测（分钟）<input type="number" min="15" value={probes.upstream_minutes} onChange={(event) => setProbes((current) => ({ ...current, upstream_minutes: Number(event.target.value) }))} /></label>
      <label>模型刷新（分钟，0 关闭）<input type="number" min="0" value={refreshMinutes} onChange={(event) => setRefreshMinutes(Number(event.target.value))} /></label>
    </div>
    <div className="operations-grid alert-grid">
      <label className="vision-proxy-toggle"><input type="checkbox" checked={alerts.enabled} onChange={(event) => setAlerts((current) => ({ ...current, enabled: event.target.checked }))} /><span><b>Webhook 告警</b><small>低可用、Resin、刷新和限流异常</small></span></label>
      <label>Webhook URL<input value={alerts.webhook_url} onChange={(event) => setAlerts((current) => ({ ...current, webhook_url: event.target.value }))} placeholder="https://hooks.example.com/relaydesk" /></label>
      <label>签名密钥<input type="password" value={secret} onChange={(event) => setSecret(event.target.value)} placeholder={alerts.has_webhook_secret ? "已保存 · 留空保持不变" : "可选 HMAC 密钥"} /></label>
      <label>低可用阈值<input type="number" min="1" value={alerts.low_proxy_threshold} onChange={(event) => setAlerts((current) => ({ ...current, low_proxy_threshold: Number(event.target.value) }))} /></label>
      <label>成功率阈值（%）<input type="number" min="1" max="100" value={alerts.success_rate_percent} onChange={(event) => setAlerts((current) => ({ ...current, success_rate_percent: Number(event.target.value) }))} /></label>
    </div>
    <fieldset className="alert-event-options">
      <legend>告警事件</legend>
      {alertEventOptions.map((event) => <label key={event.value}><input type="checkbox" checked={alerts.events.includes(event.value)} onChange={(input) => setAlerts((current) => ({ ...current, events: input.target.checked ? [...current.events, event.value] : current.events.filter((value) => value !== event.value) }))} />{event.label}</label>)}
    </fieldset>
    <button className="secondary" onClick={save} disabled={busy}>{busy ? "保存中…" : "保存运行设置"}</button>
  </section>;
}

function SettingsPage({
  notify,
  onLogout,
  registerRefresh,
}: {
  notify: (value: string, type?: ToastType) => void;
  onLogout: () => void;
  registerRefresh: RegisterRefresh;
}) {
  const [retentionDays, setRetentionDays] = useState(90);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const childRefreshers = useRef(new Set<RefreshHandler>());

  const loadRetention = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api("/api/settings/usage-retention");
      setRetentionDays(data.usage_retention_days);
      return true;
    } catch (error) {
      if ((error as Error).message === "AUTH") onLogout();
      else notify((error as Error).message, "error");
      return false;
    } finally {
      setLoading(false);
    }
  }, [notify, onLogout]);
  useEffect(() => {
    void loadRetention();
  }, [loadRetention]);
  const registerChildRefresh = useCallback<RegisterChildRefresh>((handler) => {
    childRefreshers.current.add(handler);
    return () => childRefreshers.current.delete(handler);
  }, []);
  const refreshSettings = useCallback(async () => {
    const results = await Promise.all([
      loadRetention(),
      ...Array.from(childRefreshers.current, (refresh) => refresh()),
    ]);
    return results.every(Boolean);
  }, [loadRetention]);
  useEffect(() => registerRefresh("settings", refreshSettings), [refreshSettings, registerRefresh]);

  const saveRetention = async () => {
    setSaving(true);
    try {
      const data = await api("/api/settings/usage-retention", {
        method: "PUT",
        body: JSON.stringify({ usage_retention_days: retentionDays }),
      });
      setRetentionDays(data.usage_retention_days);
      notify("使用记录保留时间已保存");
    } catch (error) {
      if ((error as Error).message === "AUTH") onLogout();
      else notify((error as Error).message, "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="page">
      <PasswordCard notify={notify} />
      <OperationsSettings notify={notify} registerRefresh={registerChildRefresh} />
      <section className="panel retention-panel">
        <div className="panel-head">
          <div>
            <span className="eyebrow">DATA RETENTION</span>
            <h3>使用记录保留时间</h3>
            <p className="muted">超过保留期限的请求记录会自动删除。</p>
          </div>
          <Database size={20} />
        </div>
        <div className="retention-controls">
          <label>
            保留期限
            <select
              value={retentionDays}
              disabled={loading || saving}
              onChange={(event) => setRetentionDays(Number(event.target.value))}
            >
              <option value={7}>7 天</option>
              <option value={30}>30 天</option>
              <option value={90}>90 天</option>
              <option value={180}>180 天</option>
            </select>
          </label>
          <button
            className="secondary"
            onClick={saveRetention}
            disabled={loading || saving}
          >
            {saving ? "保存中…" : "保存设置"}
          </button>
        </div>
      </section>
    </div>
  );
}

const usageTimeOptions: Array<{ value: UsageTimePreset; label: string }> = [
  { value: "all", label: "全部" },
  { value: "1h", label: "近 1 小时" },
  { value: "24h", label: "近 24 小时" },
  { value: "7d", label: "近 7 天" },
  { value: "30d", label: "近 30 天" },
  { value: "custom", label: "自定义" },
];

const emptyUsageFilters = (): UsageFilters => ({
  time: "all",
  model: "",
  status: "",
  customFrom: "",
  customTo: "",
});

const usageFiltersFromURL = (): UsageFilters => {
  const params = new URLSearchParams(location.search);
  const time = params.get("usage_time") as UsageTimePreset | null;
  return {
    time: usageTimeOptions.some((option) => option.value === time) ? time! : "all",
    model: params.get("usage_model") || "",
    status: ["success", "error", "external"].includes(params.get("usage_status") || "") ? params.get("usage_status") as UsageFilters["status"] : "",
    customFrom: params.get("usage_from") || "",
    customTo: params.get("usage_to") || "",
  };
};

const positiveURLNumber = (name: string, fallback: number) => {
  const value = Number(new URLSearchParams(location.search).get(name));
  return Number.isInteger(value) && value > 0 ? value : fallback;
};

const formatDuration = (milliseconds?: number) => {
  if (milliseconds === undefined || milliseconds === null) return "—";
  if (milliseconds < 1000) return `${Math.round(milliseconds)} ms`;
  return `${new Intl.NumberFormat("zh-CN", {
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  }).format(milliseconds / 1000)} s`;
};

function Usage({
  notify,
  onLogout,
  registerRefresh = noopRegisterRefresh,
}: {
  notify: (value: string, type?: ToastType) => void;
  onLogout: () => void;
  registerRefresh?: RegisterRefresh;
}) {
  const [rows, setRows] = useState<RequestRow[]>([]);
  const [models, setModels] = useState<string[]>([]);
  const [page, setPage] = useState(() => positiveURLNumber("usage_page", 1));
  const [pageSize, setPageSize] = useState(() => positiveURLNumber("usage_size", 25));
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [loading, setLoading] = useState(true);
  const [draftFilters, setDraftFilters] = useState<UsageFilters>(usageFiltersFromURL);
  const [appliedFilters, setAppliedFilters] = useState<UsageFilters>(usageFiltersFromURL);
  const [rates, setRates] = useState<UsageRates>({
    window_seconds: 60,
    rpm: 0,
    tpm: 0,
    measured_at: "",
  });
  const [dailyUsage, setDailyUsage] = useState<DailyUsage[]>([]);
  const [expandedRows, setExpandedRows] = useState<number[]>([]);
  const notifyRef = useRef(notify);
  useEffect(() => {
    notifyRef.current = notify;
  }, [notify]);

  const requestPath = useMemo(() => {
    const params = new URLSearchParams({
      page: String(page),
      page_size: String(pageSize),
    });
    const durations: Partial<Record<UsageTimePreset, number>> = {
      "1h": 60 * 60 * 1000,
      "24h": 24 * 60 * 60 * 1000,
      "7d": 7 * 24 * 60 * 60 * 1000,
      "30d": 30 * 24 * 60 * 60 * 1000,
    };
    const duration = durations[appliedFilters.time];
    if (duration) {
      params.set("from", new Date(Date.now() - duration).toISOString());
    } else if (appliedFilters.time === "custom") {
      params.set("from", new Date(appliedFilters.customFrom).toISOString());
      params.set("to", new Date(appliedFilters.customTo).toISOString());
    }
    if (appliedFilters.model) params.set("model", appliedFilters.model);
    if (appliedFilters.status) params.set("status", appliedFilters.status);
    return `/api/usage/requests?${params.toString()}`;
  }, [appliedFilters, page, pageSize]);

  const loadRecords = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      const data = (await api(requestPath, { signal })) as UsagePage;
      setRows(data.items);
      setModels(data.models);
      setTotal(data.total);
      setTotalPages(data.total_pages);
      setExpandedRows((current) => current.filter((id) => data.items.some((row) => row.id === id)));
      if (data.page !== page) setPage(data.page);
      return true;
    } catch (error) {
      if ((error as Error).name === "AbortError") return false;
      if ((error as Error).message === "AUTH") onLogout();
      else notifyRef.current((error as Error).message, "error");
      return false;
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [onLogout, page, requestPath]);

  useEffect(() => {
    const controller = new AbortController();
    void loadRecords(controller.signal);
    return () => {
      controller.abort();
    };
  }, [loadRecords]);

  useEffect(() => {
    const url = new URL(location.href);
    const setOrClear = (name: string, value: string, defaultValue = "") => {
      if (value && value !== defaultValue) url.searchParams.set(name, value);
      else url.searchParams.delete(name);
    };
    setOrClear("usage_time", appliedFilters.time, "all");
    setOrClear("usage_model", appliedFilters.model);
    setOrClear("usage_status", appliedFilters.status);
    setOrClear("usage_from", appliedFilters.time === "custom" ? appliedFilters.customFrom : "");
    setOrClear("usage_to", appliedFilters.time === "custom" ? appliedFilters.customTo : "");
    setOrClear("usage_page", String(page), "1");
    setOrClear("usage_size", String(pageSize), "25");
    history.replaceState(null, "", url);
  }, [appliedFilters, page, pageSize]);

  const loadRates = useCallback(async (force = false) => {
    if (!force && document.visibilityState !== "visible") return true;
    try {
      const data = (await api("/api/usage/rates")) as UsageRates;
      setRates(data);
      return true;
    } catch (error) {
      if ((error as Error).message === "AUTH") onLogout();
      else if (force) notifyRef.current((error as Error).message, "error");
      return false;
    }
  }, [onLogout]);

  const loadDailyUsage = useCallback(async (reportError = true) => {
    try {
      setDailyUsage((await api("/api/stats/timeseries")) as DailyUsage[]);
      return true;
    } catch (error) {
      if ((error as Error).message === "AUTH") onLogout();
      else if (reportError) notifyRef.current((error as Error).message, "error");
      return false;
    }
  }, [onLogout]);

  useEffect(() => {
    void loadRates();
    const refreshRates = () => void loadRates();
    const timer = window.setInterval(refreshRates, 10_000);
    document.addEventListener("visibilitychange", refreshRates);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refreshRates);
    };
  }, [loadRates]);

  useEffect(() => {
    void loadDailyUsage();
  }, [loadDailyUsage]);

  const refreshUsage = useCallback(async () => {
    const [recordsOK, ratesOK, dailyOK] = await Promise.all([
      loadRecords(),
      loadRates(true),
      loadDailyUsage(true),
    ]);
    return recordsOK && ratesOK && dailyOK;
  }, [loadDailyUsage, loadRates, loadRecords]);
  useEffect(() => registerRefresh("usage", refreshUsage), [refreshUsage, registerRefresh]);

  const applyFilters = (event: React.FormEvent) => {
    event.preventDefault();
    if (draftFilters.time === "custom") {
      if (!draftFilters.customFrom || !draftFilters.customTo) {
        notify("请选择自定义时间的开始和结束");
        return;
      }
      const from = new Date(draftFilters.customFrom);
      const to = new Date(draftFilters.customTo);
      if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime())) {
        notify("自定义时间格式无效");
        return;
      }
      if (from > to) {
        notify("开始时间不能晚于结束时间");
        return;
      }
    }
    setAppliedFilters({ ...draftFilters });
    setPage(1);
  };

  const clearFilters = () => {
    const cleared = emptyUsageFilters();
    setDraftFilters(cleared);
    setAppliedFilters(cleared);
    setPage(1);
  };

  return (
    <div className="page">
      <div className="usage-rate-rail usage-rate-full" aria-label="最近 60 秒速率">
          <div className="usage-rate-context">
            <Activity size={16} />
            <span>
              <b>实时速率</b>
              <small>
                全局最近 {rates.window_seconds} 秒
                {rates.measured_at
                  ? ` · ${new Date(rates.measured_at).toLocaleTimeString()}`
                  : ""}
              </small>
            </span>
          </div>
          <div className="usage-rate-value">
            <span>RPM</span>
            <strong>{fmt(rates.rpm)}</strong>
          </div>
          <div className="usage-rate-value">
            <span>TPM</span>
            <strong>{fmt(rates.tpm)}</strong>
          </div>
      </div>
      <section className="panel daily-usage-panel">
        <div className="panel-head">
          <div>
            <span className="eyebrow">DAILY TOTALS</span>
            <h3>每日 Token</h3>
            <p className="muted">按北京时间 00:00 汇总，保留最近 30 个自然日。</p>
          </div>
          <span className="count-chip">{dailyUsage.length} 天</span>
        </div>
        <div className="daily-usage-scroll">
          <div className="daily-usage-table">
            <div className="daily-usage-head">
              <span>日期</span>
              <span>请求数</span>
              <span>总 Token</span>
            </div>
            {dailyUsage.map((day) => (
              <div className="daily-usage-row" key={day.day}>
                <span>{day.day}</span>
                <span>{fmt(day.requests)}</span>
                <strong>{fmt(day.tokens)}</strong>
              </div>
            ))}
            {dailyUsage.length === 0 && <Empty text="暂无每日 Token 汇总" />}
          </div>
        </div>
      </section>
      <section className="panel">
        <form className="usage-filters" onSubmit={applyFilters}>
          <div className="usage-filter-time">
            <label>
              <CalendarRange size={14} /> 时间范围
            </label>
            <div className="time-segments" role="group" aria-label="时间范围">
              {usageTimeOptions.map((option) => (
                <button
                  type="button"
                  key={option.value}
                  className={draftFilters.time === option.value ? "active" : ""}
                  aria-pressed={draftFilters.time === option.value}
                  onClick={() =>
                    setDraftFilters((current) => ({
                      ...current,
                      time: option.value,
                    }))
                  }
                >
                  {option.label}
                </button>
              ))}
            </div>
          </div>
          <label className="usage-filter-select">
            <span>模型</span>
            <select
              value={draftFilters.model}
              onChange={(event) =>
                setDraftFilters((current) => ({
                  ...current,
                  model: event.target.value,
                }))
              }
            >
              <option value="">全部模型</option>
              {models.map((model) => (
                <option value={model} key={model}>
                  {model}
                </option>
              ))}
            </select>
          </label>
          <label className="usage-filter-select usage-result-filter">
            <span>请求结果</span>
            <select
              value={draftFilters.status}
              onChange={(event) =>
                setDraftFilters((current) => ({
                  ...current,
                  status: event.target.value as UsageFilters["status"],
                }))
              }
            >
              <option value="">全部结果</option>
              <option value="success">成功</option>
              <option value="error">用户错误</option>
              <option value="external">外部异常</option>
            </select>
          </label>
          {draftFilters.time === "custom" && (
            <div className="usage-custom-range">
              <label>
                <span>开始时间</span>
                <input
                  type="datetime-local"
                  value={draftFilters.customFrom}
                  onChange={(event) =>
                    setDraftFilters((current) => ({
                      ...current,
                      customFrom: event.target.value,
                    }))
                  }
                />
              </label>
              <label>
                <span>结束时间</span>
                <input
                  type="datetime-local"
                  value={draftFilters.customTo}
                  onChange={(event) =>
                    setDraftFilters((current) => ({
                      ...current,
                      customTo: event.target.value,
                    }))
                  }
                />
              </label>
            </div>
          )}
          <div className="usage-filter-actions">
            <button className="primary" type="submit" disabled={loading}>
              <Filter size={14} />
              应用筛选
            </button>
            <button
              className="secondary"
              type="button"
              onClick={clearFilters}
              disabled={loading}
            >
              <FilterX size={14} />
              清除
            </button>
          </div>
        </form>
        <div className="panel-head">
          <div>
            <h3>请求记录</h3>
            <p className="muted">全部历史记录 · 最新完成优先</p>
          </div>
          <span className="count-chip">{fmt(total)} 条记录</span>
        </div>
        <div className="usage-table-scroll">
          <div className={`data-table usage-table ${loading ? "is-loading" : ""}`}>
            <div className="table-head">
              <span>模型 / 完成时间</span>
              <span>请求结果</span>
              <span>UA / 流式</span>
              <span>输入 Token</span>
              <span>输出 Token</span>
              <span>首字耗时</span>
              <span>总耗时</span>
              <span>路径</span>
            </div>
            {rows.map((r) => {
              const expanded = expandedRows.includes(r.id);
              const detailsID = `usage-details-${r.id}`;
              return (
              <div className={`usage-row-group ${expanded ? "is-expanded" : ""}`} key={r.id}>
              <div className="table-row">
                <div>
                  <div className="request-model-line">
                    <b title={r.model}>{r.model}</b>
                    {r.request_kind === "vision_helper" && (
                      <span className="helper-tag">图片辅助</span>
                    )}
                    <button
                      className="usage-expand"
                      type="button"
                      title={expanded ? "收起请求详情" : "展开请求详情"}
                      aria-label={expanded ? "收起请求详情" : "展开请求详情"}
                      aria-expanded={expanded}
                      aria-controls={detailsID}
                      onClick={() => setExpandedRows((current) =>
                        current.includes(r.id)
                          ? current.filter((id) => id !== r.id)
                          : [...current, r.id],
                      )}
                    >
                      <ChevronDown size={15} />
                    </button>
                  </div>
                  <small>{new Date(r.created_at).toLocaleString()}</small>
                </div>
                <div className="usage-result-cell">
                  <span
                    className={`health ${r.status === "success" ? "healthy" : r.error_origin === "external" ? "external" : r.error_origin === "internal" ? "internal" : "cooldown"}`}
                  >
                    <i />
                    {r.status === "success"
                      ? "成功"
                      : r.error_origin === "external"
                        ? "外部异常"
                        : r.error_origin === "internal"
                          ? "内部步骤"
                          : `HTTP ${r.status_code}`}
                  </span>
                  {r.error_message && (
                    <small className="request-error" title={r.error_message}>
                      {r.error_message}
                    </small>
                  )}
                </div>
                <div className="usage-client-cell">
                  <span className="usage-client-name" title={r.client_user_agent || "未记录"}>
                    {r.client_user_agent || "未记录"}
                  </span>
                  <span
                    className={`stream-tag ${
                      r.stream === true ? "streaming" : r.stream === false ? "non-streaming" : "unknown"
                    }`}
                  >
                    {r.stream === true ? "流式" : r.stream === false ? "非流式" : "未记录"}
                  </span>
                  <span className="mobile-token-total">
                    Token {r.total_tokens === undefined ? "—" : fmt(r.total_tokens)}
                  </span>
                </div>
                <span className="usage-number">
                  {r.prompt_tokens === undefined ? "—" : fmt(r.prompt_tokens)}
                </span>
                <span className="usage-number">
                  {r.completion_tokens === undefined
                    ? "—"
                    : fmt(r.completion_tokens)}
                </span>
                <span className="usage-duration">
                  {formatDuration(r.first_token_latency_ms)}
                </span>
                <span className="usage-duration">
                  {formatDuration(r.latency_ms)}
                </span>
                <span className="mono tiny usage-path" title={r.proxy_uri || "direct"}>
                  {(r.route_engine || (r.proxy_uri ? "builtin" : "direct")) + " · " + (r.proxy_uri || "direct")}
                </span>
              </div>
              <div className="usage-mobile-detail" id={detailsID}>
                <dl>
                  <div><dt>客户端</dt><dd>{r.client_user_agent || "未记录"}</dd></div>
                  <div><dt>输入 / 输出 Token</dt><dd>{r.prompt_tokens === undefined ? "—" : fmt(r.prompt_tokens)} / {r.completion_tokens === undefined ? "—" : fmt(r.completion_tokens)}</dd></div>
                  <div><dt>首字 / 总耗时</dt><dd>{formatDuration(r.first_token_latency_ms)} / {formatDuration(r.latency_ms)}</dd></div>
                  <div><dt>路径</dt><dd>{(r.route_engine || (r.proxy_uri ? "builtin" : "direct")) + " · " + (r.proxy_uri || "direct")}</dd></div>
                  {r.error_message && <div><dt>错误</dt><dd>{r.error_message}</dd></div>}
                </dl>
              </div>
              </div>
              );
            })}
            {loading && rows.length === 0 && <Empty text="正在加载使用记录…" />}
            {!loading && rows.length === 0 && (
              <Empty text="当前筛选条件下没有使用记录" />
            )}
          </div>
        </div>
        <div className="usage-pagination" aria-label="使用记录分页">
          <span className="pagination-range" aria-live="polite">
            {total === 0
              ? "0 / 0"
              : `${(page - 1) * pageSize + 1}-${Math.min(page * pageSize, total)} / ${fmt(total)}`}
          </span>
          <label className="page-size-control">
            <span>每页</span>
            <select
              value={pageSize}
              disabled={loading}
              onChange={(event) => {
                setPageSize(Number(event.target.value));
                setPage(1);
              }}
            >
              <option value={25}>25</option>
              <option value={50}>50</option>
              <option value={100}>100</option>
            </select>
          </label>
          <div className="page-navigation">
            <button
              className="icon-btn"
              type="button"
              title="上一页"
              aria-label="上一页"
              disabled={loading || page <= 1}
              onClick={() => setPage((current) => Math.max(1, current - 1))}
            >
              <ChevronLeft size={16} />
            </button>
            <span className="page-indicator">
              第 {page} / {totalPages} 页
            </span>
            <button
              className="icon-btn"
              type="button"
              title="下一页"
              aria-label="下一页"
              disabled={loading || page >= totalPages}
              onClick={() =>
                setPage((current) => Math.min(totalPages, current + 1))
              }
            >
              <ChevronRight size={16} />
            </button>
          </div>
        </div>
      </section>
    </div>
  );
}
function Empty({ text }: { text: string }) {
  return (
    <div className="empty">
      <Database size={19} />
      <span>{text}</span>
    </div>
  );
}
export default App;
export { ClientKeysPanel, Models, Proxies, Usage };

const root = document.getElementById("root");
if (root) createRoot(root).render(<App />);
