# 🌐 Agent Mesh 企业本地 AI 协作中枢 (Monorepo)

企业级、本地优先的跨设备 AI 协作与调度平台。把每台员工电脑变成可定向调度的 AI 节点：任务点对点送达，文件与数据默认留在公司内网，不经公有云中转。

## 🏗️ 仓库结构（Monorepo）

```
agent-mesh/
├── agent-mesh-server/      # 中央汇总中枢（Go + Gin + SQLite）
│   ├── main.go             # 入口；子命令：install / uninstall / gencert
│   ├── api/device.go       # 心跳上报 / 设备列表
│   ├── api/task.go         # 审计日志上报 / 查询
│   ├── api/dispatch.go     # 下行任务通道（下发 / 领取 / 回传 / 列表）
│   ├── api/console.go      # 控制台读接口与控制台下发
│   ├── api/auth.go         # HMAC 签名鉴权中间件
│   ├── api/files.go        # 文件中转（上传 / 列表 / 下载 / 删除）
│   ├── config/             # 配置加载（flag > 配置文件 > 环境变量 > 默认）
│   ├── internal/filelog/   # 日志接管与轮转（服务模式下必需）
│   ├── service_windows.go  # Windows 服务注册与运行（build tag 隔离）
│   ├── gencert.go          # 自签 CA 与服务端证书生成
│   ├── web/                # //go:embed 内嵌的控制台单页
│   ├── store/database.go   # SQLite 初始化、轻量迁移、清理与超时回收
│   └── store/files.go      # 中转文件元数据（实体在磁盘，库里只留指针）
├── agent-mesh-client/      # 节点常驻客户端（Go + libp2p + MCP）
│   ├── main.go             # 入口；-mcp 切纯 MCP stdio 模式；install / uninstall
│   ├── config/             # 配置加载
│   ├── core/               # 引擎 / 类型 / P2P 直传 / 文件中转 / MCP 处理器 / HTTPS 客户端
│   ├── internal/filelog/   # 日志接管与轮转
│   ├── service_windows.go  # Windows 服务注册与运行
│   └── adapters/           # AI 工具适配器（零侵入扩展点）
├── deploy/                 # 安装与卸载脚本（需管理员权限）
│   ├── install-server.ps1
│   ├── install-client.ps1
│   └── uninstall.ps1
├── verify/                 # 端到端验证装置（脚本入库，运行产物不入库）
├── run.ps1                 # Windows 一键编译 + 双端联调（开发用）
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

## 📦 部署到 Windows（服务模式）

开发调试继续看下面的「快速开始」；要装到员工机器上，用 `deploy/` 下的脚本。

### 安装服务端

```powershell
# 管理员 PowerShell
cd deploy
.\install-server.ps1 -Addr ":8080" -TLS

# 客户端将用别的地址访问时（云主机 / 反向隧道 / 异地办公），必须补 -CertHost，
# 否则证书 SAN 里没有那个身份，客户端 TLS 握手必然失败：
.\install-server.ps1 -Addr ":8443" -TLS -CertHost mesh.example.com -CertHost 1.2.3.4
```

脚本会做七件事：建目录、复制 exe、生成自签证书、**监听端口预检**、写配置、放行防火墙、
注册启动服务、**健康自检**。

未显式指定 `-Secret` / `-ConsolePass` 时会自动生成强随机值并在结尾打印一次，**务必保存**。

#### 为什么要有端口预检

Windows 上 Go 程序 bind 时被别的程序占了端口，**依然会"成功"**（`SO_REUSEADDR`），
请求随后被两个 Listening socket 随机分流。表现是：服务 `Running`、日志无任何报错、
控台怎么登都登不上去——因为你的请求有一半概率落在别人程序上，而你在对着它的登录框输我们的口令。

实测最常见的撞端口者是 **Everything**（内置 HTTP 服务默认就占 8080）。

所以安装脚本会先查谁占了端口，命中就中止并直接报出 PID 与进程名：

```
[冲突] 端口 8080 已被以下进程监听：
       PID 15172  Everything.exe
              C:\Program Files\Everything\Everything.exe
       建议换一个端口重装，当前空闲： 8081, 8082, 8083
