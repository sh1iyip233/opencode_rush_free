# 部署到服务器（人话版）

这份文档只讲一件事：怎么把这个网关跑在你自己的 Linux 服务器上，并且能长期活着。
不解释架构，不讲原理，照着做就行。末尾有出问题时该看哪儿。

---

## 0. 先搞清楚你要部署的是什么

- **一个 Go 程序**（`opencode-proxy`）+ **一堆前端静态文件**（`web/dist`）+ **一个 SQLite 数据库文件**。
- 没有 MySQL、没有 Redis、没有别的中间件。数据库就是 `/data/app.db` 这一个文件。
- 程序**自己不处理 HTTPS**。公网访问要在前面挂 nginx（第 4 节）。
- 程序**入站**只开一个端口（默认 8080）；**出站**要能连到你的代理池和上游 API。
- 服务器最低配：1 核 512M 够跑，推荐 1 核 1G。系统 Debian 12 / Ubuntu 22.04 及以上都行。

---

## 1. 三个密钥（唯一需要你自己想的配置）

```bash
# 在服务器上执行，生成三个随机串
openssl rand -hex 32   # → APP_ENCRYPTION_KEY
openssl rand -hex 32   # → SESSION_SECRET
openssl rand -hex 16   # → ADMIN_PASSWORD（这就是你的后台登录密码）
```

三条硬规矩，先记住再往下走：

| 变量 | 规矩 |
| --- | --- |
| `APP_ENCRYPTION_KEY` | 至少 32 字符。**丢了就完蛋**——库里存的上游 API Key、代理账号密码全是它加密的，丢了只能全部重新录一遍。请抄进密码管理器。 |
| `SESSION_SECRET` | 至少 32 字符。换掉它 = 所有已登录会话立刻失效（不算灾难，但别随手改）。 |
| `ADMIN_PASSWORD` | **只在第一次启动时用一次**，之后密码存在数据库里，改 env 不再生效。改密码走界面。不能填 `admin`，也不能带 `change-me` 之类的字样，否则程序拒绝启动。 |

> 想换 `APP_ENCRYPTION_KEY` 又不想重新录数据？可以：先把旧值填到 `APP_ENCRYPTION_KEY_PREVIOUS`，新值填到 `APP_ENCRYPTION_KEY`，启动一次，程序会自动把库里所有加密字段用新密钥重写，日志里会打印重写了多少条。看到成功后再把 `APP_ENCRYPTION_KEY_PREVIOUS` 删掉。

---

## 2. 路线 A：Docker Compose（推荐，最省事）

### 2.1 装 Docker

```bash
curl -fsSL https://get.docker.com | sh
docker --version && docker compose version
```

### 2.2 把代码弄到服务器

```bash
git clone https://github.com/sh1iyip233/opencode_rush_free.git /opt/relaydesk
cd /opt/relaydesk
```

（不想用 git 就把整个目录 `scp` 上去，效果一样。）

### 2.3 写配置文件

```bash
cp .env.example .env
nano .env
```

`.env` 里填成这样（**文件名必须叫 `.env`，必须放在仓库根目录**，compose 就是从这里读的）：

```ini
ADMIN_PASSWORD=把第1节生成的那个16字节随机串填进来
APP_ENCRYPTION_KEY=把第1节生成的那个32字节随机串填进来
SESSION_SECRET=把第1节生成的那个32字节随机串填进来

PORT=8080
BIND_ADDRESS=127.0.0.1
COOKIE_SECURE=false
MAX_CONCURRENT_REQUESTS=100
```

`BIND_ADDRESS=127.0.0.1` 的意思是「只在宿主机本地监听 8080，外面进不来，必须经过 nginx」。这是对的，别改成 `0.0.0.0` 图省事。

### 2.4 启动

```bash
docker compose up -d --build     # 第一次要构建前端+后端，几分钟
docker compose ps                # 看到 healthy 就好了
docker compose logs -f opencode-proxy   # 看日志，Ctrl+C 退出
```

### 2.5 验证

```bash
curl -s localhost:8080/healthz
# {"status":"ok"}
```

出现 `{"status":"ok"}` 就说明程序活着、数据库能读。

### 2.6 几个要知道的点

- 数据在 Docker 卷 `relaydesk_opencode_data` 里，容器删了数据还在。别用 `docker compose down -v`，那个 `-v` 会把卷一起删掉。
- 容器以 `10001` 用户运行，卷的属主由 compose 里的 `volume-init` 自动处理。**不要手动 `chown root`**，会写不进数据库。
- 容器根文件系统是只读的，只挂了 `/data` 和 `/tmp`。这是故意的，别改成可写。

---

## 3. 路线 B：裸二进制 + systemd（不想装 Docker 就用这个）

### 3.1 在你自己电脑上构建

