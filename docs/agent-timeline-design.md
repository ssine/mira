# Agent 运行时间线设计

状态：设计提案与独立数据 demo，尚未接入生产 Web/API。目标是解释一次工作如何在模型、工具和多个 Agent 之间推进，并能在全局时间轴比较多条对话。

## 阅读方式

采用树形泳道时间线：横轴统一为真实时间，纵轴每行一个 Agent/thread。根对话可展开任意层子 Agent；关闭或归档的子 Agent 仍留在历史图里。普通 fork 是独立根对话，可显示“来源”关联，不能当成执行子任务。

提供两个入口：对话内的“时间线”页签，默认这一对话树与当前轮次；全局导航中的“运行时间线”，默认今天所有有活动的对话，可按时间、Node、模型、状态和标题筛选。全局可折叠根对话，摘要显示后代活动；展开后共享原来的时间范围，避免丢失上下文。Node 只是执行地点，不能代替 Agent 泳道。

每一行保留完整轮次基线，在线上叠加阶段区间和消息标记。多轮之间的等待用户时间保持为空白；不能跨夜连成模型等待。展开一行可看到轮次、模型请求与工具调用子轨道，并行工具分轨呈现。顶部有可拖选的并发概览，支持平移、缩放、适应选中轮次及跳到下一个活动区间；压缩闲置时间只能作为显式可选模式，并显示断轴。

优先保证信息密度。默认每行 22 px，允许连续调节 18–64 px；紧凑模式隐藏第二行元数据，以 hover 和 Inspector 补充。名称栏宽度独立调整 120–380 px，时间比例不随行高变化。Inspector 可收起，专注模式隐藏标题和汇总区域。所有布局偏好应保存在客户端；全局模式默认展开活动 Agent，以折叠开关聚合整棵树。

时间控制同时支持：输入北京时间的精确起止、常用窗口长度、拖动平移、Ctrl/⌘ + 滚轮围绕鼠标位置缩放、Shift + 拖动框选放大、概览拖选及拖动两端边界、双击适应所指轮次。普通滚轮保留纵向滚动；Shift + 滚轮横向平移。键盘左右平移、加减缩放、0 适应活动。所有方式操作同一个时间窗口，保持选择和纵向阅读位置；最小缩放达到 100 ms，放大后显示秒及毫秒刻度。所有历史仍可分页查看，不把默认视口变成历史总量限制。

| 视觉 | 含义 | 点击后的信息 |
| --- | --- | --- |
| 蓝色菱形 / 刻线 | 用户输入 / Agent 消息 | 时间、轮次、来源事件，跳到消息 |
| 紫色区间 | 模型阶段 | 按工具边界归因的时长；后续可展开请求、首 token、重试及 token |
| 绿色区间 | 工具调用 | 名称、起止、状态、耗时，展开已有工具详情 |
| 琥珀色区间 | 明确的等待 | 等待子 Agent、用户输入或审批 |
| 灰色斜纹 | 缺少返回等未决区间 | 已知事实与缺失字段 |
| 树连接 / 因果箭头 | 父子关系 / spawn、send、join | 发起工具和对应子任务 |

不依赖颜色传达含义。所有区间带类型文本、图例和键盘可访问的事件列表；浅色默认，深色主题沿用 Mira token。窄屏保留时间范围、搜索和详情，以单条 Agent 及其上下级导航替代挤压整棵树。

## 时间与计数的契约

“一条完整的线”是一个完整的时间上下文，不表示 Agent 任意时刻只能执行一种工作。异步工具、子 Agent 与模型可以并行；请求还可能含重试、限流退避、压缩与存储等待。

默认采用用于比较主要耗时的阶段归因口径：轮次开始到首次工具调用、所有在执行的工具返回到下一次调用、最后一次返回到轮次结束，归为“模型阶段”；工具调用到返回归为“工具阶段”。模型阶段包含模型请求、生成，以及请求准备、网络与调度开销；主视图使用普通实色，不在每个区间重复显示“推断”。完整口径放在详情与数据说明中。请求级埋点是后续细分阶段的方法，不作为第一版展示模型阶段的前置条件。

