# -*- coding: utf-8 -*-
"""中转文件接口的端到端验证。

为什么需要它：这套接口跨了四条容易悄悄坏掉的链路——
  1. 上传是流式的，签名只用「声明的摘要」，实体是否相符必须服务端自己兜底；
  2. 文件名是不可信输入，会进入响应头与控制台展示；
  3. 定向投递的文件不能被其它节点看到或取走；
  4. 删除要同时清掉元数据与磁盘实体。
这些点写在单测里只能验到函数级；只有在真服务、真 HTTP 上跑一遍，
才能确认「签名规范串两端一致」「Content-Length 提前拒绝」「Content-Disposition
编码」这些跨端约定没有一处对不上。

用法：python verify/files_check.py
"""
import base64
import hashlib
import hmac
import itertools
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, "bin", "server", "server.exe")
PORT = 8231
BASE = "http://127.0.0.1:%d" % PORT
SECRET = "files-verify-secret"
STORE_DIR = os.path.join(ROOT, ".files_store")

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("[PASS] " if ok else "[FAIL] ") + name + (" | " + detail if detail else ""))


def no_proxy_opener():
    """系统代理会把 127.0.0.1 的请求也拦走，必须显式绕开。"""
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def start_server():
    for name in (".files_check.db", ".files_check.db-wal", ".files_check.db-shm"):
        p = os.path.join(ROOT, name)
        if os.path.exists(p):
            os.remove(p)
    if os.path.isdir(STORE_DIR):
        for f in os.listdir(STORE_DIR):
            fp = os.path.join(STORE_DIR, f)
            if os.path.isfile(fp):
                os.remove(fp)

    cmd = [EXE,
           "-addr", "127.0.0.1:%d" % PORT,
           "-db", os.path.join(ROOT, ".files_check.db"),
           "-log-dir", ".files_check",
           "-files-dir", STORE_DIR,
           "-secret", SECRET,
           "-console-user", "admin", "-console-pass", "pw123"]
    log = open(os.path.join(ROOT, ".files_check.proc.log"), "w")
    proc = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT,
                            creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0))
    for _ in range(60):
        try:
            urllib.request.urlopen(BASE + "/healthz", timeout=1)
            return proc
        except Exception:
            time.sleep(0.3)
    return proc


def stop(proc):
    try:
        proc.send_signal(getattr(subprocess, "CTRL_BREAK_EVENT", 2))
    except Exception:
        pass
    time.sleep(0.8)
    if proc.poll() is None:
        proc.kill()


def hmac_headers(method, path, digest_hex):
    """按服务端规范构造签名头：method\\npath\\nts\\nnonce\\n<实体摘要>。"""
    ts = str(int(time.time()))
    # nonce 必须严格唯一：同一毫秒内连发多个请求时，纯时间戳会撞车，
    # 服务端会把后一个当成重放而拒绝（401），伪装成签名问题极难排查。
    nonce = "n%d-%d" % (int(time.time() * 1000), next(_NONCE_SEQ))
    raw = "\n".join([method, path, ts, nonce, digest_hex])
    sig = hmac.new(SECRET.encode(), raw.encode(), hashlib.sha256).hexdigest()
    return {"X-Mesh-Timestamp": ts, "X-Mesh-Nonce": nonce, "X-Mesh-Signature": sig}


_NONCE_SEQ = itertools.count()


def hget(headers, name):
    """按名字取响应头，大小写不敏感。

    Go 会把响应头名规范化（X-Mesh-File-SHA256 -> X-Mesh-File-Sha256），
    直接按原样取值会拿到空串，看起来像「服务端没发这个头」，实则只是大小写。
    """
    for k, v in headers.items():
        if k.lower() == name.lower():
            return v
    return ""


