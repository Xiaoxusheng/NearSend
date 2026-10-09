# AGENTS.md · 本仓库的协作约定

面向所有在本仓库中修改代码的人（也包括 AI 助手）。**动手之前先读完这一页。**

---

## 1. 这个项目是什么

局域网文件快传服务。一台机器跑 Go 服务，其它设备用浏览器连接它收发文件。

架构是**中转模型**（浏览器无法接受入站连接，也无法扫描局域网）：

```
发送方浏览器 --分块 PUT--> 服务端临时目录 --校验+原子提交--> 接收目录 --授权 GET--> 接收方浏览器
```

---

## 2. 不可协商的约束

改动任何代码前，请确认不违反以下约定。这些不是风格偏好，而是项目的正确性基础。

### 2.1 绝不说假话

- **不伪造设备。** 设备列表只能来自真实的 WebSocket 连接。不要为了「看起来更丰满」而
  编造设备、模拟自动发现效果。
- **不伪造进度。** 进度、速度、已传字节都必须来自真实计数。数据不足时显示 `—`，
  不要填一个「看起来合理」的估算值。速度静止 2 秒后归零，不保留虚高数值。
- **不伪造历史。** 不要为演示目的预置任何记录。
- **不伪造成功。** 只有分块全部到达 **且** 整文件 SHA-256 比对通过，任务才可以是 `completed`。
  任何失败分支都不允许把任务标记为成功。
- **不伪造能力。** 未实现 TLS 就不要暗示有加密；不支持续传的任务要明确说明原因，
  让前端隐藏续传入口，而不是点了才失败。
- **如实记录未完成的测试。** 没跑过跨设备验证就不要写「已完成跨设备验证」。

### 2.2 不留死按钮

- 前端不得存在「点了没反应」或「永远禁用且不解释原因」的控件。
- 禁用态必须说明**缺什么条件**（例如「还差：尚未选择文件」）。
- 弹窗若要求用户二选一（接受/拒绝），就不要渲染无意义的关闭按钮。

### 2.3 安全由后端负责

- 前端隐藏按钮只是体验优化，**不构成安全边界**。所有权限判定必须在服务端完成。
- 判断顺序固定：**鉴权 → 任务参与者校验 → 状态校验 → 参数校验**。
  顺序反了会出现「第三方能探测到任务是否存在」这类信息泄露。
- 令牌、管理令牌、绝对路径、SQL 语句、堆栈**绝不进入响应体**；只写日志。
- 下载不能用「把会话令牌放进 URL」的方式实现——用短期票据（见 `api.go` 的 `ticketStore`）。

### 2.4 路径与文件安全

- 所有文件名必须过 `fsutil.SanitizeName` / `SanitizeRelPath`。
- 所有落盘路径必须过 `fsutil.SafeJoin`，它同时检查前缀与符号链接。
- 先写临时文件，完整校验通过后再 `fsutil.AtomicCommit`。
- 默认**绝不静默覆盖**已有文件。`rename` 是默认策略。
- 清理逻辑只允许动临时目录。**任何情况下都不能删除接收目录中的用户文件。**

### 2.5 不要把整个文件读进内存

分块上传是内存中的（单块上限 64 MiB，属可控范围），但：
- 整文件哈希必须流式（`fsutil.HashFile`）；
- 下载必须走 `http.ServeContent`（支持 Range、不缓冲）；
- 目录遍历、复制都要有上限。

### 2.6 并发要有边界

- 传输槽位（`Engine.slots`）限制同时处于 `uploading` 的任务数。
- **`uploading` 之外的任何状态都必须拒绝分块写入**，否则排队任务可以绕过并发限制。
- 进度推送必须节流（当前 350ms）。
- 不允许无上限地起 goroutine。

---

## 3. 目录职责（改代码前先找对地方）

