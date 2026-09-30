# -*- coding: utf-8 -*-
"""自助入网链路的端到端验证（签发 -> 落地页 -> 一键安装 -> 自检回传）。

为什么必须做端到端，而不是只跑单测：
  这条链路的每一段单测都是绿的，但真正会坏的地方全在**跨端约定**上：
    · 服务端渲染的安装脚本里，邀请码/地址/客户端下载路径是否真的对；
    · 客户端 enroll 打出去的心跳签名，服务端是否认；
    · 「取配置包」到底有没有把名额扣掉、同机重取会不会被误拒；
    · 自检结果有没有真的落进控制台能看到的那张表。
  这些只有把两个真二进制跑起来、隔着真 HTTP 对一遍才能确认。

用法：python verify/enroll_check.py
"""
import base64
import json
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER_DIR = os.path.join(ROOT, "agent-mesh-server")
CLIENT_DIR = os.path.join(ROOT, "agent-mesh-client")
OUT_DIR = os.path.join(ROOT, ".enroll_check")
SERVER_EXE = os.path.join(OUT_DIR, "server.exe")
CLIENT_EXE = os.path.join(OUT_DIR, "client.exe")
PACK_DIR = os.path.join(OUT_DIR, "pack")
SECRET = "enroll-verify-secret"
CONSOLE_USER, CONSOLE_PASS = "admin", "pw123"

GO = os.environ.get("GO_BIN") or r"D:\guli\tools\go\bin\go.exe"
if not os.path.exists(GO):
    GO = "go"

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("[PASS] " if ok else "[FAIL] ") + name + (" | " + detail if detail else ""))


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


PORT = free_port()
BASE = "http://127.0.0.1:%d" % PORT


def opener():
    """系统代理会把 127.0.0.1 的请求也拦走，必须显式绕开。"""
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


OP = opener()


def auth_header():
    raw = ("%s:%s" % (CONSOLE_USER, CONSOLE_PASS)).encode()
    return "Basic " + base64.b64encode(raw).decode()


def call(path, method="GET", body=None, timeout=30, console=False, host=None):
    """返回 (status, headers, bytes)。4xx/5xx 不抛异常，交给调用方判断。

    host 用来伪造 Host 头：落地页给出的地址是跟着「请求 Host」走的，
    要验证「客户机视角看到的页面」就必须能换 Host。
    """
    url = path if path.startswith("http") else BASE + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if console:
        headers["Authorization"] = auth_header()
    if host:
        headers["Host"] = host
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with OP.open(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


def build():
    os.makedirs(OUT_DIR, exist_ok=True)
    os.makedirs(PACK_DIR, exist_ok=True)
    for name, src, exe in (("服务端", SERVER_DIR, SERVER_EXE), ("客户端", CLIENT_DIR, CLIENT_EXE)):
        r = subprocess.run([GO, "build", "-o", exe, "."], cwd=src,
                           capture_output=True, text=True)
        if r.returncode != 0:
            print("[构建] %s 失败:\n%s" % (name, r.stderr))
            sys.exit(2)
    # 客户端目录里放一份按约定命名的副本，模拟真实交付时的客户端分发包
    shutil.copy2(CLIENT_EXE, os.path.join(PACK_DIR, "client-windows-amd64.exe"))


def start_server():
    for name in (".enroll.db", ".enroll.db-wal", ".enroll.db-shm"):
        p = os.path.join(OUT_DIR, name)
        if os.path.exists(p):
            os.remove(p)
    cmd = [SERVER_EXE,
           "-addr", "127.0.0.1:%d" % PORT,
           "-db", os.path.join(OUT_DIR, ".enroll.db"),
           "-files-dir", os.path.join(OUT_DIR, "files"),
           "-client-pack", PACK_DIR,
           "-log-dir", os.path.join(OUT_DIR, "logs"),
           "-secret", SECRET,
           "-console-user", CONSOLE_USER, "-console-pass", CONSOLE_PASS]
    log = open(os.path.join(OUT_DIR, "server.log"), "w")
    proc = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT,
                            creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0))
    for _ in range(60):
        try:
            with OP.open(BASE + "/healthz", timeout=1) as r:
                if r.status == 200:
                    return proc
        except Exception:
            time.sleep(0.3)
    print("[启动] 服务端未能在超时内就绪，见 %s" % os.path.join(OUT_DIR, "server.log"))
    proc.kill()
    sys.exit(2)


