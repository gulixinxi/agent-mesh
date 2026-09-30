# Agent Mesh 产品定义（原始范围）与现状对照

> **本文的由来**：项目此前**没有一份独立的 PRD**（在 `*.md` 中搜索 `PRD` / `产品需求` / `需求文档` / `产品定义` 零命中）。
> 产品定义散落在根 `README.md`、`docs/benchmark-huizhi-mesh-lessons.md` 与逐轮工作记录里。
> 本文把这些原始表述汇总成 PRD 口径，并**逐条回到代码核对实现程度**。
>
> 核对基准：`HEAD = 5f6d5e7`（2026-09-30），工作区干净。凡标「证据」处均可在代码中直接复现。

---

## 一、原始定义（保留出处的原文）

| # | 出处 | 原文 |
|---|---|---|
| 1 | `README.md:3` | 企业级、本地优先的跨设备 AI 协作与调度平台。把每台员工电脑变成可定向调度的 AI 节点…… |
| 2 | `README.md:212` | 跨网段部署务必启用 TLS，否则**审计内容（含员工 AI 对话）**与控制台口令在网络上明文传输 |
| 3 | `README.md:258-263` | 零侵入适配器模式：严禁在主引擎里写针对特定 AI 工具的定制代码；**监听第三方 SQLite（如豆包本地库）必须采用 Copy-on-Read 只读快照** |
| 4 | 工作记录 2026-09-29 | **真正差异化的资产是「本地 AI 工具会话审计」**，扩到 Kimi / 通义 / 文心 / ChatGPT desktop / Cursor 本地库才能形成产品价值 |
| 5 | `benchmark-huizhi-mesh-lessons.md` §8.2 | 对话采集 harvest：员工机器上的 AI 对话按批次上报；**强制脱敏**；有日报；语义是**给老板做「公司里 AI 都在干嘛」的能见度，这是企业采购的真实动机之一** |
| 6 | 同上 §3 | 对标产品的 `mesh_agents` 把**「每台机器上装了哪些 AI agent、各自能干什么、是否需要人工批准」当作一等公民建模** |
| 7 | 同上 §2 | 对标产品「运行监控」「对话采集」是两个独立功能页 |
| 8 | 用户口径 2026-09-30 | 当初定义的是可以**监控各个客户端的 AI 模型**，**防止出现泄密**等，同时可以**登记各个模型的用量**、**对话日志**等 |

**归纳为四条产品目标**：

- **G1 监控各客户端的 AI** —— 覆盖员工机上多种 AI 客户端（不是只认一两种）
- **G2 防泄密** —— 数据不出内网 + 对 AI 使用做管控
- **G3 用量登记** —— 各模型/各客户端的 token 用量
- **G4 对话日志** —— 采集 AI 对话内容并可查

---

## 二、逐条对照现状

### G1 监控各客户端的 AI —— ⚠️ 覆盖极窄

| 项 | 现状 | 证据 |
|---|---|---|
| 已实现适配器 | **只有 2 个** | `agent-mesh-client/adapters/` 下仅 `ollama.go`、`doubao.go` |
| 注册方式 | 硬编码白名单，**不扫描已装软件** | `agent-mesh-client/main.go:374`（Ollama）、`:384`（豆包，且**必须配 `-doubao-db` 才注册**） |
| 能力形态 | 豆包=**只读采集**（刻意不实现 `Execute`，调度侧自动跳过）；Ollama=**可执行**（`POST /api/generate`） | `adapters/doubao.go:29-31`、`adapters/ollama.go:65` |
| 上报字段 | `agents[]` = 名称 + kind + `runnable` | `core/engine.go:123-136` |

**差距**：Cursor / Kimi / 通义 / 文心 / ChatGPT desktop / Trae CN **全部没有**。而 G1 的原话是"各个客户端"。
**结构性成本**：每新增一个客户端必须由人逆向其本地库结构，成本不可控（此风险在工作记录中已被标注）。

### G2 防泄密 —— ⚠️ 只有"通道与准入"，没有"内容"

| 已有 | 证据 |
|---|---|
| 传输签名 | `api/auth.go` HMAC（method+path+ts+nonce+bodyHash，双向 300s 窗口，nonce 去重） |
| 传输加密 | 可选 TLS（`gencert` 自签 CA + `-tls-cert/-tls-key`） |
| 访问准入 | 控制台 Basic Auth；**对外可达却无口令时 `/console` 根本不注册**（`api/console.go` 顶部注释 + main 路由） |
| 数据不出内网 | 审计经中枢汇总，P2P 与中转均在客户内网 |

| **缺失** | 说明 |
|---|---|
| **内容脱敏** | 全库无 `redact` / 敏感信息过滤逻辑（grep 零命中）。现状是**对话原文直接上报入库** |
| **风险判定** | 无 `risk_classifier` 类似物 |
| **策略引擎** | 无 `policy_engine` 类似物 —— 不能"禁止某类内容发出去" |
| **告警** | 无任何告警通道 |

**结论**：G2 目前是**纯事后留痕**，没有内容层防护。对标产品在同类位置上至少有 `redact`（脱敏）、`risk_classifier`（风险判定）、`policy_engine`（策略）三个独立模块，我们一个都没有。

### G3 用量登记 —— ✅ 链路通，❌ 无来源、无汇总

