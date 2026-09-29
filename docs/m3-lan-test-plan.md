# M3 双机真实组网验证计划

状态：**待执行**（装备已就绪，等待第二台机器）
前置依赖：M1 部署形态已落地（`5260c86`）；仓库转私有门禁建议在开始前完成。

## 1. 目的

单机验证掩盖了以下只在真实多机环境才暴露的问题，本计划逐一验证：

- mDNS/P2P 跨机发现（二层组播只在本广播域有效）
- 跨机心跳延迟与拓扑准确性（`localIP()` 是否报出真实内网 IP）
- 任务通道在真实网络下的领取/回传时序
- 节点宕机后的僵尸任务回收与重投（claim_token 时序在真实网络下才成立）
- Windows 服务模式 + 配置文件在非开发机的可安装性

## 2. 拓扑与前置条件

```text
机器A（服务端）                机器B（客户端）
┌──────────────────┐          ┌──────────────────┐
│ server.exe 服务   │◄──HTTP──►│ client.exe 服务   │
│ :8080 (+TLS 可选) │          │ 心跳/轮询/P2P     │
└──────────────────┘          └──────────────────┘
        同一局域网 / 同一广播域（mDNS 要求，非同网段则 P2P 用例跳过）
```

| 项 | 要求 |
|----|------|
| 网络 | 两机同一局域网；P2P 用例要求同一广播域/VLAN（跨网段 mDNS 不可达，属已知边界） |
| 防火墙 | A 放行 TCP 8080（服务端监听）；B 放行 P2P 端口（默认见客户端配置）与 mDNS UDP 5353 入站 |
| 安装 | 两机均以管理员 PowerShell 跑 `deploy/install-server.ps1` / `deploy/install-client.ps1` |
| 凭据 | 安装脚本打印的 secret / console_pass 两机共用（B 的配置里写同一个 secret） |
| TLS（可选） | 若启用：先在 A 上 `server.exe gencert`，SAN 已自动包含全部非回环 IPv4；B 的 `-tls-ca` 指向拷贝过去的 CA 证书。**A 换网段后须重新生成证书** |

## 3. 用例清单

| # | 用例 | 步骤要点 | 通过标准 |
|---|------|----------|----------|
| T1 | 跨机心跳与拓扑 | 两机装好服务后等 2 个心跳周期（约 10s） | 控制台设备列表出现 B，IP 为 B 的真实内网 IP，`agents[].runnable` 能力位正确 |
| T2 | 跨机任务闭环 | `verify/m3_lan_check.py --server http://A的IP:8080 --secret <secret> --node <B的ID>` | 脚本全 PASS：任务 create → B 领取 → 回传 completed |
| T3 | mDNS P2P 跨机发现 | 控制台下发 `mesh.sendfile`，A↔B 传一个小文件 | 文件到达且 SHA-256 校验通过；mDNS 日志显示发现对端 |
| T4 | 断连与重连 | 杀掉 B 的服务进程，等 1 分钟，再 `Start-Service` | 拓扑中 B 变 offline → 恢复 online；期间下发的任务被超时回收并重投 |
| T5 | 僵尸任务回收 | T4 前半段（B 宕机时）持续下发任务，观察 5 分钟（`-task-timeout` 默认值） | running 任务在 timeout 后被 reap：attempts+1 重投；超过 max_attempts 判死为 timeout 状态 |
| T6 | 并发与抢占 | 连续下发 5 条任务（B 执行耗时设长一点） | B 并发上限 2 生效（第 3 条起排队）；无重复执行（attempts 单调） |
| T7 | TLS 跨机 | A 启用 TLS 重装，B 走 https 上报 | B 无证书校验错误；抓包确认非明文（可选）；控制台经 https 访问正常 |
| T8 | 服务模式残留检查 | 两机 `deploy/uninstall.ps1` 后重启 | 服务不存在、Program Files 安装目录与 ProgramData 数据目录语义清晰（卸载脚本定义），无孤儿进程 |

## 4. 执行顺序

1. A 机管理员安装服务端 → `Get-Content C:\ProgramData\AgentMesh\agent-mesh.json` 抄下 secret
2. B 机管理员安装客户端（secret 用同一个）→ 重启两机验证服务自启（T1 前置）
3. 依次跑 T1 → T2（自动化）→ T3 → T6 → T4+T5（连做，共享宕机窗口）→ T7 → T8
4. 每条用例的结果即时记入 `docs/m3-lan-test-results.md`（含时间、现象、证据摘录）

## 5. 已知边界（不是缺陷，测试时不要误报）

- 跨网段/跨公网：控制面（HTTP）协议可行但当前无公网部署意图；数据面（mDNS P2P）**不可达**，属设计边界
- 审计内容含员工 AI 对话，过公网有合规风险——异地接入应走 VPN
- 服务进程工作目录是 System32，路径问题先查配置文件是否在 exe 同目录

## 6. 跨网接入（异地客户端）组网方案

### 结论：现阶段用 ZeroTier

判定依据（三条硬约束）：
1. **P2P 只有 mDNS 发现**（`core/p2p_transfer.go:82` 仅注册 libp2p mdns service，
   无静态 peer / bootstrap / relay）→ 跨网要保住 P2P 文件传输，overlay 必须承载二层广播
2. 节点是 **2-5 台长驻 Windows 机器**（服务形态），不是短命 agent 集群
   → 控制面「自动批量增删节点」的价值不成立
3. **国内网络 + 需代理** → 控制面在境外的 SaaS 登录/协调不稳定；自建控制面要公网 IP 与运维成本

| 候选 | 二层/mDNS | 运维成本 | 判定 |
|---|---|---|---|
| **ZeroTier** | 支持（L2 虚拟以太网，需开启广播） | 零（用托管控制面） | ✅ 采用 |
| Headscale | 不支持（WireGuard L3） | 高（VPS + 证书 + DB + 自建 DERP） | ⏸ 备选：节点 >25 或要求控制面自持时 |
| NetBird | 不支持 | 中（4-5 容器） | ❌ 现阶段过度 |
| Netmaker | 不支持 | 高 | ❌ 性能优势对 2-5 台 Windows 无意义 |
| Nebula | 不支持 | 中高（证书 + lighthouse） | ❌ 无 UI，数百节点才有价值 |

### ⚠️ 落地顺序坑（自签证书 SAN）

`gencert` 的 SAN 取自**生成证书时本机的网卡 IP**（`agent-mesh-server/gencert.go:105 localIdentities`）。
异地客户端是按 overlay IP 访问服务端的，所以：

1. **先**在服务端装 ZeroTier 并加入网络（拿到 172.22.x.x 之类的 overlay IP）
2. **再**跑 `deploy/install-server.ps1 -Addr ":8443" -TLS`（此时 overlay IP 才会进 SAN）
3. overlay IP 若变化，必须重新 `gencert` 并重发 ca.pem 给各客户端

### 验证项（新增 T9）

- T9-a 异地客户端跨 overlay 心跳上线，控制台拓扑可见
- T9-b 异地客户端领取任务并回传结果（T2 的跨网版本）
- T9-c 跨 overlay 的 mDNS P2P 文件传输（ZeroTier 网络需开启 Enable Broadcast）