明确的子 Agent / 用户输入等待单列，多轮间的用户空闲不纳入模型阶段。缺少工具返回的未决时间不填成模型阶段。阶段统计在每个 Agent 内取区间并集，再对 Agent 求和，避免并行工具重复累加；已知工具优先于等待，等待优先于未决，未决优先于模型归因。每个调用自己的耗时仍单独保留。并行精确请求到来后可用子轨道直接展示模型与工具重叠，不能用这个历史归因规则压平实际重叠。

分别展示：选择范围的墙钟时间、每个 Agent 的活动时间并集、所有 Agent 的工作时长之和、工具调用数、工具累计耗时、峰值并发。累计耗时允许大于墙钟时间；不能把父任务等待与子任务执行相加后称为用户等待时间。阶段占比只针对清楚定义的独占轨道；重叠部分显式表示。关键路径需要因果边完备，第一版不推算“节省了多少时间”。

工具按 `(store, thread, generation, turn, call_id)` 去重；call/output 和 lifecycle/materialized 事件是同一调用的多种证据。单次批量工具按一个顶层调用计数。`functions.exec` 等包装工具内部实际调用的数量，必须由运行时的嵌套 span 提供；不能从代码字符串或输出块数推导为实际执行次数。一次调用返回后台 session 也不表示后台进程已结束，需要关联其后续执行生命周期。

请求次数来自请求 ID 和 attempt，不从 token_count 更新次数或 reasoning 条目数量推断。工具出错、用户中断、存储失败、Node 断开以及缺失结束事件分别表示；断开不等于完成。历史快照中未结束区间在最后证据处截断并标记“未观测到结束”，不延伸到现在制造几小时等待。

## 现有数据能提供什么

canonical `codex_thread_events` 已有轮次生命周期、模型面对的工具 call/output、消息和模型上下文。`codex_thread_projections` 提供当前 generation、item_count、元数据；`mira_agent_graph_events` 与代际匹配的 `mira_agent_graph_edges` 提供关系及关闭状态。

历史时间来源需要分级，不能统一叫“精确耗时”：

1. 生命周期中明确的起止与单调时钟 duration：事件声明的时间。
2. rollout envelope timestamp、消息元数据 `internal_chat_message_metadata_passthrough.create_time`：记录时间，可还原调用/返回之间的记录区间，但不保证等于工具实际执行边界。线上样本中同一模型响应的消息与工具调用共享创建时间，区间可能包含工具参数生成；不可将其直接标为实际工具执行耗时。
3. PostgreSQL `created_at`：持久化观测时间，可能成批写入或晚于执行；导入时间不能作为历史执行时间。
4. 轮次内扣除工具、明确等待和未决区间后的时间：归为“模型阶段”，用于比较模型阶段与工具阶段的主要耗时；详情注明这是按事件边界归因，不能当作纯 provider latency 或 TTFT。

历史不能可靠拆出消息上传、broker 调度、请求排队、TTFT、流式输出与网络耗时。消息默认是时间点；只有双端埋点完整时才显示“发送中”的持续时间。token usage 不提供这些边界。

每次读取固定 `(store_id, thread_id, generation, item_count)`。不混合新旧 generation。读取全局快照时保持一致边界，普通分页使用 snapshot token；新事件通过后续增量纳入。关系同时校验父子 generation。老数据仅在持久 source 明确为 subagent 时回退父线程字段，并标记关系来源。

复制历史必须排除：优先使用持久 child-owned ordinal 或 child `thread_settings_applied` 边界。缺失时允许用原始记录时间与线程创建时间提供明确标为部分覆盖的历史视图；不能把无边界的副本默认为新执行。全局的 inherited history 不重复统计。生命周期外的记录保持单独证据，不擅自延长一个已结束轮次。

## 正式实现

### 历史投影

在 Go `node/internal/miraserver/views/` 增加纯投影器，从 canonical history 分页读取，输出 spans、instant events、edges 和 coverage。沿用未知字段兼容与 JSON/NUL 处理，读取时不改写原始记录。结果缓存按不可变边界、投影版本和所有后代水位建立，父历史未变也要刷新子任务。保留 v1/v2、导入与缺失字段适配。

