# 安装、部署与更新

Mira 的 Server、Node、CLI、Supervisor 和 Web 静态资源都在同一个 `mira` 发布镜像中。
运行时不需要 Node.js。PostgreSQL 是独立服务，Mira 不负责安装、更新或回滚数据库。

## 1.0 支持边界

Mira 1.0 支持单机 Supervisor 管理 Node，以及 Server 主机上的 Node + Server；更新允许几秒中断，
并在候选 worker 启动或健康检查失败时恢复旧版本。1.0 不承诺集群批量编排，也不承诺 Supervisor
二进制自身完全无法启动时的启动级自动回退。数据库备份和 PostgreSQL 服务生命周期仍由管理员负责。

## 角色

- `node`：Supervisor 管理一个 Node worker。
- `server`：同一个 Supervisor 管理 Node worker 和 Server worker。Server 主机因此也是一台 Mira Node。
- `mira` CLI：使用本机 Node 身份；不是额外常驻服务。
- Codex：需要时下载固定兼容版本，不随 Mira 包重复分发；Android 不运行 Codex。

生产环境只选择一种服务所有者：普通 Linux/Windows 由 Mira 安装器管理；NixOS 可选择 Nix 管理。
同一状态目录不能同时由两者接管。

## Linux / WSL Node

以希望授予 Mira 文件和进程权限的用户运行：

```sh
curl -fsSL https://raw.githubusercontent.com/ssine/mira/main/scripts/install.sh | \
  sh -s -- --role node --server https://mira.example.com --version 1.0.85
```

支持 Linux amd64/arm64。`--service-manager auto|systemd|procd|builtin` 默认为 auto：
OpenWrt/FriendlyWrt 的 Mira-owned system Node 安装优先识别 procd；其他环境检测指定 scope 的
systemd 是否可连接；两者不可用时选择 builtin。procd 要求可执行的 `/etc/rc.common` 与
`/sbin/procd`，以 root 写入、启用并启动 `/etc/init.d/mira`。

已有开发容器无需 PID 1/systemd。builtin 安装会在后台启动同一 Mira Supervisor；使用
`mira start`、`mira stop`、`mira restart`、`mira status` 管理它，必要时传 `--state-dir`。
Supervisor 管理 worker 重启及更新，通过 exec 交接自身，没有额外守护父进程。
日志在状态目录的 `supervisor.log`，每份 4 MiB、保留三份旧日志；首启错误见 `builtin-startup.log`。
容器停止、重建或 Supervisor 被杀后，需要从容器启动流程再次执行 `mira start`；builtin 不提供
开机自启或 Supervisor 自身崩溃恢复。数据目录应持久保存，实例的 OS 用户和身份保持一致。


脚本只完成首次引导：下载 GitHub Release、校验 SHA-256，再调用 `mira install` 安装 Supervisor。
默认状态目录是 `~/.local/share/mira`，命令入口位于 `~/.local/bin`，身份配置位于
`~/.config/mira`。可用 `--version 1.0.85` 固定首次安装版本，或用
`--state-dir /absolute/path` 选择状态目录。

安装器不再接受 `--update`。首次安装后统一使用 `mira update`。

## Linux Server

先准备独立 PostgreSQL 和只允许服务账户读取的环境文件，再安装 `server` 角色。下面示例使用
system scope；请替换数据库密码和公网地址：

```sh
sudo install -d -m 0700 /var/lib/mira
sudo sh -c 'umask 077; cat > /var/lib/mira/mira.env' <<'EOF'
DATABASE_URL=postgresql://mira:password@postgres.example.internal:5432/mira
LISTEN_HOST=127.0.0.1
LISTEN_PORT=8787
MIRA_SECURE_COOKIES=true
MIRA_CODEX_STORE_ENDPOINT=https://mira.example.com
MIRA_TRUST_PROXY_HEADERS=true
EOF

curl -fsSL https://raw.githubusercontent.com/ssine/mira/main/scripts/install.sh | \
  sudo sh -s -- --role server --state-dir /var/lib/mira --service-owner mira --service-scope system
```

