# VpnGate

一个本地 VPN Gate 管理工具：提供 **节点浏览 Web 页面**、**单节点 OpenVPN 测试**、**推荐节点连接**、**自动连接/监测 Runner** 和 **SOCKS5 代理出口**。

适合用来：

- 浏览并筛选 VPN Gate 在线节点
- 在本机或容器内测试节点是否可连
- 连接推荐节点或指定节点
- 通过本地 SOCKS5 代理使用已连接的 VPN
- 让 Runner 自动检测网络状态并在需要时重新选点

## 功能概览

- 从 VPN Gate iPhone API 拉取节点列表
- 按推荐规则排序节点（综合 Score / Speed / Ping 等因素）
- 按关键词、国家筛选节点
- 展示节点主机名、IP、国家、Ping、速度、在线时长、会话数、用户数、流量、备注等信息
- 支持单节点 OpenVPN 测试
- 支持连接推荐节点 / 指定节点 / 断开连接
- 提供本地 SOCKS5 代理入口
- 支持 Runner 自动连接、监控探活、失败隔离与重试

## 组件说明

项目包含两个主要进程：

### 1. `vpngate-web`

负责：

- 拉取并展示 VPN Gate 节点列表
- 页面筛选、刷新、状态展示
- 调用 Runner 控制接口发起连接 / 断开 / 测试

默认独立运行端口：`8080`

### 2. `vpngate-runner`

负责：

- 管理 OpenVPN 连接生命周期
- 提供 SOCKS5 代理
- 对外暴露本地控制接口
- 自动选点、自动连接、监控连通性、失败隔离

默认端口：

- 控制接口：`127.0.0.1:18081`
- SOCKS5：`127.0.0.1:1080`（Compose 对外映射为 `10080`）

### 为什么拆成两个进程

Web 和 Runner 分离后，可以避免 OpenVPN 修改路由时影响 Web 管理页面的可访问性。

## 目录结构

```text
.
├── main.go                       # Web 服务入口
├── cmd/
│   └── vpngate-runner/           # Runner 入口
├── internal/
│   ├── web/                      # 页面路由、渲染、筛选、刷新、控制逻辑
│   ├── runner/                   # OpenVPN / SOCKS5 / 自动连接 / 控制 API
│   ├── runnerclient/             # Web 到 Runner 的客户端封装
│   └── vpngate/                  # VPN Gate API 抓取、解析、排序、OpenVPN 测试
├── docker-compose.yml            # Linux 启动方案
├── docker-compose.macos.yml      # macOS + Docker Desktop 启动方案
└── Dockerfile                    # 同时构建 web 与 runner 二进制
```

## 环境要求

### Docker 方式

- Docker
- Docker Compose
- Linux 宿主机，或 macOS + Docker Desktop

### 本地 Go 方式

- Go `1.26.1`
- 如果要使用 Runner / OpenVPN 测试，需要本机安装 `openvpn`
- 需要具备创建网络接口、修改路由等权限的运行环境

## 快速开始

### 方式一：Docker Compose（推荐）

仓库提供两个 Compose 文件：

- `docker-compose.yml`：适用于原生 Linux 主机
- `docker-compose.macos.yml`：适用于 macOS + Docker Desktop

启动后默认访问地址：

- Web 页面：`http://localhost:8082`
- SOCKS5 代理：`127.0.0.1:10080`

#### Linux

```bash
docker compose up --build -d
```

查看日志：

```bash
docker compose logs -f vpngate-web
docker compose logs -f vpngate-runner
```

停止：

```bash
docker compose down
```

#### macOS

```bash
docker compose -f docker-compose.macos.yml up --build -d
```

查看日志：

```bash
docker compose -f docker-compose.macos.yml logs -f vpngate-web
docker compose -f docker-compose.macos.yml logs -f vpngate-runner
```

停止：

```bash
docker compose -f docker-compose.macos.yml down
```

#### macOS 说明

- Docker Desktop 中的 Linux 容器运行在一个轻量 Linux VM 里
- `vpngate-runner` 使用的是该 Linux VM / 容器侧的 TUN 能力，不是 macOS 宿主机上的 `utun`
- 因此 `docker-compose.macos.yml` 不会绑定 macOS 宿主机的 `/dev/net/tun`
- 如果 Docker Desktop 无法提供可用的 TUN / OpenVPN 能力，请改用原生 Linux 或 Linux VM 运行

