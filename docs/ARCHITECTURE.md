# 架构说明

本文说明「局域网快传」的核心数据流、状态机与关键设计取舍。
配套阅读：[安全说明](./SECURITY.md)、[关键决策记录](./DECISIONS.md)。

---

## 1. 为什么是「中转」而不是「点对点」

任务书希望设备之间直接互传。但现实约束是：

| 约束 | 后果 |
| --- | --- |
| 浏览器无法监听端口 | 接收方无法被动接收连接 |
| 浏览器无法扫描局域网 | 无法自动发现设备 |
| `crypto.subtle` 在明文 HTTP 下不可用 | 安全上下文限制了部分 Web API |
| WebRTC DataChannel 需要信令服务器与 ICE | 仍需要一个会合点，且局域网无 STUN 时需手工交换 SDP |

因此本项目的模型是：**一台机器跑 Go 服务作为会合点与存储中转**，
其它设备用浏览器连上去，由服务端负责存储、校验与分发。

数据流：

```
发送方浏览器
   │  ① POST /api/transfers            创建任务（元数据）
   │  ② 等待接收方确认（WS: transfer.requested → accepted）
   │  ③ 获得传输槽位（WS: transfer.queued, granted=true）
   │  ④ GET  /api/transfers/:id/chunks   取缺失分块清单
   │  ⑤ POST .../chunks/:index           逐块上传（每块声明 SHA-256）
   ▼
服务端
   ├─ 临时目录 <tempDir>/transfers/<taskId>/<fileId>.part
   │    · 每块先校验 SHA-256 再按偏移写入（校验不过的字节永不落盘）
   │    · 记录分块位图 → 断点续传的依据
   ├─ 收齐后：流式计算整文件 SHA-256 → 与声明值比对
   └─ 原子提交到 <receiveDir>/<日期>/<发送方>/<相对路径>
   ▼
接收方浏览器
   │  ⑥ POST .../files/:index/ticket      签发 120 秒票据
   └  ⑦ GET  /api/download?ticket=...     原生下载（支持 Range）
```

**代价（如实说明）**：文件要先完整存到服务端所在机器，接收方才下载，
因此该机器需要足够磁盘空间。这是浏览器环境下唯一不需要安装客户端就能双向传文件的方案。

---

## 2. 任务状态机

```
                  ┌──────────── 拒绝 ────────────▶ rejected（终态）
                  │
  awaiting ──接受──▶ queued ──获得槽位──▶ uploading ──全部校验通过──▶ completed（终态）
     ▲                ▲   │                  │
     │                │   └── 暂停 ──▶ paused ──继续──┘
     │                │                      │
     └── 重试（被拒）──┘                      ├── 校验失败/写盘失败 ──▶ failed（终态，可续传重试）
                                             ├── 取消 ──▶ cancelled（终态，保留分块可续传）
                                             └── 10 分钟无进展被回收 ──▶ failed
```

关键规则：

- **只有 `uploading` 允许写入分块。** `queued` 会被拒绝（`too_many_concurrent`），
  否则排队任务可以绕过并发上限。`paused` 也会被拒绝，让「暂停」具备真实语义。
- **终态只能由校验结果决定。** `completed` 的唯一入口是 `completeTask`，
  它要求所有文件状态为 `ready`/`downloaded`/`skipped`，而 `ready` 只能由
  `finalizeFile` 在校验通过后设置。
- **服务重启不会把未完成任务变成成功。** `RecoverOnStart` 把
  `uploading`/`queued`/`verifying` 标为 `failed` 且 `resumable=true`；
  `completed` 与 `awaiting` 保持不变。

---

## 3. 传输槽位与排队

`Engine.slots` 是一个容量为 `maxConcurrentTransfers` 的带缓冲 channel，充当信号量。

