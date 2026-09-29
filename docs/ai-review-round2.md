# Agent Mesh 第二轮代码审查请求（交给远程 AI 协作者）

> 生成时间：2026-09-29
> 对应提交：`a188259`（MCP 工具集 + 两端清理策略）、`267f4e0`（中央控制台 Web UI）
> 上一轮审查任务书见 `docs/ai-review-prompt.md`，本轮只审**增量**。

## 用法（重要）

**本仓库当前是公开的（Public）**，远程 AI 可直接读取。已实测：匿名 `git ls-remote` 无需凭据即可读到 HEAD = `267f4e0`。

因此把下面整份文档直接发给远程 AI，并附这一句：

> 仓库地址 `https://github.com/gulixinxi/agent-mesh`，请**把代码固定在提交 `267f4e0` 上审阅**，不要按 main 的最新状态漂移。本轮只看这个提交与前两个提交引入的增量。

### ⚠️ 隐私含义（发给外部 AI 前请知悉）

| 事项 | 说明 |
|---|---|
| 外部 AI 能看到什么 | 全部源码、提交历史、commit message 里的**内网 IP、设备名、验证细节** |
| 看不到的（已确认） | 凭据类内容已清理，`git grep ghp_ / secret 明文` 全历史无命中；但**不要**再把任何真实密钥、真实客户数据写进仓库或 commit message |
| 时效性 | 一旦按计划把仓库改为 Private，**外部 AI 立即失去访问**。届时改用下方「粘贴模式」 |
| 建议顺序 | 本轮复审在公开状态下读完 → 拿到报告 → **立刻改私有** |

### 粘贴模式（仓库转私有后使用）

若届时仓库已设为私有，请把下方「待审文件清单」里的文件内容连同本文档一起发给它。
不要让它凭记忆作答 —— 它看不到增量代码时，极易复述上一轮的旧结论。

---

## 你的角色

你是本项目的远程 AI 评审员。本轮**不做功能设计**，只做**代码审计**：
指出缺陷、竞态、资源泄漏、性能问题、安全薄弱点，并给出最小化修补方案。

硬性要求：

- **先读代码再下结论**。上一轮你出现过事实性误判（详见文末「第一轮已定案事实」），本轮再出现同类问题会直接降低整份报告的可信度。
- 每条结论必须标注**文件名 + 函数名 + 行号**，不接受「建议加强某某方面」这种无落点的泛泛而谈。
- 只给 diff 片段，禁止整文件重写。
- **不要输出"该功能尚未实现"类结论**，除非你已经在待审文件里搜过对应符号并确认不存在。
- **只读，不要写**：不要向该仓库提交 PR、Issue、Commit 或任何改动。你在本轮的唯一产出是这份审查报告。
  所有修改由我方在本地落地后，再交给你复审。

---

## 项目背景（未变化部分）

- Go 双端 Monorepo：
  - `agent-mesh-server`：gin + SQLite 中央汇总中枢（心跳、设备拓扑、审计日志、任务分发、Web 控制台）
  - `agent-mesh-client`：libp2p P2P 节点 + MCP stdio 服务 + AI 工具适配器
- 定位：**企业内网 AI 节点调度平台，数据不出内网**
- 规模预期：单集群 **10 ～ 100 个节点**，不是互联网级规模。请据此权衡方案复杂度——
  过度工程（引入 etcd/Kafka/服务网格）在本项目里是**减分项**。

## 本轮增量概况

上一轮你提的 P0/P1/P2 中，落地了以下几项（均为我方实现 + 真机验证，非你的原样代码）：

| 你的原建议 | 落地方式 | 提交 |
|---|---|---|
| P0-1 HTTP 无鉴权 | HMAC-SHA256 中间件（签 method+path+ts+nonce+bodyHash，双向 300s 窗口，nonce 去重），只挂 `/api/v1` | `09fb954` |
| P0-2 P2P 无节点过滤 | PeerID 白名单 `AllowPeer`（`-p2p-allow`，非空即严格模式，未授权立即 Reset） | `09fb954` |
| P1-1 任务下发通道 | **HTTP 轮询**（客户端 5s 拉取），未采纳你的 P2P 反向直连，理由见文末 | `97760e9` |
| P1-2 MCP 工具太少 | `mesh.status` / `mesh.agents` / `mesh.sendfile` 三个工具 | `a188259` |
| P2-1 SQLite 膨胀 | 30 天滚动清理，启动 + 每 6h | `a188259` |
| P2-2 mDNS 无清理 | peer discovered 列表 TTL 3min + 60s GC | `a188259` |

本轮新增：

1. **Step 1 —— MCP 工具集**：`mesh.status`（节点与服务端连通性、P2P 已知节点数）、
   `mesh.agents`（本地适配器清单与 `runnable` 可执行位）、`mesh.sendfile`（对指定 PeerID 发文件）。
   走的是已有的 `RegisterTool` 动态注册机制，没有写死静态数组。
