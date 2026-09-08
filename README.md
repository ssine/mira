# Mira

Mira 把 Windows、WSL、Linux、NAS 和 Android 组织成一个由用户批准的私有设备网络。每台
设备运行统一的 Mira Node 并主动连接中央 Server；Codex 仍原生运行在选定设备上，但可以通过
`home_nodes` dynamicTools 或 `mira` CLI 操作其他在线设备。PostgreSQL 是 thread 历史唯一的
持久化事实来源。

1.0.9 让低频目录组检查同时支持 NixOS 的系统 `id` 路径，确保组变更提示在
Nix 管理的 Node 上也能正常工作。

1.0.8 让 Linux/Android 内嵌 SSH 直接复用 Node 进程的 UID、GID 和附加组，
不再通过 NSS、passwd、shadow 或 PAM 重复执行一次系统账号登录。SSH 用户名仍必须
与 Node 确认的展示名一致，caller key 验证保持不变。Linux Node 还会低频、异步检查
目录组是否已变更，只在确认不一致时在控制台提示 Supervisor 重启方法。

1.0.7 精简项目对话列表：每个项目默认只显示一周内最新的四个对话，其余对话收进可展开的
隐藏区域，并在当前页面的列表刷新中保留展开状态。

1.0.6 支持 Server 通过 `/.well-known/mira` 发布直连入口；Node 会在发送凭据前完成健康探测，
优先使用可用的低延迟入口，并在连接失败时自动回退，同时保持原始 Server URL 与身份绑定不变。

1.0.5 统一 Windows SSH 非交互命令的 UTF-8 文本输出，并允许 Agent 的文件操作和受管进程
显式选择活动登录用户或 LocalSystem；安装命令固定 Release 下载地址，不再查询 GitHub API。

1.0.4 修复 Windows LocalSystem 服务的内嵌 SSH 同身份映射，并确保 Mira 管理的 systemd/procd
服务在 `repair` 重启前先持久化安装状态；即使自身 SSH 会话被重启中断，后续检查也不会误报漂移。

1.0.3 将 Linux/OpenWrt 的内嵌 OpenSSH 改为 Mira 反向授权通道专用的便携模式，不再要求
`/var/empty`、privsep 系统用户、chroot/setuid/setgid capabilities 或 seccomp，并修复 systemd 可选环境
文件的转义与 system service `HOME` 配置。

1.0.2 将 Node、Server、Supervisor 和 SSH worker 统一为 canonical `mira` 的显式子命令，移除
Mira 角色对链接文件名的依赖。

1.0.1 修复长会话历史被错误报告为不完整的问题，并让 OpenWrt/FriendlyWrt Node 安装器自动接入
已有的 procd；安装后的更新仍统一由本机 Supervisor 执行。

1.0.0 确立单一原生 Mira 镜像：Server、Node、Supervisor、CLI 和 Web 使用同一个 Go 版本发布，
PostgreSQL 保持外置。单机更新由旧 Supervisor 完整执行，允许几秒中断，并在候选 worker 启动或
健康检查失败时恢复旧版本。1.0 不承诺集群批量编排，也不包含 Supervisor 自身无法启动时的启动级
自动回退；数据库备份和 PostgreSQL 服务生命周期仍由管理员负责。

0.13.4 将远端 ThreadStore 改为按需加载单个 thread 历史，避免 App Server 启动和每次 token 更新时
反复读取或序列化全库；Windows 与 WSL 同机时会自动避开冲突的回环端口。Codex Desktop 放在 Windows
目录、但 `cwd` 属于 WSL 的会话会标记为 WSL 执行并在导入后绑定到同名 WSL Node。各平台 Codex 只在
内存投影中过滤本机无法表示的路径，PostgreSQL 中的权威会话保持完整。

0.12.0 统一使用内嵌 OpenSSH，支持原生递归 SCP、SFTP 批处理、连接复用和端口转发；Go 共享实现
直接位于 `node/internal/`，Android 是 `node/android/` 下的单一应用项目。节点升级保留身份和配置。

0.11.4 将 Mira 管理的 Codex 默认执行策略统一为 YOLO：新建和恢复 App Server thread，以及本机
`mira codex`，均默认使用 `approvalPolicy: never` 与 `danger-full-access`。显式传入的更严格策略
仍可覆盖默认值；Codex 不再额外限制联网和跨目录操作，但仍受运行 Node 的操作系统身份约束。

