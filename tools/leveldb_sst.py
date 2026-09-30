#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
LevelDB SSTable（.ldb）只读解析器。

Chromium IndexedDB 底层就是 LevelDB：近期写入在 WAL（*.log），
历史数据在 SSTable（*.ldb，默认 snappy 压缩）。要取出豆包的**历史会话**，
必须能读 .ldb，只读 WAL 只能拿到最近几百 KB。

文件格式要点：
  [data block ...] [meta block ...] [metaindex block] [index block] [footer]
  footer 固定 48 字节 = varint64 metaindex_offset/size + varint64 index_offset/size
                       + padding(至 40 字节) + magic(8B, 0xdb4775248b80fb57)
  block  格式 = 若干 entry + restart 数组(uint32*n) + num_restarts(uint32)
               + type(1B: 0=raw 1=snappy) + crc(4B)
  entry  格式 = varint shared + varint non_shared + varint value_len
               + key_delta + value
  内部 key 末 8 字节 = sequence<<8 | type(1=值 0=删除)
"""

import struct

try:
    import cramjam
except ImportError:  # pragma: no cover
    cramjam = None

FOOTER_SIZE = 48
MAGIC = b"\x57\xfb\x80\x8b\x24\x75\x47\xdb"


def read_varint64(buf, pos, limit):
    result = 0
    shift = 0
    while True:
        if pos >= limit:
            raise ValueError("varint64 越界")
        b = buf[pos]
        pos += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            break
        shift += 7
        if shift > 63:
            raise ValueError("varint64 过长")
    return result, pos


class BlockHandle:
    __slots__ = ("offset", "size")

    def __init__(self, offset, size):
        self.offset = offset
        self.size = size


def read_block(data, handle):
    """按 BlockHandle 读出 block contents（自动解压），返回 bytes。"""
    start = handle.offset
    end = handle.offset + handle.size
    if end > len(data):
        raise ValueError("block 范围越界：%d..%d > %d" % (start, end, len(data)))
    raw = data[start:end]
    comp_type = data[end]
    if comp_type == 0:
        return raw
    if comp_type == 1:
        if cramjam is None:
            raise RuntimeError("需要 cramjam 做 snappy 解压：pip install cramjam")
        return bytes(cramjam.snappy.decompress_raw(raw))
    raise ValueError("未知压缩类型 %d" % comp_type)


def iter_block_entries(block):
    """遍历一个 block 中的所有 entry，yield (internal_key, value)。

    read_block 返回的是解压后的 BlockBuilder::Finish() 输出，即
        [entry...][restart 数组 uint32*n][num_restarts uint32]
    注意：**不含** 5 字节 trailer（type+crc），trailer 在文件中但不属于
    BlockHandle.size，也不参与压缩。所以：
        num_restarts = 末 4 字节
        entries 区   = [0, len - 4*(1+num_restarts))
    """
    if len(block) < 4:
        return
    num_restarts = struct.unpack("<I", block[-4:])[0]
    restart_end = len(block) - 4 * (1 + num_restarts)
    if restart_end < 0 or restart_end > len(block):
        return
    pos = 0
    key = b""
    while pos < restart_end:
        shared, pos = read_varint64(block, pos, restart_end)
        non_shared, pos = read_varint64(block, pos, restart_end)
        value_len, pos = read_varint64(block, pos, restart_end)
        if shared > len(key):
            return
        if non_shared > restart_end - pos or value_len > restart_end - pos:
            return
        key = key[:shared] + block[pos:pos + non_shared]
        pos += non_shared
        value = block[pos:pos + value_len]
        pos += value_len
        yield key, value


def parse_footer(data):
    """读出 metaindex / index 两个 BlockHandle。"""
    if len(data) < FOOTER_SIZE:
        raise ValueError("文件太小，不是 SSTable")
    footer = data[-FOOTER_SIZE:]
    if footer[-8:] != MAGIC:
        raise ValueError("SSTable magic 不匹配")
    pos = 0
    meta_off, pos = read_varint64(footer, pos, 40)
    meta_size, pos = read_varint64(footer, pos, 40)
    idx_off, pos = read_varint64(footer, pos, 40)
    idx_size, pos = read_varint64(footer, pos, 40)
    return BlockHandle(meta_off, meta_size), BlockHandle(idx_off, idx_size)


def iter_sstable(data):
    """遍历整个 SSTable 的所有数据条目，yield (internal_key, value)。

    会跳过删除标记（type=0）；同一 user_key 只保留 sequence 最大的那条
    （LevelDB 的 mvcc 语义）。
    """
    _, index_handle = parse_footer(data)
    index_block = read_block(data, index_handle)
    for _, index_value in iter_block_entries(index_block):
        # IndexValue = varint64 offset + varint64 size
        pos = 0
        off, pos = read_varint64(index_value, pos, len(index_value))
        size, pos = read_varint64(index_value, pos, len(index_value))
        try:
            block = read_block(data, BlockHandle(off, size))
        except Exception:
            continue
        for ikey, value in iter_block_entries(block):
            if len(ikey) < 8:
                continue
            packed = struct.unpack("<Q", ikey[-8:])[0]
            vtype = packed & 0xFF
            if vtype != 1:  # 0 = deletion
                continue
            yield ikey[:-8], value


def iter_sstable_file(path):
    with open(path, "rb") as f:
        data = f.read()
    yield from iter_sstable(data)