```

确需共用端口时加 `-Force` 跳过预检。健康自检也不只看 HTTP 状态码——`/healthz` 固定返回
`{"status":"ok"}`，这个响应体被当作身份指纹，只看状态码的话别人的 HTTP 服务同样会返回 200。

#### 重装不需要先卸载，也不会把已接入的节点踢下线

同一条命令反复跑即可（换端口、换目录、升版本都一样）：

- 检测到旧版服务**装在其他目录**时会**接管**：停止旧实例 → 注销旧注册项 → 改指本次安装路径。
  不接管的话注册表里那条会一直指向旧路径，重启后先把旧版本拉起来，两套实例抢同一个端口；
- 集群密钥、控制台口令、TLS 证书一律**沿用**旧的。重新生成密钥会让所有节点瞬间鉴权失败，
  重签证书会让所有客户端 CA 校验失败——线下最贵的那类事故往往就出在"顺手换个新的"；
  确实要换就显式传 `-Secret` / `-ConsolePass`，证书加 `-RegenCert`；
- 装完最后一步打印**注册项校验**（自启动注册项指向哪个 exe），这一行才回答"重启后跑的是不是新版"。

详见 `docs/enrollment.md` §10。

#### 要让其他电脑一键装客户端

```powershell
.\install-server.ps1 -Addr ":8099" `
  -PublicUrl  "http://<对方能访问的地址>:8099" `
  -ClientPack "C:\Program Files\AgentMesh\Pack"     # 放 client-windows-amd64.exe 的目录
```

两者缺一不可：`-ClientPack` 让中枢能分发客户端，`-PublicUrl` 保证下发给对方的地址是真能连上的
那个（跨网段 / 端口映射 / 反向代理时，本机网卡 IP 对方根本到不了）。之后控制台签发邀请、
把一行动令发过去即可。跨网段怎么打通见 `docs/enrollment.md` §11。

### 安装客户端

```powershell
# 管理员 PowerShell，-ServerURL 指向服务端；HTTPS 时必须给 CA
.\install-client.ps1 -ServerURL "https://192.168.1.10:8080" `
                     -Secret "<服务端打印的密钥>" `
                     -CaPath "C:\ProgramData\AgentMesh\certs\ca.pem"
