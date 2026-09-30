# G-1 结论：豆包本机会话库的真实结构（实测）

> 状态：**已完成验证，结论为否定**。
> 本文推翻了 `adapters/doubao.go` 里那句注释所代表的假设。
> 实测环境：Windows 10 x64，豆包桌面版正在运行（15 个进程），本机实admin账号。

---

## 1. 一句话结论

**豆包桌面客户端不把对话正文存在本地。** 本地只有会话的**元数据**（谁、什么时候、跟哪个 bot、第几条），
没有用户提问文本，也没有 AI 回答文本。

因此 `SELECT id, query, response, prompt_tokens, completion_tokens FROM messages`
这条 SQL **在任何版本的豆包上都无法成立** —— 本地压根不存在一个装着 `messages` 的 SQLite 库。

---

## 2. 真实的落点

| 项 | 真实情况 |
|---|---|
| 存储位置 | `%LOCALAPPDATA%\Doubao\User Data\Default\IndexedDB\chrome_doubao-chat_0.indexeddb.leveldb\` |
| 存储形态 | **Chromium IndexedDB**（底层 LevelDB），不是 SQLite |
| 文件格式 | LevelDB SSTable（`*.ldb`，snappy 压缩）+ WAL（`*.log`） |
| 值的编码 | **SSV** —— Blink 的 V8 ValueSerializer 序列化结果，前缀 `0xFF 0x0F` |
| 主要 object store | `default-chat-im-protocol-store`、`flow-web-chat-*`、`input-engine-draft-store` |

### 目录内的实际文件

```
000005.ldb        1.5 MB   SSTable（历史）
000006.log      452 KB   WAL（近期写入）
000007.ldb        1.6 MB   SSTable（历史）
CURRENT / LOCK / LOG / MANIFEST-000001
```

---

## 3. 解析出来的真实字段

从 WAL 中成功解码出的顶层结构（V8 序列化 → Python 值）：

```json
{
  "state": {
    "mainTaskDataMap": {
      "<任务 UUID>": {
        "stage": "sync",
        "createTime": <epoch ms>,
        "sessionId": "285e032e-4cc3-41c1-ab0f-9d92b64a9d6a",
        "querySendTimestamp": <epoch ms>,
        "requestQuery": {
          "syncTask": {
            "client_meta": {
              "local_conversation_id_": "38444992920352514",
              "conversation_id": "38444992920352514",
              "bot_id": "7338286299411103781",
              "last_section_id": "3844499292035252770",
              "last_message_index": 3.0
            }
          }
        }
      }
    }
  },
  "subTaskDataMap": {}
}
```

**可拿到的数据：**

- `conversation_id` / `sessionId` —— 会话唯一标识
- `bot_id` —— 用的是哪个 AI 助手（不同 bot 区分角色/技能）
- `querySendTimestamp` / `createTime` —— 提问发生的时间
- `last_message_index` —— 该会话的第几条消息
- `stage` —— 任务状态（sync 等）

**拿不到的数据：** ❌ 提问内容 ❌ 回答内容 ❌ token 用量

---

## 4. 判定「没有正文」的依据

不是靠猜测，是全量字节扫描：

| 检查范围 | 结果 |
|---|---|
| `chrome_doubao-chat_0.indexeddb.leveldb` 全部 792 条 SSTable 记录 | **0 条**含中文或长文本正文 |
| 同目录 WAL 全部 139 条记录 | 同上 |
| `%LOCALAPPDATA%\Doubao` 全目录（排除 Cache / 沙箱运行时） | 命中的全是 **Python/Node 运行时里的 dll/pyd 二进制误命中**，无一为会话数据 |
| `%APPDATA%\Doubao` | 仅 `public_config.json`（462 字节） |

豆包是**纯云端服务**，会话正文下行后存在内存/渲染进程里，不落盘。

---

## 5. 顺带验证：ChatGPT 网页版也不行

本机 Edge 存在 `https_chatgpt.com_0.indexeddb.leveldb`（000023.log 3.7 MB），试图从浏览器侧找到突破口。结果：

- 该 IndexedDB 里最大的 value 是一条 **ECDSA P-256 私钥**（`-----BEGIN PRIVATE KEY-----`），
  object store 名为 `ecdsa-p256:v1`
- 这是 ChatGPT 端到端加密的密钥库，**会话正文不是以可读形式落盘的**

即「员工用浏览器访问 AI 网站 → 从浏览器侧采集」这条路，对 ChatGPT 也不成立。

---

## 6. 对产品的影响

| Goal | 影响 |
|---|---|
| G-1 验证 schema | ✅ **已关闭** —— 结论：本地无正文，只有元数据 |
| G-3 用量聚合 | ⚠️ **数据源缺失** —— token 用量不在本地，`input_tokens/output_tokens` 无从获得 |
| G-4 对话日志 | ❌ **不可行**（对豆包桌面端、ChatGPT 网页端） |
| G-2 脱敏 | ✅ 不受影响，已完成，且与数据来源无关 |

**唯一还能从豆包本地拿到的是「互动元数据」**：某节点在某时刻跟某 bot 开了一个会话、发了第 N 条。
这足以回答管理层的「有没有人在用、什么时候用、用了多少次」，**但回答不了「他问了什么」**。

---

## 7. 三条可选应对路径（待定）

1. **改做「AI 使用行为清单」** —— 采集 `会话创建时间 / conversation_id / bot_id / 消息序号`，
   产出「活跃度台账」。技术上需要在 Go 里实现 LevelDB SSTable 读取 + SSV 解码，工程量中等。
2. **改采集真正有本地数据的目标** —— 重新评估本机可读性后再定：
   Ollama（`11434` 端口，但历史不落盘）、Cursor / Claude 桌面端（需逐个实测）。
3. **换成网关/代理层方案** —— 要「问了什么」就必须站到流量上：
   企业网关侧解析 HTTPS（需装根证书）或客户端侧 Hook。这是另一个产品，当前架构做不到。

---

## 附：分析工具（一次性，不参与产品构建）

- `tools/probe_doubao.py` —— LevelDB WAL 解析 + IndexedDB key 归类
- `tools/leveldb_sst.py` —— LevelDB SSTable 只读解析器（含 snappy 解压）
- `tools/ssv_decode.py` —— Chromium V8 ValueSerializer（SSV）解码器

三者组合即可复现本文所有结论。
