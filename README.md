<div align="center">  
  <h1>Relay Desk</h1>  
  <p><strong>OpenAI 兼容的模型转发网关</strong></p>  
  <p>  
    <img src="https://img.shields.io/badge/platform-Docker%20%7C%20Linux-2496ED?logo=docker\&logoColor=white" alt="Docker and Linux">  
    <img src="https://img.shields.io/badge/Go-1.26.5-00ADD8?logo=go\&logoColor=white" alt="Go 1.26.5">  
    <img src="https://img.shields.io/badge/React-19-20232A?logo=react\&logoColor=61DAFB" alt="React 19">  
    <img src="https://img.shields.io/badge/SQLite-WAL-003B57?logo=sqlite\&logoColor=white" alt="SQLite WAL">  
  </p>  
</div>

Relay Desk 使用单实例 Go + SQLite，支持上游模型、HTTP/HTTPS/SOCKS5 代理池、Resin 路由、Client Key、模型别名、探测、统计、Webhook 告警，以及可自由配置摘要 AI 接口的[上下文压缩网关](docs/compression.md)。

## 快速开始

### Docker Compose（推荐）

```bash
cp .env.example .env
# 编辑 .env，填写必需密钥
docker compose up -d --build
docker compose ps
curl http://127.0.0.1:8080/healthz
```

管理台和网关默认地址：`http://127.0.0.1:8080`

`.env` 里的 `PORT` 改成别的（例如 `6320`）时，`docker compose` 的端口映射和健康检查会跟着走，访问地址也要一起换成 `http://127.0.0.1:6320`。详细的服务器部署步骤见 [docs/deploy.md](docs/deploy.md)。

### 本地开发

```bash
go test ./...
cd web && npm ci && npm run build
cd .. && go run ./cmd/server
```

要求：Go `1.26.5+`、Node.js `22+`、npm。

## 配置

首次启动至少设置：

```dotenv
ADMIN_PASSWORD=强管理员密码
APP_ENCRYPTION_KEY=至少32个字符的随机密钥
SESSION_SECRET=至少32个字符的随机密钥
```

常用选项：

| 变量                        | 默认值             | 说明                |
| ------------------------- | --------------- | ----------------- |
| `PORT`                    | `8080`          | 服务端口              |
| `BIND_ADDRESS`            | `127.0.0.1`     | 公网部署设为 `0.0.0.0`  |
| `COOKIE_SECURE`           | `false`         | HTTPS 部署设为 `true` |
| `MAX_CONCURRENT_REQUESTS` | `100`           | 网关并发上限            |
| `DATABASE_PATH`           | `./data/app.db` | SQLite 路径         |
| `LOG_API_ADDR`            | 空               | 日志接口的独立监听地址（如 `0.0.0.0:8099`），空表示跟随 `PORT` |

不要提交 `.env`、数据库文件或任何明文凭证。`APP_ENCRYPTION_KEY` 更换前需要先完成密钥迁移。

## 首次配置

1. 使用 `ADMIN_PASSWORD` 登录管理台。
2. 上游是内置的 OpenCode Zen 匿名免费通道，地址与认证写死在程序里，无需配置；只需确认「测试上游连通性」能通。免费通道要求请求呈「官方客户端形态」（流式 + `bash`/`read` 工具 + `ses_` 会话头），程序自动整形，非流式客户端也会拿回一个折好的 JSON。
3. 添加代理，或选择 Resin 路由。
4. 刷新模型，按需启停模型或创建别名。
5. 在“模型与网关”页复制客户端 Base URL；在“客户端 Key”区域创建 Client Key，并将两者配置到 OpenCode：
   ```text
   http(s)://服务器地址:端口/v1
   ```
6. 可选：在“压缩网关”页配置摘要 AI 接口（OpenAI 兼容或 Anthropic Messages），长会话会自动折叠较早消息。详见 [docs/compression.md](docs/compression.md)。

Resin 故障时保持 Resin 路由并返回错误，不会自动降级到内置代理池。旧版 `settings.client_key` 会自动迁移为 `Legacy` Key；旧 Key 只有哈希时无法恢复明文，需要在控制台中轮换。

