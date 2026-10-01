#!/usr/bin/env python3
"""AI 客户端对话可读性普查。

回答一个问题：这台机器上装的 AI 客户端，有哪些能把对话正文读出来？

为什么要有这个脚本：G-1 只测了豆包一个源就得出「本地没有可读正文」的结论，
差点把整条对话采集产品线判死。实际同一个机器上 Codex CLI 是明文 JSONL、
Cursor 是明文 SQLite——**单个客户端的结论不能外推到整条产品线**。
所以普查必须是脚本化的、可在任意机器上重跑的，而不是靠一次手工翻目录。

判定口径（三档）：
    可读    —— 能取到自然语言正文（中/英），可直接做采集
    仅元数据 —— 能取到会话 id / 时间戳 / 角色，取不到正文
    加密    —— 熵接近 8.0，或 SQLite 打不开，正文不以可读形式落盘

用法：
    python tools/probe_readability.py                # 扫本机
    python tools/probe_readability.py --json          # 输出 JSON
    python tools/probe_readability.py --min-bytes 0   # 连空目录也报（排查用）
"""

import argparse
import collections
import glob
import json
import math
import os
import re
import sqlite3
import sys

CJK = re.compile(r"[\u4e00-\u9fff]")
# 自然语言句子的粗判：有空格分词或中文标点，且长度像话而不像键名
PROSE = re.compile(r"[\u4e00-\u9fff，。？！、；]|\b(the|and|how|what|please|write|fix)\b", re.I)


def home(*parts):
    return os.path.join(os.path.expanduser("~"), *parts)


def appdata(roaming, *parts):
    base = os.environ.get("APPDATA") if roaming else os.environ.get("LOCALAPPDATA")
    if not base:
        base = home("AppData", "Roaming" if roaming else "Local")
    return os.path.join(base, *parts)


# 候选清单：改这里就能扩客户端。path 可以是目录或 glob。
CANDIDATES = [
    # name,      类别,   路径
    # 注意：WorkBuddy 的会话在 ~/.workbuddy，不在 AppData 下。
    # 2026-10-01 前这里漏了它，导致误判"WorkBuddy 本地连会话库都没有"。
    ("WorkBuddy", "桌面", home(".workbuddy")),
    ("Codex CLI", "CLI", home(".codex")),
    ("Cursor", "IDE", appdata(True, "Cursor")),
    ("VS Code", "IDE", appdata(True, "Code")),
    ("Claude", "桌面", appdata(True, "Claude")),
    ("ChatGPT", "桌面", appdata(True, "OpenAI")),
    ("ChatGPT", "桌面", appdata(False, "OpenAI")),
    ("豆包 Doubao", "桌面", appdata(False, "Doubao")),
    ("豆包 Doubao", "桌面", appdata(True, "Doubao")),
    ("MiniMax", "桌面", appdata(True, "@mmx-agent")),
    ("ChatAA", "桌面", appdata(True, "ChatAA")),
    ("Trae CN", "IDE", appdata(True, "Trae")),
    ("Trae CN", "IDE", appdata(False, "Trae")),
    ("Windsurf", "IDE", appdata(True, "Windsurf")),
    ("Zed", "IDE", appdata(False, "Zed")),
]

# 会话数据通常藏在这几类文件里
DB_SUFFIX = (".vscdb", ".sqlite", ".sqlite3", ".db", ".db3")
JSONL_SUFFIX = (".jsonl", ".ndjson")


def entropy(sample):
    """字节熵，8.0 基本等于加密或压缩。"""
    if not sample:
        return 0.0
    counts = collections.Counter(sample)
    n = len(sample)
    return -sum((c / n) * math.log2(c / n) for c in counts.values())


def looks_like_prose(text):
    if not text or len(text) < 12:
        return False
    # 键名/JSON 噪音过滤：中文占比过低且无英文词，视为结构数据
    return bool(PROSE.search(text))