大规模全局读取使用 PostgreSQL 派生 timeline index，包含 occurrence time、end time、类型、引用序号及质量标记，支持时间区间与 thread-tree 查找；它可以从 authoritative 数据重建。索引和投影版本通过新增迁移引入，旧 Server 的 SQL 契约保持兼容。先在影子投影验证，再启用全局入口。

### 补齐运行时观测

定义独立版本的 `ExecutionObservationV1`，作为 PostgreSQL 追加式执行观测事件，和 canonical rollout 并存。不要给未知 upstream rollout item 强塞一个新类型。生命周期埋点贴近实际 Codex 请求、工具分发与子任务协作边界；Server 代理只能看到 managed App Server 流量，不能作为 CLI 和跨 Node 子任务的唯一采集点。

必要事件：`input.submitted/accepted`、`turn.started/ended`、`model.request.started/first_token/ended`、`tool.queued/started/ended`、`agent.spawned`、`agent.message.sent/received`、`agent.wait.started/ended`。每次重试、退避、context compaction 与 persistence wait 单独成 span。工具包装器内部透传 parent span 与 invocation ID，才能回答嵌套工具实际数量。

字段包括版本、事件 UUID、store/thread/generation/turn、span/parent-span、call/request/attempt、执行 Node、runtime instance、单调序号、UTC 发生时间、单调时钟时长、Server 接收时间、状态及引用的 canonical item。这里只存可观测元数据；正文、参数和输出复用现有授权详情读取。身份、账户与 Node 历史绑定引用发生时的记录，不能拿当前 Node 绑定冒充整个历史的执行地点。

跨 Node 的 UTC 可能偏移，记录时钟域与不确定度。单 Node 内用单调时钟计算时长；因果边保证顺序，跨域矛盾显式显示，不悄悄拉直。执行观测事件 UUID 保持重试幂等；采集使用有界队列和背压，丢失或无法持久化时记录 coverage gap，不能无限阻塞或静默宣称完整。PostgreSQL 是观测历史的持久来源，不另建本地权威日志。

### 查询和 Web

建议新增只读管理员 API：`GET /v1/codex/timeline?from=&to=&rootThreadId=&cursor=&resolution=`，返回 `snapshot, bounds, lanes, spans, edges, aggregates, coverage, nextCursor`。时间范围必填；范围是用户可分页浏览的视口，不是任意历史总量上限。树范围可跨 Node，包含 closed/archived 子 Agent。

概览按像素时间桶聚合，返回每桶并发数、事件数、覆盖情况；放大后分页取原始 span。行与时间双维虚拟化，详情按需取。缩放和平移支持请求取消，过期响应不能覆盖新视口；弱网保留既有结果与读取进度。运行中用现有 App Server 事件即时补充，中央持久数据校正和断线补齐，CLI 历史也能出现。稳定控件 ID、CSP 与主题沿用现有无框架 Web，不把 demo 数据打进正式 embedded assets。

## 分期与验收

第一阶段提供历史树形时间线、模型/工具阶段归因及累计时间、顶层工具数、全局过滤、证据详情和覆盖标记。第二阶段上线版本化观测与精确请求/工具边界，再开放 TTFT、实际工具耗时、嵌套调用和因果箭头。第三阶段在规模数据上启用增量索引、全局聚合与告警。

验收必须覆盖：同 ID 多源事件去重、并行工具、异步进程、steering、取消与部分失败、乱序/缺失时间、持久化批次、导入、复制前缀、普通 fork、关闭/归档子 Agent、跨代替换、跨 Node 时钟偏移、CLI/App Server/subagent 一致性，以及 escaped NUL 与未知字段。生产存储改动执行现有 v1/v2/resume/subagent 回归；common Node 改动执行 native tests 与 Windows/Android cross-build。浏览器验证全局和树视图、键盘、窄屏、主题、增量不跳动以及大量事件下的有界渲染。
