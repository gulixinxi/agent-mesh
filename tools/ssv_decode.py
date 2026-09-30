#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
SSV（Structured Serialized Value）解码器
—— 解析 Chromium IndexedDB 中 V8 ValueSerializer 序列化的数据。

Chromium 从 M68 起，IndexedDB 的值统一走 Blink 的 SSV 格式：
    [0xFF 0x0F]  version tag = 15
    <V8 ValueSerializer payload>

本模块只做**只读解码**，用于 agent-mesh 确定豆包等 Electron/Chromium 系
客户端会话库的真实 schema（对应 GOAL.md 的 G-1）。

标签表取自 Chromium v8/src/value-serializer.cc 的 SerializationTag。
"""

import struct

# --- SerializationTag 常量 ---
T_VERSION = 0xFF
T_PADDING = 0x00
T_VERIFY_OBJECT_COUNT = 0x3F        # '?'
T_UNDEFINED = 0x5F                  # '_'
T_NULL = 0x30                       # '0'
T_TRUE = 0x54                       # 'T'
T_FALSE = 0x46                      # 'F'
T_TRUE_OBJECT = 0x79                # 'y'
T_FALSE_OBJECT = 0x78               # 'x'
T_NUMBER_OBJECT = 0x6E              # 'n'
T_STRING_OBJECT = 0x73              # 's'
T_INT32 = 0x49                      # 'I'
T_UINT32 = 0x55                     # 'U'
T_DOUBLE = 0x4E                     # 'N'
T_BIGINT = 0x5A                     # 'Z'
T_UTF8_STRING = 0x53                # 'S'
T_ONE_BYTE_STRING = 0x22            # '"'
T_TWO_BYTE_STRING = 0x63            # 'c'
T_OBJECT_REFERENCE = 0x5E           # '^'
T_BEGIN_JS_OBJECT = 0x6F            # 'o'
T_END_JS_OBJECT = 0x7B              # '{'
T_BEGIN_SPARSE_JS_OBJECT = 0x6A     # 'j'
T_END_SPARSE_JS_OBJECT = 0x7D       # '}'
T_BEGIN_DENSE_JS_ARRAY = 0x41       # 'A'
T_END_DENSE_JS_ARRAY = 0x40         # '@'
T_BEGIN_JS_MAP = 0x3B               # ';'
T_END_JS_MAP = 0x3A                 # ':'
T_BEGIN_JS_SET = 0x27               # "'"
T_END_JS_SET = 0x2C                 # ','
T_DATE = 0x44                       # 'D'


class SSVError(Exception):
    pass


def read_varint(buf, pos):
    """protobuf 风格 LEB128 varint"""
    result = 0
    shift = 0
    while True:
        if pos >= len(buf):
            raise SSVError("varint 越界")
        b = buf[pos]
        pos += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            break
        shift += 7
        if shift > 63:
            raise SSVError("varint 过长")
    return result, pos


def read_zigzag(buf, pos):
    v, pos = read_varint(buf, pos)
    return (v >> 1) ^ -(v & 1), pos


class Reader:
    def __init__(self, buf, pos=0):
        self.buf = buf
        self.pos = pos

    def byte(self):
        if self.pos >= len(self.buf):
            raise SSVError("读取 tag 越界")
        b = self.buf[self.pos]
        self.pos += 1
        return b

    def peek(self):
        if self.pos >= len(self.buf):
            raise SSVError("peek 越界")
        return self.buf[self.pos]

    def varint(self):
        v, self.pos = read_varint(self.buf, self.pos)
        return v

    def raw(self, n):
        end = self.pos + n
        if end > len(self.buf):
            raise SSVError("读取 %d 字节越界" % n)
        out = self.buf[self.pos:end]
        self.pos = end
        return out


class Decoder:
    """把 V8 ValueSerializer payload 解成 Python 值。

    不可识别的标签会被记录成 {"__tag__": 0xNN, "__bytes__": hex}，
    而不是抛异常中断——这样即使遇到 Blink 特有的 HostObject，
    也能把大部分可读内容取出来。
    """

    def __init__(self, max_depth=64):
        self.max_depth = max_depth
        self.notes = []

    def skip_version(self, r):
        if r.peek() == T_VERSION:
            r.byte()
            r.varint()  # version number

    def decode_value(self, r, depth=0):
        if depth > self.max_depth:
            return None
        tag = r.byte()
        return self._dispatch(tag, r, depth)

    def _dispatch(self, tag, r, depth):
        if tag in (T_PADDING, T_VERIFY_OBJECT_COUNT):
            # 无 payload；如果有 count 参数则跳过一个 varint
            if tag == T_VERIFY_OBJECT_COUNT:
                r.varint()
            return None
        if tag == T_UNDEFINED:
            return None
        if tag == T_NULL:
            return None
        if tag == T_TRUE:
            return True
        if tag == T_FALSE:
            return False
        if tag in (T_TRUE_OBJECT, T_FALSE_OBJECT):
            return bool(tag == T_TRUE_OBJECT)
        if tag in (T_NUMBER_OBJECT, T_STRING_OBJECT):
            return self.decode_value(r, depth + 1)
        if tag == T_INT32:
            return self._zigzag(r)
        if tag == T_UINT32:
            return r.varint()
        if tag == T_DOUBLE:
            return struct.unpack("<d", r.raw(8))[0]
        if tag == T_DATE:
            return {"__date__": struct.unpack("<d", r.raw(8))[0]}
        if tag == T_UTF8_STRING:
            return self._str(r, "utf-8")
        if tag == T_ONE_BYTE_STRING:
            return self._str(r, "latin-1")
        if tag == T_TWO_BYTE_STRING:
            return self._str_utf16(r)
        if tag == T_BEGIN_JS_OBJECT:
            return self._js_object(r, depth)
        if tag == T_BEGIN_SPARSE_JS_OBJECT:
            return self._sparse_object(r, depth)
        if tag == T_BEGIN_DENSE_JS_ARRAY:
            return self._dense_array(r, depth)
        if tag == T_BEGIN_JS_MAP:
            return self._js_map(r, depth)
        if tag == T_BEGIN_JS_SET:
            return self._js_set(r, depth)
        # 未知标签：记录后放弃本次解码（调用方会跳过该 value）
        raise SSVError("未知 tag 0x%02X @%d" % (tag, r.pos - 1))

    def _zigzag(self, r):
        v, r.pos = read_zigzag(r.buf, r.pos)
        return v

    def _str(self, r, enc):
        n = r.varint()
        return r.raw(n).decode(enc, "replace")

    def _str_utf16(self, r):
        n = r.varint()
        return r.raw(n * 2).decode("utf-16-le", "replace")

    def _js_object(self, r, depth):
        obj = {}
        while True:
            tag = r.peek()
            if tag == T_END_JS_OBJECT:
                r.byte()
                break
            key = self.decode_value(r, depth + 1)
            if key is None or r.pos >= len(r.buf):
                break
            val = self.decode_value(r, depth + 1)
            obj[str(key)] = val
        return obj

    def _sparse_object(self, r, depth):
        obj = {}
        n = r.varint()
        for _ in range(n):
            key = self.decode_value(r, depth + 1)
            val = self.decode_value(r, depth + 1)
            obj[str(key)] = val
        return obj

    def _dense_array(self, r, depth):
        n = r.varint()
        arr = [self.decode_value(r, depth + 1) for _ in range(n)]
        # 跳过 kEndDenseJSArray
        tag = r.byte()
        if tag != T_END_DENSE_JS_ARRAY:
            raise SSVError("期望 dense array 结束标记，实际 0x%02X" % tag)
        nprop = r.varint()
        props = {}
        for _ in range(nprop):
            k = self.decode_value(r, depth + 1)
            v = self.decode_value(r, depth + 1)
            props[str(k)] = v
        if props:
            arr.append({"__props__": props})
        arr_len = r.varint()
        if arr_len != len(arr) and arr_len != n:
            pass  # 长度不一致但不致命
        return arr

    def _js_map(self, r, depth):
        out = {}
        while True:
            tag = r.peek()
            if tag == T_END_JS_MAP:
                r.byte()
                break
            k = self.decode_value(r, depth + 1)
            v = self.decode_value(r, depth + 1)
            out[str(k)] = v
        return {"__map__": out}

    def _js_set(self, r, depth):
        out = []
        while True:
            tag = r.peek()
            if tag == T_END_JS_SET:
                r.byte()
                break
            out.append(self.decode_value(r, depth + 1))
        return {"__set__": out}

    # ---------------------------------------------------------------- 入口
    def decode(self, payload):
        """payload 必须以 [0xFF, 0x0F] 开头"""
        if len(payload) < 2 or payload[0] != 0xFF or payload[1] != 0x0F:
            raise SSVError("不是 SSV payload（缺 0xFF 0x0F 版本前缀）")
        r = Reader(payload, 2)
        self.skip_version(r)
        return self.decode_value(r)


def find_ssv_starts(value):
    """在 IndexedDB 原始 value 中找出所有 SSV payload 可能的起始位置。

    正常形态：value 开头就是 0xFF 0x0F。但 Blink 会在前面加少量内部头
    （见过 16 字节前缀的情形），所以这里把所有 0xFF 0x0F 的位置照单列出，
    由上层的「试解 + 择优」策略决定用哪一个。
    """
    starts = []
    if len(value) >= 2 and value[0] == 0xFF and value[1] == 0x0F:
        starts.append(0)
    i = value.find(b"\xff\x0f")
    while i != -1:
        if i not in starts:
            starts.append(i)
        i = value.find(b"\xff\x0f", i + 1)
    return starts[:8]  # 最多试 8 个位置，避免在大二进制里浪费时间


def try_decode(value):
    """容错解码：从任一 SSV 起点解出 Python 值。返回 (值, start) 或 (None, None)。"""
    for start in find_ssv_starts(value):
        try:
            dec = Decoder()
            return dec.decode(value[start:]), start
        except Exception:
            continue
    return None, None


# ---------------------------------------------------------------- 结构摘要
def shape(obj, _depth=0):
    """把一个解码后的 Python 值压成结构摘要：只保留 key 路径与标量类型。"""
    if _depth > 8:
        return "..."
    if isinstance(obj, dict):
        if obj.get("__map__") is not None and isinstance(obj.get("__map__"), dict):
            return {"__map__": shape(obj["__map__"], _depth + 1)}
        if "__set__" in obj:
            return {"__set__": shape(obj["__set__"][0], _depth + 1) if obj["__set__"] else "empty"}
        return {k: shape(v, _depth + 1) for k, v in list(obj.items())[:40]}
    if isinstance(obj, list):
        if not obj:
            return "[]"
        return [shape(obj[0], _depth + 1)]
    if isinstance(obj, str):
        return "str(%d)" % len(obj)
    if isinstance(obj, bool):
        return "bool"
    if isinstance(obj, float):
        return "float"
    if isinstance(obj, int):
        return "int"
    return type(obj).__name__