```

安装前会 TCP 探一次服务端地址是否可达。连不上不会中止安装，但会明确报警并给出排查顺序——
否则典型的失败形态是"服务 Running、一切正常"，实际节点在日志里无限重试。

同样，服务起来后会扫一遍日志有无 `connection refused` / `i/o timeout` / `x509` 等连接类报错
并直接回显最后三条。`服务 Running` 只说明进程活着，说明不了它连得上中枢。

### 卸载

```powershell
.\uninstall.ps1 -Role all            # 保留数据目录
.\uninstall.ps1 -Role all -RemoveData # 连数据库与日志一起清
```

卸载按**服务注册项**定位安装位置，不依赖 exe 是否还在——装到别处的旧版同样卸得掉，
并顺带清理计划任务 / 注册表 Run 键 / 启动文件夹里的同类残留（这些正是"卸完重启又冒出来"的来源）。

### 配置优先级

`命令行 flag` > `agent-mesh.json`（exe 同目录）> `AGENT_MESH_* 环境变量` > 内置默认值。

装成服务后既没有交互终端，也拿不到用户级环境变量，所以参数必须落在配置文件里。

### 服务端配置示例

```json
{
  "addr": ":8080",
  "db": "C:\\ProgramData\\AgentMesh\\agent_mesh_center.db",
  "secret": "集群共享密钥",
  "console_user": "admin",
  "console_pass": "控制台口令",
  "tls_cert": "C:\\ProgramData\\AgentMesh\\certs\\server.pem",
  "tls_key": "C:\\ProgramData\\AgentMesh\\certs\\server-key.pem",
  "log_dir": "C:\\ProgramData\\AgentMesh\\logs",
  "retention": "720h",
  "task_timeout": "5m"
}
```

### 🔒 TLS

内网没有公网域名，无从申请公信证书，所以自带自签工具：

```powershell
server.exe gencert [输出目录]
```

产出 `ca.pem`（分发给每个客户端配到 `tls_ca`）、`server.pem`、`server-key.pem`（不要分发）。

**常见坑**：客户端报证书校验失败，多半是没配 `tls_ca`，或证书 SAN 里没有实际访问的那个 IP——
`gencert` 已自动把本机所有非回环 IPv4 和主机名写进 SAN，但若之后机器换网段，要重新生成。

**控制台收口规则**：监听地址对外可达（非回环）却没配 `console_user` / `console_pass` 时，
`/console` 路由**根本不会注册**，避免设备拓扑与审计流水在内网裸奔。仅绑 `127.0.0.1` 时允许无口令调试。

### 网络能力边界

| 能力 | 局域网 | 跨网段/跨公网 |
|---|---|---|
| 心跳 / 拓扑 / 任务 / 审计 / 控制台 | ✅ | ✅（服务端地址可达即可） |
| P2P 文件传输（`mesh.sendfile`） | ✅（mDNS 发现） | ❌ mDNS 是二层组播，路由器不转发 |
| 文件中转（`mesh.relayfile` / `mesh.fetchfile`） | ✅ | ✅（经中枢中转，不依赖点对点可达性） |

跨网段部署务必启用 TLS，否则审计内容（含员工 AI 对话）与控制台口令在网络上明文传输。
异地节点建议用 VPN 把网络层打通，而不是直接暴露到公网——后者与「数据不出内网」的定位冲突。

### 文件中转（跨网段传文件 / 取回任务产出物）

P2P 只有 mDNS 发现，而 mDNS 不跨网段，所以跨网段的文件直传必然失败；
任务执行产出的文件也原本只留在执行机上，中枢只能收到一句文字结果。中转接口解决这两件事。

| 接口 | 鉴权 | 说明 |
|---|---|---|
| `POST /api/v1/files/upload` | 上传专用 HMAC（用实体摘要签名） | 流式落盘，边收边算摘要 |
| `GET /api/v1/files?node=<id>[&pending=1]` | HMAC | 列出该节点可见的文件（定向 + 广播） |
| `GET /api/v1/files/download?file_id=&node=` | HMAC | 下载；定向文件只有目标节点能取 |
| `POST /api/v1/files/delete` | HMAC | 元数据与磁盘实体一并删除 |
| `GET /console/api/files`、`/console/api/files/download` | Basic Auth | 控制台总览与直接取回产出物 |

客户端侧对应 `mesh.relayfile`（投递）与 `mesh.fetchfile`（领取）；
常驻端每 20 秒自动领取**定向给自己**的文件，广播文件需显式领取。

**限制**：单文件 512 MiB；存储总量 8 GiB（超出 507）；保留期跟随 `-retention` 清理。
存储目录默认取数据库同级的 `files/`，可用 `-files-dir` 或 `AGENT_MESH_FILES_DIR` 改写。

端到端回归：`python verify/files_check.py`（需先编译 `bin/server/server.exe`）。

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
- 部署形态：日志接管与轮转 3/3（`go test ./internal/filelog/`）
- HTTPS 端到端 7/7：服务端启用 TLS、无 CA 握手被拒、控制台 401/200、HMAC 签名、
  客户端经 HTTPS 上报并被收录、无证书校验错误
- 控制台收口 5/5：对外地址无口令时 `/console` 不注册真实路由，
  访问返回 **503**（非 404）并附关闭原因与修复指引，运维不会误判为路径写错；伪造凭据同样 503
- 请求体积限制 11/11：全局请求体上限 8MB（`Content-Length` 超限直接 413，chunked 走
  `MaxBytesReader` 截断）、任务 prompt 上限 64KB（超限 413，防单条指令打爆整个集群）、
  `?limit=` 负数/超大值收敛不拉全表、控制台关闭态 503 且不影响 `/api/v1`（`go test ./api/` + 端到端脚本）

⚠️ **尚未验证**：Windows 服务的真实注册与启动需要管理员权限，
`server.exe install` / `client.exe install` 目前只在非管理员环境下验证了报错路径
（`Access is denied` + 正确指引）。请在管理员 PowerShell 里跑一次 `deploy/install-*.ps1` 完成闭环。

## 🌐 M3 双机组网验证（装备就绪，待第二台机器）

测试计划与用例清单见 `docs/m3-lan-test-plan.md`（T1–T8：跨机心跳/任务闭环/P2P 发现/
断连重连/僵尸回收/并发/TLS/卸载残留）。

跨机检查脚本（T1+T2 自动化）：

```bash
python verify/m3_lan_check.py --server http://<服务端IP>:8080 \
    --secret "<安装时生成的集群密钥>" --node <客户端节点ID>
```

脚本已在本地冒烟（3/4：拓扑与任务创建链路全通，仅"真实客户端领取回传"留待双机闭合）。
`verify/` 目录存放可复现的验证装置，运行产物不入库。

## 🔧 依赖说明

- **SQLite 驱动选 `modernc.org/sqlite`（纯 Go）**，不用 `mattn/go-sqlite3`——后者依赖 cgo，Windows 需 MinGW。
- **libp2p 锁 v0.33.2**：0.34+ 把 mDNS 移出主仓，且 `Noise()`/`Yamux()` 已移出根包，`libp2p.New()` 默认配置即包含安全通道与多路复用。
