package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"agent-mesh-client/config"
)

// =====================================================================
// 自助接入（enroll）：用一枚邀请码把「换配置 -> 落盘 -> 装自启 -> 自检 -> 回传」
// 全部自动做完。
//
// 为什么要做成一个子命令，而不是把逻辑写在 PowerShell / bash 脚本里：
//   1. Windows 与 Linux 只有一份实现，不会出现「PS 里改了、sh 里忘了同步」的漂移；
//   2. 自检可以用**同一套签名逻辑**直接打中枢的真实接口，验的是端到端，
//      不是「文件写进去了」这种自欺欺人的假阳性；
//   3. 可单元测试。
// 服务端下发的安装脚本因此可以极薄：下载程序，然后调用本子命令。
//
// 信任模型（TOFU，与对标产品一致）：
//   取配置包这一刻手上还没有服务端 CA，只能跳过证书校验；
//   拿到 CA 后立刻固定下来，后续所有通信（含服务进程自己）都走严格校验。
//   替代方案是「先把 ca.pem 拷到每台机器」，那正是本流程要消灭的工序。
// =====================================================================

// Playbook 是服务端下发的接入配置包。
// 字段名是与服务端 api.playbookResp 的契约，改动必须双端同步。
type Playbook struct {
	Version       int               `json:"version"`
	Product       string            `json:"product"`
	Code          string            `json:"code"`
	Label         string            `json:"label"`
	ServerURL     string            `json:"server_url"`
	Secret        string            `json:"secret"`
	TLSCAPEM      string            `json:"tls_ca_pem"`
	Client        map[string]string `json:"client"`
	Install       map[string]string `json:"install"`
	Checks        []string          `json:"checks"`
	ExpiresAt     int64             `json:"expires_at"`
	RemainingUses int               `json:"remaining_uses"`
}

// ServiceHooks 是 enroll 需要的平台服务操作，由 main 注入。
//
// 这样 core 不需要知道 Windows 服务与 systemd 的差异，
// 测试也能注入假实现来覆盖「已注册」「启动失败」等分支。
type ServiceHooks struct {
	Install       func(exePath string) error
	Restart       func() error
	Stop          func() error
	Status        func() (bool, string)
	AlreadyExists func(err error) bool
	// PrivilegeCheck 在**取配置包之前**探测权限是否足够。
	//
	// 这个顺序很关键：取配置包会消费掉邀请码名额。如果先取包、再因为
	// 「没用管理员运行」而卡在注册服务这一步，那枚邀请码就白白烧掉了，
	// 管理员还得重新签发一次 —— 装机现场最不需要的就是这种返工。
	PrivilegeCheck func() error
}

// EnrollConfig 是一次接入所需的全部输入。
type EnrollConfig struct {
	ServerURL   string
	Code        string
	ClientID    string
	InstallDir  string
	DataDir     string
	P2PPort     int
	SkipService bool
	Service     ServiceHooks
}

// EnrollStep 是自检的一个步骤。
type EnrollStep struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// EnrollResult 是一次接入的结果。
type EnrollResult struct {
	Playbook *Playbook
	Steps    []EnrollStep
	OK       bool
}

// 关键步骤：任一失败即视为接入未成功（report 本身不算）。
var criticalSteps = map[string]bool{
	"fetch_playbook":    true,
	"install_binary":    true,
	"write_config":      true,
	"install_service":   true,
	"service_running":   true,
	"heartbeat_ok":      true,
	"device_registered": true,
}

// 等常驻服务第一次心跳的参数。
// 提成变量是为了让测试能把等待压到毫秒级 —— 否则一个「节点始终不上线」的
// 负例用例就要真等 20 秒，测试套件会慢到没人愿意跑。
var (
	devicePollTimeout  = 20 * time.Second
	devicePollInterval = 2 * time.Second
)

