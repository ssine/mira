# Codex Remote Thread Store Adapter Protocol v2

状态：已实现，数据库 schema 17。HTTP JSON；ThreadStore 请求使用获批 Node 的 Bearer 凭据。
管理员使用数据库 Cookie 会话，见 [auth-v1.md](./auth-v1.md)。

## 1. 目标与不变量

- PostgreSQL 的原始 rollout、元数据变化日志、历史边界和操作回执共同构成持久化事实来源。
- Codex 仍读写官方 `ThreadStore` 数据结构；adapter 只把一次本地状态变化编码成细粒度提交。
- store head 是单调递增的 `uint64`。一次有效提交恰好产生一个新版本。
- rollout item 原始 JSON 不做业务格式转换。新 Codex 字段可以原样持久化、回放。
- thread 历史在一个 generation 内不可变、只追加；重写历史必须创建新 generation。
- snapshot 按需构造，当前状态和 thread projection 可从权威记录重建；不存完整历史快照。
- 所有写入带 operation ID，从而支持超时后的安全重试。

协议版本与 Codex rollout/event 格式版本分开演进：adapter v2 不要求 Server 理解每一种
rollout item，只要求它保存合法 JSON、顺序和 generation。

## 2. 读取 store head

```http
GET /v2/stores/{storeId}
Authorization: Bearer <token>
```

响应：

```json
{
  "version": 42,
  "state": {
    "created_threads": {
      "thread-uuid": { "thread_id": "thread-uuid", "source": "vscode" }
    },
    "metadata_updates": {
      "thread-uuid": { "updated_at": "2026-09-02T08:00:00Z" }
    }
  },
  "historyManifest": {
    "thread-uuid": { "generation": 3, "itemCount": 128 }
  },
  "updatedAt": "2026-09-02T08:00:01.000Z"
}
```

`state` 不包含 rollout 历史。客户端必须使用同一响应里的 `version` 和
`historyManifest` 加载历史，从而得到一致的物化视图。

传入 `?threadId=<id>` 可只读取一个会话的状态和 manifest，常规追加和元数据操作使用此路径。
响应还包含 `historyFloor`：一次性迁移清理旧快照后最早可读取的版本。

## 3. 按版本读取 thread 历史

```http
GET /v2/stores/{storeId}/threads/{threadId}/history?generation=3&throughVersion=42
Authorization: Bearer <token>
```

响应：

```json
{
  "threadId": "thread-uuid",
  "generation": 3,
  "itemCount": 128,
  "items": [
    { "type": "session_meta", "payload": {} },
    { "type": "response_item", "payload": {} }
  ]
}
```

- `throughVersion` 把读取固定在指定 store 版本；Server 只返回该版本已经可见的 item。
- generation 与该版本 manifest 不匹配时返回 404。
- 实际数量与 manifest 不一致时返回 409，客户端不得使用部分历史。
- 省略 generation 时使用目标版本的 active generation。
- `throughVersion < historyFloor` 返回 410；客户端需重新读取 head。

## 4. 细粒度提交

```http
POST /v2/stores/{storeId}/commits
Authorization: Bearer <token>
Content-Type: application/json
X-Codex-Operation-Id: <uuid>
X-Codex-Version: 0.151.0
```

```json
{
  "expectedVersion": 42,
  "stateChanges": [
    {
      "path": ["metadata_updates", "thread-uuid", "title"],
      "mode": "set",
      "conflictPolicy": "compareAndSwap",
      "expected": { "exists": true, "value": "old title" },
      "value": "new title"
    },
    {
      "path": ["metadata_updates", "thread-uuid", "updated_at"],
      "mode": "set",
      "conflictPolicy": "lastWriteWins",
      "expected": { "exists": true, "value": "old timestamp" },
      "value": "new timestamp"
    }
  ],
  "historyChanges": [
    {
      "threadId": "thread-uuid",
      "mode": "append",
      "expectedGeneration": 3,
      "expectedItemCount": 128,
      "items": [{ "type": "response_item", "payload": {} }]
    }
  ]
}
```

成功响应：

```json
{
  "version": 43,
  "operationId": "uuid",
  "rebased": false,
  "historyManifest": {
    "thread-uuid": { "generation": 3, "itemCount": 129 }
  },
  "appendedItemCount": 1,
  "updatedAt": "2026-09-02T08:00:02.000Z"
}
```

同一 operation ID 再次提交不会重放变化；响应包含 `duplicate: true` 和原提交版本。
没有产生变化时不推进 head，并返回 `noChange: true`。

单会话提交可带 `X-Mira-Thread-Scope: <threadId>`，让响应只包含该会话的 manifest。
Server 验证所有变化确实属于这个会话；不带该头仍兼容旧客户端。