Client Key 的认证哈希和加密副本均保存在服务器侧。列表默认只显示掩码，管理员可按需显示或复制可恢复的 Key。删除或轮换会立即使旧 Key 失效。

## 网关接口

```http
GET  /v1/models
POST /v1/chat/completions
```

请求头：

```http
Authorization: Bearer <client-key>
Content-Type: application/json
```

压缩网关启用后，响应会带 `X-Relay-Compression`、`X-Relay-Compression-Reason`、`X-Relay-Compression-Saved-Tokens` 等头说明本次折叠情况；请求时带 `X-Relay-Compression: off` 可对单次调用关闭压缩，带 `force` 可强制压缩。细节见 [docs/compression.md](docs/compression.md)。

请求示例：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <client-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"<enabled-model>","messages":[{"role":"user","content":"Hello"}]}'
```

## 日志接口（给 AI / 脚本排障用）

进程把每个请求、每次上游轮换、每次鉴权失败、每行 `log.Printf` 都写进 SQLite 的
`log_events`，并通过 `/api/logs*` 暴露出来。**鉴权密钥就是面板登录密码**：

```bash
export RELAY=http://127.0.0.1:8080
export KEY=$ADMIN_PASSWORD

curl -s -H "X-Relay-Log-Key: $KEY" $RELAY/api/logs/diagnostics | jq '.summary, .checks[] | select(.status!="ok")'
curl -s -H "X-Relay-Log-Key: $KEY" "$RELAY/api/logs?level=error&limit=20" | jq '.events[]'
curl -N -H "X-Relay-Log-Key: $KEY" "$RELAY/api/logs/stream?level=error,warn&backlog=50"
```

`/api/logs/diagnostics` 会跑一组自检规则（5xx 率、延迟、panic、代理池、并发、库体积…）
并给出 `healthy` / `degraded` / `broken` 结论；`request_id` 能把一次调用的全部尝试串起来；
`/api/logs/notes` 是回写通道，改完代码可以把结论留在系统里。凭证在落库前统一脱敏。
完整接口、查询参数与排障剧本见 [docs/logs.md](docs/logs.md)。

## 管理 API

管理 API 使用登录后的 `ocp_session` Cookie。

| 分类         | 主要接口                                                                             |
| ---------- | -------------------------------------------------------------------------------- |
| 设置         | `/api/settings/upstream`、`/api/settings/proxy-engine`、`/api/settings/routing`    |
| Client Key | `/api/client-keys`、`/api/client-keys/{id}/rotate`、`/api/client-keys/{id}/secret` |
| 模型         | `/api/models`、`/api/settings/models/refresh`、`/api/model-aliases`                |
| 代理         | `/api/proxies`、`/api/proxies/import`、`/api/proxy-probes`                         |
| 统计         | `/api/stats/*`、`/api/usage/*`                                                    |
| 告警         | `/api/settings/alerts`                                                           |
| 压缩网关       | `/api/settings/compression`、`/api/compression/stats`、`/api/compression/sessions`、`/api/compression/preview` |

## 公网部署与备份

公网监听：

```dotenv
BIND_ADDRESS=0.0.0.0
```

生产环境建议使用 HTTPS 反向代理，并限制管理台来源。

升级前备份 SQLite：

```bash
docker compose stop opencode-proxy
stamp=$(date -u +%Y%m%dT%H%M%SZ)
data=/var/lib/docker/volumes/relaydesk_opencode_data/_data
cp "$data/app.db" "$data/app.db.backup-$stamp"
[ -f "$data/app.db-wal" ] && cp "$data/app.db-wal" "$data/app.db.backup-$stamp-wal"
[ -f "$data/app.db-shm" ] && cp "$data/app.db-shm" "$data/app.db.backup-$stamp-shm"
docker compose start opencode-proxy
```

## 测试

```bash
go test ./...
go vet ./cmd/server
cd web
npm test -- --run
npm run build
npm run test:e2e
```

Linux CI 可额外运行 `go test -race ./...`。
