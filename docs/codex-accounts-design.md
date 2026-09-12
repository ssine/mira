# Codex 账号管理

一个 Mira Node 管理多个模型账号，各自使用独立的 Codex App Server、认证环境和派生 SQLite 目录。
PostgreSQL 仍是所有账号共享的唯一权威对话库。SQLite 不保存权威会话，也不与 Codex Desktop 共用。
默认最多同时运行或准备四个账号，通过 `MIRA_NODE_MAX_CODEX_RUNTIMES` 可调整为 1–128。
每个 Node 最多保存 128 个账号配置。

## 使用与凭据

在 Web 的「账号」页或 Node 账号列表新增账号。创建和继续对话时分别选择执行 Node 和账号；
连接、模型目录、标题生成和额度都使用明确选中的账号。原有安装保留为该 Node 的默认账号。
本次没有给 `mira codex` CLI 新增账号选择参数。

| 类型 | 凭据方式 |
| --- | --- |
| ChatGPT | 启动账号后发起设备码登录，支持查看进度、取消和退出；组织须允许设备码登录。 |
| OpenAI API | 通过账号详情的 API key 登录配置；不提供 ChatGPT 套餐额度。 |
| 自定义 Responses 服务商 | 配置 base URL、API key 或 provider 引用的环境变量，无需官方登录。 |
| 已有 Codex home | 指定 Node 上的绝对路径，读取原 config.toml/auth.json，包括 TOML 内的 bearer token。 |

托管账号凭据保存在 Node 身份目录下受保护的 `accounts/<binding>/profile.json`，经认证通道瞬时
转发，不进入 PostgreSQL、desired state、审计记录或浏览器持久存储。URL 不接受内嵌密码或查询参数。
接管已有 home 保留原文件，provider 仍由原文件管理；修改 provider 应编辑原文件或另建托管账号。
Node 拒绝自己的重复 home 绑定，但不能检测另一套安装或外部 Codex 进程的占用。

额外账号继承基本 OS、代理、证书环境，以及 provider 引用和显式列出的变量；不继承默认账号的
provider 覆盖参数或无关 API 密钥。环境文件逐行使用 KEY=VALUE，不执行 shell 或变量展开。
文件值覆盖继承值，受保护的账号环境和密钥最后覆盖。每个配置文件最多 256 KiB，页面只显示来源
和变量名，不返回值。CODEX_HOME 和 MIRA_* 为保留名称，账号密钥使用专门的内部注入路径。

修改配置须先停止账号。登录、退出检查全部已加载对话，包括子任务；有任务时拒绝修改凭据。
会话切换与历史兼容处理只检查当前对话树，其他主对话可以继续运行。
未完成登录断开后会结束其空闲实例，避免后台继续改写凭据。

## 路由与额度

CodexAccount 是逻辑账号，NodeAccount 将其关联到 Node 的本地配置。可经创建接口的 accountId
关联已有逻辑账号；关联不会同步密钥。默认绑定保持旧 desiredAppServer/reportedAppServer 兼容，
额外绑定分别保存 desired/reported state。

对话侧栏列出全部已批准 Node 的账号，按去除首尾空格后的名称去重；同名账号使用最近一次
有效额度快照，不相加剩余百分比。离线 Node 的已知快照仍可查看。列表每五分钟读取全 Node
快照，打开详情时可刷新所选运行账号；不会为了读取余额而启动账号或对话。

未提供额度时，侧栏直接显示最近七天的估算费用，同时最多读取两个账号，缓存五分钟；详情
改为每日估算费用：每个本地日历日一根柱，叠加每小时聚合的日内累计
曲线，跨日从零开始。`GET /v1/codex/accounts/cost-history?name=...&range=24h|7d|30d&timezone=...`
为管理员只读接口，其中 `24h` 在费用视图表示今天。名称聚合跨 Node 绑定，费用直接读取
PostgreSQL 当前 generation 的 canonical token_count，复用对话标准 API 价格估算。
先按执行事件的 turn ID 确定账号；缺少 turn 事件的已绑定子会话按请求时间查询当时绑定。
不会把切换前的请求归给当前账号，也不重复计算 fork 复制的历史、重复 token_count 或子会话。
没有执行归属、有效请求时间或价格的历史不能作为完整账单回填；空记录保持未知，部分估算
保留标记。它不表示套餐扣减或自定义服务商的实际账单。接口按页流式读取历史，响应按小时
聚合，限制请求时间而不截断为部分成功；无需新增持久表或改写历史。

输入栏的账号选择在附件按钮左侧；窄屏单行布局将其隐藏，仍可通过右侧会话详情选择账号，
两个控件保持同步。

运行账号每五分钟独立采样，保存实际额度窗口和重置信息。凭据版本变化使旧身份样本失效，
普通进程重启不割断身份统计。自定义服务商不提供套餐额度时明确显示不支持；不会把 token 用量
或 API 等价费用当作套餐扣减，也不会把多 Node 的剩余百分比相加。

start/resume/turn 入口检查 binding/runtime，追加执行事件并更新可重建路由。切换只锁住当前
对话树，在旧实例中检查这棵树空闲、逐个退出并等待持久化和事件处理完成，再恢复目标账号。
同账号其他对话及其进程保持运行。旧 Node 离线或这棵树仍有任务时拒绝接管，并返回具体对话 ID；
thread/unsubscribe 并不证明线程已卸载。账号选择器的待选账号与已提交绑定明确区分。

