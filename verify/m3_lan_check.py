# -*- coding: utf-8 -*-
"""M3 跨机组网自动检查（对应 docs/m3-lan-test-plan.md 的 T1/T2）。

在任意能访问服务端的机器上运行，验证：
  1. healthz 可达
  2. HMAC 设备列表中目标节点在线（并打印其 IP 与能力）
  3. 跨机任务闭环：create → 领取 → 回传 completed

用法（在服务端机器或运维机上跑）：
  python verify/m3_lan_check.py --server http://192.168.2.131:8080 \
      --secret "<安装时生成的集群密钥>" --node NODE-02

  可选：
    --skip-task      只查拓扑不跑任务闭环
    --task-wait 300  任务回传等待秒数（默认 300，须大于服务端 task-timeout 的回收节奏）
    --timeout-secs   单请求超时（默认 15；跨机首包慢时可调大）

退出码：全部通过 0，任一失败 1。
"""
import argparse
import hashlib
import hmac
import json
import sys
import time
import urllib.error
import urllib.request

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("[PASS] " if ok else "[FAIL] ") + name + (" | " + detail if detail else ""))


def no_proxy_opener():
    """系统代理会拦内网地址（实测返回 502），必须显式绕开。"""
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def req(server, path, method="GET", body_obj=None, headers=None, timeout=15):
    url = server.rstrip("/") + path
    data = json.dumps(body_obj).encode() if body_obj is not None else None
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


def signed_headers(secret, path, method, body_bytes=b""):
    """与服务端 auth.go 一致：method \n path \n ts \n nonce \n sha256(body)。"""
    ts = str(int(time.time()))
    nonce = "n" + str(int(time.time() * 1000))
    raw = "\n".join([method, path, ts, nonce, hashlib.sha256(body_bytes).hexdigest()])
    sig = hmac.new(secret.encode(), raw.encode(), hashlib.sha256).hexdigest()
    return {"X-Mesh-Timestamp": ts, "X-Mesh-Nonce": nonce, "X-Mesh-Signature": sig}


def hmac_req(server, secret, path, method="GET", body_obj=None, timeout=15):
    payload = json.dumps(body_obj).encode() if body_obj is not None else b""
    headers = signed_headers(secret, path, method, payload)
    return req(server, path, method, body_obj, headers, timeout)


def main():
    ap = argparse.ArgumentParser(description="Agent Mesh M3 跨机检查")
    ap.add_argument("--server", required=True, help="如 http://192.168.2.131:8080")
    ap.add_argument("--secret", required=True, help="集群 HMAC 密钥（安装时生成）")
    ap.add_argument("--node", default="", help="期望在线的客户端节点 ID（空则任一在线即可）")
    ap.add_argument("--skip-task", action="store_true", help="跳过任务闭环，只查拓扑")
    ap.add_argument("--task-wait", type=int, default=300, help="任务回传等待秒数")
    ap.add_argument("--timeout-secs", type=int, default=15, help="单请求超时")
    args = ap.parse_args()

    server, secret = args.server, args.secret

    # 1. healthz
    code, _ = req(server, "/healthz", timeout=args.timeout_secs)
    check("healthz 可达", code == 200, "实际 %s" % code)
    if code != 200:
        finish()

    # 2. 设备拓扑：目标节点在线
    deadline = time.time() + 30
    devices, node_info = None, None
    while time.time() < deadline:
        code, body = hmac_req(server, secret, "/api/v1/cluster/devices",
                              timeout=args.timeout_secs)
        if code == 200:
            try:
                devices = json.loads(body)
            except Exception:
                devices = None
        if devices:
            lst = devices if isinstance(devices, list) else devices.get("devices", [])
            for d in lst:
                # 服务端 DeviceResponse 字段：client_id / status / ip_address / agents
                did = str(d.get("client_id") or "")
                status = str(d.get("status") or "")
                if status == "online" and (not args.node or did == args.node):
                    node_info = d
                    break
            if node_info:
                break
        time.sleep(3)
    if node_info:
        ip = node_info.get("ip_address") or "?"
        agents = node_info.get("agents") or []
        if isinstance(agents, list):
            kinds = [a.get("kind") for a in agents if isinstance(a, dict)] or agents
        else:
            kinds = agents
        check("节点在线（%s）" % (args.node or "任一"), True,
              "IP=%s 能力=%s" % (ip, kinds))
    else:
        check("节点在线（%s）" % (args.node or "任一"), False,
              "30s 内未在设备列表发现在线节点")
        finish()

    if args.skip_task:
        finish()

    # 3. 跨机任务闭环
    marker = "M3-LAN-CHECK %s" % time.strftime("%Y-%m-%d %H:%M:%S")
    path = "/api/v1/tasks/create"
    code, body = hmac_req(server, secret, path, "POST",
                          {"prompt": marker, "max_attempts": 1},
                          timeout=args.timeout_secs)
    task_id = ""
    if code == 200:
        try:
            task_id = json.loads(body).get("task_id", "")
        except Exception:
            task_id = ""
    check("任务创建成功", code == 200 and bool(task_id),
          "HTTP %s task_id=%s" % (code, task_id))
    if not task_id:
        finish()

    deadline = time.time() + args.task_wait
    status, result = "pending", ""
    while time.time() < deadline:
        code, body = hmac_req(server, secret, "/api/v1/tasks?limit=50",
                              timeout=args.timeout_secs)
        if code == 200:
            try:
                tasks = json.loads(body)
                tasks = tasks if isinstance(tasks, list) else tasks.get("tasks", [])
                for t in tasks:
                    if t.get("task_id") == task_id:
                        status = str(t.get("status") or "")
                        result = str(t.get("result") or "")[:120]
                        break
            except Exception:
                pass
        if status in ("completed", "failed", "timeout"):
            break
        time.sleep(5)

    check("任务回传 completed（跨机闭环）", status == "completed",
          "最终状态 %s，结果片段: %s" % (status, result))
    finish()


def finish():
    print("\n=== 结果: %d/%d 通过 ===" % (len(PASS), len(PASS) + len(FAIL)))
    if FAIL:
        print("失败项: " + ", ".join(FAIL))
        sys.exit(1)
    sys.exit(0)


if __name__ == "__main__":
    main()
