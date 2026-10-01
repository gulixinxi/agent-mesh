# 市场与开源调研：Shadow AI 治理赛道（2026-10-01）

> 触发：对标产品汇智中枢定价 **500 元/台设备 + 3000 元/家授权**，问"有没有更好的开源架构可借鉴，尽快赶上"。
> 本文只记录**可核验的事实**（附来源），并明确区分「设计思想值得学」与「代码值得抄」。

---

## 1. 赛道定义

我们做的事情在市场上的名字是 **Shadow AI Discovery / AI Governance / Workforce AI Security**，不是"agent mesh"。
三个问题：影子 AI 使用（用了没批准的工具）、数据泄漏（敏感信息进了模型）、合规暴露（无法向审计方证明 AI 使用受管）。

市场规模数据（各家口径不一，仅作量级参考）：

| 数据 | 来源 |
|---|---|
| 55% 员工在使用 IT/安全未批准的 AI 工具 | Salesforce 2024（经 Aona 引用） |
| 48% 员工向生成式 AI 输入过机密/非公开信息 | Cisco 2024 Privacy Benchmark（经 Aona 引用） |
| 平均每个企业有 97 个未批准的 AI 工具在用；73% 员工承认粘贴过工作文档 | Cloud Security Alliance 2026（经 app-lab 引用） |
| 影子 AI 平均给数据泄漏成本增加 $670,000 | IBM 2025（经 app-lab 引用） |

---

## 2. 商业竞品：主流走**浏览器层**，不是端点

| 产品 | 路线 | 关键主张 |
|---|---|---|
| **Aona** | 浏览器扩展 | "浏览器层是唯一能看到全部员工 AI 活动的地方，与设备/网络/工具无关"；无网络改造 |
| **LayerX** | 企业浏览器安全层 | 实时内容检查（粘贴/上传前拦截）、AI 工具枚举、扩展审计 |
| **Relyance AI** | 数据流图 | 把代码→云→AI 的数据流串起来，映射法律/合同义务 |
| **Microsoft Purview / Proofpoint / CyberArk** | 各自原有阵地延伸 | 数据分类 / 邮件网关 / 特权访问 |

**共同结论**：它们都不做"读本地磁盘上的 AI 会话库"。
理由很实际——浏览器层覆盖最广、部署最轻（一个扩展），而端点采集只能覆盖装了客户端的场景。

**这对我们是好消息也是警告**：
- 好消息：端点路线没人认真做，我们的本地会话采集是差异化；
- 警告：**覆盖面天然不如浏览器扩展**。只采 Codex/Cursor 这类，采不到网页版 ChatGPT/豆包。
  对标产品同样有这个盲区（它执行器列表第一位就是 codex，说明它也靠这类），但我们要清醒。

---

## 3. 开源可借鉴项目（含成熟度诚实评估）

### 3.1 AmanSK5/shadow-ai-guard —— 路线最接近，但**成熟度很低**

| 项 | 实测（GitHub API，2026-10-01） |
|---|---|
| Star / Fork / Issues | **11 / 0 / 4** |
| 语言 / 协议 | Python / **Apache-2.0（可商用借鉴）** |
| 创建 / 最后提交 | 2026-07-20 / 2026-09-28（669 commits，单人高频开发） |
| 阶段 | README 自述 **Beta** |

**架构（值得学的部分）**：

| 组件 | 职责 | 可借鉴点 |
|---|---|---|
| `receiver` | FastAPI，接收 findings，结构化 JSON，**唯一状态**在单个 SQLite（enrollment/accounts/settings/decisions） | 状态集中、 ingest 与状态分离 |
| `registry` | "什么算 AI 工具"：域名、扩展 ID、配置文件路径。编译副本随包发布，**portal 运行时可加** | ⭐ **加新工具不动端点**——我们目前把 15 个客户端目录硬编码在客户端 `inventory.go` 里 |
| `portal` | 运营视图，**不在 ingest path 上**（它挂了采集继续） | ⭐ 可靠性设计 |
| `endpoint` | macOS/Windows/Linux collectors，读本地 AI 工具配置，**报告每个工具登录的是哪个账号** | ⭐ 见下 |
| `scanner` | Entra ID / Exchange / Intune / Jamf / SentinelOne DNS / MCP 检查，每模块可选 | 模块化、可选启用 |
| `discovery` | 定时扫 DNS，未知 AI 域名进人工 review queue | ⭐ "Nothing is detected until a person decides" |
| Grafana/Loki | 可选遥测 | — |

**三个锋利的产品洞察（比代码值钱）**：

1. ⭐ **"账号才是关键，不是工具"** —— 原文：*"None of them read `~/.claude.json` to tell you which account your developers' AI CLIs are signed into - and the account is the part that matters: an approved tool on a personal account is still unmanaged data flow, invisible spend and an offboarding gap."*
   批准的工具 + **个人账号** = 依然是未管理的数据流、看不见的支出、离职交接缺口。**我们完全没想到这一层。**