从主会话切换账号时，Server 将当前 generation 的整棵父子树作为交接单位，包括关闭或归档的
子会话；普通 fork 保持独立。等待涉及的旧实例确认指定对话树卸载后，在同一事务中更新所有成员的账号、
运行实例和执行事件，随后才允许目标进程继续。子 Agent 不能单独切到与主会话不同的账号；
从子会话发起跨账号恢复会提示先打开主会话。新建子 Agent 继续使用所在 App Server 的凭据。

0.153.1-mira.10 在 ThreadStore 的读取边界将恢复用的服务商投影为当前托管账号的服务商，
保留子 Agent 原有模型及原始元数据、加密历史和持久化 diff。父会话恢复后可直接 followup_task
唤醒旧子 Agent，无须在客户端逐个恢复。旧 runtime 未确认支持账号协议 2 时拒绝整树切换，
避免仅更新路由而使用错误服务商；空账号通过只读 thread/list 完成协议探测。

切换后的旧 runtime 写入会被拒绝，已提交但丢失响应的 operation UUID 仍可重放原收据。
此机制不替代项目尚未实现的通用 writer lease、网络分区 fencing 和外部 CLI 生命周期协调。

## 加密上下文恢复

默认完整保留推理和压缩项，按 Codex 原语义继续。仅明确的模型错误 invalid_encrypted_content
触发提示，普通认证错误或工具输出里的同名文本不会触发。根据 canonical type 判断 reasoning、
compaction/context_compaction 和 compacted checkpoint，不依赖 cmp_ 等 ID 前缀。

用户明确确认后，只过滤恢复时的模型输入。如果是推理则省略旧的加密推理项；如果压缩检查点也
不兼容则从保存的完整压缩前消息重建输入，并提示输入可能变长。缺少完整来源、未知加密项或
加密工具结果时拒绝自动处理。原始历史、导出、fork 和持久化 diff 不过滤、不删除、不换 generation。
确认时只卸载当前对话树；同一账号进程下一次恢复会读取已确认策略，其他对话不受影响。

确认绑定于 thread/generation/NodeAccount/credential revision 和冻结的历史前缀；新产生的加密
推理继续保留。其他账号不继承该决定。确认 operation UUID 支持响应丢失重放；确认本身不重发
失败的 turn 或工具，用户再次发送才继续。

输入恢复需要 0.153.1-mira.9 或更新兼容 runtime。Server 根据具体 runtime 的协议观察确认支持，
旧包只保留输入并提示升级。fork 仍复制 canonical history，不能视为绕过加密兼容性的操作。

## 接口与存储

Schema 28 追加逻辑账号、Node 绑定、额度样本、执行事件/路由、兼容确认与 runtime 协议观察表。
Schema 29 将 Codex AgentGraphStore 的父子关系及 open/closed 状态保存在追加事件中，
关系索引可从事件重建；恢复只读取当前 generation 的关系，跨账号不再依赖本地 SQLite。
保留前一版 Server 的 SQL 契约，默认账号仍双写旧额度表。

- GET /v1/codex/accounts：全局列表。
- GET/POST /v1/nodes/:nodeId/codex-accounts：列出/创建；单个绑定可 PATCH 名称。
- 绑定下的 quota、quota-history、configure、login、login-status、login-cancel、logout：额度和凭据管理。
- 既有 runtime start/stop 请求及 App Server WebSocket 接受 nodeAccountId；省略用默认账号，显式无效值不回退。
- GET/POST /v1/codex/threads/:threadId/input-recovery：读取计划/确认，使用 storeId 和 nodeAccountId 限定范围。
- Node 报告 codexAccountsV1 和 codexThreadHandoffV1；appserver.open 携带 binding/runtime，
  accountThreads 指定交接范围。凭据管理仍使用账号全局锁；对话交接锁保持到路由事务完成。
- 0.153.1-mira.11 新增 mira/thread/unload，接收 threadId（根）与 threadIds（完整树，最多 4096），
  确认退出、事件排空和 ThreadStore flush 后返回相同 ID 集。重复调用可确认已卸载成员，
  不修改归档状态或持久化父子边。旧 Node/运行包明确提示升级，不回退为关闭整个账号。
- 历史兼容确认返回 reloadedThreadId，不再返回 retiredRuntimeId；旧确认收据仍按原格式重放。
- ThreadStore 请求发送 X-Mira-Accounts-Version、X-Mira-Codex-Account、X-Mira-Codex-Runtime。
  版本 1 支持账号身份与输入恢复；版本 2 增加目标账号服务商投影，需要 Mira 1.0.20 或更新 Server，
  Server 同时接受 1 和 2。认证仍以 Node 凭据为准，选择器不能冒充其他 Node；管理写操作仍要求管理员和 CSRF。
- POST /v2/stores/:storeId/agent-graph：upsert/status 写入或 children/descendants 查询。
  写入携带 operation UUID，支持原收据重放；关闭边阻断 open 后代查询，缺失边的状态更新为空操作。

## 迁移与验证

先更新 Server 和主 Node，再将旧账号专用 Node 的 Codex home 关联到主 Node。停止旧账号实例并
确认任务结束后才启用新的绑定，不自动搬迁凭据、改写旧会话或撤销旧 Node。保留原文件和身份以便回退。
Supervisor 负责版本更新；Nix 所有权不变，不执行 nixos-rebuild 或修改下游仓库。

测试覆盖环境隔离、PostgreSQL v1/v2 写入和收据、Web 账号选择/确认，以及真实 Node + Codex
App Server 对合成 Responses 服务商的切换和恢复。独立 Codex runtime 保持完整 canonical package。
