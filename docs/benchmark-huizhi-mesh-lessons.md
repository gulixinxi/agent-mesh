# 对标产品拆解：汇智中枢（huizhi-mesh）能吸收什么

> 素材：`C:\Users\Administrator\Desktop\1\huizhi-mesh-linux-x86_64-1.0.0\`（客户现场闭源安装包）
> 拆解日期：2026-09-29
> 结论一句话：**它不是"另一个 agent-mesh"，而是把 agent-mesh 想做的那件事产品化到了"可卖"的程度**——多出来的不是网络能力，而是**交付骨架**（授权/席位、组织权限、自助安装、审计、监控、回收）和**一个中枢侧 MCP 编排面**。

---

## 0. 交付形态（先把"它是什么"钉死）

包本身极简，只有 5 个东西：

| 文件 | 作用 | 关键点 |
|---|---|---|
| `install.sh` | 客户现场一键安装（Linux） | root 执行；生成 `.env`（随机 secret/管理员密码/入站密钥）；写 systemd unit；`Restart=always` |
| `uninstall.sh` | 停服务 + 删 unit | **不删数据目录**，明确提示"彻底删除请手动 rm -rf" |
| `bin/mesh-controlplane` | 中枢主程序，单文件 **110 MB** | Nuitka 编译的 Python（`compiler=nuitka`），含冻结的 CPython 3.12 + FastAPI + SQLAlchemy + uvloop |
| `bin/console/static` | 管理台前端（284 KB 三件套） | `index.html` + `app.js`（217 KB）+ `styles.css`，纯原生 JS，无框架 |
| `keys/*.ed25519.pub` | 离线授权验签公钥 | `cloud_ed25519.pub`（首装绑机）、`dev_ed25519.pub`（开发签发） |

`bin/` 里还带出一堆 `*.so`（libcrypto/libssl/libsqlite3/libpython3.12）、`bcrypt/_bcrypt.so`、`nacl/_sodium.so`、`pydantic_core`、`uvloop`、`watchfiles`（Rust）、`zstandard` —— 说明它把整个 Python 运行时**自带**了，客户机器**不需要装 Python、不需要联网 pypi**。这是"闭源单文件交付"的标准做法，也是它售后少的第一层原因。

`install.sh` 里两处设计值得直接抄：

```bash
# 1) 主动拒绝"源码泄漏包"，防运维拿错包
if find "$ROOT" -type f -name '*.py' | grep -q .; then
  die "安装包异常（含源码文件），请重新从授权站下载官方闭源包"
fi

# 2) 局域网 IP 自动探测（三级回退），直接拼进 PUBLIC_URL / TRUSTED_HOSTS / CORS
detect_lan_ip() { ip route get 1.1.1.1 → hostname -I → ipconfig getifaddr en0 → 127.0.0.1 }
```

`.env` 默认值（install.sh 自动生成）暴露了它的运行假设：`MESH_ALLOW_LAN_HTTP=true`、`MESH_PUBLIC_URL=http://<lan>:8877`、`MESH_LICENSE_REQUIRE_CLOUD=true`、`MESH_SESSION_HOURS=168`(7天)、`MESH_LOG_JSON=true`。**默认就是"内网 HTTP + 离线跑 + 首装才联网激活"**——完全对齐中小企业私有化的现实。

---

## 1. 技术栈与进程模型

从二进制字符串（Nuitka 只泄露了一部分常量，足够还原骨架）确认：

- **Web**：FastAPI（`app = FastAPI(root_path="/api/v1")`，另有 `/api/v1/openapi.json`）+ Starlette sessions + CORS 中间件
- **运行时**：uvloop + watchfiles（部署模式不重载）
- **DB**：SQLAlchemy async；生产 SQLite（`sqlite:///.../mesh.db`），**RAG 支持 Postgres + pgvector**（Docker 拉起）——即"默认零依赖，RAG 才上重装备"
- **后台循环**：`reaper_service`（`mesh-reaper`）——独立清理线程，回收过期租约/令牌/设备
- **可观测**：`/api/metrics`（JSON）+ `/api/metrics/prometheus`（`mesh_info`、`mesh_tasks_queued/running`、`mesh_login_success/failures_total`、`mesh_harness_completions/errors_total`、`mesh_license_remaining`）
- **加密**：bcrypt（口令）、PyNaCl（Ed25519 授权验签）、`secret_vault`（统一密钥保管，`activation-vault.json`）

模块清单（92 个 `mesh_platform.*`）已经是一份成熟的工程分层，**可以直接当我们的目录规划参照**：

```
api/routes/   auth dashboard users departments devices edges files tasks
              invite license monitor rag projects inbound harness settings
              setup audit public downloads
services/     device_code invite license file_grant file edge harvest
              department rag_* monitor reaper risk_classifier policy_engine
              secret_vault setup client_pack rbac job_title
runtime/      bootstrap connector supervisor autostart wake fuse winproc
              agent_detect probe harvester redact file_roots dsh_local
              bridge_workbuddy
core/         config db errors license_crypto machine_fingerprint metrics
              rate_limit security tier_catalog
harness/      orchestrator plugin_pack
tools/        file_sandbox license_sign
```

注意几个"被单独抽成模块"的东西：`policy_engine`（策略）、`risk_classifier`（风险判定）、`rbac`、`tier_catalog`（档位目录）、`machine_fingerprint`、`reaper`、`redact`（脱敏）。**这些正是"产品化"和"demo"的分水岭**——它们都不是功能，是约束。

---

## 2. 完整功能面地图（管理台 6 大板块 / 21 页）

前端 `NAV_SECTIONS` 是权威清单，每页都带 `cap`（权限点）与 `feat`（档位开关）：

| 板块 | 页面 | 门禁 |
|---|---|---|
| **组织与权限** | 用户账号 / 系统角色 / 分配角色 / 部门架构 / 归属确认 | `users.manage`、`departments.manage`、`users.confirm_department` |
| **接入与设备** | 签发邀请 / 邀请记录 / 设备管理 | `codes.issue`、`devices.view_all\|view_own` |
| **任务中心** | 派发任务 / 任务记录 | `tasks.dispatch`、`tasks.view_all\|view_own` |
| **编排协作** | 设备关系 / 文件授权 / 智能编排 | `feat: device_graph`、`feat: harness` |
| **运营能力** | 外部入站 / 运行监控 / 知识库 / 知识项目 / 对话采集 | `feat: inbound_router / monitor / rag / harvest` |
| **系统** | 许可证 / 统一密钥管理 / 中枢配置 / 操作审计 / 接入说明 | `license.manage`、`settings.manage`、`audit.view_*` |

对照我们现状：我们只有 `dispatch`（派单）+ `files`（中转）+ `console`（总览），**21 页里我们能对上 2 页**。差距不在网络层，在"经营层"。

值得单独点名的三个"我们完全没有、但很有用"的页面：

1. **部门架构 + 归属确认**：员工首次注册处于 `unassigned`，需管理员确认归属（`department_confirmed_at/by`）。这是**把"谁是谁的人"变成流程**，而不是靠口口相传。`mesh_users` 表里有 `pending_department_id`、`pending_department_label`、`department_status(unassigned|pending|confirmed)`。
2. **统一密钥管理**：所有第三方密钥（DeepSeek 等）集中进 `secret_vault`，还带 `migrate_legacy_dsh_into_vault` 迁移函数——**密钥不进配置文件**。
3. **运行监控**：`/api/monitor/overview`、`/alerts`、`/scan` 三件套 + Prometheus 出口。

---

## 3. 数据模型（从 ALTER TABLE 痕迹还原）

`mesh_` 前缀表：`users` `departments` `devices` `agents` `device_codes` `device_edges`
`tasks` `file_grants` `file_sessions` `harvest_batches` `rag_documents` `rag_chunks`
`projects` `license_state` `*_tokens` `settings` `audit_logs` `plans`

两个细节能看出它是"真上过客户现场"的：

- **所有迁移都是 `ALTER TABLE ADD COLUMN`**，且有 `PRAGMA table_info(...)` 护着 → **升级不重建库，老客户数据原地长大**。`mesh_rag_documents` 甚至同时出现 SQLite 与 Postgres 两种 `ADD COLUMN ... DEFAULT '[]'::json` 写法 → 同一套 ORM 兼容双后端。
- **`mesh_device_codes_new` + RENAME**：改表约束时用"建新表→拷贝→改名"的 SQLite 标准姿势，说明 `max_uses/use_count` 是后来加的（邀请码从"一码一机"演进到"一码可用 N 次/不掉"）。

`mesh_agents` 的列很说明问题：`agent_version`、`binary_path`、`available`、`operations(JSON)`、`privilege_mode`、`requires_user_approval`、`source`。**它把"每台机器上装了哪些 AI agent、各自能干什么、是否需要人工批准"当作一等公民建模**——这正是"编排"能成立的前提。我们的客户端目前是"一个节点一个引擎"，没有"节点上多 agent、各自能力画像"这一层。

---

## 4. 授权体系：三层门禁（这是最值得学的一块）

它把"控制"拆成三个正交维度，缺一不可：

### 4.1 档位 tier（商业）→ `core/tier_catalog.py`
`basic 基础版 / plus 增强版 / ultimate 旗舰版`
接口：`features_for_tier()`、`provision_tier()`、`normalize_tier()`、`--all-tiers`、`--tier` CLI。

### 4.2 功能 feat（档位开关）→ 10 个
`mesh 设备调度`、`company_runtime 公司运行时`、`device_graph 设备关系`、`file_grants 文件授权`、
`harness 智能编排`、`rag 知识库`、`projects 知识项目`、`harvest 对话采集`、
`inbound_router 外部入站`、`monitor 运行监控`

前端 `hasFeat()` 控制导航项**显示 + 加锁角标**；后端 `require_license_feature()` 二次拦截。**前端藏、后端拦，两层都做**。

### 4.3 权限 cap（角色）→ 25 个
`users.manage/assign_role/confirm_department`、`departments.manage/view`、`license.manage`、
`codes.issue`、`devices.view_all/view_own/kick`、`tasks.view_all/view_own/dispatch`、
`edges.manage/view`、`grants.manage`、`settings.manage`、`secrets`、`harness.manage`、
`audit.view_all/view_own`、`dashboard.summary/mine`、`rag.manage`、`harvest.manage`、
`inbound.manage`、`console.admin_nav`

角色：`boss 老板 / tech_admin 技术管理员 / supervisor 主管 / employee 员工 / knowledge_admin 知识管理员`
（管理台还写死了旧角色别名 `platform_admin / ops / member` 的兼容映射）。

**"私有/全部"成对出现**（`view_own` vs `view_all`）是它的核心手法：默认员工只能看自己的设备/任务/审计，主管看部门，老板看全部。这套 `*_own / *_all` 模式我们几乎可以直接照搬。

### 4.4 离线授权（卖软件的关键）
- 授权码是 **16 或 32 位短码**，形如 `ABCD-EFGH-IJKL-MNOP`（`short code length must be 16 or 32`）
- 用 `cloud_ed25519.pub` **离线验签**；`machine_fingerprint` 绑机（`bound_fingerprint`）
- **首装才需联网**（去 `lisn.qingnangedu.com` 绑定一次，拿 `cloud_grant_id`），之后**日常运行与续期全程离线**
- 约束：**一码一机**、**生成后 3 天内首装**
- 状态机：`license_expired`（过期）/ `license_required`（没激活）/ `feature_disabled`（档位没开）

前端三段式拦截：**license-gate（粘授权码）→ setup-gate（开箱配置）→ login**。
`setup-gate` 会按档位**自动准备环境依赖**（`/api/setup/provision`），并要求增强/旗舰版填 **DeepSeek(DSh) 密钥**。

> 对我们的启示：这解释了"为什么它敢闭源卖"——**授权 + 档位 + 席位**是护城河，不是功能。我们现在是"给客户一份 Go 二进制 + 一个 secret"，客户能无限复制。要做生意，这套迟早要做。

---

## 5. 自助交付：邀请码驱动的"零现场"入网（我们最该抄的一块）

它的邀请码不是"一串要手输的字符"，而是**一个公开命名空间**。中枢暴露的公开路由：

```
GET /i/{code}                 → 邀请落地页（浏览器打开）
GET /i/{code}.json            → 机器可读的接入信息
GET /i/{code}/playbook.json   → 装机剧本（安装步骤 + 配置 + 自检项）
GET /i/{code}/register        → 注册端点
ALL /i/{code}/mcp             → 该码对应的 MCP 端点
```

再配合**中枢自带的客户端分发**（`/downloads/`）：

```
GET /downloads/                     → JSON 清单（含 version）
GET /downloads/mesh-client.zip
GET /downloads/install_client.py
GET /downloads/install_client.ps1
```

于是管理台"接入说明"页给出的就是**两条一键命令**（原文照抄）：

```powershell
# Windows
$gw = "http://<中枢>"
irm "$gw/downloads/install_client.ps1" | iex
$py = "$env:USERPROFILE\.agent-mesh-platform\venv\Scripts\python.exe"
& $py -m mesh_platform.runtime.bootstrap --gateway $gw --enrollment-code-stdin
& $py -m mesh_client install-autostart --start     # ← 必做
```
```bash
# macOS / Linux
curl -fsSL "$GW/downloads/install_client.py" | python3 - --gateway "$GW"
PY="$HOME/.agent-mesh-platform/venv/bin/python"
"$PY" -m mesh_platform.runtime.bootstrap --gateway "$GW" --enrollment-code-stdin
"$PY" -m mesh_client install-autostart --start
```

四个"售后杀手锏"，逐条都该抄：

1. **`--enrollment-code-stdin`：邀请码从标准输入读，不进命令行**。避免出现在 `ps`/日志/历史里。这是很硬的安全细节。
2. **`install-autostart --start` 被标成"必做"**，并在页面里明写警告：
   > "勿只前台跑 supervisor：关掉窗口或重启就会断心跳"
   ——**把"最常见的售后工单"直接写进安装页**。
3. **`re-enroll same machine` / `reenroll_same_machine`**：重装/换 IP 可复用同一台机器身份，不会在控制台里堆出一串幽灵设备。
4. **装完自检回传**：`autostart_ok`、`device.enrolled`、`agent_probe` 结果回中枢，管理员在"设备管理"页就能看到成功/失败，**不用远程登机器**。

客户端运行时的模块名也很坦白：`supervisor`（常驻守护）、`autostart`（开机自启）、`wake`（唤醒）、`probe`（探活）、`agent_detect`（探测本机装了哪些 agent）、`inventory`（上报资产清单）、`winproc`（Windows 进程）、`fuse`（挂载/虚拟盘）、`bridge_workbuddy`（对接 WorkBuddy GUI）、`dsh_local`（本地 DeepSeek harness）。

**"设备清单 + agent 能力探测"是它编排的入场券**：`/api/devices/{id}/agents/probe-all`、`/agents/{agent}/probe`、`/inventory`。

---

## 6. 编排面（harness）：中枢自己就是一个 MCP Server

这是与我们**架构性**的差异点。

它的架构里，"AI 编排"不是某个客户端功能，而是**中枢对外暴露的一整套 MCP 工具 + 令牌 + 计划**：

- 路由 `/api/harness/chat`、`/plan`、`/tools`、`/tokens`，以及公开的 `/i/{code}/mcp`
- 模块 `harness/orchestrator.py` + `harness/plugin_pack.py`（可插拔工具包，有 version）
- 令牌：`mesh_harness_tokens`，**签发时明文只显示一次**，注释明写"此令牌不含设备凭据，只能调用中枢提供的编排工具"（权限最小化）
- 指标：`harness.tool.mesh_create_task`、`mesh_harness_completions_total`、`mesh_harness_errors_total`

**暴露的 MCP 工具（从二进制确认）**：

| 工具 | 作用 |
|---|---|
| `mesh_register_this_device` | 自我注册接入 |
| `mesh_list_devices` | 列出可调度设备 |
| `mesh_create_task` | 派单 |
| `mesh_propose_plan` / `mesh_apply_plan` / `mesh_harness_plans` | 计划：提议 / 应用 / 列表（**带 dry-run 预演**） |
| `mesh_enroll_playbook` / `mesh_invite_status` | 入网剧本与邀请状态 |
| `mesh_file_meta_search` | 按"来源设备 + 目录线索"搜文件元数据 |
| `mesh_file_get_session` / `mesh_file_read_excerpt` | 发起文件会话 / 读片段（`wait_seconds=15` 阻塞式） |
| `mesh_file_grant_create/list/renew/revoke` | 文件授权增删改续 |
| `mesh_rag_search` / `mesh_rag_upsert` / `mesh_rag_ingest_batch` | 知识库读写 |
| `mesh_project_members` | 项目成员 |

**计划（plan）的数据结构**（前端原样）：
```json
{"steps":[{"target_device_id":"设备编号","target_agent":"codex","instruction":"任务说明","risk_class":"read_only"}]}
```
配套 `POST /api/tasks/classify-risk`，`risk_class ∈ {auto, read_only, write}`。
**"先预演、再正式应用" + "风险分级"** 是让老板敢按下去的按钮。

支持的执行器（agent）枚举：`codex / codebuddy / workbuddy(→CodeBuddy CLI) / dsh(DeepSeek Harness) / cursor / doubao / hermes / printf(测试)`。
操作枚举：`execute_instruction / shell / file_read / file_write / edit / network / gui_chat(仅 GUI)`。
`mesh_agents.requires_user_approval` = **高风险操作需人工批准**。

> 一句话总结这一节：**它把"多机 AI 编排"做成了"中枢提供 MCP 工具 + 客户端提供执行器"的解耦架构**。我们目前是"客户端 → 服务端"单向派单，**服务端本身不是 MCP 端点**，AI 想编排还得站在某个客户端里。这个位置差，决定了它"可被任意 AI 前台驱动"，而我们只能被自家客户端驱动。

---

## 7. 文件链路：授权（grant）+ 会话（session），而不是"上传下载"

我们的 §3.5 是"上传到中枢 → 目标拉取"，解决了可达性。它的模型更细一层：

1. **文件授权 `mesh_file_grants`**：主体是 **设备 ↔ 用户（或设备）**，字段有 `root_path`、`actions`、`expires_at`、`note`、`grantee_device_id`
   - 动作语义是 **`list 浏览 / pull 读取 / push 写入`**，不是"下载/上传"
   - 状态机：`active / expiring / expired / revoked`，支持**续期 7 天**与撤销
2. **文件会话 `mesh_file_sessions`**：真正的取件动作
   - `pending → select → served/delivered`
   - **`excerpt_text / excerpt_byte_size / excerpt_charset / excerpt_truncated`**：先取**片段**给 AI 看，AI 判断要不要整份 → 省带宽、省 token
   - `/api/files/sessions/{id}/excerpt`、`/select`、`/delivered`、`/link`
3. **MCP 侧**：`mesh_file_meta_search(source_device_id, root_hint?)` —— **按"哪台机器 + 哪个目录"搜元数据**，而不是全库搜内容

> 可吸收点：
> - **把"授权"和"传输"分开**：授权是长期凭证（可续期/可撤销），传输是短会话（一次性）。我们目前是"上传即得"，没有"允不允许读这台机器这个目录"这一层。
> - **先片段后整份**：对"AI 要读跨机文件"的场景，这个优化值钱。
> - **`actions = list/pull/push` 的语义**比 upload/download 更准确，也更好做审计。

---

## 8. 运营能力三件套：RAG / 采集 / 入站

### 8.1 知识库 rag（feat 门控）
- 文档 + 分块两表；**默认 SQLite，可选 Postgres + pgvector**（`rag_pgvector.py` / `rag_embed.py` / `rag_chunk.py`）
- **ACL 到部门**：`acl_department_ids(JSON)` + `dept_scope(subtree)` + `visibility(restricted/public)`；撤销是**软删**（`status='revoked'`，chunks 同步 revoked）
- 检索回退可见于采集侧：`from_rag`（"来自知识库回退"角标）

### 8.2 对话采集 harvest（feat 门控）
- 员工机器上的 AI 对话被**按批次**上报：`source_app`、`agent_label`、`device_id`、`item_count`
- **强制脱敏**：`runtime/redact.py`，`redacted` 标志；"non non-empty items after redact" 直接丢
- 有**日报**：`/api/harvest/daily-digest`
- 语义很清楚：**给老板做"公司里 AI 都在干嘛"的能见度**。这是企业采购的真实动机之一。

### 8.3 外部入站 inbound（feat 门控）
- `POST /api/inbound/webhook` + `MESH_INBOUND_WEBHOOK_SECRET`（install.sh 随机生成 24 字节）
- `POST /api/inbound/{id}/route`：外部事件 → 路由成内部任务
- 即"让外部系统（表单/监控/IM）能触发中枢派单"

---

## 9. 生命周期治理：它不敢省的那些"脏活"

| 机制 | 证据 | 作用 |
|---|---|---|
| 任务租约 | `task_lease_seconds`、`renew_lease`、`/tasks/{id}/renew`、`task_queued_timeout_seconds`、`task_max_running_seconds`、`idle_timeout_seconds`、`idle_running_grace_seconds` | 设备中途死掉，任务不会永远 running |
| 心跳与探活 | `/heartbeat`、"device heartbeat too stale for dispatch"、`agent_health_interval/probe_timeout/window` | 派单前先确认设备真活着 |
| 设备惩戒 | `kick` / `revoke` / `restore` / `kick_cleared`；`UPDATE mesh_devices SET status='kicked' WHERE status='revoked'` | 设备丢了/被换，能踢下线 |
| 后台清理 | `reaper_service`（`mesh-reaper`）、`expire_stale_probes` | 过期授权/令牌/探测结果自动回收 |
| 速率限制 | `core/rate_limit.py`；错误码 `rate_limited` | 防爆破 |
| 审计 | `/api/audit/logs`、`/audit/mine`、`view_own/view_all` | 谁在什么时候干了什么 |
| 错误码契约 | `auth_required/auth_forbidden/license_expired/feature_disabled/setup_required/task_conflict/not_found/rate_limited/internal_error` | **前端只做映射，文案集中一处** |

最后这条（错误码契约 + 统一文案映射）是**"少售后"的隐藏功臣**：客户看到的永远是人话，不是栈。

---

## 10. 吸收清单（按"对我们的性价比"排序）

### P0 —— 不做会直接卡住交付/商业闭环

| # | 吸收项 | 依据 | 我们的落点 | 量级 |
|---|---|---|---|---|
| 1 | **邀请码 + 一键安装 + 装机自检**（`/join/<code>` 或 `/i/<code>/playbook.json` + `irm/curl` 一行） | §5 | docs/deploy-models.md §3.4 已规划，需落地 | 中（1 个新路由族 + 客户端 bootstrap） |
| 2 | **客户端开机自启 + 立即拉起，且写进安装页警示** | §5.2 | 客户端缺 `install-autostart`；这是"断心跳"工单的根 | 小（Win 计划任务 / Linux systemd user unit） |
| 3 | **Linux 服务端构建 + `install-server.sh`** | §0 | 已在待办① | 小 |
| 4 | **设备"机器指纹 + 重复注册收敛"** | `machine_fingerprint`、`re-enroll same machine` | 防控制台堆幽灵设备 | 小 |
| 5 | **错误码契约 + 统一文案** | §9 | 我们现在错误散落 | 小 |

### P1 —— 决定"能不能卖"和"售后多不多"

| # | 吸收项 | 依据 | 说明 |
|---|---|---|---|
| 6 | **中枢侧 MCP 编排面（harness）** | §6 | 让"任意 AI 前台（CodeBuddy/Claude）都能驱动中枢"，而不是只能靠我们客户端。工具面先做最小集：`mesh_list_devices`、`mesh_create_task`、`mesh_plan(dry-run/apply)` |
| 7 | **风险分级 + 预演/正式应用** | §6 | `risk_class(auto/read_only/write)` + plan dry-run；老板敢按的按钮 |
| 8 | **档位 + 功能开关 + 席位** | §4 | 商业护城河。可先只做"开关"（不接云端授权），`tier → features` 一张表 |
| 9 | **RBAC 的 `*_own / *_all` 成对权限** | §4.3 | 多角色场景刚需，模式简单 |
| 10 | **文件授权（grant，可续期/可撤销）与传输（session）分离** | §7 | 我们的 files 中转目前无授权层 |
| 11 | **任务租约 + 心跳新鲜度校验 + reaper** | §9 | 任务不会永远挂着 |
| 12 | **Prometheus 指标出口 + 运行监控页** | §1 | 客户 IT / 我们自己排障都要 |

### P2 —— 有则加分

| # | 吸收项 | 依据 |
|---|---|---|
| 13 | 知识库（先 SQLite，后 pgvector）+ 部门 ACL + 软删撤销 | §8.1 |
| 14 | 对话采集 + 强制脱敏 + 日报 | §8.2 |
| 15 | 外部入站 webhook → 派单 | §8.3 |
| 16 | 统一密钥保管（secret vault）+ 迁移旧配置 | §2 |
| 17 | 文件片段先取（excerpt）省带宽/token | §7 |
| 18 | 部门架构 + 归属确认流程 | §2 |
| 19 | 离线授权（Ed25519 签短码 + 机器指纹绑机） | §4.4 |
| 20 | 闭源单文件交付（Nuitka 那套：自带运行时、无 pypi） | §0 |

### 明确**不学**的

- ❌ **把跨网问题完全推给客户**（我们已在 deploy-models.md 决定"多走一步"：L0/L1 给标准与工具，L2 给隧道模板）。
- ❌ **用 110 MB 单文件打包脚本语言**。我们是 Go，静态单文件天然优于 Nuitka 冻 Python；别为了"看起来一样"放弃 Go 的体积/启动优势。
- ❌ **默认 HTTP 明文**（`MESH_ALLOW_LAN_HTTP=true`）。内网可以，但我们已有 `gencert` + TLS SAN，**默认 TLS、显式降级**更专业。
- ❌ **前端用原生 JS 手写 217 KB**。它这么做是为了"零构建、单文件分发"；我们控制台若继续长，值得引入构建，但**不必学它的手写方式**。

---

## 11. 一句话结论 + 下一步

**它的网络能力并不比我们强；它强在把"一个 mesh"包装成了"一个可交付、可授权、可运维、可审计、可被 AI 驱动的产品系统"。**

差距清单收敛成 3 件事：

1. **入网关（P0-1/2/4）**：把"客户自己想办法装"变成"点一行命令，装完自检"。→ 售后工时一次性归零。
2. **编排面（P1-6/7）**：中枢自己成为 MCP Server，任何人/任何 AI 都能驱动它。→ 从"我们的客户端"变成"一个平台"。
3. **治理层（P1-8/9/11/12）**：档位、权限、租约、监控。→ 从"能用"变成"能卖、能维护"。

建议近期顺序：**① Linux 服务端脚本 → ② 邀请码+一键安装+自检（对齐它 §5 的完整闭环）→ ③ 中枢侧 MCP 最小工具集（list/create/plan）→ ④ 档位开关 + `*_own/*_all` 权限 → ⑤ 租约/reaper/指标**。

> 备注：本分析中"事实"均来自安装包（脚本、`.env`、前端 `app.js`、二进制字符串）；对二进制的推断（如 harness 的 MCP 语义、plan 的 dry-run 实现方式）已在文中标注依据，落地前建议以实际接口文档/试用为准。