0.10.1 加入真正的 SSH/SFTP 节点互连：`mira ssh <设备>`、`mira scp`、`mira sftp`。
客户端内置，目标 Node 从同一二进制启动独立 SSH worker，通过单独的反向 WSS 数据流连接；
不需要开放 22 端口或安装系统 sshd。权限沿用已批准的 Node 身份，具体功能、限制和使用方式见
[SSH 协议与使用说明](./protocol/ssh-v1.md)。

当前源码已统一到 [Node 内嵌 OpenSSH](./node/openssh/README.md)，旧 Go SSH/SFTP 后端和独立原型已移除。
发布构建只接受完整原生链接包；普通 `go build` 用于开发检查，不包含可用 SSH。

0.11.3 让受控 App Server 在每次新建或恢复 thread 时，把当前执行 Node 上 `mira` CLI 的绝对
路径和 `ssh`/`scp`/`sftp` 用法合并进 Codex 开发者说明。SSH 族保持普通 CLI，不注册为
dynamicTools；旧 Node 可从随包 Codex 路径兼容推导，更新后的 Node 会直接上报实际路径。每个
Codex 运行节点还可在网页保存自己的默认工作目录；它只填充新 thread，恢复时仍使用 thread 原始
`cwd`，并允许在发送前临时覆盖。

网页发送消息时会把“连接 App Server、创建 thread、启动 turn”作为一个不可重入操作。新 thread
还带有浏览器生成的 `miraRequestId`；Server 将成功结果持久化到 PostgreSQL，并合并并发请求或重放
断线后使用同一参数的重试，避免产生不可见的孤立 thread。

0.11.2 让 Web 控制台从 PostgreSQL 权威事件生成完整历史轨迹，补齐导入/原生会话的工具调用，
并使用经过净化的 GitHub Flavored Markdown 渲染用户与 Agent 消息。会话中的节点文件链接可从
实际运行 Node 分块读取：图片、PDF、音视频和文本直接预览，其他格式下载。消息框支持选择、拖入
或粘贴图片与文件；图片使用 App Server 原生输入，普通文件先暂存到当前 Codex 运行节点并把路径
交给 Agent。

0.11.1 修复 paginated 本地会话导入后的 App Server 恢复兼容性，并改进 Web 消息轨迹布局与流式输出。

0.11.0 增加 Agent 控制台和本地会话迁移：Node 自动发现默认 `CODEX_HOME/sessions`，管理员可把
本地 JSONL 会话导入 PostgreSQL，再选择任意兼容 Node 通过受控 App Server 继续。网页可以新建、
恢复和中断 Turn，并实时展示 Agent 消息、推理摘要、文件修改和工具调用。Linux amd64 与 Windows
amd64 发布包内置同一 `CODEX_VERSION` 基线的 Mira 版 Codex 及 code-mode host；Android 仍只提供
设备能力，不在手机上运行 Codex。

## 安装与升级

在管理员网站展开「添加设备」即可复制带当前 Server 地址的安装命令。
Windows / Linux / WSL 提供一条指令安装，Android 直接安装正式签名 APK。Server、Node、CLI、
Supervisor 和 Web 已合并进同一个原生 Mira 镜像，运行时不需要 Node.js。发行仍拆成
**Mira 程序包**与**可选 Codex 运行包**：Mira 包包含单一程序镜像和内嵌 OpenSSH；首次执行 `mira codex`
或启动受控 App Server 时，按需安装固定兼容版本的完整 Codex package。缓存跨 Mira 版本复用，
节点升级不会重复下载 Codex；Android 不下载 Codex。拆分不会自动改变已经发布的 0.12.0 包。
后续桌面端执行 `mira update`；首次安装脚本不再承担更新。Android 在 APP 内检查更新。身份和配置随升级保留。
具体命令、平台要求、服务启动方式及回退说明见 [INSTALL.md](./INSTALL.md)。

### 一行安装并申请注册 Node

把下面的 `https://mira.example.com` 替换为你的 Mira Server 地址。

Linux（也适用于 WSL，支持 amd64/arm64）：

```sh
curl -fsSL https://raw.githubusercontent.com/ssine/mira/main/scripts/install.sh | sh -s -- --role node --server https://mira.example.com --version 1.0.9
```

