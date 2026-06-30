# 探针 · 服务器监控

一个自托管的、类似哪吒探针的服务器监控系统,使用 Go 实现。包含:

- **Dashboard(服务端)**:接收 Agent 上报、持久化历史数据、提供 REST API 和实时 Web 仪表盘
- **Agent(探针)**:部署到被监控机器,采集 CPU / 内存 / 磁盘 / 网络 / 进程 / 负载等指标,通过 WebSocket 上报
- **Web UI**:深色仪表盘,服务器概览列表 + 单机详情页(实时图表、磁盘/网络/进程表)

## 架构

```
┌────────────┐  wss(加密)  ┌────────────┐  wss 推送   ┌─────────┐
│   Agent    │ ───────────▶ │ Dashboard  │ ──────────▶ │  浏览器  │
│ (被监控机) │  指标(单向)  │ (服务端)    │  实时状态    │ (Web UI) │
└────────────┘              └────────────┘ ◀────────── └─────────┘
                              SQLite 持久化历史   REST API(只读)
```

Agent 到 Dashboard、浏览器到 Dashboard 都支持 TLS(`wss://`/`https`)。Agent 与 Dashboard 之间用 token 认证,数据单向流动(无远程命令执行)。Dashboard 把每个 Agent 的实时状态缓存到内存,同时写入 SQLite 做历史图表数据(默认保留 2 小时)。浏览器通过 WebSocket 接收实时推送。

## 快速部署

提供一键脚本,适合 Linux 服务器(systemd)。

### 方式一:一键脚本部署(推荐)

**在服务端(Dashboard)机器上:**

```bash
git clone <你的仓库> probe && cd probe

# 1a. 已有域名证书(推荐,Let's Encrypt 或商业证书直接复用)
sudo CERT=/etc/letsencrypt/live/你的域名/fullchain.pem \
     KEY=/etc/letsencrypt/live/你的域名/privkey.pem \
     ./scripts/install-dashboard.sh

# 1b. 有域名但没证书(脚本自动用 Let's Encrypt 申请)
sudo DOMAIN=监控.example.com ./scripts/install-dashboard.sh

# 1c. 没域名(自签证书,适合内网/测试)
sudo ./scripts/install-dashboard.sh
```

脚本会自动:安装 Go(如缺)、编译二进制、签发/复用证书、生成随机的 secret 和 web-token、创建并启动 systemd 服务。完成后会打印:

```
访问地址:    https://1.2.3.4:8443/?token=a1b2c3...
Agent 密钥:  3f7a9b...e8d2
Web 访问口令: a1b2c3...
Agent 连接命令: ./agent -server ...
```

> 务必保存上面三样信息,不会再次显示。

证书优先级:`CERT`/`KEY` 显式传入 > `DOMAIN` 自动申请 > 自签。Agent 连接时:正式证书用 `-tls`(自动校验),自签证书加 `-tls -insecure`(连接仍加密)。

**在被监控机(Agent)上:**

```bash
git clone <你的仓库> probe && cd probe

# 正式证书(有域名)
sudo SERVER=监控.example.com:8443 TOKEN=<上面打印的密钥> NAME=web-01 TLS=1 ./scripts/install-agent.sh

# 自签证书(加 INSECURE=1)
sudo SERVER=1.2.3.4:8443 TOKEN=<密钥> NAME=web-01 TLS=1 INSECURE=1 ./scripts/install-agent.sh
```

启动后该主机立即出现在面板里。多台机器重复执行,`NAME` 取不同名字区分。

**改主机名**:面板详情页点主机名旁的 ✎ 图标即可改名;或调 API:
```bash
curl -X POST -H "Content-Type: application/json" \
  -d '{"name":"生产服务器-01"}' \
  http://你的域名:8443/api/servers/<agentID>/rename
```
改名持久生效,Agent 重新上报不会覆盖。

### 方式二:交叉编译 + 手动部署

在开发机一次性编译所有平台:

```bash
./scripts/build-all.sh   # 产物在 dist/,含 linux/darwin/windows × amd64/arm64
```

把对应平台的二进制传到目标机器直接运行即可(`CGO_ENABLED=0`,无外部依赖):

```bash
# Dashboard(服务端)
./dashboard -addr 0.0.0.0:8443 \
  -secret <密钥> -web-token <口令> \
  -tls-cert cert.pem -tls-key key.pem \
  -db data/probe.db

# Agent(被监控机)
./agent -server 你的域名:8443 -token <密钥> -name "web-01" -tls
```

### 服务管理

