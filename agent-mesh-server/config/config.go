// Package config 集中管理服务端的运行参数。
//
// 取值优先级（高到低）：命令行 flag > 配置文件 > 环境变量 > 内置默认值。
// 装成 Windows 服务后既没有交互终端也拿不到用户级环境变量，
// 所以控制台口令、TLS 证书这类参数必须能落在配置文件里。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	EnvAddr        = "AGENT_MESH_ADDR"
	EnvDB          = "AGENT_MESH_DB"
	EnvDebug       = "AGENT_MESH_DEBUG"
	EnvSecret      = "AGENT_MESH_SECRET"
	EnvConsoleUser = "AGENT_MESH_CONSOLE_USER"
	EnvConsolePass = "AGENT_MESH_CONSOLE_PASS"
	EnvTLSCert     = "AGENT_MESH_TLS_CERT"
	EnvTLSKey      = "AGENT_MESH_TLS_KEY"
	EnvLogDir      = "AGENT_MESH_LOG_DIR"
	EnvRetention   = "AGENT_MESH_RETENTION"
	EnvTaskTimeout = "AGENT_MESH_TASK_TIMEOUT"
	EnvFilesDir    = "AGENT_MESH_FILES_DIR"
)

// ConfigFileName 是配置文件名，固定放在可执行文件同目录。
// 只认 exe 同目录：Windows 服务的工作目录是 System32，靠相对路径定位必然失败。
const ConfigFileName = "agent-mesh.json"

// Config 是服务端运行所需的配置集。
type Config struct {
	Addr        string
	DB          string
	Debug       bool
	Secret      string
	ConsoleUser string
	ConsolePass string
	TLSCert     string
	TLSKey      string
	LogDir      string
	Retention   time.Duration
	TaskTimeout time.Duration
	// FilesDir 是中转文件实体的落盘目录；留空时由 main 派生为数据库同级的 files/。
	FilesDir string
}

// fileConfig 是配置文件的结构，字段全用指针以区分「显式空值」与「未配置」。
type fileConfig struct {
	Addr        *string `json:"addr"`
	DB          *string `json:"db"`
	Debug       *bool   `json:"debug"`
	Secret      *string `json:"secret"`
	ConsoleUser *string `json:"console_user"`
	ConsolePass *string `json:"console_pass"`
	TLSCert     *string `json:"tls_cert"`
	TLSKey      *string `json:"tls_key"`
	LogDir      *string `json:"log_dir"`
	Retention   *string `json:"retention"`
	TaskTimeout *string `json:"task_timeout"`
	FilesDir    *string `json:"files_dir"`
}

// Load 载入配置。顺序：默认值 → 环境变量 → 配置文件 → （main 里再由 flag 覆盖）。
func Load() Config {
	c := Config{
		Addr:        envOr(EnvAddr, ":8080"),
		DB:          envOr(EnvDB, "agent_mesh_center.db"),
		Debug:       envBool(EnvDebug),
		Secret:      os.Getenv(EnvSecret),
		ConsoleUser: os.Getenv(EnvConsoleUser),
		ConsolePass: os.Getenv(EnvConsolePass),
		TLSCert:     os.Getenv(EnvTLSCert),
		TLSKey:      os.Getenv(EnvTLSKey),
		LogDir:      os.Getenv(EnvLogDir),
		Retention:   envDur(EnvRetention, 30*24*time.Hour),
		TaskTimeout: envDur(EnvTaskTimeout, 5*time.Minute),
		FilesDir:    os.Getenv(EnvFilesDir),
	}

	fc, err := loadFile()
	if err != nil {
		return c
	}
	if fc.Addr != nil {
		c.Addr = *fc.Addr
	}
	if fc.DB != nil {
		c.DB = *fc.DB
	}
	if fc.Debug != nil {
		c.Debug = *fc.Debug
	}
	if fc.Secret != nil {
		c.Secret = *fc.Secret
	}
	if fc.ConsoleUser != nil {
		c.ConsoleUser = *fc.ConsoleUser
	}
	if fc.ConsolePass != nil {
		c.ConsolePass = *fc.ConsolePass
	}
	if fc.TLSCert != nil {
		c.TLSCert = *fc.TLSCert
	}
	if fc.TLSKey != nil {
		c.TLSKey = *fc.TLSKey
	}
	if fc.LogDir != nil {
		c.LogDir = *fc.LogDir
	}
	if fc.Retention != nil {
		if d, err := time.ParseDuration(*fc.Retention); err == nil {
			c.Retention = d
		}
	}
	if fc.TaskTimeout != nil {
		if d, err := time.ParseDuration(*fc.TaskTimeout); err == nil {
			c.TaskTimeout = d
		}
	}
	if fc.FilesDir != nil {
		c.FilesDir = *fc.FilesDir
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "TRUE"
}

func envDur(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
