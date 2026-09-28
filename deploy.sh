#!/bin/bash
# =======================================================================
# Agent Mesh - 一键本地拉取 / 编译 / 双端运行脚本
#
# 安全约定：本文件内不得存放任何明文密钥。
# 使用前请先导出环境变量：
#   export GITHUB_USER="你的 GitHub 用户名"
#   export GITHUB_PAT="你的 GitHub Personal Access Token"
# 未设置时脚本会直接报错退出，不会用占位符去撞认证。
# =======================================================================
set -euo pipefail

GITHUB_USER="${GITHUB_USER:-gulixinxi}"
GITHUB_PAT="${GITHUB_PAT:-}"

if [ -z "${GITHUB_PAT}" ]; then
    echo "[Agent Mesh] 错误：环境变量 GITHUB_PAT 未设置。" >&2
    echo "            请先执行： export GITHUB_PAT=\"你的 token\"" >&2
    exit 1
fi

REPO_URL="https://${GITHUB_USER}:${GITHUB_PAT}@github.com/${GITHUB_USER}/agent-mesh.git"

echo "[Agent Mesh] 1. 同步 GitHub 私有仓库..."
if [ -d "agent-mesh/.git" ]; then
    cd agent-mesh
    git pull origin main
else
    git clone "${REPO_URL}"
    cd agent-mesh
fi

echo "[Agent Mesh] 2. 编译双端二进制（输出到 bin/）..."
mkdir -p bin
(cd agent-mesh-server && go build -o ../bin/mesh-server ./...)
(cd agent-mesh-client && go build -o ../bin/mesh-client ./...)
echo "[Agent Mesh]    服务端 -> bin/mesh-server"
echo "[Agent Mesh]    客户端 -> bin/mesh-client"

echo "[Agent Mesh] 3. 后台启动中央汇总服务端（日志写入 server.log）..."
./bin/mesh-server > server.log 2>&1 &
SERVER_PID=$!
echo "[Agent Mesh]    服务端已拉起，PID: ${SERVER_PID}"
sleep 2

echo "[Agent Mesh] 4. 启动本地节点客户端..."
echo "              （Ctrl+C 退出，服务端 PID ${SERVER_PID} 需手动 kill）"
./bin/mesh-client