def probe_sqlite(path, limit=400):
    """SQLite：列出表，对 KV 型表按 key 前缀分组，抽样判断有无正文。"""
    try:
        con = sqlite3.connect("file:" + path.replace("\\", "/") + "?mode=ro&immutable=1", uri=True)
    except Exception as exc:
        return {"kind": "sqlite", "verdict": "加密", "note": f"无法打开: {type(exc).__name__}"}

    out = {"kind": "sqlite", "tables": [], "verdict": "仅元数据", "note": ""}
    try:
        tables = [r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type='table'")]
    except Exception as exc:
        con.close()
        return {"kind": "sqlite", "verdict": "加密", "note": f"读取 schema 失败: {type(exc).__name__}"}

    prose_hits = 0
    scanned = 0
    for t in tables:
        try:
            n = con.execute("SELECT count(*) FROM '%s'" % t).fetchone()[0]
        except Exception:
            continue
        out["tables"].append({"table": t, "rows": n})

        # KV 形态（key/value 两列）是 Electron 应用存对话的典型姿势
        cols = [d[1] for d in con.execute("PRAGMA table_info('%s')" % t)]
        if len(cols) == 2 and "key" in cols[0].lower():
            try:
                rows = con.execute("SELECT key, value FROM '%s' LIMIT %d" % (t, limit)).fetchall()
            except Exception:
                continue
            for k, v in rows:
                s = v if isinstance(v, str) else (v.decode("utf-8", "replace") if isinstance(v, bytes) else "")
                scanned += 1
                if looks_like_prose(s):
                    prose_hits += 1
    con.close()

    if prose_hits:
        out["verdict"] = "可读"
        out["note"] = "抽样 %d 条，%d 条含自然语言正文" % (scanned, prose_hits)
    else:
        out["note"] = "抽样 %d 条，未命中正文" % scanned
    return out


def probe_jsonl(path, max_lines=600):
    """JSONL：找 role/content 结构，统计正文与 token 字段。"""
    hits = 0
    scanned = 0
    token_fields = set()
    sample = None
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            for i, line in enumerate(fh):
                if i >= max_lines:
                    break
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except Exception:
                    continue
                payload = obj.get("payload") if isinstance(obj.get("payload"), dict) else obj
                text = json.dumps(payload, ensure_ascii=False)
                scanned += 1
                role = payload.get("role") if isinstance(payload, dict) else None
                if role == "user" and len(text) > 40:
                    hits += 1
                    if sample is None:
                        sample = text[:200]
                for f in ("input_tokens", "output_tokens", "cached_input_tokens", "total_tokens", "token_count"):
                    if f in text:
                        token_fields.add(f)
    except Exception as exc:
        return {"kind": "jsonl", "verdict": "不可读", "note": type(exc).__name__}

    out = {"kind": "jsonl", "scanned": scanned}
    if hits:
        out["verdict"] = "可读"
        out["note"] = "抽样 %d 行，%d 条 user 消息" % (scanned, hits)
        out["sample"] = sample
    else:
        out["verdict"] = "仅元数据"
        out["note"] = "抽样 %d 行，无 user 正文" % scanned
    out["token_fields"] = sorted(token_fields)
    return out


def probe_binary(path):
    with open(path, "rb") as fh:
        head = fh.read(4096)
    ent = entropy(head)
    return {
        "kind": "leveldb/二进制",
        "verdict": "加密" if ent > 7.5 else "待人工判读",
        "note": "首 4KB 熵 %.2f/8.00" % ent,
    }


def walk_data_files(root, max_files=400):
    """收集候选数据文件，跳过缓存类目录。"""
    skip = {"cache", "gpu", "logs", "crashdumps", "code cache", "cachedata",
            "blob_storage", "gcm_store", "cachedExtensionVSIXs", "grshadercache",
            "dawnwebgpucache", "component_crx_cache", "dictionaries", "models"}
    found = {"sqlite": [], "jsonl": []}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d.lower() not in skip]
        for f in filenames:
            low = f.lower()
            p = os.path.join(dirpath, f)
            try:
                sz = os.path.getsize(p)
            except OSError:
                continue
            if sz < 4096:
                continue
            if low.endswith(DB_SUFFIX):
                found["sqlite"].append((sz, p))
            elif low.endswith(JSONL_SUFFIX):
                found["jsonl"].append((sz, p))
        if sum(len(v) for v in found.values()) > max_files:
            break
    for k in found:
        found[k].sort(reverse=True)
    return found


def probe_action_logs():
    """AI 行为台账普查：不看对话正文，只看 AI 碰过什么（文件 / 命令 / 外访 URL）。

    这一层 2026-10-01 才补上。此前只判"正文能不能读"，漏掉了最有审计价值的一层：
    **正文读不到，行为照样能读。** WorkBuddy 实测——对话正文落本地 0 条
    （5492 条 message 里含连续 6+ 汉字的有 0 条），但 12780 次工具调用、
    1684 个被触碰的文件路径、163 个外访 URL 全在，且是结构化 JSON。

    对做审计产品来说这一层比正文更值钱：不碰聊天隐私、无需破解加密、
    直接命中"公司的哪份文件被 AI 拿走了"。
    """
    roots = [home(".workbuddy"), home(".codex")]
    tools = collections.Counter()
    paths = set()
    urls = set()
    scanned = 0
    for root in roots:
        if not os.path.isdir(root):
            continue
        for p in glob.glob(os.path.join(root, "**", "*.jsonl"), recursive=True):
            scanned += 1
            try:
                fh = open(p, encoding="utf-8", errors="replace")
            except Exception:
                continue
            with fh:
                for line in fh:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        o = json.loads(line)
                    except Exception:
                        continue
                    if o.get("type") != "function_call":
                        continue
                    nm = o.get("name") or ""
                    if nm:
                        tools[nm] += 1
                    a = o.get("arguments")
                    s = a if isinstance(a, str) else json.dumps(a, ensure_ascii=False)
                    for m in re.finditer(r'"(?:file_path|path|filePath)":\s*"([^"]{6,})"', s):
                        paths.add(m.group(1))
                    for m in re.finditer(r'"url":\s*"(https?://[^"]{6,})"', s):
                        urls.add(m.group(1))
    return {"files": scanned, "calls": sum(tools.values()), "tools": tools,
            "paths": sorted(paths), "urls": sorted(urls)}