Server 只监听 loopback，由同机 Caddy 提供 HTTPS/WSS。只有在这个前提下才设置
`MIRA_TRUST_PROXY_HEADERS=true`。初始化管理员密码不依赖已运行的 Server：

```sh
printf '%s\n' 'a-long-admin-password' | \
  sudo env DATABASE_URL='postgresql://mira:password@postgres.example.internal:5432/mira' \
  /var/lib/mira/current/mira server admin set-password admin
```

密码从 stdin 读取，不放入环境文件或进程参数。数据库必须单独备份；二进制回退不会回滚 schema。

从旧源码仓库/Node.js 部署首次切换时，先备份 PostgreSQL，再停止并移除旧的 `mira-server`、
`mira-node` systemd unit 或旧 Compose stack，然后才启用新的 `mira.service`。旧、新 Server 不能同时
连接同一数据库；这一步是一次有短暂中断的部署迁移，不由日常 `mira update` 自动猜测或执行。

## NixOS Server

先准备同样的 `/var/lib/mira/mira.env`，再执行：

```sh
curl -fsSL https://raw.githubusercontent.com/ssine/mira/main/scripts/install.sh | \
  sudo sh -s -- --role server --state-dir /var/lib/mira --service-owner nix
```

这只下载并校验首个版本，然后生成 `/var/lib/mira/mira-service.nix`。Mira 不会运行
`nixos-rebuild`，也不会直接修改 Nix 管理的 systemd unit。审查生成内容，把它导入自己的 NixOS
配置，再走正常部署流程。不要再为同一状态目录执行 `--service-owner mira`。
交互安装若省略 `--service-owner` 会明确询问；无人值守安装必须显式传 `nix` 或 `mira`。

## Compose Server

`compose.homeserver.yaml` 是容器部署示例，也只连接外部 PostgreSQL：

```sh
cp .env.example .env
# 编辑 .env，固定 MIRA_VERSION，并填写 DATABASE_URL 与公网 HTTPS 地址。
docker compose --env-file .env -f compose.homeserver.yaml pull
printf '%s\n' 'a-long-admin-password' | \
  docker compose --env-file .env -f compose.homeserver.yaml run --rm -T server \
  mira server admin set-password admin
docker compose --env-file .env -f compose.homeserver.yaml up -d server
```

容器运行同一个无 Node.js runtime 的 Mira 发布镜像。容器模式由 Compose 替换镜像，不运行本机
Supervisor 更新事务；更新时修改固定的 `MIRA_VERSION`，再执行 `pull` 和 `up -d`。容器中的可选
Node 只用于隔离测试，不能代表宿主机文件和进程权限。

## Windows

