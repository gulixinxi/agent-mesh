# 🌐 Agent Mesh 企业本地 AI 协作中枢 (Monorepo)

企业级、本地优先的跨设备 AI 协作与调度平台。把每台员工电脑变成可定向调度的 AI 节点：任务点对点送达，文件与数据默认留在公司内网，不经公有云中转。

## 🏗️ 仓库结构（Monorepo）

```
agent-mesh/
├── agent-mesh-server/      # 中央汇总中枢（Go + Gin + SQLite）
│   ├── main.go             # 入口，默认 :8080（含清理与超时回收调度）
│   ├── api/device.go       # 心跳上报 / 设备列表
│   ├── api/task.go         # 审计日志上报 / 查询
│   ├── api/dispatch.go     # 下行任务通道（下发 / 领取 / 回传 / 列表）
│   ├── api/console.go      # 控制台读接口与控制台下发
│   ├── api/auth.go         # HMAC 签名鉴权中间件
│   ├── web/                # //go:embed 内嵌的控制台单页
│   └── store/database.go   # SQLite 初始化、轻量迁移、清理与超时回收
├── agent-mesh-client/      # 节点常驻客户端（Go + libp2p + MCP）
│   ├── main.go             # 入口；-mcp 切纯 MCP stdio 模式
│   ├── config/             # 配置与环境变量覆盖
│   ├── core/               # 引擎 / 类型 / P2P 文件传输 / MCP 处理器
│   └── adapters/           # AI 工具适配器（零侵入扩展点）
├── run.ps1                 # Windows 一键编译 + 双端联调
└── deploy.sh               # 拉代码 + 编译 + 运行（PAT 走环境变量）
```

## 📡 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查（含 DB Ping） |
| POST | `/api/v1/cluster/heartbeat` | 节点心跳上报（含 agents 能力清单） |
| GET | `/api/v1/cluster/devices` | 在线设备拓扑（30s 未心跳判离线） |
| POST | `/api/v1/audit/report` | 审计日志上报（UPSERT 幂等） |
| GET | `/api/v1/audit/logs` | 审计日志查询（支持 `?limit=&node=`） |
| POST | `/api/v1/tasks/create` | 下发任务（可带 `target_node` / `target_agent_kind` / `max_attempts`） |
| GET | `/api/v1/tasks/pending` | 领取待执行任务（原子领取，返回 `claim_token`） |
| POST | `/api/v1/tasks/result` | 回传结果（`completed` / `failed` / `timeout`，须带 `claim_token`） |
| GET | `/api/v1/tasks` | 任务流水（含 `attempts` / `max_attempts` / `claimed_by`） |
| GET | `/console` | 中央控制台页面（Basic Auth：`CONSOLE_USER` / `CONSOLE_PASS`） |

### 任务可靠性语义

节点领走任务后可能宕机、可能执行卡死，所以任务表带四个可靠性字段：

| 字段 | 作用 |
|---|---|
| `attempts` / `max_attempts` | 已领取次数与上限（默认 3），超出即判死，不会无限重投 |
| `timeout_at` | 本次领取的到期时刻；服务端每 30 秒扫一次，超时未回传就回收 |
| `claim_token` | 本次领取的认领凭据；重投后换发新凭据，旧凭据作废 |

两条回收路径：

- **节点主动放弃**：执行超过 3 分钟时，客户端回传 `status=timeout`，服务端按剩余次数决定重投还是判死。
- **节点彻底宕机**：没有任何回传，靠服务端 `ReapTimedOutTasks()` 依据 `timeout_at` 回收。

迟到的旧节点拿旧 `claim_token` 回传会被拒（409），避免迟到的结果覆盖新一轮的执行结果。

服务端 `-task-timeout`（默认 5 分钟）必须大于客户端 3 分钟的执行超时，否则正在正常执行的任务会被误判超时、重投给别的节点，导致同一条指令被执行两遍。启动时若检测到小于 3 分钟会强制回落为 5 分钟。

## 🚀 快速开始

```bash
# 服务端
cd agent-mesh-server && go build -o ../bin/mesh-server ./... && ../bin/mesh-server

# 客户端（另开终端）
cd agent-mesh-client && go build -o ../bin/mesh-client ./... 
../bin/mesh-client -server http://127.0.0.1:8080 -id NODE-01
```

Windows 用户直接跑根目录的 `run.ps1`，一步完成双端编译与联调。

客户端环境变量覆盖：`AGENT_MESH_SERVER` / `AGENT_MESH_CLIENT_ID` / `AGENT_MESH_P2P_PORT` / `AGENT_MESH_DL_DIR` / `AGENT_MESH_DOUBAO_DB`。

## 🛠️ AI Agent 远程开发规范（工程边界）

参与本项目的 AI 开发 Agent 必须严格遵守以下边界：

1. **协议依从度**
   - 心跳与审计日志交换严格遵循 `core/types.go` 的 `TaskPayload` 结构。
   - 客户端对外开放的 MCP 协议必须符合 **MCP 2024-11-05 / JSON-RPC 2.0**。
2. **零侵入适配器模式**
   - 严禁在主引擎逻辑中直接写针对特定 AI 工具（豆包、Ollama 等）的定制代码。
   - 新增工具支持必须在 `adapters/` 下实现 `AIAdapter` 接口后注册。
3. **数据不出内网与安全隔离**
   - P2P 传输基于 go-libp2p 加密流，尾部 SHA-256 校验文件完整性。
   - 监听第三方 SQLite（如豆包本地库）必须采用 **Copy-on-Read 只读快照**，避免锁库影响官方软件。

## ✅ 已验证状态

双端 `go build ./...` 通过，并已完成端到端真机联调：心跳上报 200、设备列表返回真实内网 IP 与 `agents[].runnable` 能力位、豆包适配器通过 Copy-on-Read 快照扫库并成功上报中央审计日志。

后续几轮通过的验证：

- HMAC 鉴权 7/7（含未来时间戳、body 篡改、nonce 重放三类攻击用例）
- 文件名穿越收敛 12/12、peer 过期清理、留存策略 4/4
- MCP stdio 真机 7/7（`mesh.status` / `mesh.agents` / `mesh.sendfile`）
- 控制台 7/7（未认证 401、页面 200、概览、设备拓扑、下发、任务流水、审计流水）
- 任务可靠性：超时回收、认领凭据失效、重复回传拒绝、重试耗尽判死（`go test ./api/ ./store/`）
- 任务闭环端到端：下发 → 领取 → mock Ollama 执行 → 回传 `completed`（`attempts=1`）

## 🔧 依赖说明

- **SQLite 驱动选 `modernc.org/sqlite`（纯 Go）**，不用 `mattn/go-sqlite3`——后者依赖 cgo，Windows 需 MinGW。
- **libp2p 锁 v0.33.2**：0.34+ 把 mDNS 移出主仓，且 `Noise()`/`Yamux()` 已移出根包，`libp2p.New()` 默认配置即包含安全通道与多路复用。