Windows x64（在管理员 PowerShell 中运行）：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((irm 'https://raw.githubusercontent.com/ssine/mira/main/scripts/install.ps1'))) -Role node -Server 'https://mira.example.com' -Version '1.0.9'"
```

命令会下载并校验指定 Release、安装并启动 Node，然后向 Server 提交注册申请。显式版本可以避免查询
GitHub API；管理员网站生成的命令会自动固定为 Server 当前版本。回到管理员网站，
核对 Node 显示的六位验证码并批准后即可完成注册；安装命令不会绕过审批。
Windows 命令中的 `Bypass` 只作用于这一个新 PowerShell 进程，不会永久修改系统执行策略；如果组织的
`MachinePolicy` 或 `UserPolicy` 仍然禁止执行，请联系管理员放行，不要尝试绕过组织策略。

## 架构

```text
Codex / mira CLI / Admin Web + Agent Console
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

本机进程管理：Server Supervisor → Server worker + Node worker
              设备 Supervisor → Node worker
```

Server 在代理的 `thread/start` 和 `thread/resume` 中注入 `home_nodes`。运行在 Node A 的 Codex
可调用 Node B；subagent 继承 dynamicTools，但仍作为独立 thread 保存，并保留父子关系。所有 HTTP
CLI 调用和 dynamicTools 最终都经过同一个 `CapabilityService`。Server 还记录每个 thread 最近一次
实际运行的 Node；网页打开输出文件时优先使用当前运行节点，并以先前运行节点和导入来源节点回退。
受控 App Server 会话默认使用 `approvalPolicy: never` 和 `sandbox: danger-full-access`，避免 CLI
联网和跨目录操作被 Codex 沙箱拦截；显式请求的更严格策略仍会保留。该模式不会提升操作系统权限，
Codex 仍以 Mira Node 所在用户身份运行，因此应只在受信任的个人节点上启用。

## 身份和权限

v1 只有两类安全身份：

- 一个管理员账号：本地命令设置 Argon2id 密码，网页使用数据库 Session、严格 Cookie 和 CSRF；
- 每台设备一个 Node credential：`mira` Node worker、CLI、本地 Codex/App Server 共用同一个
  `identity.json`。

Node 在首次启动前生成 256-bit secret，只向 Server 提交 SHA-256。管理员在网站核对六位验证码并
批准后，设备才可注册和连接。所有 approved Node 在 v1 中彼此信任；目标设备的操作系统权限、
symlink、文件/输出/session 限制以及 Android 权限仍会独立执行。没有全局 `THREAD_STORE_TOKEN`、
CLI 登录、Node ACL 或长期 token query parameter。

详细协议见 [protocol/auth-v1.md](./protocol/auth-v1.md)，存储协议见
[protocol/thread-store-v2.md](./protocol/thread-store-v2.md)，本地会话迁移见
[protocol/codex-session-import-v1.md](./protocol/codex-session-import-v1.md)，稳定架构结论见
[AGENTS.md](./AGENTS.md)。

## 已实现组件

| 路径 | 作用 |
| --- | --- |
| `node/internal/miraserver/` | 原生 Go Server、认证、审计、CapabilityService、App Server broker 与 ThreadStore API |
| `node/internal/webassets/` | 内嵌 Web 资源及仓库受控的第三方静态文件 |
| `server/public/` | 管理员设备控制台的前端源码，无独立运行时 |
| `node/cmd/mira/` | CLI 及 Windows/Linux/WSL/Android 共用的显式运行角色入口 |
| `node/internal/` | 身份、接入、反向通道、文件、进程、PTY、屏幕和平台适配 |
| `node/android/` | root/非 root 统一 APK 外壳和 Android Framework bridge |
| `node/openssh/` | 单文件内嵌 OpenSSH：平台构建、补丁、分发清单和真实节点回归 |
| `patches/codex/` | 官方 Codex ThreadStore HTTP 适配与 subagent dynamicTools 补丁 |
| `skills/mira/` | 可安装的 Codex 使用说明，不包含任何 credential 或固定设备信息 |
| `tests/` | 存储、认证、Node、App Server、subagent、多节点与 Android E2E |
| `scripts/` | 统一版本检查、跨平台 Release 构建和 Linux/Windows 首次引导 |

## 本地启动与网站验收

Server 本身只需要 Go 与 PostgreSQL；Node.js 22+ 仅用于仓库中的 JavaScript 测试和构建辅助脚本：

```bash
docker compose up -d postgres
go -C node build -o /tmp/mira ./cmd/mira

