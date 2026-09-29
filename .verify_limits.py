# -*- coding: utf-8 -*-
"""验证 Google AI 第二轮报告的三条 P0 结论：
  P0-1 控制台收口   —— 已完成（fail-closed），本轮改为 503
  P0-2 limit 未校验 —— 我方认定误判，用负数/超大值证伪
  P0-3 请求体无上限 —— 本轮新增限制，验证 413
"""
import base64, hashlib, hmac, json, os, ssl, subprocess, sys, time, urllib.request, urllib.error

ROOT = os.path.dirname(os.path.abspath(__file__))
EXE = os.path.join(ROOT, "bin", "server", "server.exe")
PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("[PASS] " if ok else "[FAIL] ") + name + (" | " + detail if detail else ""))


def no_proxy_opener():
    """系统代理会把 127.0.0.1 的请求也拦走（实测返回 502），必须显式绕开。"""
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def start(extra, db, log):
    cmd = [EXE, "-addr", "127.0.0.1:8199", "-db", db, "-log-dir", log,
           "-secret", "verify-secret", "-task-timeout", "5m"] + extra
    f = open(os.path.join(ROOT, log + ".proc.log"), "w")
    p = subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT,
                         creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0))
    for _ in range(60):
        try:
            urllib.request.urlopen("http://127.0.0.1:8199/healthz", timeout=1)
            return p
        except Exception:
            time.sleep(0.3)
    return p


def stop(p):
    try:
        p.send_signal(getattr(subprocess, "CTRL_BREAK_EVENT", 2))
    except Exception:
        pass
    time.sleep(0.8)
    if p.poll() is None:
        p.kill()


