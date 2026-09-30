package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Collectability 描述一个已发现的 AI 客户端，其本地数据能否被采集。
// 区分这几档是必要的：G-1 已实测证明「装了」不等于「读得到」。
// 把不可采集的也报上来，控制台才不会给出虚假的安全感。
type Collectability string

const (
	// CollectPlain 本地库明文可读，能做对话采集。
	CollectPlain Collectability = "plain"
	// CollectEncrypted 有本地数据，但整库加密或只存元数据，读不到对话正文。
	CollectEncrypted Collectability = "encrypted"
	// CollectAPI 不在本地落历史，但可通过本地接口探活与取型号。
	CollectAPI Collectability = "api"
	// CollectUnknown 尚未实测，能见度到此为止，不下结论。
	CollectUnknown Collectability = "unknown"
)

// DetectedApp 是本机探测到的一个 AI 客户端，随心跳上报。
type DetectedApp struct {
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Vendor      string         `json:"vendor"`
	Version     string         `json:"version,omitempty"`
	InstallPath string         `json:"install_path,omitempty"`
	Evidence    string         `json:"evidence"`
	Collectable Collectability `json:"collectable"`
	Note        string         `json:"note,omitempty"`
}

// appSpec 描述一个待探测的 AI 客户端。Paths 里命中任意一个即认为已安装。
// 路径模板支持三个占位符，按运行平台展开：
//
//	{programs} Windows: %LOCALAPPDATA%\Programs   Linux: /opt        macOS: /Applications
//	{local}    Windows: %LOCALAPPDATA%            Linux: ~/.local/share
//	{roaming}  Windows: %APPDATA%                 Linux: ~/.config
type appSpec struct {
	Kind        string
	Name        string
	Vendor      string
	Collectable Collectability
	Note        string
	Paths       []string
}

// aiAppCatalog 是已知 AI 客户端目录。目录名取自 2026-10-01 在本机的实测结果，
// 不是臆测：Trae CN / TRAE SOLO CN / Doubao / QianwenApp / Yuanbao / Claude /
// Cursor / Windsurf / MiniMax / com.lencx.chatgpt / ollama 均真实存在。
var aiAppCatalog = []appSpec{
	{
		Kind: "trae", Name: "Trae CN", Vendor: "字节跳动",
		Collectable: CollectEncrypted,
		Note:        "实测：ModularData/ai-agent/database.db 整文件加密（熵 8.00），只能检测存在性",
		Paths: []string{
			"{programs}/Trae CN", "{programs}/TRAE SOLO CN", "{programs}/Trae",
			"{roaming}/Trae CN", "{roaming}/TRAE SOLO CN", "{roaming}/Trae",
			"{local}/Trae CN", "{local}/Trae",
		},
	},
	{
		Kind: "doubao", Name: "豆包", Vendor: "字节跳动",
		Collectable: CollectEncrypted,
		Note:        "实测：会话在 IndexedDB(SSV)，仅含 conversation_id 等元数据，无提问/回答正文",
		Paths: []string{
			"{programs}/Doubao", "{local}/Doubao", "{roaming}/Doubao",
		},
	},
	{
		Kind: "chatgpt", Name: "ChatGPT", Vendor: "OpenAI",
		Collectable: CollectEncrypted,
		Note:        "实测：浏览器 IndexedDB 存的是 ECDSA 会话密钥，对话正文不可读",
		Paths: []string{
			"{programs}/ChatGPT", "{local}/com.lencx.chatgpt", "{roaming}/com.lencx.chatgpt",
		},
	},
	{
		Kind: "ollama", Name: "Ollama", Vendor: "Ollama",
		Collectable: CollectAPI,
		Note:        "本地 11434 可探活与取型号，默认不落对话历史",
		Paths: []string{
			"{programs}/Ollama", "{local}/Ollama", "{roaming}/Ollama", "{local}/Programs/Ollama",
			// 实测（2026-10-01 本机）：Ollama 的 GUI 壳把数据落在 %APPDATA% 下，
			// 目录名就叫 "ollama app.exe"，不是常规的 "Ollama"。
			"{roaming}/ollama app.exe",
		},
	},
	{
		Kind: "qianwen", Name: "通义千问", Vendor: "阿里云",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/QianwenApp", "{local}/Qianwen", "{roaming}/Qianwen"},
	},
	{
		Kind: "yuanbao", Name: "腾讯元宝", Vendor: "腾讯",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Yuanbao", "{local}/Yuanbao", "{local}/com.tencent.yuanbao", "{roaming}/Yuanbao"},
	},
	{
		Kind: "ima", Name: "腾讯 ima", Vendor: "腾讯",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/ima.copilot", "{local}/ima.copilot", "{roaming}/ima.copilot"},
	},
	{
		Kind: "claude", Name: "Claude", Vendor: "Anthropic",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Claude", "{roaming}/Claude", "{local}/Claude", "{local}/claude-cli-nodejs"},
	},
	{
		Kind: "cursor", Name: "Cursor", Vendor: "Anysphere",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Cursor", "{roaming}/Cursor", "{local}/Cursor"},
	},
	{
		Kind: "windsurf", Name: "Windsurf", Vendor: "Codeium",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Windsurf", "{roaming}/Windsurf", "{local}/Windsurf"},
	},
	{
		Kind: "minimax", Name: "MiniMax", Vendor: "稀宇科技",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/MiniMax", "{roaming}/MiniMax", "{local}/MiniMax"},
	},
	{
		Kind: "deepseek", Name: "DeepSeek", Vendor: "深度求索",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/DeepSeek", "{local}/DeepSeek", "{roaming}/DeepSeek"},
	},
	{
		Kind: "kimi", Name: "Kimi", Vendor: "月之暗面",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Kimi", "{local}/Kimi", "{roaming}/Kimi"},
	},
	{
		Kind: "zhipu", Name: "智谱清言", Vendor: "智谱华章",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Zhipu", "{local}/Zhipu", "{roaming}/Zhipu"},
	},
	{
		Kind: "wenxin", Name: "文心一言", Vendor: "百度",
		Collectable: CollectUnknown,
		Paths:       []string{"{programs}/Wenxin", "{local}/Wenxin", "{roaming}/Wenxin", "{local}/Yiyan", "{roaming}/Yiyan"},
	},
}