2. **Step 2 —— 两端清理策略**：服务端 `CleanupOldAuditLogs` / `CleanupFinishedTasks`（按 retention 滚动）；
   客户端 `peerEntry{lastSeen}` + `peerGC()`（60s 周期、TTL 3min）+ `Close()` 停 GC + `closeOnce` 防重入。
3. **Step 3 —— 中央控制台 Web UI**：`//go:embed` 内嵌单页控制台，`/console` 路由组，
   Basic Auth 保护（读 `CONSOLE_USER` / `CONSOLE_PASS`），提供 overview / devices / tasks / audit 四个读接口 + 控制台直接下发任务。

---

## 待审文件清单

请把以下文件全文粘贴给远程 AI（路径为本仓库相对路径）：

**Step 1 + 2（提交 `a188259`）**

| 文件 | 行数 | 说明 |
|---|---|---|
| `agent-mesh-client/core/mcp_handler.go` | 262 | MCP 协议处理 + 三个新工具注册 |
| `agent-mesh-client/core/p2p_transfer.go` | 386 | P2P 收发 + 白名单 + peer GC + 文件名净化 |
| `agent-mesh-client/core/engine.go` | 421 | 主引擎 + 任务轮询器 + 签名请求 |
| `agent-mesh-client/main.go` | — | 启动装配（SetEngine / SetP2P / -p2p-allow） |
| `agent-mesh-server/store/database.go` | — | 关注 `CleanupOldAuditLogs`、`CleanupFinishedTasks` |

**Step 3（提交 `267f4e0`）**

| 文件 | 行数 | 说明 |
|---|---|---|
| `agent-mesh-server/api/console.go` | 256 | 5 个控制台接口 + `startOfToday` / `queryLimit` |
| `agent-mesh-server/web/console.html` | 224 | 单页控制台（原生 JS，10s 自动刷新） |
| `agent-mesh-server/web/embed.go` | 10 | `//go:embed` 声明 |
| `agent-mesh-server/main.go` | — | 关注 55-80 行：清理调度 + `/console` 组挂载 |

**可选补充**（若它要追上下文）：`agent-mesh-server/api/dispatch.go`（任务状态机）、`agent-mesh-server/api/auth.go`（HMAC）。

---

## 工程边界（建议方案必须遵守）

1. 心跳与审计交换严格遵循 `core/types.go` 的 `TaskPayload` 结构
2. MCP 协议 = 2024-11-05 + JSON-RPC 2.0，stdio 传输
3. **stdio 模式下 stdout 被 JSON-RPC 独占**，任何调试日志必须走 stderr
4. 零侵入适配器：新增 AI 工具只能在 `adapters/` 实现 `AIAdapter` 后注册，禁止改主引擎注册逻辑
5. P2P 传输必须 libp2p 加密流 + SHA-256 尾部校验
6. 监听第三方 SQLite 必须 Copy-on-Read 只读快照
7. 依赖约束：SQLite 用 `modernc.org/sqlite`（纯 Go 免 cgo）；libp2p 锁 v0.33.2（0.34+ mDNS 移出主仓、Noise/Yamux 移出根包）
8. **放弃 spurious generality**：内网 10~100 节点规模，单机 SQLite 足够，不要引入分布式协调组件

---

## 请重点审查

### A. 控制台相关（最高优先级，这是全新代码）

1. **未设口令时控制台完全开放**：`main.go` 中 `CONSOLE_USER`/`CONSOLE_PASS` 为空时只打印警告、不加任何中间件，
   导致 `/console/api/*` 可被内网任意主机读取设备拓扑、审计日志，甚至下发任务。
   请评估：**应当 fail-closed（未配置就关闭路由或返回 503），还是保留 fail-open + 警告？**
   给出你的推荐并说明理由（注意本项目定位是"数据不出内网"）。
2. **Basic Auth 明文传输**：HTTP 明文下 Authorization 头是 base64 可逆编码。
   在内网场景是否可接受？若要加固，最小成本方案是什么（自签 TLS？绑定回环 + SSH 隧道？）
3. **`ConsoleCreateTask` 的注入与越权面**：它直接复用 `dispatch.CreateTask` 的写入路径。
   检查：目标节点是否校验存在性、payload 大小是否设限、是否存在可被构造大量 pending 任务拖垮节点的路径。
4. **CSRF 与 CORS**：控制台 POST 接口是否可能被跨站触发？Basic Auth 在跨站场景下浏览器是否会自动携带？
5. **前端安全性**：`console.html` 里所有动态渲染是否都过了 `esc()` 转义？有没有遗漏的内联取值点（尤其是 `innerHTML`、属性拼接、`setTimeout` 字符串求值）？
6. **`queryLimit` 的边界**：`limit` 参数是否有上界、非法值处理、`-1` 之类特殊值会不会被拼接进 SQL LIMIT 导致全表拉取。

### B. MCP 工具集

