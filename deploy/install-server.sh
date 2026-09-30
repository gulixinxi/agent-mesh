#!/usr/bin/env bash
# AgentMesh 中枢（服务端）Linux 一键安装。
#
#   sudo bash install-server.sh
#
# 会用 systemd 注册 agent-mesh-server 服务并立即启动，装完打印控制台地址。
# 没有 systemd 的环境（WSL、多数容器）加 AGENT_MESH_NO_SYSTEMD=1，
# 脚本会改为直接后台拉起，并打印手工启动命令。
#
# 可用环境变量覆盖：
#   AGENT_MESH_ADDR        监听地址，默认 :8099
#   AGENT_MESH_CONSOLE_USER 控制台账号，默认 admin
#   AGENT_MESH_CONSOLE_PASS 控制台口令，留空则随机生成（装完打印，务必记下）
#   AGENT_MESH_PUBLIC_URL  对外基址，反向代理/隧道场景必填，如 https://mesh.example.com
#   AGENT_MESH_PREFIX      安装目录，默认 /opt/agent-mesh
#   AGENT_MESH_NO_SYSTEMD  非空则不用 systemd
#
# 卸载：见同目录 uninstall-server.sh（若未附带，停止服务后删除 $PREFIX 与下面三个路径即可）。

set -euo pipefail

PREFIX="${AGENT_MESH_PREFIX:-/opt/agent-mesh}"
ADDR="${AGENT_MESH_ADDR:-:8099}"
CONSOLE_USER="${AGENT_MESH_CONSOLE_USER:-admin}"
CONSOLE_PASS="${AGENT_MESH_CONSOLE_PASS:-}"
PUBLIC_URL="${AGENT_MESH_PUBLIC_URL:-}"
NO_SYSTEMD="${AGENT_MESH_NO_SYSTEMD:-}"

SERVICE_NAME="agent-mesh-server"
DATA_DIR="/var/lib/agent-mesh"
LOG_DIR="/var/log/agent-mesh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "[错误] $*" >&2; exit 1; }
info() { echo "[信息] $*"; }

# ---------- 0. 前置检查 ----------
command -v bash >/dev/null 2>&1 || die "需要 bash"

if [[ "$(id -u)" -ne 0 ]]; then
  die "需要 root 权限，请用 sudo 运行（安装要写 $PREFIX 与 /etc/systemd/system）"
fi

# ---------- 1. 选二进制：按本机架构，且优先用脚本同目录的 dist ----------
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) BIN_NAME="server-linux-amd64" ;;
  aarch64|arm64) BIN_NAME="server-linux-arm64" ;;
  *) die "不支持的架构 $ARCH（目前提供 amd64 / arm64）" ;;
esac

SRC=""
for cand in "$SCRIPT_DIR/$BIN_NAME" "$SCRIPT_DIR/../dist/$BIN_NAME" "./$BIN_NAME" "./dist/$BIN_NAME"; do
  if [[ -f "$cand" ]]; then SRC="$cand"; break; fi
done
[[ -n "$SRC" ]] || die "找不到 $BIN_NAME，请把它放到脚本同目录（或 dist/ 目录）"
info "使用二进制: $SRC ($ARCH)"

# ---------- 2. 口令与密钥 ----------
gen_rand() {
  if command -v openssl >/dev/null 2>&1; then openssl rand -hex 24
  else head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi
}
if [[ -z "$CONSOLE_PASS" ]]; then
  CONSOLE_PASS="$(gen_rand)"
  PASS_GENERATED=1
else
  PASS_GENERATED=0
fi
SECRET="$(gen_rand)"

# ---------- 3. 落盘 ----------
install -d -m 0755 "$PREFIX"
install -m 0755 "$SRC" "$PREFIX/agent-mesh-server"

# 客户端分发包：不装的话 /join/<code>/client 会返回 503，客户机取不到客户端。
PACK_DIR="$PREFIX/client-pack"
install -d -m 0755 "$PACK_DIR"
for c in client-linux-amd64 client-linux-arm64; do
  for cand in "$SCRIPT_DIR/$c" "$SCRIPT_DIR/../dist/$c" "./$c" "./dist/$c"; do
    if [[ -f "$cand" ]]; then install -m 0755 "$cand" "$PACK_DIR/$c"; info "放入分发包: $c"; break; fi
  done
done