# 只在 Server 主机本地运行；密码从隐藏终端或 stdin 读取
printf '%s\n' 'mira-local-admin-password' | \
  DATABASE_URL=postgresql://mira:mira-local@127.0.0.1:55432/mira \
  /tmp/mira server admin set-password admin

# loopback HTTP 验收时允许非 Secure Cookie；生产不要设置 false
DATABASE_URL=postgresql://mira:mira-local@127.0.0.1:55432/mira \
MIRA_SECURE_COOKIES=false /tmp/mira server-worker
```

打开 [http://127.0.0.1:8787](http://127.0.0.1:8787)。网站可登录、查看待审批申请、批准/拒绝、
查看 Node 状态/能力、撤销设备并检查追加式审计记录。Agent 控制台可选择 Codex 运行节点、扫描并
导入节点默认位置的本地会话、新建或继续 PostgreSQL thread，并实时展示消息和工具轨迹。每个运行
节点可以单独保存新会话默认工作目录，发送前仍能临时修改；恢复会话不会覆盖其原始目录。连续工具
调用在 Web 对话中默认折叠为按工具名称统计的摘要，需要时可展开查看完整输入和输出。恢复已导入
会话时，网页使用 PostgreSQL 投影保存的原始工作目录，不会用上一个会话的目录覆盖；正常 Turn
生命周期和没有正文的推理事件只更新运行状态，不生成短暂的空消息卡片。恢复后的混合格式历史会
按 Turn 去重并保留新消息，实时 App Server 事件只进入其所属会话，不会在切换会话后串台。每台在线设备
会话消息中的本机路径可点击；网页通过 Node 文件能力分块读取，支持进度、关闭弹窗取消、常见媒体、PDF 和
渐进式文本预览或原文件下载。消息输入不限制附件数量、单文件大小和总大小；图片与文件分块上传，
显示字节进度并支持取消（取消不会发出消息，会清理本次暂存文件）。附件
暂存于实际运行节点的系统临时目录（受限根配置下回退到 thread 工作目录）；它不是跨节点同步盘。
点击发送后立即显示准备/等待提示，首条正文出现后自动隐藏，不写入聊天记录。桌面导入支持本地
Codex Desktop、CLI 和归档会话的来源筛选、搜索、分批显示、进度及取消，无 JSONL 总大小/记录数上限。
已验证真实 320 MiB 桌面记录，以及引用祖先 `history_base` 的分支导入后恢复出 25 个 turn。
嵌套分支会按精确边界补齐祖先历史并保留原始引用；不导入分叉后的父会话内容。
详情见 [导入协议](protocol/codex-session-import-v1.md)。
每台在线设备还提供独立工作台：只读文件
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
go -C node run ./cmd/mira node-worker
```

Node 显示 enrollment ID 和六位验证码，等待网站批准。身份文件默认位于 Linux/WSL 的
`~/.config/mira/identity.json`、Windows 的 `%USERPROFILE%\\.mira\\identity.json`，Android 使用
APK 私有 no-backup 目录；`MIRA_IDENTITY_FILE` 可覆盖。写入采用临时文件、原子 rename 和用户
Unix `0600` 权限；Windows 使用受保护的当前用户 / SYSTEM / Administrators DACL。

未配置 `MIRA_NODE_ALLOWED_ROOTS` 时，Linux/WSL/Android 从 `/` 开始，Windows 自动列出所有当前
可用盘符。最终能否读取仍由 `mira` Node worker 的 OS 用户权限决定。如需把某台 Node 收紧到特定工作区，
可显式设置 `MIRA_NODE_ALLOWED_ROOTS='["/path/to/workspace"]'`。