// defaultBases 按当前运行平台给出三个占位符的真实目录。
// 取不到就留空，expandPath 会跳过对应候选，不会退化成扫描根目录。
func defaultBases() map[string]string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return map[string]string{
			"programs": joinNonEmpty(os.Getenv("LOCALAPPDATA"), "Programs"),
			"local":    os.Getenv("LOCALAPPDATA"),
			"roaming":  os.Getenv("APPDATA"),
		}
	case "darwin":
		support := joinNonEmpty(home, "Library", "Application Support")
		return map[string]string{
			"programs": "/Applications",
			"local":    support,
			"roaming":  support,
		}
	default: // linux 及其它
		return map[string]string{
			"programs": "/opt",
			"local":    joinNonEmpty(home, ".local", "share"),
			"roaming":  joinNonEmpty(home, ".config"),
		}
	}
}

func joinNonEmpty(parts ...string) string {
	for _, p := range parts {
		if p == "" {
			return ""
		}
	}
	return filepath.Join(parts...)
}

// expandPath 把 "{local}/Doubao" 这样的模板展开为真实路径。
// base 缺失时返回 ok=false：宁可漏报，也不要把 "/Doubao" 当成有效候选去 stat。
func expandPath(bases map[string]string, tmpl string) (string, bool) {
	if len(tmpl) < 2 || tmpl[0] != '{' {
		return tmpl, true
	}
	end := -1
	for i := 1; i < len(tmpl); i++ {
		if tmpl[i] == '}' {
			end = i
			break
		}
	}
	if end < 0 {
		return "", false
	}
	base, ok := bases[tmpl[1:end]]
	if !ok || base == "" {
		return "", false
	}
	rest := tmpl[end+1:]
	if rest == "" {
		return base, true
	}
	return base + string(filepath.Separator) + filepath.FromSlash(rest[1:]), true
}

// packageJSONCandidates 是 Electron/VS Code 系应用常见的版本号落点，按命中概率排序。
var packageJSONCandidates = []string{
	"resources/app/package.json",
	"Contents/Resources/app/package.json",
	"resources/package.json",
}

// probeVersion 尽力取版本号；读不到就返回空串，不构造占位值。
func probeVersion(root string) string {
	for _, rel := range packageJSONCandidates {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &pkg); err == nil && pkg.Version != "" {
			return pkg.Version
		}
	}
	return ""
}

// scanAIAppsWith 按给定的目录基址扫描，抽出来是为了让测试能用临时目录，
// 而不必真的去改进程环境变量。
func scanAIAppsWith(bases map[string]string) []DetectedApp {
	out := make([]DetectedApp, 0, len(aiAppCatalog))
	seen := make(map[string]bool, len(aiAppCatalog))
	for _, spec := range aiAppCatalog {
		if seen[spec.Kind] {
			continue
		}
		hit, evidence := "", ""
		for _, tmpl := range spec.Paths {
			p, ok := expandPath(bases, tmpl)
			if !ok {
				continue
			}
			if fi, err := os.Stat(p); err == nil && fi != nil {
				hit, evidence = p, tmpl
				break
			}
		}
		if hit == "" {
			continue
		}
		seen[spec.Kind] = true
		out = append(out, DetectedApp{
			Kind:        spec.Kind,
			Name:        spec.Name,
			Vendor:      spec.Vendor,
			Version:     probeVersion(hit),
			InstallPath: hit,
			Evidence:    evidence,
			Collectable: spec.Collectable,
			Note:        spec.Note,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// 扫描结果缓存：心跳每 10 秒一次，没必要每次都打磁盘。
// 5 分钟足够跟上装机/卸载，又不至于频繁 stat。
const appScanTTL = 5 * time.Minute

var (
	appCacheMu sync.Mutex
	appCache   []DetectedApp
	appCacheAt time.Time
)

// ScanAIApps 返回本机已安装的 AI 客户端清单（带缓存）。
// 只做存在性探测，不读任何用户数据。
func ScanAIApps() []DetectedApp {
	appCacheMu.Lock()
	defer appCacheMu.Unlock()
	if appCache != nil && time.Since(appCacheAt) < appScanTTL {
		return appCache
	}
	appCache = scanAIAppsWith(defaultBases())
	appCacheAt = time.Now()
	return appCache
}
