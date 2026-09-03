# Mira

Mira 是一个面向个人设备集群的 Codex 控制与持久化实验项目。它让 Windows、WSL、Linux、
NAS 和 Android 节点主动连接到一个中心 Control Server，并允许 Codex 在指定节点运行、跨节点
调用文件/进程/PTY/屏幕能力，同时把 thread 历史统一保存在 PostgreSQL。

> 当前状态：功能完整的 PoC，尚未完成面向公网和无人值守环境所需的安全、调度与运维能力。

## 核心目标

- PostgreSQL 是 Codex thread 的唯一持久化事实来源；
- 每台桌面或服务器节点仍可原生运行自己的 Codex CLI/App Server；
- Node Agent 只需主动连出，不需要在家庭设备上开放入站端口；
- 中央端能够选择 App Server 的运行节点，并把其他在线节点暴露为 Codex `dynamicTools`；
- Android 可以只作为设备节点，不要求安装或运行 Codex；
- root thread、subagent 和跨节点恢复使用同一套存储与工具协议。

## 架构

```text
CLI / Desktop / future Web & Mobile
               |
               | App Server WebSocket proxy
               v
        Mira Control Server ---------------- PostgreSQL
          |           |                      authoritative store events
          |           +-- node registry      + immutable rollout items
          |           +-- desired state      + rebuildable projections
          |           +-- dynamicTools
          |
          +======== outbound WebSocket channels ========+
          |                    |                        |
     Windows agent         WSL agent          Linux/NAS/Android agent
          |                    |                        |
  file/process/PTY       native Codex          device capabilities
  local App Server       local App Server       optional App Server
```

Control Server 会给经过代理的 `thread/start` 和 `thread/resume` 注入 `home_nodes` namespace。
因此运行在节点 A 的 Codex 可以安全地请求节点 B 执行受限操作。父 thread 创建的 subagent 和
delegate 会继承同一组动态工具。

## 已实现能力

### Control Server 与存储

- PostgreSQL 权威事件日志与可重建投影；
- 细粒度 JSON state delta 和 thread history append/replace/delete；
- compare-and-swap、operation UUID 幂等重试和非冲突自动 rebase；
- thread generation、父子 thread 投影与版本一致历史读取；
- 带 checksum 的数据库 migration；
- 节点注册、heartbeat、desired/reported App Server 状态；
- Node Agent 反向 WebSocket、多路 capability RPC 和 App Server 代理。

完整存储协议见 [ADAPTER_PROTOCOL_V2.md](./ADAPTER_PROTOCOL_V2.md)。

### 桌面与服务器 Node Agent

- 自动发现 Codex binary，并上报版本、App Server 支持和 SHA-256；
- 识别 Windows、WSL 和 Linux，采集 hostname、系统、CPU、内存、磁盘、网络与 session 状态；
- 根据中心 desired state 启动、停止和重启本地 `codex app-server`；
- 文件：roots/stat/list/read/write/mkdir/move/remove；
- 进程：系统进程列表，以及 start/poll/signal 托管进程；
- PTY：open/write/poll/close/list；Linux/WSL 使用 `util-linux script`，Windows 当前使用 pipes fallback；
- allowed roots、realpath/symlink、文件大小、输出大小和 session 数量边界。

### Android

- ADB Agent：在连接手机的桌面/服务器上运行，支持 screenshot、display、UI hierarchy、
  tap/swipe/key/text、受限文件与进程操作；
- Mira Node APK：内置 ARM64 Go Agent，手机可脱离 ADB、Node.js 和 Termux 独立回连；
- root 模式：APK 经用户授权使用 KernelSU、Magisk 或 APatch；
- 非 root 模式：通过 Accessibility、MediaProjection 和应用/共享存储权限提供受限能力；
- APK 管理原生子进程生命周期、自动重连、开机启动和私有配置文件。

APK 不会自行取得 root。root 设备仍需安装 root provider 并由用户明确授权；Mira 不再需要额外
安装 KernelSU module。

## 仓库布局

| 路径 | 内容 |
| --- | --- |
| `server/` | Node.js Control Server、PostgreSQL schema 和 thread store |
| `node-agent/` | Windows/WSL/Linux Agent 与 Android ADB Agent |
| `android-native-agent/` | APK 内嵌的 Android ARM64 Go 数据面 |
| `android-app/` | root/非 root 统一 Android APK |
| `runtime/` | 存储、App Server、subagent、多节点与 Android E2E |
| `patches/codex/` | 针对官方 Codex 的可重放 ThreadStore 补丁 |
| `ADAPTER_PROTOCOL_V2.md` | 细粒度存储 wire protocol |

官方 Codex 源码不会 vendoring 到本仓库。当前补丁基于 `rust-v0.151.0`（`78c2908`），应用和
升级方法见 [patches/codex/README.md](./patches/codex/README.md)。

## 本地启动

需要 Docker、Node.js 22+ 和 npm：

```bash
git clone https://github.com/ssine/mira.git
cd mira

docker compose up -d postgres
npm ci --prefix server
THREAD_STORE_TOKEN=local-poc-token npm --prefix server start
```

本地 Compose 只把 PostgreSQL 暴露在 `127.0.0.1:55432`。Control Server 默认监听
`127.0.0.1:8787`，默认数据库 URL 与本地 Compose 一致。

修改版 Codex 的配置示例：

```toml
[experimental_thread_store]
type = "remote_http"
endpoint = "http://127.0.0.1:8787"
store_id = "my-mira-store"
bearer_token = "local-poc-token"

[features]
multi_agent_v2 = true
```

使用同一个 `store_id` 的 CLI 和 App Server 会共享同一份 thread 历史。

