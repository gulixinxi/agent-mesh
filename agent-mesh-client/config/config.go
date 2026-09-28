// Package config 集中管理客户端常驻进程的运行参数。
// 取值优先级：环境变量 > 内置默认值（命令行 flag 在 main.go 里仍可覆盖）。
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

const (
	// EnvServerURL 指定中央服务端地址。
	EnvServerURL = "AGENT_MESH_SERVER"
	// EnvClientID 指定本节点唯一标识。
	EnvClientID = "AGENT_MESH_CLIENT_ID"
	// EnvP2PPort 指定 libp2p 监听端口。
	EnvP2PPort = "AGENT_MESH_P2P_PORT"
	// EnvDownloadDir 指定 P2P 文件落地目录。
	EnvDownloadDir = "AGENT_MESH_DOWNLOAD_DIR"
	// EnvDoubaoDB 指定豆包本地会话库路径。
	EnvDoubaoDB = "AGENT_MESH_DOUBAO_DB"
	// EnvMCPOnly 为 1 时只以 MCP stdio 模式运行。
	EnvMCPOnly = "AGENT_MESH_MCP_ONLY"
	// EnvSecret 集群共享密钥；非空时客户端对每个上报请求附带 HMAC 签名。
	EnvSecret = "AGENT_MESH_SECRET"
	// EnvP2PAllow 逗号分隔的 PeerID 白名单；非空时只接受名单内节点的文件传输。
	EnvP2PAllow = "AGENT_MESH_P2P_ALLOW"
)

// Config 是客户端运行所需的最小配置集。
type Config struct {
	ServerURL   string
	ClientID    string
	P2PPort     int
	DownloadDir string
	DoubaoDB    string
	MCPOnly     bool
	Secret      string
	P2PAllow    string
}

// Load 载入配置，未设置的环境变量回落到默认约定。
func Load() Config {
	return Config{
		ServerURL:   envOr(EnvServerURL, "http://127.0.0.1:8080"),
		ClientID:    os.Getenv(EnvClientID), // 留空时由 main 回落为主机名
		P2PPort:     envInt(EnvP2PPort, 6001),
		DownloadDir: envOr(EnvDownloadDir, defaultDownloadDir()),
		DoubaoDB:    envOr(EnvDoubaoDB, "doubao_message_mock.db"),
		MCPOnly:     envBool(EnvMCPOnly),
		Secret:      os.Getenv(EnvSecret),
		P2PAllow:    os.Getenv(EnvP2PAllow),
	}
}

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "AgentMeshDownloads"
	}
	return filepath.Join(home, "AgentMeshDownloads")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "TRUE"
}