def stop(proc):
    try:
        proc.send_signal(getattr(subprocess, "CTRL_BREAK_EVENT", 2))
        proc.wait(timeout=8)
    except Exception:
        try:
            proc.kill()
        except Exception:
            pass


def issue(label, ttl_minutes, max_uses):
    status, _, body = call("/console/api/invites", "POST",
                           {"label": label, "ttl_minutes": ttl_minutes, "max_uses": max_uses},
                           console=True)
    if status != 200:
        check("签发邀请码", False, "HTTP %d %s" % (status, body[:200]))
        return None
    return json.loads(body)


def run_enroll(code, install_dir, data_dir, extra=None):
    """用真实客户端二进制跑一次接入（--no-service，不触碰系统服务）。"""
    cmd = [CLIENT_EXE, "enroll",
           "--server", BASE, "--code", code,
           "--install-dir", install_dir, "--data-dir", data_dir,
           "--no-service",
           "--id", os.environ.get("COMPUTERNAME", "verify-node")]
    if extra:
        cmd += extra
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
    return r.returncode, r.stdout + r.stderr


def main():
    build()
    proc = start_server()
    hostname = os.environ.get("COMPUTERNAME", "verify-node")
    try:
        # ---------- 1. 签发 ----------
        inv = issue("前台工位", 30, 1)
        if inv is None:
            return
        code = inv["code"]
        norm = code.replace("-", "")
        check("签发邀请码返回明文码与命令", bool(code) and "install.ps1" in inv["windows_cmd"]
              and "install.sh" in inv["linux_cmd"], code)
        check("签发时客户端已就绪（client_ready=true）", inv.get("client_ready") is True,
              str(inv.get("client_ready")))
        check("一码一机时 max_uses=1", inv.get("max_uses") == 1, str(inv.get("max_uses")))

        # ---------- 2. 落地页 ----------
        st, _, body = call("/join/" + norm)
        page = body.decode("utf-8", "replace")
        check("落地页可打开", st == 200, "HTTP %d" % st)
        check("落地页展示邀请码", code in page)
        check("落地页标为可用", "可用" in page)
        check("落地页给出 Windows 与 Linux 两条命令",
              "/install.ps1" in page and "/install.sh" in page)
        snap = os.path.join(OUT_DIR, "landing.html")
        with open(snap, "w", encoding="utf-8") as f:
            f.write(page)

        # 地址来源：本地开发机从 127.0.0.1 打开，页面必须警告「只对本机有效」，
        # 否则管理员会把一份只对自己有效的命令发给客户机。
        check("回环地址下警告该地址只对本机有效",
              "只有在「中枢这台机器」上才有效" in page)
        check("回环地址下给出「发给客户让他自己打开」的指引",
              "把下面的链接发给客户" in page)

        # 客户机视角：Host 换成内网地址，页面给出的地址必须跟着变、且不再警告。
        lan_host = "192.168.1.20:4024"
        st, _, body = call("/join/" + norm, host=lan_host)
        lan_page = body.decode("utf-8", "replace")
        check("以客户机看到的内网地址打开落地页正常", st == 200, "HTTP %d" % st)
        check("内网地址下命令里就是该地址",
              "http://%s/join/%s" % (lan_host, norm) in lan_page)
        check("内网地址下不再出现回环警告",
              "只有在「中枢这台机器」上才有效" not in lan_page)
        with open(os.path.join(OUT_DIR, "landing-lan.html"), "w", encoding="utf-8") as f:
            f.write(lan_page)

        # ---------- 3. 引导脚本 ----------
        st, _, body = call("/join/%s/install.ps1" % norm)
        ps1 = body.decode("utf-8", "replace")
        check("Windows 引导脚本可下载", st == 200 and "text/plain" in _.get("Content-Type", ""),
              "HTTP %d" % st)
        check("PS 脚本走 enroll 且码经 stdin 传入",
              "enroll --server" in ps1 and "--code-stdin" in ps1)
        check("PS 脚本指向本中枢的客户端下载地址",
              "/join/%s/client" % norm in ps1)

        st, _, body = call("/join/%s/install.sh" % norm)
        sh = body.decode("utf-8", "replace")
        check("Linux 引导脚本可下载", st == 200)
        check("sh 脚本含 root 提权指引", "id -u" in sh and "sudo" in sh)
        check("sh 脚本走 enroll 且码经 stdin 传入",
              "enroll --server" in sh and "--code-stdin" in sh)

        # ---------- 4. 客户端分发包 ----------
        st, hdrs, body = call("/join/%s/client?os=windows" % norm)
        check("客户端包可下载", st == 200 and len(body) > 100000,
              "HTTP %d / %d 字节" % (st, len(body)))
        check("客户端包带附件文件名",
              "client-windows-amd64.exe" in hdrs.get("Content-Disposition", ""),
              hdrs.get("Content-Disposition", ""))
        st, _, _ = call("/join/%s/client?os=linux" % norm)
        check("缺 Linux 包时明确报 503（而非静默给错文件）", st == 503, "HTTP %d" % st)

        # ---------- 5. playbook 的有效性与「同机重取不重复扣次数」 ----------
        st, _, body = call("/join/%s/playbook.json?client_id=%s" % (norm, hostname))
        check("取配置包成功", st == 200, "HTTP %d" % st)
        pb = json.loads(body)
        check("配置包含集群密钥", pb.get("secret") == SECRET)
        check("配置包含服务端地址", pb.get("server_url", "").startswith("http"))
        check("配置包含自检项清单", isinstance(pb.get("checks"), list) and pb["checks"])
        check("HTTP 部署时不带 CA", not pb.get("tls_ca_pem"))

        st, _, _ = call("/join/%s/playbook.json?client_id=%s" % (norm, hostname))
        check("同一台机器重取配置包不被拒（不重复扣次数）", st == 200, "HTTP %d" % st)
        st, _, body = call("/join/%s/playbook.json?client_id=ANOTHER-NODE" % norm)
        check("另一台机器取包被明确拒绝（409 invite_exhausted）",
              st == 409 and json.loads(body).get("error") == "invite_exhausted",
              "HTTP %d %s" % (st, body[:120]))

        # ---------- 6. 真的跑一次客户端接入 ----------
        install_dir = os.path.join(OUT_DIR, "node-install")
        data_dir = os.path.join(OUT_DIR, "node-data")
        for d in (install_dir, data_dir):
            if os.path.isdir(d):
                shutil.rmtree(d)
        # 上面已经把这个码取空了；重新签一枚给真正的装机流程用
        inv2 = issue("真机接入", 30, 1)
        rc, output = run_enroll(inv2["code"], install_dir, data_dir)
        with open(os.path.join(OUT_DIR, "enroll-output.txt"), "w", encoding="utf-8") as f:
            f.write(output)
        check("客户端 enroll 退出码为 0", rc == 0, "rc=%d" % rc)
        check("自检各步骤全部通过",
              output.count("✓") >= 8 and "✗" not in output,
              "通过 %d 步" % output.count("✓"))
        check("自检含端到端心跳", "心跳连通性" in output)
        check("自检确认控制台可见", "控制台可见" in output)

        # ---------- 7. 落盘结果 ----------
        cfg_path = os.path.join(install_dir, "agent-mesh.json")
        check("配置文件已落盘", os.path.exists(cfg_path), cfg_path)
        with open(cfg_path, encoding="utf-8") as f:
            cfg = json.load(f)
        check("配置写入正确的服务端地址与密钥",
              cfg.get("server") == BASE and cfg.get("secret") == SECRET)
        check("配置显式写出 doubao_db（空值也有意义）", "doubao_db" in cfg)
        # 安装后的可执行文件名由客户端 installBinaryFile 决定（Windows 带 .exe）
        bin_name = "agent-mesh-client.exe" if os.name == "nt" else "agent-mesh-client"
        installed_bin = os.path.join(install_dir, bin_name)
        check("程序已复制到安装目录（不是留在临时目录）",
              os.path.exists(installed_bin), installed_bin)
        check("日志与下载目录已指向数据目录",
              cfg.get("log_dir", "").startswith(data_dir)
              and cfg.get("download_dir", "").startswith(data_dir))

        # ---------- 8. 控制台闭环可见 ----------
        st, _, body = call("/console/api/enrollments", console=True)
        recs = json.loads(body)
        mine = [r for r in recs if r.get("client_id") == hostname]
        check("控制台能看到入网自检记录", len(mine) >= 1, "共 %d 条" % len(recs))
        if mine:
            check("自检记录判定为成功", mine[0].get("ok") is True)
            check("自检记录保留了分步明细",
                  any(s.get("name") == "heartbeat_ok" and s.get("ok")
                      for s in json.loads(mine[0].get("steps") or "[]")))
        check("入网记录里不含明文邀请码",
              all(code not in json.dumps(r) and norm not in json.dumps(r) for r in recs))

        st, _, body = call("/console/api/invites", console=True)
        invites = json.loads(body)
        used = [i for i in invites if i.get("used_by") == hostname]
        check("邀请记录已记录使用者与用量",
              len(used) >= 1 and used[0].get("use_count") == 1
              and used[0].get("status") == "exhausted",
              json.dumps(used[0], ensure_ascii=False) if used else "无")
        check("邀请列表不返回明文码", all("code" not in i for i in invites))

        st, _, body = call("/console/api/devices", console=True)
        devices = json.loads(body)
        me = [d for d in devices if d.get("client_id") == hostname]
        check("客户端真的把自己报进了设备列表", len(me) >= 1, "共 %d 台" % len(devices))
        if me:
            check("设备状态为 online", me[0].get("status") == "online", me[0].get("status"))

        # ---------- 9. 用尽的码：换新机器仍被拒 + 落地页如实说明 ----------
        st, _, body = call("/join/%s/playbook.json?client_id=THIRD-NODE" % norm)
        check("已用尽的码对第三台机器仍拒绝", st == 409, "HTTP %d" % st)
        st, _, body = call("/join/" + norm)
        check("已用尽后落地页显示「已用尽」", "已用尽" in body.decode("utf-8", "replace"))

        # ---------- 10. 不限次数的码 ----------
        inv3 = issue("机房批量", 60, 0)
        st, _, body = call("/join/%s/playbook.json?client_id=BATCH-1" % inv3["code"].replace("-", ""))
        check("不限次数的码可被多台使用（第 1 台）", st == 200, "HTTP %d" % st)
        st, _, body = call("/join/%s/playbook.json?client_id=BATCH-2" % inv3["code"].replace("-", ""))
        check("不限次数的码可被多台使用（第 2 台）", st == 200, "HTTP %d" % st)
        st, _, body = call("/join/" + inv3["code"].replace("-", ""))
        check("不限次数的落地页显示「不限」", "不限" in body.decode("utf-8", "replace"))

        # ---------- 11. 作废 ----------
        st, _, body = call("/console/api/invites/revoke", "POST",
                           {"selector": inv3["id"]}, console=True)
        check("作废邀请码成功", st == 200, "HTTP %d" % st)
        st, _, body = call("/join/%s/playbook.json" % inv3["code"].replace("-", ""))
        check("作废后取包被拒（410 invite_revoked）",
              st == 410 and json.loads(body).get("error") == "invite_revoked",
              "HTTP %d %s" % (st, body[:120]))

        # ---------- 12. 无效码 ----------
        st, _, body = call("/join/ZZZZ-ZZZZ-ZZZZ-ZZZZ/playbook.json")
        check("不存在的码返回 404 invite_not_found",
              st == 404 and json.loads(body).get("error") == "invite_not_found",
              "HTTP %d" % st)
        st, _, body = call("/join/ZZZZ-ZZZZ-ZZZZ-ZZZZ")
        check("不存在的码给落地页「无效」而不是白屏",
              st == 200 and "无效" in body.decode("utf-8", "replace"))

    finally:
        stop(proc)

    print()
    print("=" * 60)
    print("通过 %d 项，失败 %d 项" % (len(PASS), len(FAIL)))
    if FAIL:
        print("失败项：")
        for f in FAIL:
            print("  - " + f)
    print("现场快照（可用于人工核对）:")
    print("  落地页(回环)  : %s" % os.path.join(OUT_DIR, "landing.html"))
    print("  落地页(内网IP): %s" % os.path.join(OUT_DIR, "landing-lan.html"))
    print("  接入输出: %s" % os.path.join(OUT_DIR, "enroll-output.txt"))
    print("  服务日志: %s" % os.path.join(OUT_DIR, "server.log"))
    print("=" * 60)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
