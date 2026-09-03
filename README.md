# Mira

Mira 把 Windows、WSL、Linux、NAS 和 Android 组织成一个由用户批准的私有设备网络。每台
设备运行统一的 Mira Node 并主动连接中央 Server；Codex 仍原生运行在选定设备上，但可以通过
`home_nodes` dynamicTools 或 `mira` CLI 操作其他在线设备。PostgreSQL 是 thread 历史唯一的
持久化事实来源。

当前版本是可运行的工程 PoC：存储、Node 接入、能力路由、App Server broker、共享 CLI 身份和
管理员网站已连通。0.9.0 增加原生 Windows ConPTY、统一版本、安装器和签名 APK 发布。
durable scheduler、writer lease、完整恢复演练和集群批量更新仍待完成。

## 安装与升级

在管理员网站展开「添加设备」即可复制带当前 Server 地址的安装命令。
Windows / Linux / WSL 提供一条指令安装，Android 直接安装正式签名 APK。
后续桌面端执行 `mira update`；Android 在 APP 内检查更新。身份和配置随升级保留。
具体命令、平台要求、服务启动方式及回退说明见 [INSTALL.md](./INSTALL.md)。

## 架构

```text
Codex / mira CLI / Admin Web
              │ HTTPS / WSS
              ▼
         Mira Server ───────── PostgreSQL
         │ auth + audit          authoritative events
         │ CapabilityService     immutable thread items
         │ App Server broker     rebuildable projections
         │
         └════ outbound WebSocket channels ════╗
              │                    │            │
          Windows/WSL          Linux/NAS    Android APK
          file/process/PTY     file/process  file/process
          Codex App Server     /PTY/Codex    screen/input
```

Server 在代理的 `thread/start` 和 `thread/resume` 中注入 `home_nodes`。运行在 Node A 的 Codex
可调用 Node B；subagent 继承 dynamicTools，但仍作为独立 thread 保存，并保留父子关系。所有 HTTP
CLI 调用和 dynamicTools 最终都经过同一个 `CapabilityService`。

## 身份和权限

v1 只有两类安全身份：

- 一个管理员账号：本地命令设置 Argon2id 密码，网页使用数据库 Session、严格 Cookie 和 CSRF；
- 每台设备一个 Node credential：`mira-node`、`mira` CLI、本地 Codex/App Server 共用同一个
  `identity.json`。

Node 在首次启动前生成 256-bit secret，只向 Server 提交 SHA-256。管理员在网站核对六位验证码并
批准后，设备才可注册和连接。所有 approved Node 在 v1 中彼此信任；目标设备的操作系统权限、
symlink、文件/输出/session 限制以及 Android 权限仍会独立执行。没有全局 `THREAD_STORE_TOKEN`、
CLI 登录、Node ACL 或长期 token query parameter。

详细协议见 [protocol/auth-v1.md](./protocol/auth-v1.md)，存储协议见
[protocol/thread-store-v2.md](./protocol/thread-store-v2.md)，稳定架构结论见 [AGENTS.md](./AGENTS.md)。

## 已实现组件

| 路径 | 作用 |
| --- | --- |
| `server/` | Node.js Mira Server、认证、审计、CapabilityService、App Server broker 与 ThreadStore API |
| `server/public/` | Server 同源提供的管理员设备控制台，无独立前端构建链 |
| `node/cmd/mira-node/` | Windows/Linux/WSL/Android 共用的常驻 Node |
| `node/cmd/mira/` | 人类和 Codex 共用的远程控制 CLI |
| `node/internal/node/` | 身份、接入、反向通道、文件、进程、PTY、屏幕和平台适配 |
| `node/android/app/` | root/非 root 统一 APK 外壳和 Android Framework bridge |
| `patches/codex/` | 官方 Codex ThreadStore HTTP 适配与 subagent dynamicTools 补丁 |
| `skills/mira/` | 可安装的 Codex 使用说明，不包含任何 credential 或固定设备信息 |
| `tests/` | 存储、认证、Node、App Server、subagent、多节点与 Android E2E |
| `scripts/` | 统一版本检查、跨平台 Release 构建、Linux/Windows 安装与更新 |

## 本地启动与网站验收

需要 Docker、Node.js 22+、Go 1.23+ 和 npm：

```bash
docker compose up -d postgres
npm ci --prefix server

# 只在 Server 主机本地运行；密码从隐藏终端或 stdin 读取
npm run admin --prefix server -- set-password admin

# loopback HTTP 验收时允许非 Secure Cookie；生产不要设置 false
MIRA_SECURE_COOKIES=false npm start --prefix server
```

