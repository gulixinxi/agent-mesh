#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
豆包本机会话库探针（一次性分析工具，不参与产品构建）

用途：解析 Chromium IndexedDB 底层的 LevelDB（WAL .log + SSTable .ldb），
      还原出豆包聊天会话库里真实存在的 object store 与其 key/value 结构，
      用于确定 adapters/doubao.go 中会话表/列的真实 schema。

用法：
    python tools/probe_doubao.py               # 扫描本机豆包 IndexedDB 并输出结构摘要
    python tools/probe_doubao.py --dump N      # 额外打印前 N 条 value 的原始字节

设计原则：Copy-on-Read —— 先把库文件复制到临时目录再解析，绝不直接打开
", "+str(len(parts))+"生产中的文件（豆包客户端持有 LOCK）。
"""

import argparse
import collections
import math
import os
import re
import shutil
import struct
import sys
import tempfile

BLOCK_SIZE = 32768
HEADER_SIZE = 7
# WAL record type
K_FULL, K_FIRST, K_MIDDLE, K_LAST = 1, 2, 3, 4
# LevelDB batch record kind
TYPE_VALUE, TYPE_DELETION = 1, 2

DOUBAO_IDB = os.path.join(
    os.environ.get("LOCALAPPDATA", ""),
    "Doubao", "User Data", "Default", "IndexedDB",
    "chrome_doubao-chat_0.indexeddb.leveldb",
)


# ---------------------------------------------------------------- LevelDB WAL
def read_varint32(buf, pos):
    """读 LevelDB varint32，返回 (值, 新位置)。"""
    result = 0
    shift = 0
    while True:
        b = buf[pos]
        pos += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            break
        shift += 7
        if shift > 35:
            raise ValueError("varint32 过长")
    return result, pos


def iter_wal_records(data):
    """遍历 LevelDB WAL 中的所有逻辑 record（自动处理跨 block 分片）。"""
    pos = 0
    pending = bytearray()
    while pos + HEADER_SIZE <= len(data):
        offset_in_block = pos % BLOCK_SIZE
        if offset_in_block + HEADER_SIZE > BLOCK_SIZE:
            # 记录头跨 block 边界，跳到下一个 block 起点（LevelDB 不允许跨 block 写头）
            pos += BLOCK_SIZE - offset_in_block
            continue
        raw_len = data[pos + 4] | (data[pos + 5] << 8)
        rec_type = data[pos + 6]
        payload = data[pos + HEADER_SIZE: pos + HEADER_SIZE + raw_len]
        pos += HEADER_SIZE + raw_len

        if len(payload) != raw_len:
            break
        if rec_type == K_FULL:
            yield bytes(payload)
            pending = bytearray()
        elif rec_type == K_FIRST:
            pending = bytearray(payload)
        elif rec_type == K_MIDDLE:
            pending.extend(payload)
        elif rec_type == K_LAST:
            pending.extend(payload)
            yield bytes(pending)
            pending = bytearray()
        else:
            # 未知类型通常是被 ZeroRecord 填充的 padding，直接结束
            break


def parse_batch(batch):
    """解析一条 WAL record（WriteBatch），yield (key_bytes, value_bytes_or_None)。"""
    if len(batch) < 12:
        return
    count = struct.unpack("<I", batch[8:12])[0]
    pos = 12
    for _ in range(count):
        kind = batch[pos]
        pos += 1
        klen, pos = read_varint32(batch, pos)
        key = batch[pos: pos + klen]
        pos += klen
        if kind == TYPE_VALUE:
            vlen, pos = read_varint32(batch, pos)
            value = batch[pos: pos + vlen]
            pos += vlen
            yield key, value
        elif kind == TYPE_DELETION:
            yield key, None
        else:
            break


# ---------------------------------------------------------------- IndexedDB key
def decode_idb_key_prefix(key):
    """
    Chromium IndexedDB internal key 的前缀 binary 承载 database/object-store id。
    这里取前若干字节做归类用，不做完整语义还原。
    """
    return key[:16]


def strings_from(raw, min_len=4):
    """从原始字节里抽出可读文本片段（UTF-8 与 UTF-16LE 都试）。"""
    out = []
    for m in re.finditer(rb"[\x20-\x7e]{%d,}" % min_len, raw):
        out.append(m.group().decode("ascii", "replace"))
    for m in re.finditer(rb"(?:[\x20-\x7e]\x00){%d,}" % min_len, raw):
        out.append(m.group().decode("utf-16-le", "replace"))
    for m in re.finditer(rb"\xe4[\x80-\xbf][\x80-\xbf](?:[\x00-\xff]{0,3}?[\xe0-\xef][\x80-\xbf]{2}){%d,}" % max(1, min_len // 3), raw):
        out.append(m.group().decode("utf-8", "replace"))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", default=DOUBAO_IDB, help="IndexedDB leveldb 目录")
    ap.add_argument("--dump", type=int, default=0, help="打印前 N 条 value 原始数据")
    args = ap.parse_args()

    if not os.path.isdir(args.source):
        print("找不到豆包 IndexedDB 目录：%s" % args.source)
        return 1

    workdir = os.path.join(tempfile.gettempdir(), "doubao_idb_probe")
    if os.path.isdir(workdir):
        shutil.rmtree(workdir, ignore_errors=True)
    os.makedirs(workdir, exist_ok=True)
    copy = {}
    for name in sorted(os.listdir(args.source)):
        src = os.path.join(args.source, name)
        if os.path.isfile(src) and (name.endswith(".log") or name.endswith(".ldb")):
            dst = os.path.join(workdir, name)
            try:
                shutil.copy2(src, dst)
                copy[name] = dst
            except Exception as e:
                print("  跳过 %s：%s" % (name, e))
    print("已复制 %d 个库文件到 %s" % (len(copy), workdir))

    total = 0
    key_groups = collections.Counter()
    samples = []
    for name in ("000006.log", "000007.ldb"):
        if name not in copy:
            continue
        data = open(copy[name], "rb").read()
        # .ldb 是 SSTable 不是 WAL，只有当它恰好能被当作 WAL 扫时才会有结果
        # 这里只对 .log 做 WAL 解析，SSTable 另行处理
        if not name.endswith(".log"):
            continue
        print("\n=== 解析 %s (%d 字节) ===" % (name, len(data)))
        for batch in iter_wal_records(data):
            for key, value in parse_batch(batch):
                total += 1
                prefix = decode_idb_key_prefix(key)
                group = ";".join(strings_from(prefix, 2)[:2]) or repr(prefix[:8])
                key_groups[group] += 1
                if value is not None and len(samples) < 64:
                    samples.append((group, key, value))

    print("\n共扫描 %d 条 record" % total)
    print("\n=== key 前缀分组（top 20）===")
    for g, n in key_groups.most_common(20):
        print("  %6d  %s" % (n, g[:120]))

    if args.dump:
        print("\n=== value 样本 ===")
        for i, (g, key, value) in enumerate(samples[: args.dump]):
            print("\n--- #%d group=%s value_len=%d ---" % (i, g[:80], len(value)))
            print("key   :", repr(key[:80]))
            print("value :", repr(value[:400]))

    return 0


if __name__ == "__main__":
    sys.exit(main())