前端（需要 Node 22+）：

```bash
cd web
npm ci
npm run build          # 产物在 web/dist
cd ..
```

后端（Windows 上也能交叉编译出 Linux 版本）：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o opencode-proxy ./cmd/server
```

> 要 arm 服务器（比如甲骨文 ARM）就把 `GOARCH=amd64` 换成 `GOARCH=arm64`。
> 需要 Go 1.26。

### 3.2 传到服务器

```bash
ssh root@你的服务器
useradd --system --home /opt/relaydesk --shell /usr/sbin/nologin relaydesk
mkdir -p /opt/relaydesk/web /var/lib/relaydesk
```

回到本地电脑上传：

```bash
scp opencode-proxy root@你的服务器:/opt/relaydesk/
scp -r web/dist root@你的服务器:/opt/relaydesk/web/
```

### 3.3 写环境文件和权限

```bash
cat > /etc/relaydesk.env <<'EOF'
ADMIN_PASSWORD=第1节的16字节随机串
APP_ENCRYPTION_KEY=第1节的32字节随机串
SESSION_SECRET=第1节的32字节随机串
PORT=8080
DATABASE_PATH=/var/lib/relaydesk/app.db
WEB_DIR=/opt/relaydesk/web/dist
COOKIE_SECURE=false
MAX_CONCURRENT_REQUESTS=100
EOF

chmod 600 /etc/relaydesk.env
chown -R relaydesk:relaydesk /opt/relaydesk /var/lib/relaydesk
chmod 755 /opt/relaydesk/opencode-proxy
```

### 3.4 systemd 服务

```bash
cat > /etc/systemd/system/relaydesk.service <<'EOF'
[Unit]
Description=Relay Desk gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=relaydesk
Group=relaydesk
WorkingDirectory=/opt/relaydesk
EnvironmentFile=/etc/relaydesk.env
ExecStart=/opt/relaydesk/opencode-proxy
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/relaydesk
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now relaydesk
systemctl status relaydesk --no-pager
journalctl -u relaydesk -f        # 看实时日志
```

### 3.5 裸机部署必须注意的一件事

这个程序**只认 `PORT`，不认 `BIND_ADDRESS`**，所以它监听的是 `0.0.0.0:8080`——所有网卡都开着。
所以一定要做下面两件事之一（两个都做最好）：

```bash
# 只允许本机访问 8080（有 nginx 反代时这样做）
ufw deny 8080
# 或者干脆只放行 22/80/443
ufw allow 22 && ufw allow 80 && ufw allow 443 && ufw enable
```

---

## 4. 前面挂 nginx + HTTPS（两条路线都要做）

### 4.1 装 nginx 和证书

```bash
apt install -y nginx
apt install -y certbot python3-certbot-nginx
```

### 4.2 站点配置

`/etc/nginx/sites-available/relaydesk`：

```nginx
server {
    listen 80;
    server_name 你的域名;

    client_max_body_size 32m;          # 请求体可能带图片，别用默认的 1m

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # ↓↓↓ 这三行是流式输出（SSE）能不能正常工作的关键 ↓↓↓
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
    }
}
```

```bash
ln -s /etc/nginx/sites-available/relaydesk /etc/nginx/sites-enabled/
nginx -t && systemctl reload nginx
certbot --nginx -d 你的域名          # 自动改配置 + 自动续期
```

> **`proxy_buffering off` 忘了写会怎样？** 请求不会报错，但流式回答会「憋一大坨再一次性吐出来」，看起来像卡死。这是最常见的部署翻车点。

### 4.3 上了 HTTPS 之后改一个变量

```bash
# .env（或 /etc/relaydesk.env）里
COOKIE_SECURE=true
```

```bash
docker compose up -d          # 或者 systemctl restart relaydesk
```

不改的话：浏览器会拒绝保存登录 Cookie，表现为「登录成功但立刻又跳回登录页」。

---

## 5. 第一次打开后台，按这个顺序做

浏览器打开 `https://你的域名/`，只有密码框，填 `ADMIN_PASSWORD`。

**顺序别乱，每一步都依赖上一步：**

1. **上游设置**（Settings → Upstream）
   - `Base URL`：填到 `/v1` 之前那一层。比如上游是 `https://api.example.com/v1/chat/completions`，这里就填 `https://api.example.com/v1`。多写一层 `/v1` 会变成 `/v1/v1/...`，请求全 404。
   - `API Key`：上游的 key。
   - 有识图需求再填 Vision 那几项，没需求留空。
   - 填完点「测试」，通了再往下。
