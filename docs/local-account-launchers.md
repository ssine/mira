# 本机账号启动器：`mira codex` 与 `mira claude`

Node 上配置好的 Codex 和 Claude 账号可以直接从终端启动，并在 Mira Server 不可用时继续工作。
启动器只读取 Node 身份目录中的本地文件，不需要 Server、Web 登录或额外 CLI 凭据。

```bash
mira accounts                      # 列出本机 Codex / Claude 账号（离线可用）
mira claude                        # 交互选择 Claude 账号后启动 Claude Code
mira claude --account <selector> -p "..."
mira codex                         # 交互选择 Codex 账号；Server 可达时写入 PostgreSQL
mira codex --account <selector> --offline exec "..."
```

## 选择账号

- 不传 `--account` 时，终端里会列出该引擎的全部账号，输入编号、selector 或名称；直接回车
  重复上次的选择（`*` 标记，记录在 `accounts/cli-recent.json`）。这只是“最近使用”，不是默认账号。
- 只有一个账号时直接使用它。非终端（脚本、Agent）且有多个账号时失败并列出可用 selector，
  不会猜测。
- selector 可以是 Node 账号 UUID、至少 4 位的唯一 UUID 前缀、精确账号名称（不区分大小写）、
  Claude 订阅登录邮箱，或 `local`（尚未接收 Server 账号列表时的本机 Codex home）。
- 被 Mira 停用、未登录或缺少 API key 的账号会显示原因，不能被选中。
- Mira 选项必须写在引擎参数前；`--` 或第一个未知参数之后的内容原样传给 Codex / Claude Code。

## 账号环境与代理

两个启动器复用托管运行时的账号环境：

| | Codex | Claude |
| --- | --- | --- |
| 配置目录 | 账号自己的 `CODEX_HOME` | 账号私有的 `CLAUDE_CONFIG_DIR` |
| 凭据 | `auth.json`、托管 `profile.json` 与环境文件 | 订阅 OAuth 或 API key / Bedrock |
| 默认参数 | 与 `mira codex` 原有一致：1000 subagent、`never`、`danger-full-access` | 账号 model、effort |
| 代理 | 账号 egress | 账号 egress |

账号配置了 egress 时，启动器用同一份代理配置在**自己的随机本机端口**上运行一个私有 sing-box，
引擎退出时停止并删除临时配置（`account-egress/.cli-*`）。它不复用 Node 的网关：Node 的网关按托管
会话引用计数，空闲时会关闭，借用它会在网页会话结束时让 CLI 断网（ECONNREFUSED）；反过来占用
Node 的固定端口也会让之后的托管会话依赖 CLI 的生命周期。
sing-box 路径依次取 `MIRA_NODE_SING_BOX`、Supervisor 状态目录的 `mira.env`，最后是 `PATH`。

Claude Code 可执行文件依次取 `MIRA_NODE_CLAUDE_BINARY`、Node 为当前版本准备的 SDK、其他已准备的
SDK（最新优先），最后是 `PATH` 中的 `claude`。启动器不会下载或安装 Claude Code。

启动器与 Web 托管会话共享同一个账号目录，不复制凭据：OAuth refresh token 轮换后，复制出来的
另一份凭据会失效。不要在启动的会话里 `/logout` 或重新登录，凭据变更应在 Mira 账号页完成。

## 会话保存位置

- **Codex，Server 可达**：使用 Mira PostgreSQL ThreadStore，并带上账号绑定，与 Web 会话一致。
- **Codex，Server 不可达或 `--offline`**：启动时提示后回退到离线模式，rollout 保存在该账号
  `CODEX_HOME/sessions`。`--online` 要求 Server 可达，否则失败而不回退。
- **Claude**：始终只写该账号目录下的原生 transcript，不镜像到 PostgreSQL，Web 中看不到。

离线会话是 Server 故障时的应急路径，不是第二个权威存储。它们不会自动合并回 PostgreSQL。

## 本地账号索引

账号名称、启用状态以及 Codex 的 desired 配置（config overrides、环境文件、继承变量、Codex 路径）
保存在 Server。Node 每次收到 Server 确认的 desired state 后，把启动所需的部分写入
`<identity-dir>/accounts/local-index.json`（0600，仅在内容变化时重写）。

- 它是可丢弃的缓存，不是权威数据；Server 不可达时不会被改写。
- Server 在注册和心跳响应中附带 `claudeAccounts`（仅 ID、名称、启用状态），用于给 Claude 账号显示名称。
  旧 Server 不返回该字段时保留已缓存的名称。
- 索引出现之前创建的账号仍能从 `accounts/<id>/` 目录发现，只是没有名称。

## 范围

- Android Node 不支持这两个启动器。
- Server 侧的“Node 默认账号”仍然存在（旧安装的 App Server 实例）。启动器不依赖它：
  在菜单里它只是一个普通条目。
