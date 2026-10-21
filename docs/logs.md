# 日志系统（Log API）

Relay Desk 内置一套**外部可访问**的日志系统：进程把每一次请求、每一次上游轮换、
每一次鉴权失败、每一行 `log.Printf` 都写进 SQLite 的 `log_events` 表，并通过一组
HTTP 接口暴露出去。设计目标是**让 AI（或任何脚本）直接连上来定位问题**，而不是让人
去翻容器日志。

- 鉴权密钥 = **面板登录密码**（`ADMIN_PASSWORD`，也就是你登录面板时用的那个）。
- 默认与面板共用端口，路径前缀 `/api/logs`。
- 只读接口永不修改业务状态；写接口只有 `notes` / `settings` / `prune` 三个。

---

## 1. 在哪里、怎么连

| 部署方式 | 日志接口地址 |
| --- | --- |
| 同端口（默认） | `http://<host>:<PORT>/api/logs...` |
| 独立端口 | 设 `LOG_API_ADDR=0.0.0.0:8099`，然后 `http://<host>:8099/api/logs...` |

同一套路由在两种模式下都在，独立端口只是把日志面单独暴露出来（比如面板只在
`127.0.0.1` 上、但你想让另一台机器上的 AI 读日志）。

> `BIND_ADDRESS=127.0.0.1`（默认值）意味着**只有本机能连**。要让外部访问，
> 把 `BIND_ADDRESS` 改成 `0.0.0.0`，或者用 `LOG_API_ADDR=0.0.0.0:8099` 单独开一个
> 监听。两种方式都需要你自己在前面加 TLS / 反代，日志接口本身说 HTTP。

### 鉴权

三种凭证，任选一种：

```bash
# 1) 专用头（推荐给 AI / 脚本）
-H "X-Relay-Log-Key: $ADMIN_PASSWORD"

# 2) Bearer
-H "Authorization: Bearer $ADMIN_PASSWORD"

# 3) 面板会话 cookie（浏览器里已登录时天然可用）
```

- 密钥就是当前面板密码，服务端用 bcrypt 比对 `settings.admin_hash`。
- 比对结果按 `sha256(admin_hash + 密码)` 缓存 10 分钟，所以**改密码后旧密码立刻失效**，
  新密码立刻可用。
- 连续 10 次失败会把该来源 IP 封 15 分钟（`429`，带 `Retry-After`）。这个计数与面板
  登录的计数是分开的，不会互相干扰。
- 失败会记一条 `log.api.denied` 事件——所以暴力试探在日志里是可见的。

---

## 2. 接口一览

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/api/logs` | 查询事件（分页、过滤） |
| GET | `/api/logs/stream` | SSE 实时流（可回放最近 N 条） |
| GET | `/api/logs/stats` | 聚合统计（按级别/来源/事件/路径/小时） |
| GET | `/api/logs/diagnostics` | **体检报告**：自检规则 + 进程信息 + 最近错误 |
| GET | `/api/logs/export` | 一次性导出（JSON，可当附件下载） |
| GET | `/api/logs/guide` | 自描述使用说明（Markdown，含全部 curl 配方） |
| GET | `/api/logs/settings` | 读日志设置 |
| PUT | `/api/logs/settings` | 改日志设置（保留天数、行数上限、级别门槛…） |
| POST | `/api/logs/prune` | 清理（`all` / `before` / `older_than`，默认按保留策略） |
| GET | `/api/logs/notes` | 读 AI 回写的结论 |
| POST | `/api/logs/notes` | 写一条结论/发现/待办 |
| PATCH | `/api/logs/notes/{id}` | 局部更新 |
| DELETE | `/api/logs/notes/{id}` | 删除 |

### 先看体检报告

```bash
curl -s -H "X-Relay-Log-Key: $ADMIN_PASSWORD" \
  http://127.0.0.1:8080/api/logs/diagnostics | jq