在管理员 PowerShell 中运行（Windows x64）：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((irm 'https://raw.githubusercontent.com/ssine/mira/main/scripts/install.ps1'))) -Role node -Server 'https://mira.example.com' -Version '1.0.85'"
```

Windows 使用系统服务运行 Supervisor。默认状态目录为 `%USERPROFILE%\.mira`；`-StateDirectory`
可覆盖，`-NoPath` 不修改用户 PATH。Server 角色还需由管理员在安装前配置系统级
`DATABASE_URL`、监听地址和公网 endpoint。
这里的 `Bypass` 只作用于这一个新 PowerShell 进程，不会永久修改系统执行策略。组织通过
`MachinePolicy` 或 `UserPolicy` 强制的限制仍然有效，需要由组织管理员放行。

Windows 与 WSL 是两台独立 Node，各自拥有身份。不要复制 identity 文件，也不要同时运行两个
指向同一状态目录的 Supervisor。

## 接入

安装不会绕过审批。Node 首次启动时生成本地凭据并提交申请：

```sh
mira status
```

在 Server 网站核对六位验证码并批准。身份和 `node.json` 位于版本目录之外，更新不会重建。

## 更新、回退与修复

```sh
mira version
mira update --check
mira update
mira doctor
```

从安装目录启动的 `mira` 会自动识别对应状态目录。使用复制出来的二进制或排障时可显式传
`--state-dir /var/lib/mira`；Server/Nix system 安装也建议在运维命令中保留这个参数。

更新请求发送给本机 Supervisor。旧 Supervisor负责完整事务：下载和校验候选版本；停止旧 worker；
启动新 worker；等待 Node/Server 健康；失败时停止候选并恢复旧 worker。Server 更新接受几秒中断，
不会同时运行两个 Server 写数据库。发起更新的终端、SSH、Node 通道或 Server 随后断开，都不影响
本地 Supervisor 完成回滚。

`mira update --version <version>` 可选择已发布版本。更新会中断当前 worker 上的活动会话；不要再次运行
bootstrap 脚本更新，也不要并发执行两个更新。`mira repair` 只修复已记录且所有权一致的服务定义；
`mira uninstall` 删除 Mira 管理的服务但保留版本、身份和配置。Nix 所有权下的修复和移除仍通过
审查过的 Nix 配置完成。

SSH 服务端及客户端都链接在正式 Mira 镜像中。手动只复制 `mira` 文件时，1.0.81 会在身份目录下
的私有版本缓存自动创建所需 OpenSSH 角色链接，并验证它们指向当前镜像；无需安装系统 sshd。

## 出站代理与 HTTPS 降级

外部 Node 控制、App Server 客户端、SSH/SCP/SFTP 和文件流默认先尝试 WebSocket，握手失败时
降级为普通 HTTPS GET 长轮询和 POST。Node 的短时 WebSocket 断连连续发生两次后，后续连接
选择 HTTPS。HTTPS 始终重新校验相同身份；认证拒绝不会被绕过。断开的 RPC 不自动重新提交。Server 与客户端都需 1.0.81 或更新版本；
旧客户端继续使用原 WebSocket 协议。Node 内部 loopback Codex App Server 仍使用原生 WebSocket。

`MIRA_NODE_TRANSPORT=auto|websocket|https` 控制 Node 与 CLI；Node 配置文件也可写
`"transport": "https"`。网页可通过 `?miraTransport=https` 强制 HTTPS（标签页内也可设置
`sessionStorage["mira.transport"]`），默认自动降级。协议及重试边界见 [HTTPS transport v1](protocol/https-transport-v1.md)。

Mira 出站请求使用标准 `HTTPS_PROXY`/`HTTP_PROXY` 和 `NO_PROXY`。例如 builtin 安装：

```sh
HTTPS_PROXY=http://proxy.example.test:7890 \
NO_PROXY=localhost,127.0.0.1,::1 \
MIRA_NODE_TRANSPORT=https \
mira restart --state-dir /data/mira
```

代理地址按当前容器的网络视角填写。Supervisor 启动时继承变量并传给 worker，exec 更新保持环境；
已运行的服务需要重启。systemd/procd/Nix 管理的实例通过其现有服务配置传入变量。
变量作用于进程的全部网络请求（包括可选 runtime 下载）；如需不同路由，应配置代理或 NO_PROXY。

## 发布

`VERSION` 是 Mira 版本事实源。正式 Release 包含 Linux amd64/arm64、Windows amd64、Android APK、
bootstrap 脚本和校验和，并发布相同版本的 `ghcr.io/ssine/mira` 镜像。桌面归档只有一个 canonical
`mira` 程序；Node、Server、Supervisor 和 SSH worker 由显式子命令选择。版本私有目录中的
`ssh`/`sshd` 等名字只是同一镜像的 OpenSSH 入口，不是不同实现。

正式构建必须先生成并验证内嵌 OpenSSH，再运行：

```sh
./scripts/build-release.sh dist
node tests/installers_e2e.mjs
```

installer e2e 只验证首次 node/server 引导、幂等性、校验失败和对旧 `--update` 的拒绝。更新、
健康检查与回滚由 Go Supervisor 测试覆盖。