```bash
systemctl status probe-dashboard       # Dashboard 状态
systemctl restart probe-dashboard      # 重启
journalctl -u probe-dashboard -f       # 实时日志

systemctl status probe-agent           # Agent 状态
systemctl restart probe-agent
journalctl -u probe-agent -f
```
开机自启已配好。证书续期后需重启服务才生效(Let's Encrypt 可加 deploy-hook):
```bash
# /etc/letsencrypt/renewal-hooks/deploy/restart-probe.sh
#!/bin/sh
systemctl restart probe-dashboard
```

### 参数速查

**Dashboard**

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `-addr` | `PROBE_ADDR` | 监听地址,默认 `127.0.0.1:8000` |
| `-secret` | `PROBE_SECRET` | Agent 认证密钥(**必填**) |
| `-web-token` | `PROBE_WEB_TOKEN` | Web 登录口令,留空则不启用登录 |
| `-tls-cert` | `PROBE_TLS_CERT` | TLS 证书(开启 wss/https) |
| `-tls-key` | `PROBE_TLS_KEY` | TLS 私钥 |
| `-db` | `PROBE_DB` | SQLite 路径,默认 `data/probe.db` |

**Agent**

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `-server` | `PROBE_SERVER` | Dashboard 地址 `host:port` |
| `-token` | `PROBE_TOKEN` | 与 Dashboard 的 secret 一致(**必填**) |
| `-name` | `PROBE_NAME` | 主机名,默认取系统主机名 |
| `-tls` | `PROBE_TLS` | 开启 TLS(`wss://`) |
| `-insecure` | `PROBE_INSECURE` | 跳过证书校验(自签证书用) |
| `-interval` | — | 采集间隔,默认 `3s` |

**在服务端(Dashboard)机器上:**

```bash
git clone <你的仓库> probe && cd probe
sudo DOMAIN=监控.example.com ./scripts/install-dashboard.sh
```

脚本会自动:安装 Go(如缺)、编译二进制、签发证书(有域名用 Let's Encrypt,否则自签)、生成随机的 secret 和 web-token、创建并启动 systemd 服务。完成后会打印访问地址、Agent 密钥和连接命令。

- `DOMAIN` 留空:用自签证书,监听 `:8443`,Agent 连接时加 `-tls -insecure`
- `DOMAIN` 指定:用 Let's Encrypt 正式证书,Agent 连接时只需 `-tls`(自动校验)

**在被监控机(Agent)上:**

```bash
git clone <你的仓库> probe && cd probe
sudo SERVER=监控.example.com:8443 TOKEN=<上面打印的密钥> NAME=主机名 ./scripts/install-agent.sh
# TLS 场景:TLS=1;自签证书再加:INSECURE=1
```

脚本自动编译、配置 systemd,启动后该主机就会出现在 Dashboard 列表里。

### 方式二:交叉编译 + 手动部署

在开发机一次性编译所有平台:

```bash
./scripts/build-all.sh          # 产物在 dist/,含 linux/darwin/windows × amd64/arm64
```

把对应的 `dist/dashboard-linux-amd64-*` 传到服务器,`dist/agent-linux-arm64-*` 传到 ARM 设备,直接运行即可(二进制无外部依赖,`CGO_ENABLED=0`)。

### 服务管理

```bash
systemctl status probe-dashboard     # 查看 Dashboard
systemctl restart probe-dashboard    # 重启
journalctl -u probe-dashboard -f     # 实时日志

systemctl status probe-agent         # 查看 Agent
journalctl -u probe-agent -f         # 实时日志
```

## 快速开始

### 编译

```bash
go build -o bin/dashboard ./cmd/dashboard
go build -o bin/agent ./cmd/agent
```

### 启动 Dashboard

```bash
# 不带 TLS(本机或反代后面)
./bin/dashboard -addr :8000 -secret <你的密钥> -db data/probe.db

# 直接出 wss/https(推荐,自签或正式证书)
./bin/dashboard -addr :8443 -secret <你的密钥> \
  -tls-cert data/cert.pem -tls-key data/key.pem \
  -web-token <访问口令> -db data/probe.db
```

生成自签证书:

```bash
openssl req -x509 -newkey rsa:2048 -keyout data/key.pem -out data/cert.pem \
  -days 365 -nodes -subj "/CN=<你的域名或IP>"
```

然后浏览器打开 `http://<服务器IP>:8000/`。

### 部署 Agent(在被监控机器上)

明文(仅本机/内网):

```bash
./bin/agent -server <服务器IP>:8000 -token <同样的密钥> -name "主机名"
```

TLS 加密(Dashboard 启用了 `-tls-cert` 时,推荐):

```bash
# 正式证书(域名匹配)直接校验
./bin/agent -server <域名>:8443 -token <密钥> -name "主机名" -tls
# 自签证书需跳过校验(仅私有网络,连接仍加密)
./bin/agent -server <IP>:8443 -token <密钥> -name "主机名" -tls -insecure
```

支持的环境变量替代: `PROBE_SERVER` / `PROBE_TOKEN` / `PROBE_NAME` / `PROBE_ID`。

### 交叉编译到其他平台

```bash
# Linux amd64
GOOS=linux GOARCH=amd64 go build -o bin/agent-linux-amd64 ./cmd/agent
# Linux arm64
GOOS=linux GOARCH=arm64 go build -o bin/agent-linux-arm64 ./cmd/agent
```

`modernc.org/sqlite` 是纯 Go 实现,Dashboard 无需 CGO 即可交叉编译。

## 采集的指标

- CPU:总使用率、每核使用率、型号、核心数、温度
- 内存:物理内存/Swap 总量与用量
- 磁盘:各挂载点设备、类型、容量、使用率
- 网络:上下行速率(增量计算)、累计流量、各网卡的 IPv4/IPv6/MAC
- 进程:按内存排序的 Top 10(名称、PID、CPU%、RSS)
- 系统:操作系统、架构、运行时间、负载均值、TCP 连接数

## 传输加密

Agent 与 Dashboard 之间支持原生 TLS(`wss://`):

- **Dashboard** 用 `-tls-cert`/`-tls-key` 指定证书即可启用,同时 Web UI 也变成 HTTPS。
- **Agent** 用 `-tls` 连接 `wss://`;默认严格校验证书,自签证书场景加 `-insecure` 跳过校验(连接仍然加密,但不验证服务端身份,仅适合受信私有网络)。
- 强制最低 TLS 1.2。

这样 Agent 上报的 secret 和所有指标数据在网络上是加密的,无法被中间人窃听或篡改。生产环境建议用 Let's Encrypt 等签发的正式证书,既加密又验证服务端身份。

## 通信协议

Agent ↔ Dashboard 走 WebSocket(`/agent`),消息为 JSON 封装,类型定义在 [internal/proto/proto.go](internal/proto/proto.go):

- `auth` / `auth_result`:连接后认证
- `state`:周期性指标快照
浏览器 ↔ Dashboard 走 WebSocket(`/ws`):服务端单向推送 `state`,浏览器不发送任何控制指令。

## 安全说明
本系统**不提供远程命令执行(WebShell)**。Agent 是纯上报模式,不接受任何来自服务端的命令;浏览器也不下发任何控制指令。这样可以避免像哪吒探针那样,一旦 Dashboard 被攻破,所有挂载的 Agent 被批量接管执行任意命令的风险。

此外还做了以下加固:

- **Web UI / API 访问口令**:`-web-token` 给浏览器面(`/api`、`/ws`、UI)加一层访问控制。配置后用 `http://host/?token=xxx` 打开页面,WS 和 fetch 会自动带上。留空则默认不设防(适合放在已有鉴权的反向代理后面)。
- **Agent 认证**:`-secret` 是 Agent 与 Dashboard 之间的共享密钥,用恒定时间比较(`subtle.ConstantTimeCompare`),认证失败不返回具体原因,直接关闭连接。
- **认证超时**:未认证的 Agent 连接必须在 10 秒内完成握手,否则被关闭,防止空连接占资源。
- **默认只监听本机**:`-addr` 默认 `127.0.0.1:8000`,不会无意暴露到公网;需要外网访问请显式设置并放到 TLS 反向代理后面。
- **XSS 防护**:前端表格(Disk/网卡/进程)用 DOM API(`textContent`)渲染,不拼 `innerHTML`,Agent 上报的进程名/挂载点等不可信数据无法注入脚本。
- **响应安全头**:静态资源返回 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`;JSON 接口返回 `nosniff`。
- **历史查询上限**:`/api/servers/{id}/history?minutes=` 限制在 24 小时内,避免一次请求拉取全表。

**生产部署建议**:Dashboard 放在 Nginx/Caddy + TLS 后面,用 `-addr 127.0.0.1:8000`,通过反向代理做 TLS 终止和(额外的)HTTP Basic Auth;`-secret` 和 `-web-token` 都用足够长的随机串。


本系统**不提供远程命令执行(WebShell)**。Agent 是纯上报模式,不接受任何来自服务端的命令;浏览器也不下发任何控制指令。这样可以避免像哪吒探针那样,一旦 Dashboard 被攻破,所有挂载的 Agent 被批量接管执行任意命令的风险。

## 目录结构

```
cmd/dashboard/      Dashboard 入口
cmd/agent/          Agent 入口
internal/proto/     共享消息类型
internal/dashboard/ 服务端:store(SQLite) + hub(连接/广播) + server(HTTP/WS)
internal/agent/     采集器 + WebSocket 客户端
web/                前端(index.html / style.css / app.js)
```

## 技术栈

- Go + gorilla/websocket + modernc.org/sqlite + gopsutil
- 前端:原生 JS + Chart.js(CDN)