2. **代理池**（Proxies）
   - 添加或批量导入。**批量导入是一行一个完整 URI**，不是 `host:port:user:pass` 那种冒号串：
     ```
     http://user:pass@1.2.3.4:8080
     socks5://user:pass@5.6.7.8:1080
     http://9.9.9.9:3128          ← 没有账号密码就省略 user:pass@
     ```
     支持的协议只有 `http` / `https` / `socks5` / `socks5h`。
   - 导入后跑一次「探测」，看每个出口的延迟、出口 IP、健康状态。
   - 探测全绿再继续，否则后面调模型会一直 503。
3. **客户端 Key**（Client Keys）
   - 新建一个，**密钥只显示一次**，当场复制走。
4. **客户端接进来**
   - `Base URL` 填 `https://你的域名/v1`
   - `API Key` 填第 3 步那个客户端 key
   - 先用 `curl https://你的域名/v1/models -H "Authorization: Bearer <key>"` 确认能列模型。

---

## 6. 日常运维

### 备份（一定要做，5 分钟的事）

数据库是 SQLite，**冷备份最稳**：

```bash
# Docker 路线
docker compose stop opencode-proxy
cp -a /var/lib/docker/volumes/relaydesk_opencode_data/_data/app.db* /root/backup/
docker compose start opencode-proxy

# 裸机路线
systemctl stop relaydesk
cp -a /var/lib/relaydesk/app.db* /root/backup/
systemctl start relaydesk
```

要热备份就用 sqlite3 的在线备份（不用停机）：

```bash
sqlite3 /var/lib/relaydesk/app.db ".backup '/root/backup/app-$(date +%F).db'"
```

> 备份里包含加密后的上游 key 和代理密码。**`APP_ENCRYPTION_KEY` 要和备份一起保管**，只有 db 没有密钥，等于一堆乱码。

### 升级

```bash
# Docker
git pull && docker compose up -d --build

# 裸机
# 本地重新构建 → scp 新二进制 → systemctl restart relaydesk
```

数据库结构变更在启动时自动完成，不用手动跑迁移。

### 回滚

留一份上一个版本的二进制 + 一份对应时间的 db 备份。回滚就是换回二进制、恢复 db、重启。

---

## 7. 出问题看哪儿

先跑这一条，90% 的问题它就能告诉你原因：

```bash
docker compose logs --tail=100 opencode-proxy     # 或 journalctl -u relaydesk -n 100
```

| 现象 | 日志关键字 / 原因 | 怎么办 |
| --- | --- | --- |
| 容器起不来 | `APP_ENCRYPTION_KEY must contain at least 32 characters` | 密钥不够 32 字符，重新生成 |
| 容器起不来 | `ADMIN_PASSWORD is required for first startup` | 首次启动没填密码，或者填了 `admin`/占位符 |
| 打开是白页 / 404 | 日志没有报错，但 `WEB_DIR` 指错了 | 确认 `web/dist/index.html` 真的存在 |
| 登录成功又跳回登录页 | `COOKIE_SECURE=true` 但访问的是 http | 上 HTTPS，或者临时改回 `false` |
| 请求返回 503 `no healthy proxies available` | 代理全在冷却/过期/停用 | 去代理页跑探测；刚冷却的出口 1~30 分钟会自动回池 |
| 请求返回 502 `all proxies failed` | 上游 key 或 base_url 错，或者代理全挂 | 点「测试上游」；看详情里的 attempt 记录 |
| 流式回答像卡住 | nginx 没关 `proxy_buffering` | 第 4.2 节那三行补上 |
| 上传图片 413 | nginx 请求体上限 | `client_max_body_size 32m;` |
| 后台密码忘了 | 密码在库里，改 env 没用 | `sqlite3 app.db "DELETE FROM settings WHERE key='admin_hash';"` 然后重启，会用 env 里的 `ADMIN_PASSWORD` 重设 |
| 所有出口都 401/403 | 上游在按模型拒你，不是代理坏了 | 同一个出口对同一个模型连续被拒 2 次就会被标记封禁，10 分钟后自动解除；换模型或补出口即可，界面暂时不展示这个状态 |

---

## 8. 一句话检查清单

上线前对着勾一遍：

- [ ] `APP_ENCRYPTION_KEY` / `SESSION_SECRET` 都是随机生成、≥32 字符、已抄进密码管理器
- [ ] `ADMIN_PASSWORD` 不是 `admin`，不是占位符
- [ ] `.env` 权限 600，没有提交进 git
- [ ] `curl localhost:8080/healthz` 返回 `{"status":"ok"}`
- [ ] nginx 里写了 `proxy_buffering off` 和 `client_max_body_size`
- [ ] 上了 HTTPS 且 `COOKIE_SECURE=true`
- [ ] 8080 端口没有直接暴露在公网
- [ ] 后台里：上游测试通过 → 代理探测全绿 → 客户端 key 已创建并试过 `/v1/models`
- [ ] 备份跑过一次，且知道 `APP_ENCRYPTION_KEY` 存在哪