| 目录 | 职责 | 不该做的事 |
| --- | --- | --- |
| `cmd/nearsend` | 启动、参数、优雅关闭、面向用户的启动/端口错误提示 | 不写业务逻辑 |
| `internal/api` | 路由、鉴权中间件、REST 处理、静态托管 | 不写文件 I/O 细节 |
| `internal/transfer` | 传输引擎：状态机、分块、续传、校验、槽位、清理 | 不碰 HTTP、不碰 SQL |
| `internal/store` | SQLite 访问与迁移 | 不写业务规则 |
| `internal/fsutil` | 文件名/路径安全、原子提交、哈希、磁盘探测 | 不知道任务是什么 |
| `internal/hub` | WebSocket 连接生命周期 | 不碰数据库（令牌解析靠回调注入） |
| `internal/protocol` | 事件名与错误码常量 | 前后端唯一真相，改这里必须同步前端 |
| `internal/netinfo` | 网络枚举与诊断事实 | 不下「确定结论」，只输出可验证的事实 |
| `internal/config` | 默认值、校验、热更新、管理令牌 | 不写传输逻辑 |
| `web/src/lib` | API 客户端、WS 客户端、哈希、上传引擎 | 不放 UI |
| `web/src/store` | Zustand 状态与业务编排 | 不放样式 |
| `web/src/pages` | 页面 | 不复用逻辑应下沉到 components |

**新增文件前先确认它不属于已有目录的职责范围。**

---

## 4. 前后端协议同步

修改以下任一项时，必须**同时**修改另一侧：

| 后端 | 前端 |
| --- | --- |
| `internal/protocol/protocol.go` 的事件名 | `web/src/lib/ws.ts` 的 `WS_EVENTS` |
| `internal/protocol/protocol.go` 的错误码 | `web/src/lib/types.ts` 的 `ErrorCode` + `store/notify.ts` 的 `explainErrorCode` |
| `internal/model/model.go` 的 json tag | `web/src/lib/types.ts` |
| 新增接口 | `web/src/lib/api.ts` |

**错误码必须在前端 `explainErrorCode` 里有对应文案**，否则用户只会看到默认的兜底提示。
这是「可执行的下一步」这条产品要求的落点。

---

## 5. 开发流程

```bash
# 后端
go build ./...
go vet ./...
gofmt -l ./internal ./cmd      # 必须无输出
go test ./... -count=1

# 前端
cd web
npx tsc -b                     # 必须无错误（strict + noUnusedLocals）
npm run build
npm run dev                    # 开发模式，/api 与 /ws 代理到 8787
```

### 改动后必须做的验证

| 改了什么 | 至少验证 |
| --- | --- |
| 传输引擎 | `go test ./internal/transfer -count=1` |
| HTTP / 鉴权 / 事件 | `go test ./internal/api -count=1` |
| 文件路径相关 | `go test ./internal/fsutil -count=1` |
| 任意端到端行为 | `python scripts/e2e_test.py http://127.0.0.1:8787` |
| 任意 UI | `node scripts/ui_check.mjs`（需先启动服务，见 README） |

UI 巡检脚本会驱动真实 Edge 完成一次真实传输并逐页截图，
比肉眼检查快且能发现 2px 级别的溢出问题。**提交前请确保它是 0 问题。**

---

## 6. 已经踩过的坑（不要重犯）

这些是开发过程中真实遇到并修复的问题，改动相关代码时请留意。

1. **`transfer.queued` 事件的双重语义。**
   该事件既表示「进入排队」也表示「已获得槽位可以开始上传」。后端用
   `waiting=true` / `granted=true` 区分。前端**不能**要求本地状态已经变成
   `uploading` 才启动上传——事件到达时本地状态往往还停在 `queued`，
   会导致上传永远不启动。上传器自己会向服务端确认分块状态，先启动再校验是安全的。

2. **并发槽位泄漏。**
   任何把任务状态改出 `uploading` 的路径都必须归还槽位。`RecoverOnStart` 尤其要注意
   （进程重启后内存记账已失效，需 `resetSlots()`）。`tryDispatch` 里对「在册但状态不符」
   的槽位做了自愈，这是有意为之，不要删。

3. **僵死任务吃光槽位。**
   发送方关闭浏览器后任务会永远停在 `uploading`。`reapStaleTasks` 依据 `updated_at`
   （每次分块落盘都会刷新，是真实存活信号）10 分钟无进展即回收。**不要回收 `queued`**——
   排队任务等待是正常状态且不占槽位。

