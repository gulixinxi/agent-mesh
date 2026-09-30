#!/usr/bin/env bash
# 卸载 AgentMesh 中枢（服务端）。数据库与中转文件默认保留，避免误删审计记录。
#
#   sudo bash uninstall-server.sh            # 保留数据
#   sudo AGENT_MESH_PURGE=1 bash uninstall-server.sh   # 连数据一起删（不可恢复）

set -euo pipefail

PREFIX="${AGENT_MESH_PREFIX:-/opt/agent-mesh}"
DATA_DIR="/var/lib/agent-mesh"
LOG_DIR="/var/log/agent-mesh"
SERVICE_NAME="agent-mesh-server"
PURGE="${AGENT_MESH_PURGE:-}"

info() { echo "[信息] $*"; }

if [[ "$(id -u)" -ne 0 ]]; then
  echo "[错误] 需要 root 权限，请用 sudo 运行" >&2
  exit 1
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
  systemctl daemon-reload 2>/dev/null || true
  info "systemd 服务已移除"
else
  pkill -f "$PREFIX/agent-mesh-server" 2>/dev/null || true
  info "已停止后台进程"
fi

rm -rf "$PREFIX"
info "已删除 $PREFIX"

if [[ -n "$PURGE" ]]; then
  rm -rf "$DATA_DIR" "$LOG_DIR"
  info "已删除数据与日志（AGENT_MESH_PURGE=1）"
else
  info "保留数据: $DATA_DIR（含审计库，如需彻底清除请手动删除）"
  info "保留日志: $LOG_DIR"
fi

echo "卸载完成。"
