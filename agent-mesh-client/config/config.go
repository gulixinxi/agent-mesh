// Package config 集中管理客户端常驻进程的运行参数。
//
// 取值优先级（高到低）：命令行 flag > 配置文件 > 环境变量 > 内置默认值。
// 配置文件存在的意义是让安装脚本把参数一次写死，不必再依赖那串 AGENT_MESH_* 环境变量——
// 服务模式下既没有交互终端，也拿不到用户级环境变量。
package config

import (
	"encoding/json"
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
	// EnvTLSCA 指定信任的自签 CA 证书路径（内网 HTTPS 用）。
	EnvTLSCA = "AGENT_MESH_TLS_CA"
	// EnvTLSInsecure 为 1 时跳过服务端证书校验，仅供联调，生产禁用。
	EnvTLSInsecure = "AGENT_MESH_TLS_INSECURE"
	// EnvLogDir 指定日志落盘目录；服务模式下必须设置，否则日志无人接收。
	EnvLogDir = "AGENT_MESH_LOG_DIR"
)

// ConfigFileName 是配置文件名，固定放在可执行文件同目录。
// 只认 exe 同目录，不看当前工作目录——Windows 服务的工作目录是 System32，靠它定位必然失败。
const ConfigFileName = "agent-mesh.json"

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
	// TLSCA 是信任的自签 CA 证书路径；连 HTTPS 服务端且证书是自签时必须指定。
	TLSCA string
	// TLSInsecure 跳过服务端证书校验。仅联调用，启动时打印醒目警告。
	TLSInsecure bool
	// LogDir 是日志落盘目录，服务模式下为必填。
	LogDir string
}

// fileConfig 是配置文件的结构。字段全用指针，
// 这样才能区分「配置里显式写了空值」和「配置里没写这一项」——后者不应该覆盖上层取值。
type fileConfig struct {
	ServerURL   *string `json:"server"`
	ClientID    *string `json:"id"`
	P2PPort     *int    `json:"p2p_port"`
	DownloadDir *string `json:"download_dir"`
	DoubaoDB    *string `json:"doubao_db"`
	MCPOnly     *bool   `json:"mcp_only"`
	Secret      *string `json:"secret"`
	P2PAllow    *string `json:"p2p_allow"`
	TLSCA       *string `json:"tls_ca"`
	TLSInsecure *bool   `json:"tls_insecure"`
	LogDir      *string `json:"log_dir"`
}

// Load 载入配置。顺序：默认值 → 环境变量 → 配置文件 → （main 里再由 flag 覆盖）。
func Load() Config {
	c := Config{
		ServerURL:   envOr(EnvServerURL, "http://127.0.0.1:8080"),
		ClientID:    os.Getenv(EnvClientID), // 留空时由 main 回落为主机名
		P2PPort:     envInt(EnvP2PPort, 6001),
		DownloadDir: envOr(EnvDownloadDir, defaultDownloadDir()),
		DoubaoDB:    envOr(EnvDoubaoDB, "doubao_message_mock.db"),
		MCPOnly:     envBool(EnvMCPOnly),
		Secret:      os.Getenv(EnvSecret),
		P2PAllow:    os.Getenv(EnvP2PAllow),
		TLSCA:       os.Getenv(EnvTLSCA),
		TLSInsecure: envBool(EnvTLSInsecure),
		LogDir:      os.Getenv(EnvLogDir),
	}

	fc, err := loadFile()
	if err != nil {
		return c
	}
	if fc.ServerURL != nil {
		c.ServerURL = *fc.ServerURL
	}
	if fc.ClientID != nil {
		c.ClientID = *fc.ClientID
	}
	if fc.P2PPort != nil {
		c.P2PPort = *fc.P2PPort
	}
	if fc.DownloadDir != nil {
		c.DownloadDir = *fc.DownloadDir
	}
	if fc.DoubaoDB != nil {
		c.DoubaoDB = *fc.DoubaoDB
	}
	if fc.MCPOnly != nil {
		c.MCPOnly = *fc.MCPOnly
	}
	if fc.Secret != nil {
		c.Secret = *fc.Secret
	}
	if fc.P2PAllow != nil {
		c.P2PAllow = *fc.P2PAllow
	}
	if fc.TLSCA != nil {
		c.TLSCA = *fc.TLSCA
	}
	if fc.TLSInsecure != nil {
		c.TLSInsecure = *fc.TLSInsecure
	}
	if fc.LogDir != nil {
		c.LogDir = *fc.LogDir
	}
	return c
}

// ConfigPath 返回配置文件应当所在的路径（可执行文件同目录）。
func ConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ConfigFileName
	}
	return filepath.Join(filepath.Dir(exe), ConfigFileName)
}

// ConfigPathIn 返回指定目录下的配置文件路径。
//
// enroll 需要它：引导脚本会把程序下载到临时目录再执行，而配置必须写进
// **最终安装目录**，也就是服务进程将来所在的位置——否则服务读不到配置。
func ConfigPathIn(dir string) string {
	return filepath.Join(dir, ConfigFileName)
}

// persistedConfig 是落盘时的字段顺序定义。
//
// 刻意不用 map：map 的键序是乱的，配置文件是给人看的，稳定顺序更好读。
// 也不用 omitempty：空值在这里是有意义的（例如 doubao_db="" 表示「本机不监听豆包会话库」，
// 少了它就会退化成默认相对路径，凭空多出一个扫不到东西的空适配器）。
type persistedConfig struct {
	Server      string `json:"server"`
	ID          string `json:"id"`
	Secret      string `json:"secret"`
	P2PPort     int    `json:"p2p_port"`
	DownloadDir string `json:"download_dir"`
	DoubaoDB    string `json:"doubao_db"`
	MCPOnly     bool   `json:"mcp_only"`
	P2PAllow    string `json:"p2p_allow"`
	TLSCA       string `json:"tls_ca"`
	TLSInsecure bool   `json:"tls_insecure"`
	LogDir      string `json:"log_dir"`
}

// WriteTo 把配置写入指定路径。
//
// 文件里含集群密钥，权限按 0600 落盘（Windows 上忽略该位，由目录 ACL 兜底）。
func WriteTo(path string, c Config) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	p := persistedConfig{
		Server:      c.ServerURL,
		ID:          c.ClientID,
		Secret:      c.Secret,
		P2PPort:     c.P2PPort,
		DownloadDir: c.DownloadDir,
		DoubaoDB:    c.DoubaoDB,
		MCPOnly:     c.MCPOnly,
		P2PAllow:    c.P2PAllow,
		TLSCA:       c.TLSCA,
		TLSInsecure: c.TLSInsecure,
		LogDir:      c.LogDir,
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// loadFile 读取配置文件；文件不存在属于正常情况，不算错误。
func loadFile() (fileConfig, error) {
	var fc fileConfig
	raw, err := os.ReadFile(ConfigPath())
	if err != nil {
		return fc, err
	}
	if err := json.Unmarshal(raw, &fc); err != nil {
		return fc, err
	}
	return fc, nil
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