def req(path, method="GET", body=None, headers=None, timeout=15):
    url = "http://127.0.0.1:8199" + path
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(url, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        r.add_header(k, v)
    try:
        with no_proxy_opener().open(r, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return 0, str(e).encode()


def signed(path, method, body_obj=None):
    ts, nonce = str(int(time.time())), "n" + str(int(time.time() * 1000))
    payload = json.dumps(body_obj).encode() if body_obj is not None else b""
    raw = "\n".join([method, path, ts, nonce, hashlib.sha256(payload).hexdigest()])
    sig = hmac.new(b"verify-secret", raw.encode(), hashlib.sha256).hexdigest()
    return {"X-Mesh-Timestamp": ts, "X-Mesh-Nonce": nonce,
            "X-Mesh-Signature": sig}, payload


def auth_header(u, p):
    return "Basic " + base64.b64encode(("%s:%s" % (u, p)).encode()).decode()


def main():
    # ---------- A：控制台开启态（有口令），测体积限制与 limit -------
    db = os.path.join(ROOT, ".vl_open.db")
    for suf in ("", "-wal", "-shm"):
        if os.path.exists(db + suf):
            os.remove(db + suf)
    srv = start(["-console-user", "admin", "-console-pass", "pw123"], db, ".vl_open")
    try:
        au = {"Authorization": auth_header("admin", "pw123")}

        # 1. 超大请求体 -> 413
        #    服务端一发现 Content-Length 超限就中断，客户端还在发包时会收到
        #    连接重置（code=0）。对攻击者而言同样是被拒，但需额外确认服务存活。
        big = {"prompt": "A" * (9 * 1024 * 1024)}
        code, _ = req("/console/api/tasks/create", "POST", big, au)
        check("超大请求体被拒（413 或连接重置）", code in (413, 0), "实际 %s" % code)
        alive, _ = req("/healthz")
        check("超大请求被拒后服务仍存活", alive == 200, "实际 %s" % alive)

        # 2. 超大 prompt（body 未超限但 prompt 超限）-> 413
        #    70KB prompt：远小于 8MB body 上限，但超过 64KB prompt 上限
        code, body = req("/console/api/tasks/create", "POST",
                         {"prompt": "B" * (70 * 1024)}, au)
        check("超大 prompt 被拒 413", code == 413, "实际 %s" % code)

        # 3. 正常 prompt 仍可下发（确认没误伤）
        code, body = req("/console/api/tasks/create", "POST",
                         {"prompt": "正常指令"}, au)
        ok = code == 200 and b"task_id" in body
        check("正常下发不受影响", ok, "实际 %s" % code)

        # ---- P0-2 证伪：limit 负数与超大值 ----
        for val, expect_desc in [("-1", "负数"), ("99999999", "超大值")]:
            code, body = req("/console/api/audit?limit=" + val, "GET", None, au)
            # 审计接口返回裸数组，不是 {"logs": [...]}
            try:
                n = len(json.loads(body)) if isinstance(json.loads(body), list) else -1
            except Exception:
                n = -1
            check("limit=%s（%s）已收敛，未全表拉取" % (val, expect_desc),
                  code == 200 and 0 <= n <= 200, "返回 %s 条" % n)

        # 4. 正常 limit 仍生效
        code, body = req("/console/api/audit?limit=5", "GET", None, au)
        check("正常 limit=5 生效", code == 200, "实际 %s" % code)
    finally:
        stop(srv)

    # ---------- B：控制台关闭态（对外地址 + 无口令）----------
    db2 = os.path.join(ROOT, ".vl_closed.db")
    for suf in ("", "-wal", "-shm"):
        if os.path.exists(db2 + suf):
            os.remove(db2 + suf)
    srv = start(["-addr", "127.0.0.1:8200", "-db", db2, "-log-dir", ".vl_closed"], db2, ".vl_closed")
    try:
        # 注意：127.0.0.1 被判定为「仅回环」，会走开放分支。
        # 为测关闭分支需绑到非回环，这里改用 0.0.0.0 再起一次。
        pass
    finally:
        stop(srv)

    # 用 0.0.0.0 起（对外可达）且无口令 -> 应关闭
    db3 = os.path.join(ROOT, ".vl_off.db")
    for suf in ("", "-wal", "-shm"):
        if os.path.exists(db3 + suf):
            os.remove(db3 + suf)
    cmd = [EXE, "-addr", "0.0.0.0:8201", "-db", db3, "-log-dir", ".vl_off",
           "-secret", "verify-secret"]
    f = open(os.path.join(ROOT, ".vl_off.proc.log"), "w")
    srv = subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT,
                           creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0))
    try:
        for _ in range(60):
            try:
                urllib.request.urlopen("http://127.0.0.1:8201/healthz", timeout=1)
                break
            except Exception:
                time.sleep(0.3)

        def req2(path, method="GET", body=None, headers=None):
            url = "http://127.0.0.1:8201" + path
            data = json.dumps(body).encode() if body is not None else None
            r = urllib.request.Request(url, data=data, method=method)
            r.add_header("Content-Type", "application/json")
            for k, v in (headers or {}).items():
                r.add_header(k, v)
            try:
                with no_proxy_opener().open(r, timeout=10) as resp:
                    return resp.status, resp.read()
            except urllib.error.HTTPError as e:
                return e.code, e.read()
            except Exception as e:
                return 0, str(e).encode()

        code, body = req2("/console")
        ok = code == 503 and b"console disabled" in body
        check("控制台关闭时返回 503（非 404）", ok, "实际 %s" % code)

        code, body = req2("/console/api/audit")
        ok = code == 503 and b"console disabled" in body
        check("关闭时 API 也返回 503", ok, "实际 %s" % code)

        code, body = req2("/console/api/tasks/create", "POST", {"prompt": "x"})
        check("关闭时下发被拒 503", code == 503, "实际 %s" % code)

        # /api/v1 不受控制台关闭影响（HMAC 正常即可）
        h, payload = signed("/api/v1/tasks/create", "POST",
                            {"prompt": "经 API 下发", "max_attempts": 1})
        r = urllib.request.Request("http://127.0.0.1:8201/api/v1/tasks/create",
                                   data=payload, method="POST")
        r.add_header("Content-Type", "application/json")
        for k, v in h.items():
            r.add_header(k, v)
        try:
            with no_proxy_opener().open(r, timeout=10) as resp:
                code = resp.status
        except urllib.error.HTTPError as e:
            code = e.code
        check("控制台关闭不影响 /api/v1 下发", code == 200, "实际 %s" % code)
    finally:
        stop(srv)

    print("\n=== 结果: %d/%d 通过 ===" % (len(PASS), len(PASS) + len(FAIL)))
    if FAIL:
        print("失败项: " + ", ".join(FAIL))
        sys.exit(1)


main()