7. **`mesh.sendfile` 的入参校验**：目标 PeerID 解析失败、文件不存在、路径穿越、以及"未启用 P2P 时调用"这四种情况的错误返回是否符合 JSON-RPC 2.0 规范（是 `-32602` 参数错误还是 `-32603` 内部错误？请逐一核对）。
8. **MCP 工具执行是同步阻塞的**：`HandleMessage` 直接调用同步的 `SendFileToPeer`，stdio 主循环在大文件传输期间会完全卡住，期间 `initialize`/`ping` 都无法响应。
   请判断这个问题的实际严重程度，并给出最小改动方案（异步任务化 vs 明确标注为长阻塞操作）。
9. **工具 Schema 合规性**：三个工具的 inputSchema 是否符合 MCP 2024-11-05（required 字段标注、类型枚举合法性）。

### C. 清理与生命周期

10. **僵尸 running 任务**：`CleanupFinishedTasks` 只清理 `completed`/`failed`。若节点在执行中宕机，任务永远停在 `running`，既不会被回收也不会重投。
    请给出**超时认领/重投机制**的最小设计（需要哪些字段、SQL 怎么写、重投次数上限放哪）。这是我认为当前最实质的功能缺口。
11. **清理任务的并发安全**：`CleanupOldAuditLogs` / `CleanupFinishedTasks` 跑在同一 SQLite 连接上，和业务写入是否有锁竞争？WAL + busy_timeout 是否已足够？长时间 DELETE 会不会阻塞心跳写入。
12. **peer GC 的竞态**：`expirePeers()` 遍历 `sync.Map` 删除，`handleIncomingFileStream` 期间 peer 恰好过期会怎样？`isAllowed` 与删除之间是否有 TOCTOU。
13. **`Close()` 幂等与资源释放**：`closeOnce` 覆盖了哪些资源？libp2p host、goroutine、打开的文件句柄是否已全部释放，有没有漏。

### D. 其他

14. 上一轮 `auth.go` 的 **nonce 去重表**在内网攻击者于 300s 窗口内灌大量唯一 nonce 时会无界增长，请给出约束方案（容量上限？LRU？按时间分桶？）。
15. 其他你发现的 bug、竞态、资源泄漏、错误 swallowed（`_ =`）、或不规范之处。

---

## 第一轮已定案事实（不要重复讨论）

以下六点已在本轮之前核实过，请勿再作为新发现提出：

1. **`mcp_handler.go` 的 `toolList()` 是动态遍历**，不是写死静态数组 —— 上一轮称其为静态是误判。本轮继续保持动态注册。
2. **任务下发选 HTTP 轮询而非 P2P 反向直连**，理由：(a) 心跳上报字段不含 PeerID，你的原方案前提不成立；(b) server 引入完整 libp2p 依赖树运维成本高；(c) server 主动连 client 在 NAT/防火墙/休眠场景不可达。
   若你要反驳，请针对这三条给证据，不要重述原方案。
3. **HMAC 签名已覆盖 body 哈希 + nonce**，且时间窗是双向的（未来时间戳也会被拒）。
4. **签名用的 path 不含 query**（服务端用 `c.Request.URL.Path`），GET 请求不能把 query 拼进签名串。
5. **P2P 文件名穿越已修**（`filepath.Base` + `filepath.Rel` 兜底），单文件已有 2GiB 上限。
6. **"控制台能用 HMAC 鉴权"不成立** —— 浏览器拿不到集群密钥，无法逐一签名，这是设计选择不是遗漏。

---

## 输出格式

1. **分级清单**：P0（安全/数据泄露/崩溃）/ P1（功能缺口/资源泄漏）/ P2（规范与体验）。
   每条包含：`文件:行号` + 函数名 + 现状 + 危害 + 最小修改（diff 片段）。
2. **针对上文 A 组第 1 题**给出明确推荐（fail-closed 还是 fail-open），不要两边都列不做选择。
3. **针对上文第 10 题（僵尸 running 任务）**给出完整的最小数据表设计 + SQL + 状态机说明。
4. 最后给一句总结：**按你的判断，这批代码现在能不能上生产内网？** 以及最必须先改的三条。

---

## 附：我方已完成的验证（供你判断哪些区域已被覆盖）

不要因为这些已通过就跳过审查，但可以据此把精力放在未覆盖处。

- `TestSanitizeFileName` 12/12 通过
- `TestPeerExpiry` 通过
- HMAC 鉴权中间件 7/7（含未来时间戳拒绝、body 篡改拒绝、nonce 重放拒绝）
- 任务下发闭环：mock Ollama 端到端 下发 → 领取 → 执行 → 回传 completed
- MCP stdio 真机 7/7（tools/list 与 tools/call 结果一致）
- 留存策略 4/4（旧审计清、新审计留、旧完结任务清、**未完结任务留**）
- 控制台 7/7（无认证 401、带认证 200、overview、devices 含真实 IP、下发、任务流水、审计流水）

**已知未被测试覆盖**：并发下发、节点宕机场景、大文件传输期间 MCP 可用性、SQLite 锁竞争压测、CSRF。