| 项 | 现状 | 证据 |
|---|---|---|
| 数据字段 | ✅ 完整 | `core/types.go:32-33`（`InputTokens`/`OutputTokens`）→ `core/engine.go:452-453`（上报）→ `store/database.go`（`audit_logs.input_tokens/output_tokens`）→ `api/task.go:75`（可查） |
| 数据来源 | ⚠️ **只有豆包一个**，且 SQL 是**假设的 schema** | `adapters/doubao.go:97` 原文注释：**"真实的豆包 schema 尚未确认"** |
| 聚合统计 | ❌ **不存在** | 控制台概览只有 `total_devices / online_devices / runnable_agents / pending_tasks / today_audit / active_invites`（`api/console.go` ConsoleOverview）——**没有任何 token 维度** |
| 导出 | ❌ 无 | |

**差距**：G3 的"登记"只是把两个整数写进了一行记录；**没有"哪个模型这个月用了多少"这类可看的数**。

### G4 对话日志 —— ✅ 链路通，❌ 未脱敏、未验证、无日报

| 项 | 现状 | 证据 |
|---|---|---|
| 采集 | Copy-on-Read 快照后增量扫库（符合原始工程约束） | `adapters/doubao.go:64-138`，每 3 秒一轮 |
| 上报入库 | ✅ | `POST /api/v1/audit/report` → `audit_logs(task_id, target_node, agent_kind, prompt, result, input_tokens, output_tokens, timestamp)` |
| 查询 | ✅ | `GET /api/v1/audit/logs?limit=&node=` |
| 控制台可见 | ✅ | `web/console.html` 审计流水 |
| **脱敏** | ❌ **无** | 对标产品有 `runtime/redact.py` + `redacted` 标志；我们无对应实现 |
| **日报** | ❌ **无** | 对标产品有 `/api/harvest/daily-digest` |
| **真实库验证** | ❌ **未做** | 仅用 `doubao_message_mock.db`（自造 mock）验证过 |

---

## 三、Trae CN 在这个定义里的位置

按 G1（监控各个客户端的 AI），**Trae CN 属于应当被覆盖的对象**。但实测它的本地数据形态与豆包完全不同：

| 文件 | 大小 | 是否可读 |
|---|---|---|
| `%APPDATA%\Trae CN\ModularData\ai-agent\database.db` | 12.5 MB | ❌ **整文件加密**（前 16 字节非 `SQLite format 3`；熵 **8.00/8.00**；无 `CREATE TABLE`/`session`/`prompt` 明文） |
| `%APPDATA%\Trae CN\Local Storage\config.db` | 86 KB | ❌ 加密 |
| `%APPDATA%\Trae CN\ModularData\ckg_server\env_codekg.db` | 36 KB | ❌ 加密 |
| `%APPDATA%\Trae CN\User\globalStorage\state.vscdb` | 472 KB | ✅ 明文 SQLite，但**只有草稿/模型列表/终端历史，没有对话正文** |

**因此**：豆包那条"Copy-on-Read 扫明文 SQLite"的路子对 Trae CN **不成立**。
Trae CN 只能落在"**知道装了、读不到内容**"这一类里。

**旁证（对标产品）**：汇智中枢支持的执行器枚举为
`codex / codebuddy / workbuddy(→CodeBuddy CLI) / dsh(DeepSeek Harness) / cursor / doubao / hermes / printf(测试)`
—— **同样不含 Trae**。说明这不是本项目的孤立短板，而是"IDE 类客户端对话库加密"这个共性难题。

**可选路线**（详见 §四 建议）：
1. 降级为**存在性检测**：只上报"这台装了 Trae CN"，`runnable=false`。可满足 G1 的"看得见"，不满足 G4。
2. **反向接入**：客户端自带 `--mcp` 纯 stdio 模式（`core/mcp_server_entry.go`，flag 注释：*供 Cursor 等宿主接入*）→ 把 agent-mesh 挂进 Trae CN 的 MCP 设置，让 Trae 调用中枢。这是"接进来"而非"采出去"。
3. 采集：需绕过加密，**不建议**（不稳定、易随版本失效、合规上站不住）。

---

## 四、结论与建议顺序

> **一句话**：原始定义的四条目标里，**只有 G4「对话日志」真正跑通了一条窄链路**（豆包客户端 → 中枢）；G1 覆盖面几乎为零，G2 没有内容层防护，G3 没有可看的数。

**且三个目标都压在同一个未验证的假设上** —— 豆包适配器里那句
*「真实的豆包 schema 尚未确认」*（`adapters/doubao.go:100`）。
schema 一旦不匹配，G3／G4 的数据源直接为空。

**建议排序**（先夯实地基，再扩面）：

1. **验证豆包真实 schema** —— 拿到真机豆包的库，确认表名与列名。这一步不做，后面全是空中楼阁。
2. **补脱敏（G2 内容层）** —— 上报前过滤手机号/身份证/银行卡/密钥等；加 `redacted` 标志；脱敏后为空的条目直接丢弃（对标产品的做法）。
3. **补用量聚合（G3 呈现层）** —— 控制台加"按客户端 / 按节点 / 按天"的 token 汇总；这是"老总看板"真正会看的那一栏。
4. **补日报（G4 增量）** —— 对标产品的 daily-digest 是采购动机的直接承载。
5. **最后才是扩客户端（G1）** —— 每接一个都要先探查其本地数据是否可读（Trae CN 这类加密库直接排除在"采集"之外，只能做存在性检测）。

**一个必须提前说清的产品边界**：本产品的"监控"是**事后采集与留痕**，不是"实时阻断"。
对标产品也一样 —— 它靠的是 `redact` 脱敏 + 审计可见性形成的威慑，而非拦截。
如果客户期待的是"员工往 AI 粘贴敏感数据时当场拦下来"，那是另一个产品（需要端侧驱动/代理层），当前架构做不到。