## 启动 Node Agent

```bash
CONTROL_SERVER_URL=http://127.0.0.1:8787 \
CONTROL_SERVER_TOKEN=local-poc-token \
NODE_AGENT_KEY=wsl-main \
NODE_AGENT_ALLOWED_ROOTS='["/home/user/projects"]' \
CODEX_BINARY=/absolute/path/to/codex \
APP_SERVER_CODEX_HOME=/path/to/codex-home \
APP_SERVER_LISTEN_URL=ws://127.0.0.1:4510 \
node node-agent/agent.mjs
```

`APP_SERVER_CONFIG_OVERRIDES` 是 JSON string array，会转换为重复的 Codex `-c` 参数。token 等
秘密应仅保存在节点本地，中心只下发 endpoint、store ID 等非秘密运行选择。

连接 ADB Android 设备：

```bash
CONTROL_SERVER_URL=http://127.0.0.1:8787 \
CONTROL_SERVER_TOKEN=local-poc-token \
ANDROID_ADB_SERIAL=<adb-serial> \
ANDROID_ADB_ALLOWED_ROOTS='["/sdcard","/data/local/tmp"]' \
node node-agent/android-adb-agent.mjs
```

`ANDROID_ADB_USE_ROOT=true` 会通过 `su -c` 执行并在注册前验证 uid 0；默认关闭。

## 构建 Android APK

需要 Android SDK 35、JDK 17+、Gradle 8.13+ 和 Go 1.23+：

```bash
cd android-app
ANDROID_HOME=/path/to/android-sdk gradle :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

在 APK 中填写 Control Server URL、Bearer token 和稳定 node key，然后选择：

- `Auto`：先尝试 `su`，未安装或未授权 root 时退回普通应用模式；
- `Root`：必须得到 KernelSU/Magisk/APatch 授权；
- `App`：始终使用普通应用 UID。

非 root 模式不能读取其他应用私有目录、执行任意系统进程或静默批准权限。MediaProjection 每次
建立投屏会话仍需要遵循 Android 的用户确认流程。

## Home Server Compose

```bash
cp .env.example .env
# 为 THREAD_STORE_TOKEN 和 POSTGRES_PASSWORD 设置随机值
docker compose --env-file .env -f compose.homeserver.yaml up -d --build
```

该 Compose 会监听 `0.0.0.0:8787`，只适合可信局域网或已有网络隔离的环境。真正控制宿主文件和
进程时，应使用专用低权限用户运行原生 Node Agent，并显式设置 allowed roots；不要挂载 `/` 或
Docker socket。

## API 概览

除 `/healthz` 外，所有接口都要求 `Authorization: Bearer <token>`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/healthz` | PostgreSQL 与 schema 状态 |
| `GET` | `/v1/capabilities` | 服务能力和协议版本 |
| `GET` | `/v1/nodes` | 节点、机器状态、Codex 与 App Server 状态 |
| `POST` | `/v1/nodes/register` | Node Agent 注册 |
| `POST` | `/v1/nodes/{id}/heartbeat` | 状态上报与 desired state 拉取 |
| `PUT` | `/v1/nodes/{id}/desired-app-server` | 设置节点 App Server 运行配置 |
| `POST` | `/v1/nodes/{id}/invoke` | 调用 file/process/PTY/screen/status capability |
| `WS` | `/v1/nodes/{id}/connect` | Node Agent 反向复用通道 |
| `WS` | `/v1/nodes/{id}/app-server` | 代理节点 App Server |
| `GET` | `/v1/dynamic-tools` | 当前 dynamic tool specs |
| `POST` | `/v1/dynamic-tools/call` | 调试 dynamic tool |
| `GET` | `/v2/stores/{storeId}` | 读取 store head/state/history manifest |
| `GET` | `/v2/stores/{storeId}/threads/{threadId}/history` | 一致历史读取 |
| `POST` | `/v2/stores/{storeId}/commits` | 细粒度原子 delta commit |
| `GET/PUT` | `/v1/stores/{storeId}` | v1 snapshot 兼容接口 |
| `POST` | `/v1/stores/{storeId}/rebuild` | 从权威事件重建 snapshot |

## 验证

在 Control Server 与 PostgreSQL 已启动后：

```bash
npm run check --prefix server
npm run check --prefix node-agent
go test ./...            # 在 android-native-agent/ 中

node runtime/storage_v2_e2e.mjs
node runtime/storage_e2e.mjs
python3 runtime/app_server_e2e.py
python3 runtime/cli_appserver_e2e.py
node runtime/node_agent_e2e.mjs
node runtime/dynamic_tools_model_e2e.mjs
```

需要远端节点或真机的测试会读取 `CONTROL_SERVER_URL`、`CONTROL_SERVER_TOKEN`、
`CODEX_TEST_BINARY`、`ANDROID_ADB_SERIAL` 和相应 node key 环境变量。

## 安全边界与待办

当前版本尚缺少：

- thread 到执行节点的 durable assignment、任务队列和 scheduler；
- writer lease、quiesce/release/claim 与网络分区 fencing；
- 多 Codex 版本兼容矩阵和自动 migration 验证；
- Windows ConPTY；
- mTLS、短期 capability ticket、每工具授权、审批与不可变审计；
- Web/手机任务控制 UI 和 Codex Desktop 节点迁移界面；
- 大历史分页缓存、压缩、正式备份恢复、指标与告警。

仓库中的 `local-poc-token` 和本地数据库口令仅用于 loopback 开发。不要将默认配置暴露到公网；
跨越可信局域网前必须增加 HTTPS/WSS、独立节点凭据和严格的网络访问控制。