def call(path, method="GET", body=None, headers=None, timeout=30):
    """通用请求。body 为 bytes 时原样发送（文件上传），为 dict 时按 JSON 发送。

    /api/v1 下的接口一律需要 HMAC 签名，这里统一代签；调用方显式传了
    X-Mesh-Signature 时以调用方为准（上传用例要构造特定签名，就靠这条）。
    """
    if isinstance(body, dict):
        data = json.dumps(body).encode()
        ctype = "application/json"
    else:
        data = body
        ctype = "application/octet-stream"

    hdrs = dict(headers or {})
    sign_path = path.split("?", 1)[0]
    if sign_path.startswith("/api/v1/") and "X-Mesh-Signature" not in hdrs:
        body_bytes = data if isinstance(data, (bytes, bytearray)) else b""
        hdrs.update(hmac_headers(method, sign_path, hashlib.sha256(body_bytes).hexdigest()))

    req = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", ctype)
    for k, v in hdrs.items():
        req.add_header(k, v)
    try:
        with no_proxy_opener().open(req, timeout=timeout) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)
    except Exception as e:
        return 0, str(e).encode(), {}


def upload(name, content, target="", declared_digest=None, sign_digest=None, fake_length=None):
    """上传文件。declared_digest / sign_digest 分开传，以构造「签名合法但实体被替换」。"""
    path = "/api/v1/files/upload"
    real = hashlib.sha256(content).hexdigest()
    declared = declared_digest or real
    signed = sign_digest or declared

    headers = hmac_headers("POST", path, signed)
    headers["X-Mesh-File-Name"] = urllib.parse.quote(name)
    headers["X-Mesh-Content-SHA256"] = declared
    headers["X-Mesh-File-Uploader"] = urllib.parse.quote("NODE-A")
    if target:
        headers["X-Mesh-File-Target"] = urllib.parse.quote(target)
    if fake_length is not None:
        headers["Content-Length"] = str(fake_length)

    return call(path, "POST", content, headers)


def list_files(node, pending=False):
    q = "/api/v1/files?node=" + urllib.parse.quote(node)
    if pending:
        q += "&pending=1"
    code, body, _ = call(q)
    if code != 200:
        return code, None
    return code, json.loads(body)