// Enroll 执行一次完整接入。
//
// 返回的 EnrollResult 一定非 nil：即使中途失败，也要把「哪些步骤过了、
// 哪一步卡住、原因是什么」完整带回去，并且尽量把结果回传到中枢 ——
// 现场没人能看日志，控制台里那条记录就是唯一的线索。
func Enroll(ctx context.Context, cfg EnrollConfig, out io.Writer) *EnrollResult {
	res := &EnrollResult{}
	logf := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }

	add := func(name, label string, ok bool, detail string) {
		res.Steps = append(res.Steps, EnrollStep{Name: name, Label: label, OK: ok, Detail: detail})
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		if detail != "" {
			logf("  %s %s：%s", mark, label, detail)
		} else {
			logf("  %s %s", mark, label)
		}
	}

	server, err := normalizeServerURL(cfg.ServerURL)
	if err != nil {
		add("normalize_server", "解析服务端地址", false, err.Error())
		return res
	}
	cfg.ServerURL = server
	code := normalizeCode(cfg.Code)
	if code == "" {
		add("normalize_server", "解析邀请码", false, "邀请码为空")
		return res
	}
	// 用归一化后的码去拼 URL：用户抄进来的可能带空格或小写，服务端虽然也能容错，
	// 但日志与回传里统一用规范形式，排查时不会因为多了个空格而看花眼。
	cfg.Code = code

	logf("开始接入：中枢 %s", cfg.ServerURL)
	logf("节点标识：%s", cfg.ClientID)
	logf("安装目录：%s", cfg.InstallDir)
	logf("")

	// ---- 0. 先验权限，避免白烧一枚邀请码 ----
	if !cfg.SkipService && cfg.Service.PrivilegeCheck != nil {
		if err := cfg.Service.PrivilegeCheck(); err != nil {
			add("privilege", "权限检查", false, err.Error())
			logf("")
			logf("尚未消耗邀请码，修好权限后可直接重跑同一条命令。")
			return res
		}
		add("privilege", "权限检查", true, "具备注册系统服务所需权限")
	}

	// ---- 1. 取配置包（这一步会消费邀请码名额）----
	pb, err := fetchPlaybook(ctx, cfg)
	if err != nil {
		add("fetch_playbook", "取得接入配置", false, err.Error())
		return res
	}
	res.Playbook = pb
	detail := fmt.Sprintf("已取得配置（服务端声明版本 %d", pb.Version)
	if pb.ExpiresAt > 0 {
		detail += fmt.Sprintf("，本码有效期至 %s", time.Unix(pb.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
	}
	detail += "）"
	add("fetch_playbook", "取得接入配置", true, detail)

	// ---- 2. 落盘 CA ----
	caPath := ""
	if strings.TrimSpace(pb.TLSCAPEM) == "" {
		add("write_ca", "信任服务端 CA", true, "服务端未下发 CA（HTTP 部署或使用公信证书），跳过")
	} else {
		caPath = filepath.Join(cfg.InstallDir, "ca.pem")
		if err := writeFileAtomic(caPath, []byte(pb.TLSCAPEM), 0o644); err != nil {
			add("write_ca", "信任服务端 CA", false, err.Error())
			return res
		}
		add("write_ca", "信任服务端 CA", true, "已写入 "+caPath)
	}

	// ---- 3. 安装程序到安装目录 ----
	// 若服务已在运行，先停掉：Windows 上正在运行的程序文件是被锁住的，直接覆盖必然失败；
	// Linux 上虽然能覆盖，但会造成「旧进程还在跑、新文件已替换」的错觉。
	if !cfg.SkipService && cfg.Service.Stop != nil && cfg.Service.Status != nil {
		if running, _ := cfg.Service.Status(); running {
			if err := cfg.Service.Stop(); err != nil {
				add("install_binary", "安装程序文件", false, "停止已有服务失败: "+err.Error())
				return res
			}
			logf("  · 已停止在运行的服务，准备更新程序文件")
		}
	}

	binPath, copied, err := installBinaryFile(cfg.InstallDir)
	if err != nil {
		add("install_binary", "安装程序文件", false, err.Error())
		return res
	}
	if copied {
		add("install_binary", "安装程序文件", true, "已复制到 "+binPath)
	} else {
		add("install_binary", "安装程序文件", true, "已在目标位置，无需复制")
	}

	// ---- 4. 写配置 ----
	configPath := filepath.Join(cfg.InstallDir, "agent-mesh.json")
	if err := config.WriteTo(configPath, config.Config{
		ServerURL:   cfg.ServerURL,
		ClientID:    cfg.ClientID,
		Secret:      pb.Secret,
		P2PPort:     cfg.P2PPort,
		DownloadDir: filepath.Join(cfg.DataDir, "downloads"),
		// 显式置空：明确表示本机不监听豆包会话库。
		// 少了这一项会退化成默认相对路径，凭空多出一个永远扫不到东西的空适配器。
		DoubaoDB: "",
		TLSCA:    caPath,
		LogDir:   filepath.Join(cfg.DataDir, "logs"),
	}); err != nil {
		add("write_config", "写入节点配置", false, err.Error())
		return res
	}
	add("write_config", "写入节点配置", true, configPath)

	// ---- 5/6. 注册并启动服务 ----
	if cfg.SkipService {
		add("install_service", "注册开机自启", true, "已按参数跳过（--no-service）")
		add("service_running", "服务运行状态", true, "已跳过")
	} else {
		if err := ensureService(cfg.Service, binPath, out); err != nil {
			add("install_service", "注册开机自启", false, err.Error())
			add("service_running", "服务运行状态", false, "服务未能就绪")
			return res
		}
		add("install_service", "注册开机自启", true, "服务已注册")

		running, state := serviceRunning(cfg.Service)
		if running {
			add("service_running", "服务运行状态", true, state)
		} else {
			add("service_running", "服务运行状态", false, "当前状态："+state)
		}
	}

	// ---- 7/8. 用签名请求做端到端自检 ----
	client, clientNote := buildVerifiedClient(pb)
	logf("  （自检网络客户端：%s）", clientNote)

	hbOK, hbDetail := checkHeartbeat(ctx, client, cfg, pb.Secret)
	add("heartbeat_ok", "心跳连通性", hbOK, hbDetail)

	regOK, regDetail := checkDeviceRegistered(ctx, client, cfg, pb.Secret)
	add("device_registered", "控制台可见", regOK, regDetail)

	// ---- 9. 回传自检结果（失败也必须回传）----
	res.OK = allCriticalOK(res.Steps)
	if err := reportEnrollment(ctx, cfg, res); err != nil {
		add("report", "回传自检结果", false, err.Error())
	} else {
		add("report", "回传自检结果", true, "控制台「入网记录」可见")
	}

	return res
}

// allCriticalOK 判断所有关键步骤是否都通过。
func allCriticalOK(steps []EnrollStep) bool {
	for _, s := range steps {
		if criticalSteps[s.Name] && !s.OK {
			return false
		}
	}
	return true
}

// normalizeServerURL 规整服务端地址：补协议、去尾部斜杠。
func normalizeServerURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("服务端地址为空")
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("服务端地址非法: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("服务端地址缺少主机名")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// normalizeCode 归一化邀请码：去掉分隔符与空白并转大写（与服务端一致）。
func normalizeCode(raw string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch r {
		case '-', ' ', '\t', '\n', '\r':
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// fetchPlaybook 用邀请码换取配置包。
//
// 注意：本函数**故意**跳过证书校验 —— 此刻手上还没有服务端 CA。
// 拿到 CA 后（返回的 Playbook.TLSCAPEM）后续一切通信都走严格校验。
func fetchPlaybook(ctx context.Context, cfg EnrollConfig) (*Playbook, error) {
	// #nosec G402 -- 首次信任（TOFU），仅用于取回 CA 这一步
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	endpoint := fmt.Sprintf("%s/join/%s/playbook.json?client_id=%s",
		cfg.ServerURL, url.PathEscape(cfg.Code), url.QueryEscape(cfg.ClientID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上中枢 %s（请确认地址、端口与网络可达）: %w", cfg.ServerURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var eb struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &eb)
		return nil, errors.New(playbookErrorMessage(resp.StatusCode, eb.Error, eb.Message))
	}

	var pb Playbook
	if err := json.Unmarshal(body, &pb); err != nil {
		return nil, fmt.Errorf("中枢返回的配置包格式异常: %w", err)
	}
	if pb.ServerURL == "" {
		// 服务端没声明基址时回落到我们请求用的地址，保证配置里一定有值。
		pb.ServerURL = cfg.ServerURL
	}
	return &pb, nil
}

// playbookErrorMessage 把服务端的错误契约翻译成现场能照做的人话。
//
// 这层翻译很重要：装机的是「非技术同事 + 远程指导」，错误信息必须直接给出下一步。
func playbookErrorMessage(status int, code, msg string) string {
	switch code {
	case "invite_not_found":
		return "邀请码不存在。请核对是否抄错（注意区分 0/O、1/I 这类字符），或让管理员重新签发。"
	case "invite_expired":
		return "邀请码已过期。邀请码默认只有 30 分钟有效期，请让管理员重新签发一个。"
	case "invite_revoked":
		return "邀请码已被管理员作废，请让管理员重新签发一个。"
	case "invite_exhausted":
		return "邀请码的可用次数已用尽（默认一码一机）。请让管理员签发新的邀请码，或签发「不限次数」的码。"
	case "rate_limited":
		return "请求过于频繁，被中枢限流。请等一分钟后再试。"
	case "client_pack_unavailable":
		return "中枢上没有对应平台的客户端安装包，请联系管理员先编译并放置客户端。"
	case "bad_json":
		return "中枢拒绝了本次请求（请求格式异常），请联系管理员。"
	default:
		if msg != "" {
			return fmt.Sprintf("中枢返回 HTTP %d：%s", status, msg)
		}
		return fmt.Sprintf("中枢返回 HTTP %d", status)
	}
}

// buildVerifiedClient 按配置包构造「已验证」的 HTTP 客户端。
//
// 有 CA 就固定 CA（严格校验）；没有则退回系统根证书
// （服务端用公信证书的正常场景）。
func buildVerifiedClient(pb *Playbook) (*http.Client, string) {
	if strings.TrimSpace(pb.TLSCAPEM) != "" {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM([]byte(pb.TLSCAPEM)) {
			return &http.Client{
				Timeout: 15 * time.Second,
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
				},
			}, "已固定服务端 CA，后续通信严格校验"
		}
	}
	return &http.Client{Timeout: 15 * time.Second}, "使用系统根证书校验"
}

// signedDo 发一个带 HMAC 签名的请求。
//
// 签名用的 path 必须是**不含 query** 的路径，与服务端 c.Request.URL.Path 对齐。
func signedDo(ctx context.Context, client *http.Client, method, fullURL, secret string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if secret != "" {
		u, perr := url.Parse(fullURL)
		if perr != nil {
			return nil, perr
		}
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := newNonce()
		req.Header.Set(headerTimestamp, ts)
		req.Header.Set(headerNonce, nonce)
		req.Header.Set(headerSignature, computeSignature(secret, method, u.Path, ts, nonce, body))
	}
	return client.Do(req)
}

// checkHeartbeat 用与常驻端完全相同的方式打一次心跳。
//
// 这是真正的端到端验证：一次调用同时证明了「网络可达 + 密钥正确 + 证书可信 +
// 时间戳偏差在窗口内」。任何一环不对，这一步就会亮红。
func checkHeartbeat(ctx context.Context, client *http.Client, cfg EnrollConfig, secret string) (bool, string) {
	payload := map[string]any{
		"client_id":   cfg.ClientID,
		"client_name": cfg.ClientID,
		"os":          runtime.GOOS,
		"ip_address":  localIP(),
		"status":      "online",
		"agents":      []any{},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, "构造心跳失败: " + err.Error()
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := signedDo(reqCtx, client, http.MethodPost, cfg.ServerURL+"/api/v1/cluster/heartbeat", secret, body)
	if err != nil {
		return false, "心跳请求失败: " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, fmt.Sprintf("中枢拒绝了心跳（HTTP %d）：集群密钥不一致或时间偏差超过 5 分钟。原文：%s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Sprintf("心跳异常（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return true, "中枢已确认心跳"
}

// checkDeviceRegistered 轮询中枢的设备列表，确认本节点真的上线了。
//
// 为什么还要这一步：心跳成功只说明「我能发出去」，不代表服务进程活着。
// 服务进程才是那个要长期跑的东西 —— 它在 10 秒内会自己发一次心跳，
// 这里就是等那一次发生。
func checkDeviceRegistered(ctx context.Context, client *http.Client, cfg EnrollConfig, secret string) (bool, string) {
	deadline := time.Now().Add(devicePollTimeout)
	var lastDetail = "等待常驻服务上报心跳"

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return false, "已取消"
		}
		reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		resp, err := signedDo(reqCtx, client, http.MethodGet, cfg.ServerURL+"/api/v1/cluster/devices", secret, nil)
		cancel()
		if err != nil {
			lastDetail = "查询设备列表失败: " + err.Error()
		} else {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				lastDetail = fmt.Sprintf("查询设备列表异常（HTTP %d）", resp.StatusCode)
			} else if ok, d := findOnlineDevice(raw, cfg.ClientID); ok {
				return true, d
			} else {
				lastDetail = d
			}
		}
		time.Sleep(devicePollInterval)
	}
	return false, lastDetail
}

// findOnlineDevice 在设备列表响应里找本节点。
// 与服务端 store 的 staleAfter（30 秒）对齐：超过 60 秒没心跳视为未上线。
func findOnlineDevice(raw []byte, clientID string) (bool, string) {
	var list []struct {
		ClientID      string `json:"client_id"`
		Status        string `json:"status"`
		LastHeartbeat int64  `json:"last_heartbeat"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return false, "设备列表格式异常"
	}
	now := time.Now().Unix()
	for _, d := range list {
		if d.ClientID != clientID {
			continue
		}
		if d.Status == "online" && now-d.LastHeartbeat <= 60 {
			return true, fmt.Sprintf("中枢已看到本节点 %s（状态 online）", clientID)
		}
		return false, fmt.Sprintf("本节点已登记但心跳不新鲜（状态 %s，%d 秒前）", d.Status, now-d.LastHeartbeat)
	}
	return false, fmt.Sprintf("中枢设备列表中暂时没有 %s，可能常驻服务尚未发出第一次心跳", clientID)
}

// reportEnrollment 把自检结果回传中枢，供控制台展示。
func reportEnrollment(ctx context.Context, cfg EnrollConfig, res *EnrollResult) error {
	steps, err := json.Marshal(res.Steps)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"client_id":  cfg.ClientID,
		"hostname":   cfg.ClientID,
		"os":         runtime.GOOS,
		"ip_address": localIP(),
		"ok":         res.OK,
		"steps":      json.RawMessage(steps),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// 回传用的是**不受 CA 严格校验约束**的客户端吗？不是 —— 这里已经拿到 CA，
	// 走的是固定 CA 的客户端。但回传失败不该影响接入结论，因此调用方只把它
	// 记成非关键步骤。
	endpoint := fmt.Sprintf("%s/join/%s/report", cfg.ServerURL, url.PathEscape(cfg.Code))
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 该端点不需要签名（它在 /join 公开组下），但带上签名头也无害 ——
	// 服务端对它不做 HMAC 校验。
	client, _ := buildVerifiedClient(res.Playbook)
	resp, err := signedDo(reqCtx, client, http.MethodPost, endpoint, "", body)
	if err != nil {
		return fmt.Errorf("回传失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("回传被拒（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// ensureService 注册服务；已注册则重启让它读到新配置。
//
// 「已注册」不是错误：运维很可能是在改完中枢地址后重跑一次接入命令。
func ensureService(h ServiceHooks, binPath string, out io.Writer) error {
	if h.Install == nil {
		return errors.New("当前平台未提供服务注册能力")
	}
	err := h.Install(binPath)
	if err == nil {
		return nil
	}
	if h.AlreadyExists != nil && h.AlreadyExists(err) {
		if h.Restart == nil {
			return nil
		}
		fmt.Fprintf(out, "  · 服务已注册，改为重启以加载新配置\n")
		return h.Restart()
	}
	return err
}

// serviceRunning 查询服务状态；未注入检查函数时按「未知但不算失败」处理。
func serviceRunning(h ServiceHooks) (bool, string) {
	if h.Status == nil {
		return true, "未提供状态查询"
	}
	ok, state := h.Status()
	return ok, state
}

// installBinaryFile 把当前可执行文件复制到安装目录，返回目标路径与是否发生了复制。
//
// 必须先复制再注册服务：引导脚本是在临时目录里执行下载来的程序的，
// 服务若指向那一份，临时目录一清理服务就再也起不来。
func installBinaryFile(installDir string) (string, bool, error) {
	binName := "agent-mesh-client"
	if runtime.GOOS == "windows" {
		binName = "agent-mesh-client.exe"
	}
	dest := filepath.Join(installDir, binName)

	self, err := os.Executable()
	if err != nil {
		return "", false, fmt.Errorf("定位自身路径失败: %w", err)
	}

	absSelf, err1 := filepath.Abs(self)
	absDest, err2 := filepath.Abs(dest)
	if err1 == nil && err2 == nil && samePath(absSelf, absDest) {
		return dest, false, nil
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", false, fmt.Errorf("创建安装目录失败: %w", err)
	}

	src, err := os.Open(self)
	if err != nil {
		return "", false, fmt.Errorf("打开自身可执行文件失败: %w", err)
	}
	defer src.Close()

	// 先写临时文件再改名：避免直接覆盖导致「写了一半被中断，程序和配置都不完整」。
	tmp := dest + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", false, fmt.Errorf("写入安装目录失败（需管理员/root 权限）: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(tmp)
		return "", false, fmt.Errorf("复制程序失败: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		// 目标被正在运行的服务占用时，Windows 上会走到这里。
		return "", false, fmt.Errorf(
			"覆盖安装目录中的程序失败（旧版本可能正在运行）：%w\n"+
				"        请先停止并卸载已有服务，再重新执行接入命令", err)
	}
	return dest, true, nil
}

// samePath 比较两个路径是否指向同一个文件（Windows 大小写不敏感）。
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// writeFileAtomic 原子写文件。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录失败: %w", err)
		}
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