2. ⭐ **区分"干净"与"坏了"** —— 原文：*"A source that reports nothing might be clean or might be broken; those look identical in a log stream, so the portal tells you the difference."*
   这正中我们的软肋：服务端装了但 Stopped、8080 被 Everything 占了、客户端在无限重试连不上——**日志流里这些都长得一样**。
3. **预算视角** —— AI 工具开销 vs 实际使用：没人用的席位、用了但没席位的人、付费与个人账号并行。

**不学/慎用**：它部署依赖 K8s + Loki + Grafana + 多个 scanner 模块，对中小企业门槛远高于我们；11 star 意味着**几乎无社区验证**，代码不宜直接依赖。

### 3.2 fleetdm/fleet + osquery —— 架构成熟，值得学思想

| 项 | 情况 |
|---|---|
| Fleet | Go + React + MySQL，28,267 commits，2026-09 仍在高频提交，非常活跃 |
| osquery | 端点资产采集事实标准（Facebook 开源，Linux 基金会维护），272+ 张表，Apache-2.0 |

**Fleet 最值得抄的四条**：

1. ⭐ **控制面 / 数据面双平面**：`Orbit`（Go，管注册、密钥、自更新、重启注入）与 `osqueryd`（数据面，管查询执行）分离。职责分离，便于独立升级与故障恢复。
2. ⭐ **一次性注册密钥 → 换取 node key**：MDM 铸造**每设备唯一、单次使用**的 enroll secret；Orbit 从注册表/文件读取后存入 keystore，**用后清除临时副本**；再换取长期 node key；开启后**禁用共享密钥**；安装日志不再泄露密钥。
   → 对比我们：邀请码是"一码 N 机"（`max_uses`），**这是安全性差距**。
3. **策略即查询，结果驱动门禁**：SQL 策略下发到终端，回收结果决定是否执行动作（如跳过软件安装）。
4. **有界重试与超时**：node key 被拒 45s 后重新注册；MDM 同步限流最多 5 分钟一次；长任务用 `systemd-run` transient unit 托管，超时杀脚本不影响底层。

### 3.3 赛道开源生态稀薄

按 `shadow ai governance / AI usage audit endpoint` 搜索 GitHub，**total_count = 1**。
这是一个几乎没有开源竞品的新赛道——机会与风险同源：没有现成高质量代码可抄，也没有免费替代品分流。

---

## 4. 可直接落地的借鉴清单（按性价比）

| # | 借鉴项 | 来源 | 我们的落点 | 量级 |
|---|---|---|---|---|
| 1 | **AI 工具清单下沉到服务端**（registry），加新工具不动端点 | shadow-ai-guard | 客户端 `inventory.go` 改为服务端下发 | 中 |
| 2 | **上报"登录账号"而非仅"装了什么"**，个人账号单独标红 | shadow-ai-guard | 新增账号探测（如 `~/.codex/config`、Cursor 登录态） | 中 |
| 3 | **"干净 vs 坏了"的覆盖率页**：每个源上次上报时间、静默多久 | shadow-ai-guard | 控制台新增「采集源健康」 | 小 |
| 4 | **邀请码改为一次性/单设备**，换取长期 node key | Fleet | `store/invites.go` + enroll 流程 | 中 |
| 5 | **控制面/数据面分离**（自更新通道） | Fleet Orbit | 客户端拆 supervisor + worker | 大，缓做 |
| 6 | **有界重试与超时**已部分具备，补齐 MDM 式"恢复通道" | Fleet | 客户端重连退避 | 小 |
| 7 | 端点资产建模参照 osquery 表设计 | osquery | 资产清单字段命名 | 小 |

---

## 5. 商业判断：500/台 + 3000/家这个价，我们的位置

汇智的定价（500/台 + 3000/家授权）在**中小企业**区间是偏高的：20 人公司一次性就要 13,000。
它能卖这个价，前提是**客户没有可用的免费替代**——而 shadow-ai-guard 虽然免费，却要 K8s + Loki + Grafana，中小企业根本跑不起来。**"免费但装不上"等于不存在。**

所以真正的护城河不是功能多少，是三点：

1. **部署门槛**：Go 静态单文件、一条命令装完、自带 TLS——这是开源方案和 Nuitka 冻 Python 都给不了的；
2. **跨网**：多厂区/多办公室/光猫接路由器，所有方案（含汇智、含开源）都把这块推给客户，我们多走一步；
3. **国内 AI 客户端覆盖**：豆包、Kimi、通义、文心等，海外开源 registry 里基本没有。

**定价建议**：既然开源方案存在且免费，我们不宜照抄 500/台。
更稳的是**按中枢授权 + 低单价节点**：授权费做低（甚至免费基础版），节点按年收少量费用，用"部署 + 跨网 + 本地化客户端库"收服务费。
功能上先把 §4 第 1–4 条做掉，这几条正好是"看起来简单、但客户自己搞不定"的部分。

---

## 6. 未做的事

- 未逐行阅读 shadow-ai-guard 源码（11 star，投入产出比低），只取其 README 与架构描述中的设计思想；
- 未调研工业级 PII 脱敏方案（如 Microsoft Presidio）与我们 `core/redact.go` 的差距，可作为后续项；
- 未调研国内同类产品定价（汇智为唯一已知样本）。