4. **错误响应前必须丢弃请求体。**
   分块请求体可达数十 MiB，而失败判定发生在读 body 之前。直接写响应会让客户端
   在发送中途收到连接重置，浏览器只能显示「网络错误」。见 `handlers_transfer.go`。

5. **未知 `/api/*` 路径必须 404。**
   不能让 SPA 回退把接口路径也返回 index.html——调用方会拿到 HTML 并报 JSON 解析错误，
   而不是「接口不存在」这个明确结论。

6. **`crypto.subtle` 在明文 HTTP 下不可用。**
   局域网访问通常是 `http://192.168.x.x`，非安全上下文，`crypto.subtle` 为 `undefined`。
   若只依赖它，分块哈希校验会在真实部署场景静默失效。因此有三级降级：
   Web Crypto → Web Worker 池 + 纯 JS → 主线程纯 JS（分片让出）。
   Worker 文件名带哈希、通过 `worker-src 'self' blob:` 放行。

7. **CSRF/CSP 与 Worker。**
   CSP 的 `script-src` 需要 `blob:`，否则 Worker 被拦截并静默降级。见 `api.go`。

8. **`http.ServeContent` 与手动 Content-Length。**
   已显式设置 `Content-Length` 与 `Accept-Ranges`；下载要统计真实写出字节数，
   只有整份完整送出才登记「已下载」，分段下载不算。

9. **同名设备无法区分。**
   同一台机器上用不同浏览器打开会得到同样的主机名。`dedupeDeviceName` 在重名时
   追加 `(浏览器)` / `(系统)`。设备身份始终靠随机会话 ID，名称只是展示。

10. **重启不得把未完成任务标成成功。**
    `RecoverOnStart` 把 `uploading`/`queued`/`verifying` 标为 `failed` 并保留
    `resumable=true`，等发送方决定是否重试。`completed` 与 `awaiting` 不动。

11. **测试里的「第三方」必须真的非特权。**
    从 `127.0.0.1` 访问的会话被视为主机自身（有管理权限），
    用它测越权会假通过。`scripts/e2e_test.py` 通过局域网 IP 建第二个会话来测。

12. **本环境有系统代理/TUN。**
    本机测试脚本要显式禁用代理（Go 的 `ProxyFromEnvironment`、Python 的
    `ProxyHandler({})`），并**复用 TCP 连接**（浏览器行为），否则会出现与环境有关的
    偶发 `ConnectionResetError`，误判成服务端 bug。

---

## 7. 代码风格

**Go**

- 中文注释，解释**为什么**而不是**是什么**。安全相关的判断必须写明拒绝的理由。
- 错误码用 `internal/protocol` 的常量，不要散落字符串字面量。
- 新增对外函数需要文档注释；`gofmt` 必须通过。
- 事务性写入放进 `store` 的一个方法里，不要在处理函数里拼多条 SQL。

**TypeScript / React**

- `strict` + `noUnusedLocals` + `noUnusedParameters` 已开启，`npx tsc -b` 必须干净。
- 样式只用 `src/styles/tokens.css` 里的 CSS 变量，**不硬编码颜色**。
- 不使用 Emoji 冒充 UI 图标，统一用 `@ant-design/icons`。
- 长驻的副作用要清理；定时器要 `clearInterval`；事件监听要 `removeEventListener`。
- 在 store 里访问另一个 store 用 `getState()`，注意避免循环 import
  （现有做法：`store/identity.ts` 打破循环，`setConfigSnapshotReader` 注入读取器）。

---

## 8. 提交前自查清单

- [ ] `gofmt -l ./internal ./cmd` 无输出，`go vet ./...` 干净，`go test ./... -count=1` 全绿
- [ ] `cd web && npx tsc -b && npm run build` 成功
- [ ] `python scripts/e2e_test.py <url>` 全部通过
- [ ] `node scripts/ui_check.mjs` 0 问题
- [ ] 没有新增死按钮；新增的禁用态都说明了原因
- [ ] 新增/修改的错误码已在 `explainErrorCode` 中有可执行文案
- [ ] 没有把 token / 绝对路径 / 堆栈写进响应体
- [ ] 没有伪造任何设备、进度、记录或成功状态
- [ ] 文档（README / docs）与实际行为一致