打开 [http://127.0.0.1:8787](http://127.0.0.1:8787)。网站可登录、查看待审批申请、批准/拒绝、
查看 Node 状态/能力、撤销设备并检查追加式审计记录。每台在线设备还提供独立工作台：只读文件
浏览器默认从该 Node 运行身份可见的完整文件系统开始；交互式 Shell 复用带游标的 PTY session；概况页通过
轻量 `process/count` 展示当前 Node 可见的系统进程数，并展示 OS/CPU 配置、CPU 采样、内存和
各 allowed root 所在磁盘的用量、运行时间及网络接口。能力调试器直接读取注入 Codex 的
`home_nodes` dynamicTools schema，按 tool/action 提供预制参数表单，并通过同一调度路径执行真实调用；
高级模式仍可直接编辑完整 JSON arguments。生产由 Caddy 提供 HTTPS/WSS 后保持
`MIRA_SECURE_COOKIES=true`。

## 启动 Node 与审批

```bash
MIRA_SERVER_URL=http://127.0.0.1:8787 \
MIRA_NODE_KEY=wsl-main \
MIRA_IDENTITY_FILE=/home/user/.config/mira/identity.json \
CODEX_BINARY=/absolute/path/to/codex \
APP_SERVER_CODEX_HOME=/path/to/codex-home \
go -C node run ./cmd/mira-node
```

Node 显示 enrollment ID 和六位验证码，等待网站批准。身份文件默认位于 Linux/WSL 的
`~/.config/mira/identity.json`、Windows 的 `%USERPROFILE%\\.mira\\identity.json`，Android 使用
APK 私有 no-backup 目录；`MIRA_IDENTITY_FILE` 可覆盖。写入采用临时文件、原子 rename 和用户
Unix `0600` 权限；Windows 使用受保护的当前用户 / SYSTEM / Administrators DACL。

未配置 `MIRA_NODE_ALLOWED_ROOTS` 时，Linux/WSL/Android 从 `/` 开始，Windows 自动列出所有当前
可用盘符。最终能否读取仍由 `mira-node` 的 OS 用户权限决定。如需把某台 Node 收紧到特定工作区，
可显式设置 `MIRA_NODE_ALLOWED_ROOTS='["/path/to/workspace"]'`。

构建两个桌面命令：

```bash
go -C node build -o dist/mira-node ./cmd/mira-node
go -C node build -o dist/mira ./cmd/mira
```

## `mira` CLI

CLI 不单独登录，直接读取当前设备的 Node identity。所有命令支持 `--json` 和 `--timeout 30s`：

```bash
mira identity show --json
mira status
mira version
mira update --check
mira nodes list --json
mira codex                         # 本机 Codex 继承同一 Node credential
mira file read --node nas --path /data/report.txt --output /tmp/report.txt
mira process count --node homeserver --json
mira process run --node homeserver -- /usr/bin/git status --short
mira pty open --node wsl-main -- /bin/bash
mira screen screenshot --node android-phone --output /tmp/phone.png
mira app-server start --node wsl-main
mira app-server connect --node wsl-main
```

Node selector 可以是 UUID、精确 `nodeKey` 或唯一 hostname；歧义时失败。进程命令始终使用
executable + argv，不拼 shell 字符串。截图与大文件通过本地绝对路径/stdin 传输，避免进入 argv。

Windows 文件、系统进程数/列表、进程启动/终止、CPU/内存/磁盘/网络、真实 ConPTY 输入/VT/
resize/Ctrl-C，以及本机官方 Codex 自动发现和 App Server 启停已在 Windows 11 实机验证。
这不代表官方未修改 Codex 已接入 Mira ThreadStore；数据库唯一存储仍需要下面的 Codex 补丁。

## Codex ThreadStore

修改版 Codex 仍配置 endpoint 和 store ID。Node 启动 App Server 时通过环境继承同一 Node
credential，不把 token 放入进程参数：

```toml
[experimental_thread_store]
type = "remote_http"
endpoint = "https://mira.ssine.cc"
store_id = "personal"

[features]
multi_agent_v2 = true
```

本机直接运行时使用 `mira codex` 包装命令从 identity file 安全设置 `MIRA_NODE_TOKEN`；不要打印该变量。
补丁保留显式 `bearer_token` 仅用于受控开发兼容。

## Android APK

APK 内嵌相同 Go `mira-node`，不依赖 ADB、Node.js 或 Termux。Java 负责 Activity、前台服务、
Accessibility、MediaProjection、权限与子进程生命周期；Go 负责共同协议和数据面。root 只能由用户
通过 KernelSU、Magisk 或 APatch 明确授权，APK 无法自行获得 root。非 root 模式遵循 Android
权限限制。

```bash
cd node/android
ANDROID_HOME=/path/to/android-sdk gradle :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

## Home Server Compose

```bash
cp .env.example .env
docker compose --env-file .env -f compose.homeserver.yaml up -d postgres
printf '%s\n' 'your-admin-password' | \
docker compose --env-file .env -f compose.homeserver.yaml run --rm -T control \
  npm run admin -- set-password admin
docker compose --env-file .env -f compose.homeserver.yaml up -d --build postgres control
```

`.env` 只需要 PostgreSQL 密码。Mira 管理员密码不进入 Compose/Nix 环境。Server 只发布到宿主机
`127.0.0.1:8787`，由同机 Caddy 提供 TLS/WSS；不要把 8787 直接暴露到 LAN 或公网。Compose 只在
这个 loopback-only 前提下信任 Caddy 写入的 `X-Forwarded-For`，用于登录限速和审计来源地址。
Node 继续只主动连出。宿主原生 Node 的 OS 运行身份决定整机实际可访问范围；需要额外隔离时再显式
配置较窄的 allowed roots。PostgreSQL 与 Server 使用 `restart: unless-stopped`，数据保存在命名卷
`postgres-data`。

Home Server 自身的 Node 应优先作为原生 systemd 服务运行，文件、进程和 PTY 才对应真实主机，
而不是容器命名空间。Compose 中的 `node` 服务只保留作隔离测试，需要时显式启用
`--profile container-node`；不要把它当作 Home Server 主机 Node。

## 主要 API

| 方法 | 路径 | 身份与用途 |
| --- | --- | --- |
| `GET` | `/healthz` | 公开健康与 schema 信息 |
| `POST/GET` | `/v1/node-enrollments[/{id}]` | 公开提交；原 Node token 轮询 |
| `POST` | `/v1/admin/login`, `/v1/admin/logout` | 管理员 Session |
| `GET` | `/v1/admin/session` | 刷新 Session 与 CSRF token |
| `GET/POST` | `/v1/admin/enrollments[/{id}/approve|reject]` | 管理员审批 |
| `POST` | `/v1/admin/nodes/{id}/revoke|restore` | 管理员撤销；使用新 enrollment 恢复 |
| `GET` | `/v1/admin/audit-events` | 管理员追加式审计 |
| `GET` | `/v1/nodes[/{id}]` | approved Node 或管理员查看设备 |
| `GET/POST` | `/v1/dynamic-tools[/call]` | 读取并调用注入 Codex 的最终 dynamicTools |
| `POST` | `/v1/nodes/register`, `/v1/nodes/{id}/heartbeat` | 仅凭证对应的 Node 自身 |
| `POST` | `/v1/nodes/{id}/invoke` | 可信身份经 CapabilityService 调用目标 |
| `PUT` | `/v1/nodes/{id}/desired-app-server` | 可信身份选择 App Server 状态 |
| `WS` | `/v1/nodes/{id}/connect` | Node 反向通道，Node ID 严格绑定 |
| `WS` | `/v1/nodes/{id}/app-server` | Cookie 或 `auth.*` subprotocol；无 token query |
| `GET/POST` | `/v2/stores/...` | 细粒度权威 event/delta ThreadStore |
| `GET/PUT` | `/v1/stores/{storeId}` | snapshot 兼容接口 |

## 验证

```bash
npm run check --prefix server
go -C node test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go -C node build -o /tmp/mira.exe ./cmd/mira
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go -C node build -o /tmp/mira-node.exe ./cmd/mira-node
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go -C node build -o /tmp/mira-node-android ./cmd/mira-node
for file in tests/*.mjs; do node --check "$file"; done
python3 -m compileall -q tests
node tests/auth_enrollment_e2e.mjs
node tests/web_console_e2e.mjs
```

官方 Codex 补丁基于 `rust-v0.151.0`（`78c2908`）。升级原则是保留上游原始 payload、使用追加式
migration、让旧事件始终可回放，并把兼容逻辑限制在 ThreadStore/App Server 边界。