Windows 系统服务默认以 LocalSystem 运行，但 Agent 的 `file` 操作和 `process/start` 可显式选择
`executionContext: "user" | "system"`。未指定时 Windows 优先使用当前活动的交互用户，从而继承该用户
的 Profile、环境和网络登录凭据；需要系统服务权限时必须选 `system`。节点状态的 `execution`
字段会列出实际 OS 身份和可用的 `userSessionId`。多个用户会话同时活跃且无法唯一选择时，
Mira 会拒绝猜测；用户令牌只在单次操作/启动期间使用，不持久化。受限根目录检查仍然适用；访问
NAS 时建议让用户上下文的受管进程使用 UNC 路径。这个选择不改变 SSH、PTY 或 App Server 的 OS 身份；
`mira ssh` 仍始终代表 Node 服务身份。用户上下文的受管进程仍是可捕获输出的非交互后台进程，
不会进入用户桌面；需要桌面 UI 时使用相应的屏幕/UI 能力，而不是依赖这个执行上下文。

构建桌面程序：

```bash
go -C node build -o dist/mira ./cmd/mira
```

## `mira` CLI

CLI 不单独登录，直接读取当前设备的 Node identity。所有命令支持 `--json` 和 `--timeout 30s`：

```bash
mira identity show --json
mira status
mira version
mira update --check
mira --help
mira nodes                         # 面向人的紧凑表格
mira nodes list --summary --json   # 面向 Agent 的精简、稳定 JSON
mira nodes get --node 软路由 --json
mira codex                         # 本机 Mira Codex；personal PostgreSQL store，默认 YOLO
mira file read --node nas --path /data/report.txt --output /tmp/report.txt
mira process count --node homeserver --json
mira process run --node homeserver -- /usr/bin/git status --short
mira process run --node windows --execution-context user -- cmd.exe /d /c whoami
mira process run --node windows --execution-context system -- sc.exe query Mira
mira pty open --node wsl-main -- /bin/bash
mira screen screenshot --node android-phone --output /tmp/phone.png
mira app-server start --node wsl-main
mira app-server connect --node wsl-main
```

Node selector 可以是 UUID、精确 `nodeKey`、用户定义的 alias 或唯一 hostname；歧义时失败。管理员可在
Web 设备卡中设置显示名称、最多 8 个全局唯一 aliases，以及用于筛选的 key/value labels。alias 可直接用于
CLI、SSH/SCP/SFTP 和动态工具；labels 不会隐式选择单台设备。使用 `--label role=router`、
`--capability ssh`、`--online` 和 `--summary` 可进一步限制 Agent 的发现结果。进程命令始终使用
executable + argv，不拼 shell 字符串。截图与大文件通过本地绝对路径/stdin 传输，避免进入 argv。
SSH relay 默认允许全局 128 路、每个相关 Node 32 路并发连接；可用
`MIRA_SSH_MAX_SESSIONS` 和 `MIRA_SSH_MAX_SESSIONS_PER_NODE` 调整，但始终保留有界保护。

部署可以在配置的 Server URL 下通过 `/.well-known/mira` 发布按 priority 排序的备用入口。Node 在发送
凭据前先匿名探测候选入口的 `/healthz`，选择可用的最高优先级入口，并在连接失败时回退；配置和 identity
仍保留原始 Server URL，不会因为动态入口变化而重新注册。协议与降级防护见
[`protocol/endpoint-discovery-v1.md`](protocol/endpoint-discovery-v1.md)。

Windows 文件、系统进程数/列表、进程启动/终止、CPU/内存/磁盘/网络、真实 ConPTY 输入/VT/
resize/Ctrl-C，以及 Codex 自动发现和 App Server 启停已在 Windows 11 实机验证。普通官方 Codex
仍可独立使用；受控 App Server 和 `mira codex` 只选择通过远端 ThreadStore 探针的 Mira 兼容构建。

## Codex ThreadStore

发布流程从根目录 `CODEX_VERSION` 指定的官方 tag 应用最小补丁，再通过官方 canonical package
builder 组装入口、code-mode host、Linux bwrap / Windows sandbox helpers、`rg` 和 package manifest。
Node 启动 App Server 时通过环境继承同一 Node
credential，不把 token 放入进程参数：

```toml
[experimental_thread_store]
type = "remote_http"
endpoint = "https://mira.ssine.cc"
store_id = "personal"

[features]
multi_agent_v2 = true
```

本机直接运行时使用 `mira codex`。包装器从 identity file 安全设置 `MIRA_NODE_TOKEN`，并自动注入
当前 Server endpoint、`personal` store 及默认 YOLO 策略；后置 Codex 参数可显式覆盖默认策略，
`MIRA_CODEX_STORE_ID` 可选择其他 store。不要打印 token。
补丁保留显式 `bearer_token` 仅用于受控开发兼容。