- `tryDispatch(id)`：有空位则授予并置为 `uploading`，否则进等待队列。
- `releaseIfGranted(id)`：归还槽位并调度队首任务。
- 授予时通过 WS 发 `transfer.queued { granted: true }` 通知发送方开始上传；
  仅排队时发 `transfer.queued { waiting: true }`。

**两道防护，缺一不可：**

1. 服务端只接受 `uploading` 状态的分块写入；
2. 前端只在收到 `granted: true` 时才启动上传器。

### 槽位泄漏与僵死任务

两个真实存在的问题及处理：

| 问题 | 处理 |
| --- | --- |
| 状态被改出 `uploading` 但没有归还槽位 | `tryDispatch` 检测「在册但状态不符」并自愈；`RecoverOnStart` 调用 `resetSlots()` |
| 发送方消失，任务永远停在 `uploading` | `reapStaleTasks` 依据 `updated_at`（每次分块落盘都会刷新）超过 10 分钟即回收为 `failed`，并释放槽位 |

`queued` 任务不会被回收：等待是其正常状态，且不占槽位。

---

## 4. 分块与断点续传

### 分块划分

- `chunkSize` 默认 8 MiB，可配置 256 KiB ~ 64 MiB。
- 文件第 `i` 块的期望长度 = `min(chunkSize, size - i*chunkSize)`。
- 空文件 `chunkCount = 0`，在接收确认时直接走「创建空临时文件 → 原子提交」。
- 分块索引与长度都在服务端校验，超长请求体（多 1 字节）即被拒绝。

### 写入路径

```
读取 body（上限 = 期望长度 + 1，用于检测超长）
  → SHA-256 校验（与 X-Chunk-SHA256 声明值比对）
  → WriteAt(part, offset)          # 校验不过的字节永不落盘
  → fsync
  → MarkChunk()（幂等：重复提交不会重复计数）
  → 若该文件分块收齐 → finalizeFile()
```

**幂等性**：`MarkChunk` 先 `SELECT COUNT(*)` 判断是否已存在，再 UPSERT；
返回 `duplicate` 标记。重复提交不会改变 `done_size`（它由 `SUM(chunk.size)` 重算）。

### 续传判定

`GET /api/transfers/:id/chunks` 返回每个文件的 `missing[]` 与**源文件指纹**
（`size` + `srcModTime`）。前端上传器在开始前比对本地 `File`：

- 一致 → 只上传 `missing[]`；
- 不一致 → 先 `POST .../reset` 清空该文件进度，再整体重传。

这条检查不可省略：源文件被改过却续传，会产生由两份不同内容拼起来的损坏文件。

### 完成判定

```
接收目录与最终文件名解析（按重名策略）
  → 校验临时文件大小 == 声明大小
  → 流式 SHA-256（含与声明值比对）
  → SafeJoin 校验目标路径 → AtomicCommit（同目录 rename）
  → 文件状态置为 ready
全部文件 ready/skipped → completeTask() → 任务 completed + verified
```

`AtomicCommit` 在 Windows 上若目标已存在且允许覆盖会先删除再 rename；
跨卷时退化为「复制 + fsync + 删除」。若目标为目录，一律拒绝。

---

## 5. 数据集与持久化

### SQLite 表

| 表 | 内容 |
| --- | --- |
| `meta` | schema 版本 |
| `devices` | 设备与令牌（令牌持久化，刷新页面不掉线） |
| `transfers` | 任务主表（状态、时间、校验结论、续传能力、临时入口来源） |
| `files` | 任务内文件（路径、大小、哈希、分块计数、最终名） |
| `chunks` | 已落盘分块（`(file_id, idx)` 主键，兼作位图） |
| `dropboxes` | 临时入口（令牌、有效期、真实收件统计） |
| `app_settings` | 配置（单行 JSON） |

迁移是**追加式**的：`migrations` 数组的第 `i` 项目标版本为 `i+1`，
已发布的语句不允许修改。新增变更只能往数组末尾追加。

### 连接策略

`SetMaxOpenConns(1)` + WAL + `busy_timeout(10000)`。

