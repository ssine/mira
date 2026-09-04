# Codex Remote Thread Store Adapter Protocol v2

状态：PoC 已实现。HTTP JSON；ThreadStore 请求使用带 `store:read` / `store:write` scope 的
客户端 Bearer token。管理员和 Node 认证不复用这个凭证，见 [auth-v1.md](./auth-v1.md)。

## 1. 目标与不变量

- PostgreSQL 中的 `codex_store_events` 与 `codex_thread_events` 是唯一持久化事实来源。
- Codex 仍读写官方 `ThreadStore` 数据结构；adapter 只把一次本地状态变化编码成细粒度提交。
- store head 是单调递增的 `uint64`。一次有效提交恰好产生一个新版本。
- rollout item 原始 JSON 不做业务格式转换。新 Codex 字段可以原样持久化、回放。
- thread 历史在一个 generation 内不可变、只追加；重写历史必须创建新 generation。
- snapshot 和 thread projection 都是缓存，可从权威事件重建。
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

一个 Codex 进程内部对同一 store 的 mutation 串行化；Server 通过
`codex_store_heads` 的行锁串行提交。`turn/completed` 是模型生命周期事件，不等于最后一批
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
GET /v1/codex/threads/{threadId}/transcript?storeId=personal
```

Server 在同一个 store head 上读取 active generation，并从权威 rollout items 重建有序的
`user`、`assistant`、`reasoning`、`tool` 和 `system` 轨迹。投影会配对原始
`custom_tool_call` / `custom_tool_call_output`，也能读取 paginated rollout 的
`item_completed` materialized items。每条轨迹保留稳定 key、turn ID、源 item 序号、状态以及
是否应按 Markdown 展示。

这个接口不是新的事实来源，也不修改或替代 App Server 协议。它只解决不同 Codex 历史模式在
`thread/resume` 中可能返回不完整展示 items 的问题；继续对话仍由官方 App Server 完成。投影可以
在任何时候从 `codex_thread_events` 重建，未知原始字段继续完整保存在数据库。为避免单个工具输出
拖垮浏览器，每条投影正文最多 1 MiB，截断只发生在 Web 响应中。