```

返回结构（节选）：

```json
{
  "generated_at": "2026-10-21T10:43:52Z",
  "process": { "pid": 8123, "go_version": "go1.26.5", "bind": "0.0.0.0", "port": "8080", "log_api_addr": "" },
  "sink": { "queued": 0, "ring_size": 42, "subscribers": 0, "stored": 98123, "dropped": 0, "failed": 0, "settings": { "...": "..." } },
  "checks": [
    { "id": "traffic", "status": "warn", "title": "elevated 5xx rate", "detail": "18% of 412 calls in the last hour", "metric": 0.18 }
  ],
  "database": { "bytes": 20111360, "log_rows": 98123, "usage_rows": 412, "oldest_log": "...", "newest_log": "..." },
  "recent": [ { "level": "error", "event": "http.request", "request_id": "req-…", "detail": { "...": "..." } } ],
  "error_events": [ { "key": "upstream.rotate", "count": 7 } ],
  "notes": [ { "id": 3, "kind": "finding", "title": "…" } ],
  "summary": { "errors": 1, "warnings": 2, "verdict": "degraded" }
}
```

`summary.verdict` 只有三种：`healthy` / `degraded` / `broken`。**AI 的第一条命令应该是它。**

内置自检规则：

| id | 触发条件 |
| --- | --- |
| `log_storage` | 写库失败次数 > 0 |
| `log_queue` | 队列积压或丢弃 > 0（写库跟不上） |
| `log_retention` | 从未执行过保留清理，或超过 24h 未清理 |
| `log_capture` | 未捕获 `log.Printf` 输出（排障时会缺一层信息） |
| `log_requests` | 未记录 HTTP 请求（几乎无法回溯） |
| `traffic` | 近 1 小时 5xx 占比 ≥ 10%（warn）/ ≥ 50%（error） |
| `latency` | 近 1 小时平均延迟 > 30s |
| `panics` | 24 小时内有 panic |
| `proxy_pool` | 内置引擎下没有启用的出口（Resin 引擎自选账号，本地池为空属正常，不会报 warn） |
| `client_keys` | 没有启用的客户端密钥 |
| `models` | 没有可用模型 |
| `maintenance` | 维护模式开启中 |
| `concurrency` | 并发闸门占用 ≥ 90% |
| `vision` | 视觉链路配置不完整 |
| `upstream_calls` | 近 1 小时上游调用有失败（≥ 50% 为 error） |
| `database` | 数据库 > 1 GiB |

### 再按 request_id 钻下去

每个被记录的请求都会带 `X-Request-Id` 响应头，同时写进日志。拿它就能把一次调用
的全部上下文捞出来：

```bash
curl -s -H "X-Relay-Log-Key: $ADMIN_PASSWORD" \
  "http://127.0.0.1:8080/api/logs?request_id=req-9f2c1a7b3d4e5f60&detail=1" | jq '.events[].detail'
```

### 查询参数（`GET /api/logs`）

| 参数 | 说明 |
| --- | --- |
| `level` | `error,warn`（逗号分隔） |
| `source` | `request,gateway,auth,system,stderr,panic,logs` |
| `event` | 精确事件名，如 `http.request` |
| `request_id` | 精确请求 ID |
| `path` / `path_prefix` | 精确 / 前缀匹配 URL 路径 |
| `q` | 在 message、event、path、model、detail 里做子串搜索 |
| `since` / `until` | RFC3339、unix 毫秒，或相对窗口（`30m`、`2h`、`7d`） |
| `window` | 简写：`since = now - window` |
| `limit` | 默认 100，上限 1000 |
| `offset` | 分页偏移 |
| `order` | `asc` 按时间正序（默认倒序） |
| `detail` | `0` / `false` 表示不返回 detail |

响应：`{"events":[...],"count":n,"total":N,"limit":..,"offset":..,"order":"desc","has_more":bool,"next_offset":..,"query":{...}}`

### 实时流

```bash
# 先回放最近 50 条，再持续跟随
curl -N -H "X-Relay-Log-Key: $ADMIN_PASSWORD" \
  "http://127.0.0.1:8080/api/logs/stream?backlog=50&level=error"