def main():
    proc = start_server()
    try:
        # ---- 1. 正常闭环：上传 -> 目标可见 -> 下载 ----
        content = "季度经营报表的内容".encode("utf-8")
        code, body, _ = upload("季度报表.xlsx", content, target="NODE-B")
        ok = code == 200 and b"file_id" in body
        check("上传成功且返回 file_id", ok, "实际 %s" % code)
        if not ok:
            raise SystemExit("上传失败，后续用例无法继续: %s" % body[:200])
        meta = json.loads(body)
        fid = meta["file_id"]

        check("中文文件名原样保留", meta["file_name"] == "季度报表.xlsx", "实际 %r" % meta["file_name"])
        check("大小与摘要正确",
              meta["size"] == len(content) and meta["sha256"] == hashlib.sha256(content).hexdigest(),
              "size=%s" % meta["size"])
        check("磁盘实体已落盘", os.path.isfile(os.path.join(STORE_DIR, fid)))

        code, files = list_files("NODE-B")
        check("目标节点可见该文件", code == 200 and len(files) == 1, "实际 %s 条" % (len(files) if files is not None else "?"))

        code, files = list_files("NODE-C")
        check("非目标节点看不到定向文件", code == 200 and len(files) == 0,
              "NODE-C 看到 %s 条" % (len(files) if files is not None else "?"))

        code, body, hdrs = call("/api/v1/files/download?file_id=%s&node=NODE-B" % fid)
        check("目标节点可下载且内容一致",
              code == 200 and body == content, "实际 %s，内容长度 %d" % (code, len(body)))
        cd = hget(hdrs, "Content-Disposition")
        check("下载响应带 UTF-8 文件名与摘要头",
              "filename*=UTF-8''" in cd and hget(hdrs, "X-Mesh-File-SHA256") == meta["sha256"],
              "CD=%r, SHA=%r" % (cd, hget(hdrs, "X-Mesh-File-SHA256")))

        code, files = list_files("NODE-B", pending=True)
        check("领取后不再出现在待领取列表", code == 200 and len(files) == 0,
              "实际 %s 条" % (len(files) if files is not None else "?"))

        # ---- 2. 越权下载 ----
        code, _, _ = call("/api/v1/files/download?file_id=%s&node=NODE-C" % fid)
        check("非目标节点下载被拒 403", code == 403, "实际 %s" % code)

        # ---- 3. 内容与声明摘要不一致（签名覆盖声明摘要，实体被替换）----
        original = "原始内容".encode("utf-8")
        code, body, _ = upload("tampered.txt", "被替换的内容".encode("utf-8"),
                               declared_digest=hashlib.sha256(original).hexdigest())
        check("实体与声明摘要不符被拒 400", code == 400, "实际 %s" % code)
        leftovers = [f for f in os.listdir(STORE_DIR)
                     if f != "tmp" and f != fid and os.path.isfile(os.path.join(STORE_DIR, f))]
        check("拒绝后不留磁盘残留", not leftovers, "残留: %s" % leftovers)

        # ---- 4. 签名错误 ----
        code, body, _ = upload("bad.txt", b"x", sign_digest="0" * 64)
        check("错误签名被拒 403", code == 403, "实际 %s" % code)

        # ---- 5. 路径穿越文件名被收敛（不落盘到目录外，也不会带路径成分进响应头）----
        code, body, _ = upload("../../evil.txt", b"traversal", target="NODE-B")
        ok = code == 200 and json.loads(body)["file_name"] == "evil.txt"
        check("路径穿越文件名被收敛为纯文件名", ok,
              "实际 %s / %r" % (code, json.loads(body).get("file_name") if code == 200 else body[:120]))

        # ---- 6. 超限：伪造一个超过上限的 Content-Length ----
        # 服务端一发现声明长度超限就中断，客户端还在发包时会收到连接重置（code=0）。
        # 对调用方而言同样是被拒；关键是别把超大载荷读进内存。
        code, body, _ = upload("huge.bin", b"small", fake_length=513 * 1024 * 1024)
        check("声明超限的上传被拒（413 或连接重置）", code in (413, 0), "实际 %s" % code)
        alive, _, _ = call("/healthz")
        check("超限被拒后服务仍存活", alive == 200, "实际 %s" % alive)

        # ---- 7. 广播文件对所有节点可见 ----
        code, body, _ = upload("公告.pdf", b"broadcast")
        bid = json.loads(body)["file_id"] if code == 200 else ""
        ok_all = all(len(list_files(n)[1] or []) >= 1 for n in ("NODE-B", "NODE-C"))
        check("广播文件对所有节点可见", code == 200 and ok_all, "上传 %s" % code)
        code, _, _ = call("/api/v1/files/download?file_id=%s&node=NODE-C" % bid)
        check("广播文件任意节点可下载", code == 200, "实际 %s" % code)

        # ---- 8. 控制台可总览并直接取回（任务产出文件的出口）----
        auth = {"Authorization": "Basic " + base64.b64encode(b"admin:pw123").decode()}

        code, _, _ = call("/console/api/files")
        check("控制台无凭据被拒 401", code == 401, "实际 %s" % code)

        code, body, _ = call("/console/api/files", headers=auth)
        total = len(json.loads(body)) if code == 200 else -1
        check("控制台可总览中转文件", code == 200 and total >= 2, "实际 %s 条" % total)

        code, body, _ = call("/console/api/files/download?file_id=%s" % fid, headers=auth)
        check("控制台可直接下载定向文件", code == 200 and body == content, "实际 %s" % code)

        # ---- 9. 删除：元数据与磁盘实体一并清掉 ----
        code, _, _ = call("/api/v1/files/delete", "POST", {"file_id": bid})
        check("删除成功", code == 200, "实际 %s" % code)
        check("删除后磁盘实体已移除", not os.path.isfile(os.path.join(STORE_DIR, bid)))
        code, _, _ = call("/api/v1/files/download?file_id=%s&node=NODE-C" % bid)
        check("删除后下载 404", code == 404, "实际 %s" % code)

    finally:
        stop(proc)
        for name in (".files_check.db", ".files_check.db-wal", ".files_check.db-shm"):
            p = os.path.join(ROOT, name)
            if os.path.exists(p):
                try:
                    os.remove(p)
                except Exception:
                    pass

    print("\n=== 结果: %d/%d 通过 ===" % (len(PASS), len(PASS) + len(FAIL)))
    if FAIL:
        print("失败项: " + ", ".join(FAIL))
        sys.exit(1)


main()
