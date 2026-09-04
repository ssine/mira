# 安装、接入与更新

Mira 的 Server、Node、CLI 与 Android APK 使用同一个版本号。当前版本 **0.12.0**，发布来源为
[GitHub Releases](https://github.com/ssine/mira/releases)。无需安装 Go、Node.js 或 Termux。
Codex 运行包独立发行，其版本号由官方版本和 Mira 补丁修订号组成，例如 `0.151.0-mira.1`。
以下拆分机制属于当前源码；已发布的 0.12.0 归档仍保持原样，需要发布新版 Mira 才会生效。

## Linux / WSL / NAS

在需要加入的设备上，以你希望授予 Mira 的操作系统用户执行：

```sh
curl -fsSL https://raw.githubusercontent.com/ssine/mira/main/scripts/install.sh | sh -s -- --server https://mira.ssine.cc
```

支持 Linux amd64、arm64；WSL 使用 Linux 版本。需要 `curl`、`tar`、`sha256sum`。
去掉 `--server` 参数会交互询问 Server URL。支持 `--version 0.9.4` 固定版本。
脚本会下载 GitHub Release、核对 SHA-256，然后按当前用户安装，不使用 `sudo`。
如希望先审查脚本，下载并阅读后执行 `sh install.sh --server ...`。

- 命令：`~/.local/bin/mira`、`~/.local/bin/mira-node`；不在 PATH 时使用完整路径或将该目录加入 PATH。
- 版本目录：`~/.local/share/mira/versions/<version>/`。
- 身份与配置：`~/.config/mira/identity.json`、`node.json`，不会随程序升级重建。
- 有 user systemd 时，自动安装并启动 `mira-node.service`，用户登录后启动。
  开机未登录也要运行时，由管理员决定是否执行 `loginctl enable-linger <用户名>`。
- 没有 user systemd 的 NAS：安装器显示启动命令；在 NAS 的任务/服务管理器中将它设置为常驻服务。
  不会擅自修改厂商系统或启用 root。
- `--no-service` 仅安装二进制，更新保留此选择；`--prefix /absolute/path` 是不安装服务的便携模式。

## Windows

在普通用户 PowerShell 中运行（Windows 10 1809+ / Windows 11，x64）：

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/ssine/mira/main/scripts/install.ps1))) -Server https://mira.ssine.cc
```

去掉 `-Server` 会询问地址；`-Version 0.9.4` 固定版本。无需 Node.js、Python 或编译工具。
安装器创建当前用户的登录计划任务，不以 SYSTEM 运行；不需要给 Mira 全机管理员权限。
如果组织策略禁止创建任务，可使用 `-NoService`，然后手工启动 `mira-node`。

- 程序与旧版本：`%USERPROFILE%\.mira\versions\<version>\`。
- PATH 入口：`%USERPROFILE%\.mira\bin`，安装器同时更新当前 PowerShell 的 PATH。
  对其他窗口的更新通知最多等待两秒；已有终端可能需要重启启动器或重新登录 Windows。
- 身份与配置：`%USERPROFILE%\.mira\identity.json`、`node.json`。
  凭据 DACL 只允许当前用户、SYSTEM 和管理员，移除继承的宽泛访问权限。
- 计划任务：`MiraNode-<用户名>`，登录后启动；不承诺用户注销后继续运行。
- 终端是真实 ConPTY，支持 VT、交互输入、Ctrl-C、窗口尺寸同步。
  普通非 PTY 子进程的 `SIGTERM`/`SIGKILL` 在 Windows 上使用终止进程，并不等同于 Unix 信号。
- 如调用系统程序，请使用 `.exe` 后缀；原生 PATH 与 WSL PATH 是不同环境。
- `-NoService` / `-NoPath` 支持便携运行，后续更新保留设置。

Windows 与 WSL 是两个独立 Node，各安装一次即可；二者不会共享一个设备身份。
0.9.1 避开了 MSIX 打包终端对 AppData 的重定向，安装不会落入 Codex 的私有缓存。
安装器通过一个临时、非提权的计划任务写入真实用户 PATH，完成后移除该辅助任务。
旧版 `%LOCALAPPDATA%\Mira\identity.json` 仍能识别；不应复制成第二个同时运行的 Node。

## Android

从 [最新 Release](https://github.com/ssine/mira/releases/latest) 下载 `mira_<version>_android_arm64.apk`，
在 Android 8+ 的 arm64 手机上安装。只有一个 APK：

1. 填写 Mira Server URL，点击 **Save and start**。
2. 在 Server 网页核对设备显示的六位验证码，再批准接入。
3. 按需要授权 Accessibility、屏幕捕获与共享文件访问；有 root 时由已安装的 root 管理器授权。

APK 不会自行获得 root，也不依赖特定的 KernelSU。非 root 模式遵循 Android 系统权限边界，
不可读取其他应用的私有数据；屏幕捕获仍可能需要系统确认。Android 不运行 Codex。

APP 内的 **Check for updates** 查询 GitHub 的最新正式版，跳转浏览器下载 APK，随后由 Android
确认安装。正式版使用固定签名，覆盖更新会保留身份和设置，无需重新审批。系统可能要求允许
浏览器安装未知来源应用；这不是静默升级，也不需要把 root 交给安装器。

之前的开发/debug APK 使用不同签名，**首次迁移到正式签名版需要卸载旧包、重新接入一次**。
以后正式版之间不需要卸载。签名私钥必须持续备份，不能每次发布重新生成。

## 批准接入

安装不会绕过权限模型。Node 在本地产生身份，向 Server 提交申请；等待批准时执行：

```sh
mira status
```

它显示 Server 地址、申请状态和验证码，不显示 token。到 Server 网页的「接入申请」核对并批准。
批准后该设备持续主动回连，CLI 也复用同一身份，无需另行登录。
安装器不会覆盖已有身份、改变其绑定的 Server，或替换由其他方式管理的同名服务。

## 更新与回退

```sh
mira version
mira update --check
mira update
```

`mira update` 从 GitHub Release 获取最新稳定版、核对校验和，切换版本目录并重启安装器管理的
服务。旧目录保留，身份/配置不变。默认不会把较新的开发版降级到较旧的 latest。
操作需要能访问 GitHub；下载或校验失败不会替换现有程序。

更新前检查当前 Node 上的运行中托管进程、PTY、Codex App Server 和入站/出站 SSH；存在活跃会话或无法确认
状态时拒绝更新。关闭会话后重试；明确接受中断时可用 `mira update --force`。
已批准但离线的 Node 也会拒绝普通更新，因为无法确认其本地会话是否仍在运行。
此检查是尽力而为，不是分布式 drain/锁，检查后仍应避免开始新任务。
不要从同一个 Node 的远程 shell 中更新它本身；请从本机终端执行。

显式选择某个已发布的兼容版本（也可用于回退）：

```sh
mira update --version 0.9.4
```

已安装相同版本时默认不重启；加 `--force` 可重装同一正式版。版本目录内容不同会拒绝覆盖。
二进制回退不会回滚 Server 数据库 schema；Server 升级前仍需数据库备份。
Home Server 上由 Nix/systemd/Compose 管理的 Server 与 Node，应继续由部署配置管理，
不要再运行安装器创建第二个同名服务。

目前是**每台设备一个更新命令**，不是自动批量更新所有设备。各 Node 可以滚动更新，不必同一
时刻升级；当前 wire protocol 仍是 v1，Server 接受未携带新 build metadata 的旧 Node。
旧 Node 不会因为 Server 升级就获得新的 ConPTY 等能力。
HTTPS/WSS 接入建议使用 0.9.2 或更新版本；它修复了 HTTP/2 注册后 WebSocket TLS 协商失败的问题。
Server 建议使用 0.9.3 或更新版本，避免节点升级重连时，旧连接的关闭事件把新连接错误标记为离线。
Android 域名接入需要 0.9.4 或更新版本；APK 通过 NDK/cgo 使用 Android 系统 DNS，无需用户安装额外组件。
SSH/SFTP 需要 Server 与目标 Node 升级至 0.10.1；调用方 CLI 也需升级。先升级并备份 Server，
再逐台更新 Node。0.10.0 为开发验收候选版，正式版使用 0.10.1，确保 Android 也能发现更新。

## 发布维护

日常 Server/Web 改动走 **Server CI**，只构建 Server 镜像并验证 PostgreSQL、鉴权、Web API 与
会话迁移；部署时只替换 `control` 服务，不需要重编或更新 Node、CLI、OpenSSH 和 Android APK。
Node、安装器或 SSH 通道变更走 **Node CI**，才会执行多平台客户端构建。Android 签名验收只在
`node/**` 变化时自动运行，也可手动触发。推送 `v<VERSION>` 仍表示一次完整客户端正式发布。

`VERSION` 是版本事实源，Go 开发默认值、Server package/lock 与 Docker 默认值由
`scripts/check-version.mjs` 检查一致性。Android versionName/versionCode 从 VERSION 派生；
发布二进制同时携带 commit、buildTime、protocolVersion。SemVer 的 minor/patch 须小于 1000，
以保持 Android versionCode 单调且不重叠。

`scripts/build-release.sh dist` 构建桌面归档、安装器和 SHA256SUMS。推送 `v<VERSION>` tag 后，
Release workflow 构建桌面归档及正式签名 APK，再发布 GitHub Release。签名材料仅从 GitHub
Secrets 注入：`MIRA_ANDROID_KEYSTORE_BASE64`、`MIRA_ANDROID_STORE_PASSWORD`、
`MIRA_ANDROID_KEY_ALIAS`、`MIRA_ANDROID_KEY_PASSWORD`，不得进入 Git 或构建日志。

Mira 的 Release workflow 只构建 Node/CLI/Server 对应产物与 APK，不再编译或附带 Codex。
独立的 **Codex runtime release** workflow 使用 `codex-v<version>` tag，并始终以 `--latest=false`
发布，不能抢占 Mira/Android 更新器的 `latest`。`node/internal/codex-runtime.json` 固定版本和
补丁 SHA-256；更改 Codex 基线或补丁必须递增 `mira.N`，不得覆盖已有运行包。

首次拆分可在 Codex workflow 指定 `codex_release=v0.12.0`，复用已经验收的完整 canonical package。
它先验证对应 tag 的 `CODEX_VERSION`、全部 Codex 补丁和归档 SHA-256；不一致即失败。
这种首次发行保留原始文件哈希，节点可直接从旧版本复制，避免重新下载大包。留空则从源码构建。
`scripts/build-codex-release.mjs OUTPUT linux-amd64=PACKAGE windows-amd64=PACKAGE` 生成独立归档、
逐文件哈希/大小清单与 SHA256SUMS；workflow 同时附带上游 LICENSE/NOTICE。

发布顺序：先用 `publish=false` 验收 Codex 运行包，再发布独立 Codex tag；随后为 Mira 选择新的
`VERSION`，验收 Node/APK 并发布 `v<VERSION>`。Mira 正式发布会检查固定 Codex release 已存在且
不是草稿。普通 Mira 更新不需要重编 Codex，也不需要重发旧 Codex tag。
发布前使用 `publish=false` 验收签名产物，再为同一提交创建正式 tag 和 Release。

若本地上传较慢，可先创建指向已验收提交的 Release 草稿，再运行 `Publish verified release`
工作流，传入成功的 Release workflow run ID。它会校验来源仓库、构建结果、目标提交、完整资产
校验和及安装器源码，直接从云端发布同一份 `mira-signed-acceptance`，不会重新编译或覆盖正式版。

SHA-256 用于检测传输/文件损坏；发行信任边界是固定 GitHub 仓库与 HTTPS，不是独立离线签名
的软件供应链。发布权限和 Android 签名私钥应严格保管。

## 独立 Codex 运行包与缓存

只有执行 Codex 的 Linux/WSL amd64、Windows amd64 节点需要运行包。文件、进程、PTY、SSH
节点不下载；Android 不下载也不运行 Codex。Linux arm64 目前可通过显式路径提供兼容构建。

```sh
mira codex-runtime status             # 无需批准接入/联网，查看固定版本和本地缓存
mira codex-runtime install            # 提前下载，亦可在节点获批前执行
mira codex                           # 获批后，自动准备运行包，再启动共享 PostgreSQL 的 CLI
mira codex-runtime install --release-directory /absolute/path/to/codex-release
```

离线目录需包含独立发行的 `codex-runtime.json` 和本平台归档；安装仍验证固定版本、补丁、
归档与逐文件 SHA-256。不会改写系统官方 Codex 安装。Codex 的模型登录/凭据仍需单独配置。

缓存位于 identity file 同级的 `runtimes/codex/<运行包版本>/<平台>/`，即 Linux 默认
`~/.config/mira/runtimes/codex/`、Windows 默认 `%USERPROFILE%\.mira\runtimes\codex\`。
自定义 `--identity` / `MIRA_IDENTITY_FILE` 沿用其目录；可用绝对路径 `MIRA_NODE_CODEX_CACHE`
覆盖，CLI 与常驻 Node 应使用相同设置。缓存不放进 `versions/<Mira版本>`，不会随节点更新删除。

第一次准备只请求小清单，再尝试从本机旧 Mira 版本的 `mira-codex-package` 校验并复制整包。
只有每个文件都与固定发行完全一致才复用，否则下载；之后缓存完整时不访问网络。
并发 CLI/App Server 共用操作系统文件锁，下载中断不发布半包，停止 App Server 会取消下载。
网页会显示首次准备提示；失败保留具体原因，Node 的其他能力不受下载阻塞。

App Server 默认选择固定运行包；显式 `CODEX_BINARY`、配置 `codexBinary` 或控制面 `codexPath`
优先，不会自动换掉用户指定的构建，远端 ThreadStore 仍必须通过兼容性探针。
切换 Codex 版本通过更新 Mira 的兼容锁完成，不提供任意追逐 Codex `latest` 的自动升级。
旧缓存/旧 Mira 版本不会自动清理，便于回退；缓存损坏时会拒绝覆盖，先停用相关会话并手动将
报错的精确版本目录移到备份位置，再安装，不要删除身份目录或整个 Mira 数据目录。

## 内嵌 OpenSSH 的安装布局

当前源码只保留内嵌 OpenSSH。Linux 版本目录保存一个主程序和角色软链接；Windows 安装器从 ZIP 中的一份 EXE 建立 NTFS 硬链接，无需管理员权限，硬链接失败则中止安装。不会覆盖系统 ssh，也不会修改 SSH 配置或安装 sshd 服务。Android APK 内仅一份原生程序，升级后自动重建角色链接。

新版安装器拒绝旧的非内嵌归档。升级会保留已经安装的旧程序；若需下载更早的非内嵌版本进行回退，应使用那个版本随附的安装器，不能混用新版安装器。

从 0.12.0 起，`mira update` 会先下载并验证目标版本的安装器，再安装对应归档，避免旧安装器
不认识新包格式。0.11.x 及更早的 Windows CLI 不具备此自举逻辑；首次迁移请用新版官方
`install.ps1 -Update -Version 0.12.0`（或先验证校验和后刷新本地安装器，再执行 `mira update`）。
这不需要卸载、重新注册或重新授权节点。

更新仍保留节点身份、配置和旧版本。SCP/SFTP 现在使用原生覆盖、递归和批处理语义；旧 `--overwrite` 选项不再使用。发布打包需要先构建完整内嵌包，见 [构建说明](node/openssh/README.md)。这不代表当前 GitHub 已发布的旧版本会自动改变。

跨旧/新 SSH 后端迁移时，先更新 Server 和调用方 CLI，再更新目标 Node。新 CLI 能连接旧 Node；
旧 0.11.x CLI 固定使用 SSH 用户名 `mira`，不能连接使用实际操作系统用户名的新原生后端。