def main():
    ap = argparse.ArgumentParser(description="AI 客户端对话可读性普查")
    ap.add_argument("--json", action="store_true", help="输出 JSON 而非表格")
    ap.add_argument("--min-bytes", type=int, default=1, help="小于此大小的目录直接跳过")
    args = ap.parse_args()

    results = []
    seen = set()
    for name, kind, path in CANDIDATES:
        key = (name, path)
        if key in seen or not os.path.isdir(path):
            continue
        seen.add(key)

        files = walk_data_files(path)
        entry = {"client": name, "category": kind, "path": path, "findings": []}

        # JSONL 优先：Codex 这类 CLI 的会话日志，信号最强
        for sz, p in files["jsonl"][:3]:
            r = probe_jsonl(p)
            r["file"] = os.path.relpath(p, path)
            r["size_mb"] = round(sz / 1e6, 2)
            entry["findings"].append(r)
        for sz, p in files["sqlite"][:3]:
            r = probe_sqlite(p)
            r["file"] = os.path.relpath(p, path)
            r["size_mb"] = round(sz / 1e6, 2)
            entry["findings"].append(r)

        if not entry["findings"]:
            # 没有 SQLite/JSONL，就退到熵检测：LevelDB 之类
            ldb = []
            for dirpath, dirnames, filenames in os.walk(path):
                for f in filenames:
                    if f.lower().endswith((".ldb", ".log")) and "leveldb" in dirpath.lower():
                        ldb.append(os.path.join(dirpath, f))
                if len(ldb) > 4:
                    break
            for p in ldb[:2]:
                try:
                    r = probe_binary(p)
                    r["file"] = os.path.relpath(p, path)
                    entry["findings"].append(r)
                except Exception:
                    pass

        if entry["findings"]:
            # 取最好的结论作为该客户端的整体判定
            order = {"可读": 3, "仅元数据": 2, "待人工判读": 1, "加密": 0, "不可读": 0}
            entry["verdict"] = max((f.get("verdict", "不可读") for f in entry["findings"]),
                                   key=lambda v: order.get(v, 0))
            results.append(entry)

    if args.json:
        print(json.dumps(results, ensure_ascii=False, indent=2))
        return

    print("=" * 78)
    print("AI 客户端对话可读性普查")
    print("=" * 78)
    for e in results:
        print("\n[%s] %s  —— %s" % (e["verdict"], e["client"], e["category"]))
        print("    路径: %s" % e["path"])
        for f in e["findings"]:
            line = "    · %-22s %7.1fMB  %s" % (f.get("file", "")[:22], f.get("size_mb", 0), f.get("note", ""))
            print(line)
            if f.get("token_fields"):
                print("      token 字段: %s" % ", ".join(f["token_fields"]))
    print("\n" + "=" * 78)
    tally = collections.Counter(e["verdict"] for e in results)
    print("汇总: " + "  ".join("%s %d" % (k, v) for k, v in tally.most_common()))

    # 行为层：正文普查之外单独一节。两者结论可以完全相反。
    act = probe_action_logs()
    print("\n" + "=" * 78)
    print("AI 行为台账普查（不看正文，只看 AI 碰过什么）")
    print("=" * 78)
    print("  扫描会话文件 %d 个，工具调用 %d 次" % (act["files"], act["calls"]))
    print("  被 AI 触碰的本地文件路径: %d 个" % len(act["paths"]))
    print("  AI 访问过的外部 URL: %d 个" % len(act["urls"]))
    if act["tools"]:
        print("  工具分布 TOP8: " + ", ".join("%s %d" % kv for kv in act["tools"].most_common(8)))
    for p in act["paths"][:5]:
        print("    · 文件 %s" % p[:92])
    for u in act["urls"][:5]:
        print("    · 外访 %s" % u[:92])
    if act["calls"] == 0:
        print("  （无行为记录：本机可能没用过带工具调用的 AI 客户端）")


if __name__ == "__main__":
    sys.exit(main())