所有数据库操作都是微秒级的短事务，串行化换来了「绝不出现 `SQLITE_BUSY`」的确定性。
真正的长 I/O（分块读写、下载流式响应）全部在连接之外完成，不受此限制。

### 临时数据与清理

| 状态 | 临时数据 | 清理时机 |
| --- | --- | --- |
| `uploading` / `queued` / `paused` / `awaiting` | 保留 | **永不清理** |
| `completed` | 已提交，只剩空目录 | 立即回收 |
| `failed` / `cancelled` / `rejected` | 保留以支持续传 | 超过 `tempRetentionHours` 后清理 |
| 无对应任务记录 | 孤儿目录 | 超过保留期后清理 |

`CleanupTemp` 在数据库异常时保守跳过（宁可留下临时文件也不误删）。
`historyRetentionDays` 只影响记录可见性，**从不删除磁盘文件**。

---

## 6. 实时通信

### 鉴权

浏览器无法为 WebSocket 设置自定义请求头，把令牌放进 URL 又会进入访问日志。
因此采用**连接后握手**：客户端必须在 8 秒内发送 `client.hello` 携带令牌，
否则连接被关闭。令牌从不进入 URL。

### 连接管理

- 每设备可有多条连接（多标签页），全部断开后才广播 `device.offline`。
- 服务端每 25 秒 ping，客户端 60 秒内需有读取活动，否则关闭。
- 发送队列满（64 条）即断开慢客户端，避免拖垮服务。
- `CheckOrigin` 比对 Host，不使用通配。

### 重连与状态同步

客户端指数退避重连（上限 10 秒 + 抖动）。**重连成功后主动重新拉取**
设备、任务、配置与地址——绝不依赖内存中的旧数据。
页面从后台恢复可见时也会重连并刷新。

### 事件节流

`transfer.progress` 最快每 350ms 一条；文件数超过 40 时不带文件明细，
避免高频事件体积膨胀。文件级完成、状态变更等关键节点强制立即推送。

---

## 7. 前端数据流

```
lib/api.ts        REST 客户端：统一错误为 ApiError(带稳定 code)
lib/ws.ts         WebSocket：握手、重连、事件常量
lib/uploader.ts   分块上传引擎：并发、暂停、取消、续传、重试、哈希
        │
        ▼
store/session.ts  启动编排：健康检查 → 会话 → 初始数据 → 建 WS → 分发事件
store/devices.ts  设备列表与在线状态
store/transfers.ts 任务、乐观进度、上传器生命周期、错误文案
store/config.ts   公开配置与完整设置
store/identity.ts 当前设备身份（打破 store 循环依赖）
store/theme.ts    主题偏好与 <html data-theme>
store/notify.ts   提示桥接 + explainErrorCode（错误码 → 可执行下一步）
```

### 进度显示的两个来源

- **本地乐观进度**（`localDone`）：分块上传是本地驱动的，先给即时反馈；
- **服务端权威进度**（WS `transfer.progress`）：到达后覆盖。

显示时取两者的**最大值**，避免权威值短暂落后于本地值造成进度回退闪烁。

### 为什么上传器在 store 里创建

上传器持有 `File` 对象引用，其生命周期必须与「本次页面会话」绑定。
页面刷新后 `File` 失效，此时**不做任何假装续传的展示**，而是明确提示重新选择文件。

---

## 8. 已知的架构级限制

1. **浏览器内存中的源文件**：刷新即失效，无法跨刷新续传（已计划用 IndexedDB + 文件指纹改善）。
2. **中转需要额外磁盘**：服务端磁盘占用约为传输总量。
3. **无原生设备发现**：发现协议与 Web 注册机制已解耦，便于后续接入独立守护进程。
4. **明文 HTTP**：不具备端到端加密。
5. **单进程**：SQLite 与内存槽位都在单进程内，水平扩展需要换成外部协调。
