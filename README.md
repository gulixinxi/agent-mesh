# 🌐 Agent Mesh 企业本地 AI 协作中枢 (Monorepo Master)

本项目是一个企业级、本地优先的跨设备 AI 协作与调度管理平台。其核心宗旨是将每台员工电脑转换为可定向调度的 AI 节点，任务点对点送达，文件与数据默认留在公司内网（不经过公有云中转）。

## 🏗️ 项目架构布局 (Monorepo Layout)

本仓库采用单体系统仓库 (Monorepo) 结构管理，内部包含两个核心可执行项目：

*   `/agent-mesh-server`：中央汇总中枢服务端（基于 Go Gin 框架 + SQLite）。负责维护设备拓扑心跳、集中审计任务日志、并提供 API。
*   `/agent-mesh-client`：节点常驻客户端（基于 Go 语言 + Libp2p + MCP 协议）。常驻于员工电脑，负责接收定向任务、P2P 局域网大文件直传、以及通过插件适配器集成案头 AI 工具（如 Ollama, 豆包本地端）。

---

## 🛠️ AI Agent 远程自动开发规范

所有负责参与本项目的 AI 开发 Agent，在编写、扩充代码时必须严格遵守以下工程边界：

1. **协议依从度**：
   * 客户端与服务端的心跳包与审计日志交换必须严格遵循 `/core/types.go` 中的 `TaskPayload` 结构定义。
   * 客户端向本地编辑器（如 Cursor）开放的协议必须严格符合 Anthropic Model Context Protocol (MCP) 2024-11-05 版本规范，并基于 JSON-RPC 2.0 构建。
2. **零侵入适配器模式**：
   * 严禁在主引擎逻辑中直接写入针对特定 AI 工具（如豆包、Ollama）的定制代码。
   * 新增或修改客户端工具支持时，必须在 `/adapters/` 目录下实现 `AIAdapter` 接口。
3. **数据不出内网与安全隔离**：
   * P2P 文件传输必须使用 `go-libp2p` 建立加密点对点流，并在传输尾部通过 SHA-256 校验文件完整性。
   * 监听豆包等 SQLite 数据库对流时，必须采用“只读复制隔离机制（Copy-on-Read）”，防止死锁官方软件。

---

## 🤖 本地 WorkBuddy 自动化测试与运行指南

本专案完成后，将完全交由本地自动化代理 **WorkBuddy** 进行落盘、依赖整理、联调与运行。

### 1. 自动化拉取与编译 (WorkBuddy Deploy)
当远程代码 Commit 完成后，可以通过 WorkBuddy 触发本地部署脚本（需配置 GitHub PAT 认证）：
```bash
git clone https://<USERNAME>:<PAT>@://github.com
cd agent-mesh
cd agent-mesh-server && go mod tidy && go build -o server_bin main.go && cd ..
cd agent-mesh-client && go mod tidy && go build -o client_bin main.go && cd ..
```

### 2. 自动化本地联动联调测试步骤 (WorkBuddy Test Run)
1. **拉起中央控制台**：
   WorkBuddy 执行命令運行服務端：`./agent-mesh-server/server_bin`（默认监听内网 8080 端口）。
2. **拉起多台设备节点**：
   WorkBuddy 運行客戶端常駐程序：`./agent-mesh-client/client_bin`。
3. **功能验证**：
   * **心跳验证**：观察服务端终端，是否每 10 秒成功打印收到 `client_id` 的心跳数据。
   * **P2P 传输验证**：在客户端触发 `SendFileToPeer`，观察目标设备的 `~/AgentMeshDownloads` 文件夹内是否成功生成带 `mesh_` 前缀的、经过哈希校验的实体验证文件。