### 方式二：本地 Go 运行

#### 只启动 Web 页面

```bash
go run .
```

默认监听：`http://127.0.0.1:8080`

如果此时没有单独启动 Runner，页面仍可浏览节点列表，但 VPN 连接、状态查询、测试等 Runner 相关能力会不可用。

#### 启动完整本地环境

终端 1：启动 Runner

```bash
go run ./cmd/vpngate-runner
```

终端 2：启动 Web

```bash
go run .
```

自定义 Web 端口：

```bash
PORT=8081 go run .
```

## 常用命令

```bash
# 运行 Web
go run .

# 运行 Runner
go run ./cmd/vpngate-runner

# 构建
go build .
go build ./...

# 测试
go test ./...
go test ./internal/vpngate

# 联机测试（默认跳过）
VPNGATE_LIVE_TEST=1 go test ./internal/vpngate -run '^TestFetchIPhoneServersLive$' -count=1

# 静态检查
go vet ./...

# 格式化
gofmt -w main.go internal/web/app.go
gofmt -l .
```

## 环境变量

### Web 服务

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | Web 服务监听端口 |
| `RUNNER_API_URL` | `http://127.0.0.1:18081` | Runner 控制接口地址 |

### Runner 服务

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RUNNER_CONTROL_ADDR` | `:18081` | Runner 控制接口监听地址 |
| `SOCKS_LISTEN_ADDR` | `0.0.0.0:1080` | SOCKS5 代理监听地址 |
| `SOCKS_BYPASS_CIDRS` | 空 | SOCKS5 直连网段，逗号分隔 |
| `AUTO_CONNECT` | `true` | 是否启用自动连接与自动守护 |
| `MONITOR_URL` | `https://www.gstatic.com/generate_204` | HTTP 探活地址 |
| `MONITOR_FAILURE_THRESHOLD` | `3` | 连续多少次“VPN 探活失败但直连复核成功”后才判定节点失效 |
| `TCP_PROBE_ADDRESS` | 空 | 可选 TCP 探针地址 |
| `TCP_PROBE_TIMEOUT` | `3s` | TCP 探针超时 |
| `OPENVPN_CONNECT_TIMEOUT` | `30s` | OpenVPN 整体建连超时（Runner watchdog） |
| `MONITOR_INTERVAL` | `20s` | 监测间隔 |
| `MONITOR_TIMEOUT` | `6s` | 监测超时 |
| `FETCH_TIMEOUT` | `30s` | 自动选点时抓取节点列表超时 |
| `CONNECT_COOLDOWN` | `5s` | 自动重连冷却时间 |
| `MONITOR_STABLE_AFTER` | `10s` | 建连后多长时间开始稳定性监测 |
| `NODE_QUARANTINE` | `5m` | 节点失败后的基础隔离时间 |
| `BYPASS_ROUTE_TABLE` | `100` | Linux 策略路由表编号 |
| `BYPASS_FWMARK` | `1` | Linux 路由标记 |

例如：

```bash
AUTO_CONNECT=false MONITOR_INTERVAL=15s docker compose -f docker-compose.macos.yml up --build -d
```

## 健康检查与控制接口

### Web

- `GET /health`：健康检查
- `POST /refresh`：刷新节点列表

### Runner

- `GET /health`：健康检查
- `GET /status`：获取连接状态
- `POST /connect`：连接指定节点
- `POST /disconnect`：断开当前连接
- `POST /test`：测试指定节点

## 说明与限制

- 节点数据来自上游 VPN Gate API：`https://www.vpngate.net/api/iphone/`
- 首次启动时会自动刷新一次节点列表；如果失败，服务仍会继续启动
- 单节点测试和持久连接依赖 `openvpn`
- Docker 场景下，Runner 需要 `privileged` / `NET_ADMIN` 等网络能力
- macOS Docker Desktop 场景依赖 Linux VM 的网络能力，不等同于直接控制 macOS 宿主机网络
- 如果你只需要查看节点列表，可以只运行 Web；如果需要连接、SOCKS5、自动守护能力，则需要同时运行 Runner