## Android APK

APK 内嵌相同 Go `mira` 程序并以 `node-worker` 启动，不依赖 ADB、Node.js 或 Termux。Java 负责 Activity、前台服务、
Accessibility、MediaProjection、权限与子进程生命周期；Go 负责共同协议和数据面。root 只能由用户
通过 KernelSU、Magisk 或 APatch 明确授权，APK 无法自行获得 root。非 root 模式遵循 Android
权限限制。

```bash
cd node/android
ANDROID_HOME=/path/to/android-sdk gradle :assembleDebug
adb install -r build/outputs/apk/debug/mira-node-debug.apk
```

## Home Server Compose

```bash
cp .env.example .env
# 编辑外部 DATABASE_URL、公网 HTTPS endpoint，并固定 MIRA_VERSION。
docker compose --env-file .env -f compose.homeserver.yaml pull
printf '%s\n' 'your-admin-password' | \
docker compose --env-file .env -f compose.homeserver.yaml run --rm -T server \
  mira server admin set-password admin
docker compose --env-file .env -f compose.homeserver.yaml up -d server
```

PostgreSQL 独立部署和备份，不属于这个 Compose stack。Mira 管理员密码不进入 Compose/Nix 环境。Server 只发布到宿主机
`127.0.0.1:8787`，由同机 Caddy 提供 TLS/WSS；不要把 8787 直接暴露到 LAN 或公网。Compose 只在
这个 loopback-only 前提下信任 Caddy 写入的 `X-Forwarded-For`，用于登录限速和审计来源地址。
Node 继续只主动连出。宿主原生 Node 的 OS 运行身份决定整机实际可访问范围；需要额外隔离时再显式
配置较窄的 allowed roots。容器部署通过固定镜像版本更新；需要 Supervisor 自动回滚时使用
`mira install --role server` 的原生服务部署。

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
| `GET` | `/v1/nodes/{id}/codex-sessions` | 管理员扫描 Node 默认位置的本地 Codex 会话 |
| `POST` | `/v1/nodes/{id}/codex-session-imports` | 管理员保存原始 JSONL 并导入统一 ThreadStore |
| `GET` | `/v1/codex/threads` | 管理员读取统一 thread 投影和导入来源 |
| `GET` | `/v1/codex/threads/{id}/transcript` | 管理员按页读取可重建的 Web 消息与工具轨迹 |
| `POST` | `/v1/codex/runtimes/{id}/start\|stop` | 管理员选择 Node 启停受控 App Server |
| `PUT` | `/v1/nodes/{id}/desired-app-server` | 可信身份选择 App Server 状态 |
| `WS` | `/v1/nodes/{id}/connect` | Node 反向通道，Node ID 严格绑定 |
| `WS` | `/v1/nodes/{id}/app-server` | Cookie 或 `auth.*` subprotocol；无 token query |
| `GET/POST` | `/v2/stores/...` | 细粒度权威 event/delta ThreadStore |
| `GET/PUT` | `/v1/stores/{storeId}` | snapshot 兼容接口 |

每个运行节点可在 Web 的“运行节点”页配置一个本机 UTF-8 Developer Message 文件。Mira Server
会在托管 App Server 的 `thread/start`、`thread/resume` 和 `thread/fork` 请求中，通过该 Node
的文件能力读取最多 256 KiB 并注入 `developerInstructions`；读取失败会阻止请求，避免策略被
静默忽略。内容会进入 Codex 会话的长期上下文，因此不应包含密钥。该设置不影响 `mira codex`
CLI。

## 验证

```bash
node scripts/check-version.mjs
diff -qr --exclude vendor server/public node/internal/webassets/web
go -C node test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go -C node build -o /tmp/mira.exe ./cmd/mira
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go -C node build -o /tmp/mira-android ./cmd/mira
for file in tests/*.mjs; do node --check "$file"; done
python3 -m compileall -q tests
node tests/web_console_e2e.mjs
```

官方 Codex 补丁基于 `rust-v0.151.0`（`78c2908`）。升级原则是保留上游原始 payload、使用追加式
migration、让旧事件始终可回放，并把兼容逻辑限制在 ThreadStore/App Server 边界。
