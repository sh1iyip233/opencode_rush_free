# 压缩网关（Context Compression Gateway）

压缩网关把「接近上下文上限的长对话」在转发前折叠成一份自包含摘要，再把「摘要 + 最近若干条消息」发给上游。摘要本身由一个**你自由配置的 AI API** 生成，可以是任何 OpenAI 兼容接口（`/chat/completions`），也可以是 Anthropic Messages 接口（`/messages`）。

它和主模型通道完全解耦：

- 主模型请求仍然走内置的 OpenCode Zen 匿名通道；
- 摘要请求走你填写的供应商，可直连，也可复用代理池；
- 摘要请求单独记账（`usage_requests.request_kind = 'compression'`），不计入仪表盘的聊天统计。

参考实现思路来自 [billion-context](https://github.com/ranxianglei/billion-context) 的 ACP（Active Context Pruning）：把已消耗的消息区间折叠成 summary block，并按会话持久化，下一轮只摘要新增内容。

## 快速开始

1. 登录管理台，左侧导航进入「压缩网关」。
2. 填写「摘要 Base URL」「摘要模型」「摘要 API Key」，选择接口风格。
3. 点「测试摘要接口」，确认返回摘要。
4. 勾选「启用压缩网关」，点「保存配置」。
5. 用「压缩预览」验证折叠效果，再让客户端正常发请求。

也可以直接调用管理 API：

```bash
# 读取配置
curl -s -b cookie.txt http://127.0.0.1:8080/api/settings/compression

# 保存配置
curl -s -b cookie.txt -X PUT http://127.0.0.1:8080/api/settings/compression \
  -H 'Content-Type: application/json' \
  -d '{
    "enabled": true,
    "api_style": "openai",
    "base_url": "https://api.example.com",
    "api_key": "sk-...",
    "model": "gpt-4o-mini",
    "trigger_ratio": 0.75,
    "context_window": 200000,
    "keep_recent_messages": 8,
    "fail_open": true
  }'

# 测试摘要接口（空对象表示使用已保存的配置）
curl -s -b cookie.txt -X POST http://127.0.0.1:8080/api/settings/compression/test \
  -H 'Content-Type: application/json' -d '{}'
```

## 配置字段

配置以 JSON 形式存放在 `settings` 表的 `compression_gateway` 键下，API Key 用 `APP_ENCRYPTION_KEY` 派生密钥做 AES-GCM 加密后保存（字段 `api_key_enc`），读取接口只返回 `has_api_key`。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 总开关；关闭时请求原样转发，配置保留 |
| `api_style` | `openai` | `openai` → `/chat/completions` + `Authorization: Bearer`；`anthropic` → `/messages` + `x-api-key` + `anthropic-version: 2023-06-01` |
| `base_url` | 空 | 摘要接口 Base URL；未以 `/v1` 结尾时自动补 `/v1` |
| `api_key` | 空 | 摘要接口密钥，只写不读 |
| `model` | 空 | 摘要模型 ID |
| `use_proxy` | `false` | `true` 时摘要请求通过代理池当前出口发送 |
| `trigger_ratio` | `0.75` | 触发阈值：估算 tokens ≥ 比例 × 上下文窗口时压缩，范围 0.3–0.95 |
| `context_window` | `200000` | 模型元数据里查不到窗口时使用的兜底窗口 |
| `keep_recent_messages` | `8` | 尾部原样保留的消息条数（2–500） |
| `min_fold_messages` | `4` | 至少要折叠的消息条数，否则跳过（1–500） |
| `max_summary_tokens` | `1200` | 摘要长度上限（128–16000），写入摘要请求的 max tokens |
| `temperature` | `0.2` | 摘要请求温度（0–2） |
| `timeout_seconds` | `90` | 摘要请求超时（5–600） |
| `summary_role` | `system` | 摘要消息的角色：`system` 或 `user` |
| `marker` | 见下 | 摘要消息前缀模板，支持 `{count}`、`{before}`、`{after}`、`{model}` |
| `prompt` | 见下 | 压缩提示词（摘要请求的指令） |
| `fail_open` | `true` | 摘要失败时：`true` 原样转发请求；`false` 返回 502 |

默认标记：

```text
📦 [Relay Desk 压缩摘要] 已折叠 {count} 条较早消息（约 {before} → {after} tokens）。
```

## 算法

### 1. 估算与触发

- `estimateTextTokens`：ASCII 字符按 4 个算 1 token（不足 4 个也按 1 计），非 ASCII 字符（中文、emoji 等）每个算 1 token。
- 每条消息额外 4 tokens 开销；`tool_calls` 按 JSON 序列化计费；图片部件按 800 tokens 固定折算。
- 上下文窗口优先从模型元数据读取：`limit.context`、`context_length`、`context_window`、`max_context_tokens`、`top_provider.context_length`；都没有时用 `context_window` 配置。
- 触发条件：`估算 tokens ≥ trigger_ratio × 窗口`，或客户端带请求头 `X-Relay-Compression: force`（`always`/`on`/`1`/`true` 等价）。

### 2. 折叠区间

- 头部连续的 `system`/`developer` 消息固定保留（pin）。
- 从第 `pin` 条开始向后扫描，只折叠**纯文本**的 `user`/`assistant` 消息；遇到 `tool_calls`、`tool_call_id`、`function_call` 或图片部件立即停止，避免破坏工具调用链。
- 尾部至少保留 `keep_recent_messages` 条（`maxFold = len(messages) - keepRecent`）。
- 折叠条数必须 ≥ `min_fold_messages`，否则跳过（`reason=below_threshold`）。

折叠后的消息序列是：`pin 消息…` + `一条摘要消息` + `尾部保留消息…`。

### 3. 增量摘要

每个会话（`compression_sessions.session_key`）保存一份摘要和「已折叠区间的摘要指纹」：

- `folded_digest = sha256(已折叠区间的文本转录)`；
- 下一轮请求若历史前缀指纹一致，只把**新增**的消息作为 delta 摘要，并提示模型「合并既有摘要」；
- 历史被改写（指纹不匹配）时丢弃旧摘要，整段重新摘要；
- delta 为空且已有摘要时，不调用摘要接口。

`session_key` 与网关的会话号算法一致：优先 `X-Relay-Session-ID` / `X-OpenCode-Session-ID` / `X-Session-ID`，其次 payload 里的 `user`，再次首条 user 消息指纹，最后 `hash(client_id + session_id)`。

### 4. 失败处理

| 情况 | `fail_open=true` | `fail_open=false` |
| --- | --- | --- |
| 摘要接口超时/5xx/返回空 | 原样转发，响应头标记 `failed` | 返回 `502 {"code":"compression_failed"}` |
| 配置读取失败 | 原样转发（`settings_unavailable`） | 同左（读取失败不阻塞） |

无论成功失败，都会写 `compression_events` 与 `usage_requests`（失败时 `status='error'`），便于排查。

## 响应头

压缩状态通过响应头回传（`copyResponse` 只覆盖 connection/content-length 等，`X-Relay-*` 会保留）：

| 头 | 说明 |
| --- | --- |
| `X-Relay-Compression` | `applied` / `skipped` / `failed` / `disabled` |
| `X-Relay-Compression-Reason` | 跳过或失败原因，如 `below_threshold`、`not_configured`、`summarizer_failed` |
| `X-Relay-Compression-Messages` | 本次折叠的消息条数 |
| `X-Relay-Compression-Before` / `-After` | 折叠前后的估算 tokens |
| `X-Relay-Compression-Saved-Tokens` | 净节省 tokens |
| `X-Relay-Compression-Model` | 实际使用的摘要模型 |

客户端可以用 `X-Relay-Compression: off` 关闭单次压缩（`none`/`disabled`/`0`/`false` 等价）。

## 数据表

`compression_sessions`（每会话一行）：

| 列 | 说明 |
| --- | --- |
| `session_key` | 主键 |
| `model` | 最近一次压缩时的聊天模型 |
| `summary` | 当前摘要正文 |
| `folded_digest` | 已折叠区间指纹 |
| `folded_messages` / `folded_tokens` | 已折叠条数与 tokens |
| `compressions` | 累计压缩次数 |
| `tokens_before` / `tokens_after` | 最近一次折叠前后 tokens |
| `created_at` / `updated_at` | 时间戳（UTC） |

`compression_events`（每次尝试一行）：`created_at`、`session_key`、`model`、`api_style`、`summary_model`、`folded_messages`、`tokens_before`、`tokens_after`、`summary_tokens`、`latency_ms`、`status`（`success`/`error`）、`error_message`。

## 管理 API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/settings/compression` | 读取配置（含 `has_api_key`、`configured`、`default_marker`、`default_prompt`） |
| PUT | `/api/settings/compression` | 保存配置；`clear_api_key: true` 清除密钥；字段校验失败返回 400 |
| POST | `/api/settings/compression/test` | 用固定样例对话试跑摘要；可临时覆盖 `base_url`/`model`/`api_style`/`api_key`/`use_proxy`/`timeout_seconds` |
| GET | `/api/compression/stats?days=7` | 近 N 天（1–90）压缩次数、失败数、节省 tokens、平均延迟、会话数、每日趋势、最近 20 条事件 |
| GET | `/api/compression/sessions?limit=50` | 会话摘要列表（含 160 字符预览） |
| DELETE | `/api/compression/sessions/{key}` | 删除单个会话摘要 |
| POST | `/api/compression/sessions/reset` | 清空全部会话摘要 |
| POST | `/api/compression/preview` | 干跑：`{"messages":[…],"model":"…","force":true}`，返回折叠结果与重组后的 messages，不写库 |

## 成本与容量建议

- 摘要模型选便宜的小模型即可（`gpt-4o-mini`、`claude-3-5-haiku` 一类）；摘要请求通常只有一次，增量模式下每次压缩只处理新增消息。
- `max_summary_tokens` 越大，压缩比越低但信息保留更完整；默认 1200 适合代码类长对话。
- `trigger_ratio` 调低会更早压缩（省上游额度、摘要请求更多）；调高则更接近上限才压缩。
- 摘要请求失败但 `fail_open=true` 时，请求会带着完整历史继续打到上游，可能直接超出上游限制；对长会话建议 `fail_open=false`。

## 排查

| 现象 | 排查方向 |
| --- | --- |
| 响应头 `X-Relay-Compression: skipped` + `not_configured` | 没填 `base_url` 或 `model`，或没勾启用 |
| `below_threshold` | 消息总量没到阈值；调低 `trigger_ratio` 或用 `force` 头验证 |
| `summarizer_failed` | 看「压缩运行统计」里的事件 `error_message`；多为 401/404（Base URL 少了 `/v1`）或超时 |
| 压缩后上游仍报超长 | 摘要过长或尾部保留过多：调小 `max_summary_tokens` / `keep_recent_messages` |
| 工具调用报错 | 折叠在 `tool_calls` 处停止是设计行为；检查是否把 `min_fold_messages` 设得过小 |
| 想重新压缩某会话 | 「会话摘要」里删除该行，或 `DELETE /api/compression/sessions/{key}` |

## 测试

```bash
go test ./cmd/server -run 'Compression|EstimateTextTokens|PlanCompression' -v
cd web && npx vitest run src/main.test.tsx
```

后端覆盖：tokens 估算、折叠计划（pin/停止条件/阈值）、配置往返与校验、摘要请求形状（OpenAI 与 Anthropic）、增量复用与历史改写回退、`X-Relay-Compression` 头控制、失败放行与失败阻断、管理端点、网关端到端（摘要请求与折叠后的上游请求体）。
