# 🌐 Agent Mesh 企业本地 AI 协作中枢 (Monorepo)

企业级、本地优先的跨设备 AI 协作与调度平台。把每台员工电脑变成可定向调度的 AI 节点：任务点对点送达，文件与数据默认留在公司内网，不经公有云中转。

## 🏗️ 仓库结构（Monorepo）

```
agent-mesh/
├── agent-mesh-server/      # 中央汇总中枢（Go + Gin + SQLite）
│   ├── main.go             # 入口，默认 :8080
│   ├── api/device.go       # 心跳上报 / 设备列表
│   ├── api/task.go         # 审计日志上报 / 查询
│   └── store/database.go   # SQLite 初始化与轻量迁移
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

## 🔧 依赖说明

- **SQLite 驱动选 `modernc.org/sqlite`（纯 Go）**，不用 `mattn/go-sqlite3`——后者依赖 cgo，Windows 需 MinGW。
- **libp2p 锁 v0.33.2**：0.34+ 把 mDNS 移出主仓，且 `Noise()`/`Yamux()` 已移出根包，`libp2p.New()` 默认配置即包含安全通道与多路复用。