```

SSE 格式：先 `event: hello`（带 sink 状态与过滤器回显），之后每条日志一个
`event: log`，`data` 是单行 JSON。每 15 秒一个 `: keepalive` 注释。慢客户端会被
丢弃而不是拖住请求路径。

### 统计

```bash
curl -s -H "X-Relay-Log-Key: $ADMIN_PASSWORD" \
  "http://127.0.0.1:8080/api/logs/stats?window=1h" | jq
```

给出 `total` / `errors` / `warnings` / `by_level` / `by_source` / `top_events` /
`by_hour` / `paths`（每个路径的 `count`、`errors`、`avg_ms`、`p95_ms`、`max_ms`、
`last_seen`）。路径统计最多采样 5000 行，超出时返回 `"sampled": true`。

### 导出

```bash
curl -s -H "X-Relay-Log-Key: $ADMIN_PASSWORD" \
  "http://127.0.0.1:8080/api/logs/export?window=24h&limit=5000" -o logs.json
# 当附件下载
curl -s -OJ -H "X-Relay-Log-Key: $ADMIN_PASSWORD" \
  "http://127.0.0.1:8080/api/logs/export?window=24h&download=1"
```

包含 `settings` / `sink` / `events` / `notes` / `truncated`——一份可以直接丢给
另一个 AI 的自包含快照。

---

## 3. 记录了什么

每条事件有统一的字段：`created_at`、`level`（`debug|info|warn|error`）、`source`、
`event`、`request_id`、`method`、`path`、`status`、`duration_ms`、`client_ip`、
`user_agent`、`model`、`route_engine`、`client_key_id`、`message`、`detail`（JSON）。

| source | 内容 |
| --- | --- |
| `request` | 每个 `/api/*` 与 `/v1/*` 请求一条 `http.request`：状态码、耗时、字节数、客户端 IP/UA、trace 字段（模型、解析后模型、路由引擎、代理数、压缩是否生效、尝试次数、首字延迟、错误）以及逐次尝试的 notes |
| `gateway` | 网关内部决策（鉴权、容量、代理池、轮换结论） |
| `auth` | 面板登录成功/失败、日志接口鉴权失败 |
| `system` | 启动/停止、设置变更、清理 |
| `stderr` | 进程自己 `log.Printf` 的每一行（带级别推断） |
| `panic` | `panic.recovered`，含完整堆栈 |
| `logs` | 日志子系统自身的事件（设置更新、笔记创建等） |

### 脱敏

写库之前统一过滤，**永远不会**落盘的形态：

- `password=…` / `token: …` / `api_key=…` / `authorization: …` / `cookie=…` → `<redacted>`
- `Bearer xxxxxxxx` → `Bearer <redacted>`
- `ocp-…`（客户端密钥）→ `ocp-<redacted>`
- `sk-…`（上游密钥）→ `sk-<redacted>`
- 代理 URI 里的 `user:pass@` → `://<redacted>@`

正文按 2000 字节、detail 按 8000 字节截断（按 rune 边界切，不会产生半个汉字），
末尾加 `…`。

---

## 4. 设置与保留策略

```bash
curl -s -H "X-Relay-Log-Key: $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  -X PUT http://127.0.0.1:8080/api/logs/settings \
  -d '{"retention_days":7,"max_rows":200000,"min_level":"info","capture_stderr":true,"record_requests":true}'
```

| 键 | 默认 | 范围 / 说明 |
| --- | --- | --- |
| `retention_days` | 7 | 0–365，`0` 表示永不按时间删 |
| `max_rows` | 100000 | 1000–5000000，超出后按最旧删除 |
| `min_level` | `info` | 写入门槛（`debug` 会记下一切） |
| `capture_stderr` | true | 是否接管 `log.Printf` |
| `record_requests` | true | 是否逐请求记录 |

清理在后台每 60 秒跑一次；也可以手动：

```bash
# 全清
curl -s -X POST -H "X-Relay-Log-Key: $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  http://127.0.0.1:8080/api/logs/prune -d '{"all":true}'
# 清 7 天前的
curl -s -X POST -H "X-Relay-Log-Key: $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  http://127.0.0.1:8080/api/logs/prune -d '{"older_than":"7d"}'
```

写入是批量事务（默认每 250 ms 或满 128 条落盘），队列 8192 条；队列满时**丢日志而
不阻塞请求**，丢弃数在 `sink.dropped` 里可见。

---

## 5. 笔记：把结论留在系统里

`log_notes` 是给"连上来修 bug 的 AI"准备的回写通道：查完日志、改完代码，把结论写在
这里，下次（人或 AI）打开就知道发生过什么。

```bash
curl -s -X POST -H "X-Relay-Log-Key: $ADMIN_PASSWORD" -H "Content-Type: application/json" \
  http://127.0.0.1:8080/api/logs/notes \
  -d '{
        "title": "代理 3 每 4 分钟被 429 冷却",
        "body": "upstream_lane 的重试把 retry-after 当成了固定 4m，实际返回 90s；已改为读取响应头。",
        "kind": "fix",
        "severity": "warn",
        "request_id": "req-9f2c1a7b3d4e5f60",
        "tags": ["proxy", "retry"]
      }'
```

- `kind`：`note` / `finding` / `fix` / `todo`
- `severity`：`info` / `warn` / `error` / `debug`
- `request_id` 与 `event_id` 可选，用来把结论钉在具体事件上
- 创建时会同时产生一条 `log.note.created` 事件，所以笔记本身也在时间线里

---

## 6. AI 排障剧本

```bash
export RELAY=http://127.0.0.1:8080
export KEY=$ADMIN_PASSWORD

# 1. 有没有问题、哪一类
curl -s -H "X-Relay-Log-Key: $KEY" $RELAY/api/logs/diagnostics | jq '.summary, .checks[] | select(.status!="ok")'

# 2. 最近 24 小时错误分布
curl -s -H "X-Relay-Log-Key: $KEY" "$RELAY/api/logs/stats?window=24h" | jq '{total,errors,by_level,top_events,paths}'

# 3. 挑一条错误看现场
curl -s -H "X-Relay-Log-Key: $KEY" "$RELAY/api/logs?level=error&limit=5" | jq '.events[] | {created_at,event,path,status,request_id,message}'

# 4. 用 request_id 还原整次调用（含每次代理轮换）
curl -s -H "X-Relay-Log-Key: $KEY" "$RELAY/api/logs?request_id=req-…&order=asc" | jq '.events[] | {event,level,message,detail}'

# 5. 盯住新错误
curl -N -H "X-Relay-Log-Key: $KEY" "$RELAY/api/logs/stream?level=error,warn"

# 6. 把结论写回去
curl -s -X POST -H "X-Relay-Log-Key: $KEY" -H "Content-Type: application/json" \
  $RELAY/api/logs/notes -d '{"title":"…","body":"…","kind":"finding"}'
```

---

## 7. 常见问题

**401 `authentication required`**
密码不对，或用了面板旧密码。密钥就是当前面板密码；改过密码后旧值立刻失效。

**429 `too many log authentication attempts`**
同一 IP 连续失败 10 次，封 15 分钟。等一会儿，或从别的 IP 连。

**`/api/logs` 有数据但 `/v1/...` 的请求没有**
检查 `record_requests` 是否为 true，以及请求是否真到了网关（`/healthz`、`OPTIONS`、
`GET /api/logs*` 按设计不记录）。

**`sink.dropped` 在涨**
写库跟不上（磁盘慢或库被长事务占住）。看 `sink.last_error`，必要时调小
`max_rows`、把 `min_level` 提到 `warn`。

**日志里有 `stderr` 但没有 `request`**
`capture_stderr` 开着、`record_requests` 关了。

**独立端口连不上**
`LOG_API_ADDR` 必须写成 `host:port`（如 `0.0.0.0:8099`），启动失败会记一条
`log.api.failed`。它和 `PORT` 是两个独立监听。