install -d -m 0755 "$DATA_DIR" "$DATA_DIR/files" "$LOG_DIR"

# 配置文件必须与可执行文件同目录：服务端只认 exe 同目录下的 agent-mesh.json，
# 服务模式下工作目录是 /，写相对路径必然找不到。
cat > "$PREFIX/agent-mesh.json" <<EOF
{
  "addr": "$ADDR",
  "db": "$DATA_DIR/agent_mesh_center.db",
  "log_dir": "$LOG_DIR",
  "files_dir": "$DATA_DIR/files",
  "client_pack": "$PACK_DIR",
  "console_user": "$CONSOLE_USER",
  "console_pass": "$CONSOLE_PASS",
  "secret": "$SECRET",
  "public_url": "$PUBLIC_URL"
}
EOF
chmod 0600 "$PREFIX/agent-mesh.json"
info "已写入配置 $PREFIX/agent-mesh.json"

# ---------- 4. 服务账号（有 useradd 就用，没有就以 root 跑） ----------
RUN_USER="root"
if command -v useradd >/dev/null 2>&1; then
  if ! id -u agent-mesh >/dev/null 2>&1; then
    useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin agent-mesh 2>/dev/null || true
  fi
  if id -u agent-mesh >/dev/null 2>&1; then
    RUN_USER="agent-mesh"
    chown -R agent-mesh:agent-mesh "$DATA_DIR" "$LOG_DIR"
  fi
fi
info "运行身份: $RUN_USER"

start_with_systemd() {
  local unit="/etc/systemd/system/${SERVICE_NAME}.service"
  cat > "$unit" <<EOF
[Unit]
Description=AgentMesh Server (中枢)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
WorkingDirectory=$PREFIX
ExecStart=$PREFIX/agent-mesh-server
Restart=on-failure
RestartSec=5
# 日志目录与服务账号目录要先存在，否则服务起来就退出
RuntimeDirectory=agent-mesh

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
  systemctl restart "$SERVICE_NAME"
  sleep 2
  systemctl is-active --quiet "$SERVICE_NAME" || {
    systemctl status "$SERVICE_NAME" --no-pager || true
    die "服务未进入 active 状态，请看上面的 status 输出"
  }
  info "systemd 服务已启用并启动"
}

start_without_systemd() {
  info "未使用 systemd，直接后台拉起（重启后不会自动运行）"
  su -s /bin/bash "$RUN_USER" -c "cd '$PREFIX' && nohup ./agent-mesh-server >>'$LOG_DIR/stdout.log' 2>&1 &"
  sleep 2
}

if [[ -z "$NO_SYSTEMD" ]] && command -v systemctl >/dev/null 2>&1; then
  start_with_systemd
else
  start_without_systemd
fi

# ---------- 5. 健康检查 ----------
PORT="${ADDR##*:}"
[[ "$PORT" =~ ^[0-9]+$ ]] || PORT=8099
HEALTH=""
for _ in $(seq 1 15); do
  if command -v curl >/dev/null 2>&1; then
    HEALTH="$(curl -fsS --noproxy '*' --max-time 3 "http://127.0.0.1:${PORT}/healthz" || true)"
  fi
  [[ -n "$HEALTH" ]] && break
  sleep 1
done
[[ -n "$HEALTH" ]] || die "健康检查失败：/healthz 无响应，请查 $LOG_DIR 下的日志"
info "健康检查通过: $HEALTH"

# ---------- 6. 打印怎么用 ----------
echo
echo "================= 安装完成 ================="
echo "控制台账号: $CONSOLE_USER"
if [[ "$PASS_GENERATED" -eq 1 ]]; then
  echo "控制台口令: $CONSOLE_PASS   （随机生成，请立即保存）"
else
  echo "控制台口令: （你指定的值）"
fi
echo
echo "本机访问:  http://127.0.0.1:${PORT}/console"
IPS="$(hostname -I 2>/dev/null || ip -o -4 addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1)"
if [[ -n "$IPS" ]]; then
  echo "局域网地址（发给客户机用这些，别用 127.0.0.1）:"
  for ip in $IPS; do echo "  http://${ip}:${PORT}/console"; done
fi
echo
echo "下一步：到控制台「签发邀请码」，把落地页链接发给客户机即可接入。"
echo "注意：签发时请用上面的局域网地址打开控制台——命令里的地址跟着你打开时用的地址走。"
echo "=========================================="