`rebased: true` 表示 Server 在高于 `expectedVersion` 的 head 上合并了提交。adapter 必须使本地
物化 cache 失效，下次操作从 PostgreSQL 重载，不能只把本地 cache 的 version 改成新 head。

### 4.1 state change

`path` 是 1–16 段的 JSON object 路径。数组视为叶节点整体比较，不能在数组内部寻址。
`histories` 顶层路径保留给 `historyChanges`，不能通过 state change 修改。

`mode`：

- `set`：创建或替换叶值，必须有 `value`；
- `remove`：删除叶值，不发送 `value`。

`expected` 表示 adapter 生成 delta 时观察到的叶值：

- `{ "exists": false }`：路径原本不存在；
- `{ "exists": true, "value": ... }`：路径原值必须相等。

冲突策略由 Server 强制校验：

- `metadata_updates/{threadId}/updated_at/**`
- `metadata_updates/{threadId}/advance_recency_at/**`
- `metadata_updates/{threadId}/token_usage/**`

只有以上易变元数据允许 `lastWriteWins`；所有其他路径必须使用 `compareAndSwap`。CAS
的当前值若已经等于目标值，则该 change 视为幂等成功。

### 4.2 history change

`mode`：

- `append`：在当前 generation 后追加 item；
- `replace`：开启下一 generation，并以 `items` 作为完整历史；
- `delete`：移除 active history，但旧 generation 的权威事件仍保留。

`replace` 与 `delete` 要求 generation 和 item count 完全匹配。`append` 可在
`expectedGeneration` 仍为 active 且当前 item count 不小于 `expectedItemCount` 时 rebase：

1. 已存在且逐项相同的重叠前缀会去重；
2. 剩余 suffix 按数据库提交顺序追加；
3. generation 已变化时拒绝，不能把旧分支静默拼到新历史。

这允许 App Server 的 history writer 与 metadata writer 从同一旧 head 并发提交，也允许
不同 thread 在同一 store 内并发推进。

## 5. 错误与重试

- 400：请求结构、路径、模式或策略非法；修正请求，不重试原 payload。
- 401：凭据错误。
- 404：store/thread/generation 不存在。
- 409：CAS、generation 或一致读取冲突。adapter 清空本地物化缓存；调用方重新执行高层
  `ThreadStore` 操作时，会重新读取 head 和 histories 后产生新的 delta。
- 500：服务端故障。使用相同 operation ID 重试，直到确认提交结果。

客户端不能把一次 409 改成 LWW 后重试。是否可合并是 Server 协议规则，而非客户端偏好。

## 6. 写入和执行节点切换

一个 Codex 进程内部的 mutation 有序执行。Server 通过会话级锁保护冲突，独立会话可并发准备写入；
只有事务末尾发布版本时短暂锁住 `codex_store_heads`，不在此锁下读取或处理完整历史。`turn/completed` 是模型生命周期事件，不等于最后一批
持久化 callback 已完成。跨节点接管必须按以下顺序：

1. 停止给旧 App Server 分配新 turn；
2. 等待目标 assistant response 与 terminal turn event 已进入 PostgreSQL；
3. 等待 store head 短暂稳定，随后优雅关闭旧 App Server；
4. 新节点从一个 head/version 原子加载 state 和所有 history；
5. `thread/resume` 成功后再开放新 turn。

PoC 的 E2E 已验证这个 quiescent handoff。生产版仍应把它升级为显式 writer lease 和
`release/claim` API，避免网络分区中的双 writer。

## 7. 兼容与升级

- `/v1/stores/{storeId}` 保留完整 snapshot 兼容投影，可从 v2 权威事件重建。
- 每个 store event 记录 `event_format_version` 与产生它的 `codex_version`。
- rollout item payload 原样保存；未知字段不阻止旧 Server 存储。
- 数据库 migration 有递增版本、名称与 SQL checksum；已应用 migration 被修改时拒绝启动。
- 新投影以新表或新列添加，回填后再切换读取；不要原地重写旧 rollout payload。
- generation 保留合法历史重写前的旧数据，便于审计、回滚和离线升级。
- schema 17 按管理员授权进行一次性切换：保留原始历史、导入溯源和当前状态，清除旧完整快照，
  在原 head 建立一个 baseline。低于 `historyFloor` 的旧版本不能继续读取或写入。
- 具体表职责、事务顺序和回滚限制见 [存储设计](../docs/thread-storage-redesign.md)。

## 8. 当前资源上限

- 单个 HTTP body：64 MiB；
- 单次 state changes：100,000；
- 单次 history changes：10,000 threads；
- state path：16 段，每段 256 字符；
- Mira Node 单次文件读写：4 MiB；
- Mira Node 每类托管 session：128；
- process/PTY 游标输出缓存：1 MiB。

这些是传输边界，不限制一个 thread 的总历史大小；大历史通过 append 和按 generation 读取。

## 9. Web transcript 投影

管理员 Web 控制台通过下面的只读接口读取可展示轨迹：

```http
GET /v1/codex/threads/{threadId}/transcript?storeId=personal&limit=60&cursor={nextCursor}
```

新版 Web 使用 `tail=1`，通过主键倒序读取最多 `max(120, limit * 4)` 条原始记录，先显示最近
一页。只补读前一个 turn 的起始标记以保持 turn ID 和时间语义，不读取整个 store state 或全量
会话历史。`t2:` cursor 记录 generation、排他的原始序号边界和首次读取时的 item count；追加不
影响向前翻页，generation 变化返回 `409 stale_transcript_cursor`。这一模式不为计数扫描完整
历史，因此 `totalTraceItems` 为 `null`，一页可能少于 `limit` 条。跨页工具输入、输出和 materialized
记录通过 `toolFragment` 与稳定的 turn/call key 合并。分页不修改任何权威事件。

不带 `tail=1` 的旧版数字 cursor 继续兼容。Web 读取最近消息与运行节点连接并行；
`thread/resume` 使用 `excludeTurns: true`，模型恢复上下文不再阻塞消息首屏。页面在可见且联网时
退避重连，在前台恢复、网络恢复和 BFCache 恢复时检查通道，补读最近历史；重连不重放 `turn/start`。

Server 在同一个 store head 上读取 active generation，并从权威 rollout items 重建有序的
`user`、`assistant`、`reasoning`、`tool` 和 `system` 轨迹。投影会配对原始
`custom_tool_call` / `custom_tool_call_output`，也能读取 paginated rollout 的
`item_completed` materialized items。每条轨迹保留稳定 key、turn ID、源 item 序号、状态以及
是否应按 Markdown 展示。

默认响应最新 60 条轨迹，并返回 `nextCursor` 和 `totalTraceItems`。网页只保留最近一页作为首次
载荷；用户请求更早历史时把 `nextCursor` 原样传回，Server 从当前位置向前返回下一页。每页保持
时间正序，因此客户端只需把旧页插到当前列表顶部。`limit` 允许 10–200；`nextCursor` 为 `null`
表示已经到达最早记录。cursor 是 Server 实现细节，客户端不得自行推算。

这个接口不是新的事实来源，也不修改或替代 App Server 协议。它只解决不同 Codex 历史模式在
`thread/resume` 中可能返回不完整展示 items 的问题；继续对话仍由官方 App Server 完成。投影可以
在任何时候从 `codex_thread_events` 重建，未知原始字段继续完整保存在数据库。为避免单个工具输出
拖垮浏览器，每条投影正文最多 1 MiB，截断只发生在 Web 响应中。

## 10. 管理员归档和永久删除

Schema 15 的 `mira_thread_actions` 记录不含正文的归档、恢复和删除操作，按操作 UUID 幂等。
Web 归档只改变列表可见性，不改变 Codex history 或中断运行；默认列表排除归档，
`GET /v1/codex/threads?archived=1` 可查询归档。投影重建不会丢失归档状态。

管理员 `POST /v1/codex/threads/{id}/archive` 和 `/restore` 接收 `generation`、`operationId`。
`DELETE /v1/codex/threads/{id}` 还要求确认时的 `itemCount`，会话已变化时返回 409。
这三个接口均要求管理员 cookie 和 CSRF。Web 在删除前确认，并要求先停止已知的运行。

永久删除是明确的管理员内容擦除，区别于普通 v2 history delete：在同一 store writer lock 和
数据库事务中，删除该会话所有 generation 的 rollout、投影和运行绑定，擦除旧 store event 的
对应 metadata/manifest，失效兼容 snapshot，清理创建响应与不再被其他分支引用的导入原文。
已有独立分支保留自己的 canonical history；仍供分支使用的共享导入 provenance 也保留。
此操作只删除数据库会话，不删除执行机器上的工作文件或原始 Desktop rollout 文件。

内容为空的永久删除操作记录用于拒绝旧进程继续写入该 thread ID；v1 snapshot、v2 delta、
导入及 App Server 恢复均不可复活该会话，持久化写入返回 410 `thread_deleted`。
store version 前进，其余会话的事件和历史保持可读取、可重建。普通工具执行和 runtime 的
history delete 不获得管理员擦除能力。
