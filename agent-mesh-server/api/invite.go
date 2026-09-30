package api

import (
	"encoding/json"
	"errors"
	"fmt"
	htmltmpl "html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	texttmpl "text/template"
	"time"

	"agent-mesh-server/store"
	"agent-mesh-server/web"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 自助交付：邀请码 -> 一行命令 -> 装完自检回传
//
// 为什么要有这一层：原来的接入方式是「手工把服务端地址、集群密钥、CA 路径
// 填到每台机器的 agent-mesh.json 里」。十几个人的公司意味着十几次远程协助，
// 每一次都是一通电话加一次截图。把这段工序开发一次，售后工时一次性归零。
//
// 模型边界（必须说清楚，避免误解成强席位控制）：
//   我们的鉴权是「集群共享密钥」。邀请码负责**安全地把密钥送到机器上**，
//   并留下谁在什么时候接入的审计线索；它**不是**按设备签发独立密钥的席位系统。
//   因此 max_uses 只能约束「换取配置包」这个动作的次数。
//   真正的强席位控制需要改成按设备签发密钥，属于后续工作项。
// =====================================================================

// 入网相关的运行期上下文，由 main 在启动时注入。
//
// 之所以用包级变量而不是参数透传：与既有的 api.TaskTimeout / api.FileStoreDir
// 保持一致，改动面最小；这些值在进程生命周期内不变。
var (
	// ClusterSecret 是要下发给客户端的集群共享密钥。
	// 它只会出现在「有效邀请码 + 限流」的 /join/<code>/playbook.json 响应里。
	ClusterSecret string

	// PublicBaseURL 覆盖对外基址（反向代理 / 隧道场景）。
	// 留空时按请求的 Host 与协议推导，适用于客户端直连中枢的常见内网部署。
	PublicBaseURL string

	// ClientPackDir 是客户端二进制所在目录。留空表示该能力不可用，
	// 落地页会给出「管理员需要先编译并放置客户端」的明确指引。
	ClientPackDir string

	// TLSCAPEM 是服务端自签 CA 的 PEM 内容；为空表示未启用自签 HTTPS。
	TLSCAPEM string

	// EnrollDefaultTTL 是签发邀请码时的默认有效期。
	EnrollDefaultTTL = 30 * time.Minute

	// EnrollMaxTTL 是允许签发的最大有效期（7 天）。
	EnrollMaxTTL = 7 * 24 * time.Hour
)

// 邀请码相关的请求/响应结构。

// issueInviteReq 是控制台签发邀请码的请求体。
type issueInviteReq struct {
	Label      string `json:"label"`
	TTLMinutes int    `json:"ttl_minutes"`
	MaxUses    *int   `json:"max_uses"` // nil 视为 1（一码一机）
}

// reportReq 是客户端自检结果回传体。
type reportReq struct {
	ClientID  string          `json:"client_id"`
	Hostname  string          `json:"hostname"`
	OS        string          `json:"os"`
	IPAddress string          `json:"ip_address"`
	OK        bool            `json:"ok"`
	Steps     json.RawMessage `json:"steps"`
}

// playbookResp 是客户端 enroll 时获取的「配置包」。
// 字段名一旦发布就是契约，改动必须双端同步。
type playbookResp struct {
	Version       int               `json:"version"`
	Product       string            `json:"product"`
	Code          string            `json:"code"`
	Label         string            `json:"label,omitempty"`
	ServerURL     string            `json:"server_url"`
	Secret        string            `json:"secret"`
	TLSCAPEM      string            `json:"tls_ca_pem,omitempty"`
	Client        map[string]string `json:"client"`
	Install       map[string]string `json:"install"`
	Checks        []string          `json:"checks"`
	ExpiresAt     int64             `json:"expires_at"`
	RemainingUses int               `json:"remaining_uses"`
}

// =====================================================================
// 控制台接口（由 main 挂在 Basic Auth 保护下的 /console/api）
// =====================================================================

// ConsoleIssueInvite 签发一条邀请码，返回明文码与可直接复制的接入命令。
//
// 明文码只在这一刻出现一次：库里存的是 sha256 哈希，之后任何接口都查不出来。
// 这是刻意的设计——控制台被翻遍也不该翻出可用的凭证。
func ConsoleIssueInvite(c *gin.Context) {
	var req issueInviteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_json", "message": "请求体不是合法 JSON"})
		return
	}

	ttl := EnrollDefaultTTL
	if req.TTLMinutes > 0 {
		ttl = time.Duration(req.TTLMinutes) * time.Minute
	}
	if ttl > EnrollMaxTTL {
		ttl = EnrollMaxTTL
	}
	if ttl < time.Minute {
		ttl = time.Minute
	}

	maxUses := 1
	if req.MaxUses != nil {
		maxUses = *req.MaxUses
		if maxUses < 0 {
			maxUses = 0
		}
		if maxUses > 1000 {
			maxUses = 1000
		}
	}

	code, inv, err := store.CreateInvite(req.Label, ttl, maxUses)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create_failed", "message": err.Error()})
		return
	}

	base := requestBaseURL(c)
	fmt.Printf("[入网] 已签发邀请码 %s（备注:%q 有效期:%s 次数:%s）\n",
		store.MaskInviteCode(code), inv.Label, ttl, usesLabel(maxUses))

	c.JSON(http.StatusOK, gin.H{
		"code":         code,
		"id":           inv.ID(),
		"label":        inv.Label,
		"max_uses":     inv.MaxUses,
		"use_count":    0,
		"status":       inv.Status,
		"expires_at":   inv.ExpiresAt,
		"landing_url":  base + "/join/" + store.NormalizeInviteCode(code),
		"windows_cmd":  joinCommand(base, code, "windows"),
		"linux_cmd":    joinCommand(base, code, "linux"),
		"client_ready": clientPackAvailable(),
	})
}

// ConsoleListInvites 返回邀请码列表（只含哈希前缀，不含明文）。
func ConsoleListInvites(c *gin.Context) {
	list, err := store.ListInvites()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "message": err.Error()})
		return
	}
	type row struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		MaxUses   int    `json:"max_uses"`
		UseCount  int    `json:"use_count"`
		Status    string `json:"status"`
		ExpiresAt int64  `json:"expires_at"`
		CreatedAt int64  `json:"created_at"`
		UsedBy    string `json:"used_by"`
	}
	out := make([]row, 0, len(list))
	for _, it := range list {
		out = append(out, row{
			ID: it.ID(), Label: it.Label, MaxUses: it.MaxUses, UseCount: it.UseCount,
			Status: it.Status, ExpiresAt: it.ExpiresAt, CreatedAt: it.CreatedAt, UsedBy: it.UsedBy,
		})
	}
	c.JSON(http.StatusOK, out)
}

// ConsoleRevokeInvite 作废邀请码。selector 可以是明文码、完整哈希或列表里的短 ID。
func ConsoleRevokeInvite(c *gin.Context) {
	var req struct {
		Selector string `json:"selector"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_json", "message": "请求体不是合法 JSON"})
		return
	}
	n, err := store.RevokeInvite(req.Selector)
	if err != nil {
		status, code := inviteErrorStatus(err)
		c.JSON(status, gin.H{"error": code, "message": err.Error()})
		return
	}
	fmt.Printf("[入网] 已作废 %d 条邀请码\n", n)
	c.JSON(http.StatusOK, gin.H{"revoked": n})
}

// ConsoleEnrollments 返回入网自检记录。
func ConsoleEnrollments(c *gin.Context) {
	limit := queryLimit(c, 50, 200)
	list, err := store.ListEnrollments(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

// =====================================================================
// 路由注册
//
// 放在 api 包里而不是 main：这样集成测试能直接建同构的路由表来验证，
// 不必把 main 的路由逻辑复制一份到测试里（复制出来的那份必然和真的漂移）。
// =====================================================================

// RegisterJoinRoutes 注册公开的入网路由组，并挂上按 IP 的限流。
//
// 路由形状刻意与邀请码命名空间对齐（/join/<码>/xxx）：
// 「一个码 = 一个可分享的落地页」在 URL 上就自解释了。
func RegisterJoinRoutes(r *gin.Engine) {
	join := r.Group("/join")
	join.Use(JoinRateLimitMiddleware())
	join.GET("/:code", JoinLanding)
	join.GET("/:code/playbook.json", JoinPlaybook)
	join.GET("/:code/client", JoinClientPack)
	join.GET("/:code/install.ps1", JoinInstallPS1)
	join.GET("/:code/install.sh", JoinInstallSH)
	join.POST("/:code/report", JoinReport)
}

// RegisterConsoleInviteRoutes 在已挂好 Basic Auth 的 /console 组上注册入网管理接口。
func RegisterConsoleInviteRoutes(console *gin.RouterGroup) {
	console.GET("/api/invites", ConsoleListInvites)
	console.POST("/api/invites", ConsoleIssueInvite)
	console.POST("/api/invites/revoke", ConsoleRevokeInvite)
	console.GET("/api/enrollments", ConsoleEnrollments)
}

// =====================================================================
// 公开端点（挂 /join，限流保护，无需签名）
// =====================================================================

// JoinLanding 返回邀请码落地页。
//
// 这一步**不消费名额**：员工手滑刷新页面不该烧掉一个装机名额。
func JoinLanding(c *gin.Context) {
	if err := initJoinTemplates(); err != nil {
		c.String(http.StatusInternalServerError, "模板初始化失败: %v", err)
		return
	}
	code := store.NormalizeInviteCode(c.Param("code"))
	base := requestBaseURL(c)

	data := map[string]any{
		"Code":        displayCode(code),
		"Server":      base,
		"Usable":      false,
		"ClientReady": clientPackAvailable(),
		"WinCmd":      joinCommand(base, code, "windows"),
		"ShCmd":       joinCommand(base, code, "linux"),
		"ExpiresAt":   "—",
		"Remaining":   "—",
		"Label":       "",
		"Reason":      "邀请码不存在或已被清理。",
		"StatusLabel": "无效",
		"StatusClass": "expired",
	}

	inv, err := store.GetInviteByCode(code)
	if err == nil {
		data["ExpiresAt"] = time.Unix(inv.ExpiresAt, 0).Format("2006-01-02 15:04:05")
		data["Remaining"] = remainingLabel(inv)
		data["Label"] = inv.Label
		data["StatusLabel"] = inviteStatusLabel(inv.Status)
		data["StatusClass"] = inv.Status
		if inv.Status == store.InviteActive {
			data["Usable"] = true
		} else {
			data["Reason"] = inviteReason(inv.Status)
		}
	}

	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	if err := joinPageTmpl.Execute(c.Writer, data); err != nil {
		fmt.Printf("[入网] 渲染落地页失败: %v\n", err)
	}
	c.Status(http.StatusOK)
}

// JoinPlaybook 下发客户端配置包 —— 这是**唯一会消费名额**的端点。
//
// 语义：取走配置包就等于用掉一次名额。
// 例外：同一台机器（同名 client_id）重复获取不重复扣次数，
// 这样「脚本重跑一次」「网络抖动重试」不会白白烧掉名额。
func JoinPlaybook(c *gin.Context) {
	code := store.NormalizeInviteCode(c.Param("code"))
	clientID := strings.TrimSpace(c.Query("client_id"))

	// 先只判「存在」——不能一开始就用 ValidateInvite 判可用性，
	// 否则一码一机在首次消费后就变成 exhausted，同一台机器重取会被误拒。
	inv, err := store.GetInviteByCode(code)
	if err != nil {
		status, errCode := inviteErrorStatus(err)
		c.JSON(status, gin.H{"error": errCode, "message": err.Error()})
		return
	}

	// 同一台机器重取：不重复消费。
	// 两个非空条件都要判，否则「空 client_id」会命中「空 used_by」而白放行。
	if !(clientID != "" && inv.UsedBy != "" && inv.UsedBy == clientID) {
		// 消费本身就把「存在 / 未作废 / 未过期 / 还有名额」四条规则一并校验了，
		// 且是原子的，不需要在这里再重复判一遍。
		inv, err = store.ConsumeInvite(code, clientID)
		if err != nil {
			status, errCode := inviteErrorStatus(err)
			c.JSON(status, gin.H{"error": errCode, "message": err.Error()})
			return
		}
		fmt.Printf("[入网] 邀请码 %s 已用于 %s（%d/%s）\n",
			store.MaskInviteCode(code), fallback(clientID, "未知主机"),
			inv.UseCount, usesLabel(inv.MaxUses))
	}

	base := requestBaseURL(c)
	resp := playbookResp{
		Version:       1,
		Product:       "agent-mesh",
		Code:          displayCode(code),
		Label:         inv.Label,
		ServerURL:     base,
		Secret:        ClusterSecret,
		TLSCAPEM:      TLSCAPEM,
		Client:        map[string]string{"windows": base + "/join/" + code + "/client?os=windows", "linux": base + "/join/" + code + "/client?os=linux"},
		Install:       map[string]string{"windows": joinCommand(base, code, "windows"), "linux": joinCommand(base, code, "linux")},
		Checks:        []string{"config_written", "tls_ca_trusted", "service_running", "heartbeat_ok", "device_registered"},
		ExpiresAt:     inv.ExpiresAt,
		RemainingUses: remainingUses(inv),
	}

	// 配置包含密钥，任何中间层都不许缓存。
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.JSON(http.StatusOK, resp)
}

// JoinClientPack 分发客户端二进制，让员工不必手工拷贝 EXE。
func JoinClientPack(c *gin.Context) {
	code := store.NormalizeInviteCode(c.Param("code"))
	if _, err := store.ValidateInvite(code); err != nil {
		status, code2 := inviteErrorStatus(err)
		c.JSON(status, gin.H{"error": code2, "message": err.Error()})
		return
	}

	osName := strings.ToLower(strings.TrimSpace(c.DefaultQuery("os", "windows")))
	path, err := findClientPack(osName)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "client_pack_unavailable",
			"message": err.Error(),
			"action":  "在服务端编译客户端并放到 -client-pack 指定目录，例如 client-windows-amd64.exe 与 client-linux-amd64",
		})
		return
	}

	c.Header("Content-Disposition", "attachment; filename=\""+filepath.Base(path)+"\"")
	c.Header("Cache-Control", "no-store")
	c.File(path)
}

// JoinInstallPS1 下发 Windows 引导脚本（内容随邀请码动态生成）。
func JoinInstallPS1(c *gin.Context) {
	joinScript(c, "windows")
}

// JoinInstallSH 下发 Linux/macOS 引导脚本。
func JoinInstallSH(c *gin.Context) {
	joinScript(c, "linux")
}

// JoinReport 接收客户端自检结果。
//
// 这里**不消费名额**（名额在取配置包时已扣），只做记录与失败归因。
// 因此即使码已用尽，也必须允许上报 —— 否则最后一步的失败信息会凭空消失，
// 运维就只能看着一台不上线的机器猜原因。
func JoinReport(c *gin.Context) {
	code := store.NormalizeInviteCode(c.Param("code"))
	if _, err := store.GetInviteByCode(code); err != nil {
		status, code2 := inviteErrorStatus(err)
		c.JSON(status, gin.H{"error": code2, "message": err.Error()})
		return
	}

	var req reportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_json", "message": "请求体不是合法 JSON"})
		return
	}

	steps := "[]"
	if len(req.Steps) > 0 {
		steps = string(req.Steps)
	}

	inv, _ := store.GetInviteByCode(code)
	label := ""
	if inv != nil {
		label = inv.Label
	}

	if err := store.RecordEnrollment(
		store.MaskInviteCode(code), label, req.ClientID, req.Hostname,
		req.OS, req.IPAddress, steps, req.OK,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "record_failed", "message": err.Error()})
		return
	}

	mark := "失败"
	if req.OK {
		mark = "成功"
	}
	fmt.Printf("[入网] 自检%s：%s（%s / %s）\n",
		mark, fallback(req.ClientID, "未命名节点"), req.OS, req.IPAddress)

	c.JSON(http.StatusOK, gin.H{"recorded": true})
}

// =====================================================================
// 内部工具
// =====================================================================

var (
	joinPageTmpl *htmltmpl.Template
	joinPSTmpl   *texttmpl.Template
	joinSHTmpl   *texttmpl.Template
	joinTmplOnce sync.Once
	joinTmplErr  error
)

// initJoinTemplates 惰性解析内嵌模板，只做一次。
// 解析失败会一直返回同一个错误（而不是 panic），避免服务端因为一个模板笔误直接起不来。
func initJoinTemplates() error {
	joinTmplOnce.Do(func() {
		joinPageTmpl, joinTmplErr = htmltmpl.New("join").Parse(web.JoinHTML)
		if joinTmplErr != nil {
			return
		}
		joinPSTmpl, joinTmplErr = texttmpl.New("ps1").Parse(web.InstallClientPSTmpl)
		if joinTmplErr != nil {
			return
		}
		joinSHTmpl, joinTmplErr = texttmpl.New("sh").Parse(web.InstallClientSHTmpl)
	})
	return joinTmplErr
}

// joinScript 按平台渲染并下发引导脚本。
func joinScript(c *gin.Context, osName string) {
	if err := initJoinTemplates(); err != nil {
		c.String(http.StatusInternalServerError, "模板初始化失败: %v", err)
		return
	}
	code := store.NormalizeInviteCode(c.Param("code"))
	if _, err := store.ValidateInvite(code); err != nil {
		status, code2 := inviteErrorStatus(err)
		c.JSON(status, gin.H{"error": code2, "message": err.Error()})
		return
	}

	base := requestBaseURL(c)
	data := struct {
		Server    string
		Code      string
		ClientURL string
		HTTPS     bool
	}{
		Server:    base,
		Code:      displayCode(code),
		ClientURL: base + "/join/" + code + "/client?os=" + osName,
		HTTPS:     strings.HasPrefix(base, "https://"),
	}

	// 脚本是纯文本，必须声明正确类型，否则 irm | iex 与 curl | sh 都可能被浏览器/代理改写。
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/plain; charset=utf-8")

	var err error
	if osName == "windows" {
		err = joinPSTmpl.Execute(c.Writer, data)
	} else {
		err = joinSHTmpl.Execute(c.Writer, data)
	}
	if err != nil {
		fmt.Printf("[入网] 渲染引导脚本失败: %v\n", err)
	}
}

// requestBaseURL 推导本次请求对外的基址。
func requestBaseURL(c *gin.Context) string {
	if PublicBaseURL != "" {
		return strings.TrimRight(PublicBaseURL, "/")
	}
	scheme := "http"
	if c.Request != nil && c.Request.TLS != nil {
		scheme = "https"
	}
	// 走反向代理/隧道时，由代理声明真实协议。
	// 这里只在没有显式配置 PublicBaseURL 时采用，配置项始终优先。
	if c.Request != nil {
		if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
			if comma := strings.Index(p, ","); comma > 0 {
				p = p[:comma]
			}
			p = strings.TrimSpace(p)
			if p == "http" || p == "https" {
				scheme = p
			}
		}
	}
	host := "127.0.0.1"
	if c.Request != nil && c.Request.Host != "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

// joinCommand 生成给用户复制的接入命令。
func joinCommand(base, code, osName string) string {
	code = store.NormalizeInviteCode(code)
	if osName == "windows" {
		return fmt.Sprintf(`irm "%s/join/%s/install.ps1" | iex`, base, code)
	}
	return fmt.Sprintf(`curl -fsSL "%s/join/%s/install.sh" | sudo sh`, base, code)
}

// displayCode 把归一化的码重新分组为 XXXX-XXXX-XXXX-XXXX 便于念与抄。
func displayCode(code string) string {
	n := store.NormalizeInviteCode(code)
	if len(n) != 16 {
		return n
	}
	return n[0:4] + "-" + n[4:8] + "-" + n[8:12] + "-" + n[12:16]
}

// clientPackAvailable 判断客户端二进制是否已就位（落地页据此决定是否展示命令）。
func clientPackAvailable() bool {
	if ClientPackDir == "" {
		return false
	}
	if _, err := findClientPack("windows"); err == nil {
		return true
	}
	if _, err := findClientPack("linux"); err == nil {
		return true
	}
	return false
}

// clientPackCandidates 返回按优先级排列的候选文件名。
// 约定命名 let 运维一眼能看出哪个文件给哪个平台。
func clientPackCandidates(osName string) []string {
	switch osName {
	case "windows":
		return []string{"client-windows-amd64.exe", "mesh-client.exe", "client.exe", "agent-mesh-client.exe"}
	default:
		return []string{"client-linux-amd64", "mesh-client-linux-amd64", "agent-mesh-client", "client"}
	}
}

// findClientPack 在客户端目录里定位指定平台的二进制。
func findClientPack(osName string) (string, error) {
	dir := ClientPackDir
	if dir == "" {
		return "", errors.New("服务端未配置客户端目录（-client-pack）")
	}
	for _, name := range clientPackCandidates(osName) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}

	// 兜底：目录里任意一个文件名含平台关键字的普通文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("读取客户端目录失败: %w", err)
	}
	key := "linux"
	if osName == "windows" {
		key = "windows"
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.Contains(strings.ToLower(e.Name()), key) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("客户端目录 %s 中找不到 %s 平台的安装包", dir, osName)
}

// inviteErrorStatus 把存储层错误映射成 HTTP 状态与稳定的错误码。
//
// 错误码是契约：控制台与客户端的文案都按它来选，不再靠猜 HTTP 语义。
func inviteErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrInviteNotFound):
		return http.StatusNotFound, "invite_not_found"
	case errors.Is(err, store.ErrInviteRevoked):
		return http.StatusGone, "invite_revoked"
	case errors.Is(err, store.ErrInviteExpired):
		return http.StatusGone, "invite_expired"
	case errors.Is(err, store.ErrInviteExhausted):
		return http.StatusConflict, "invite_exhausted"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

// inviteStatusLabel 是状态的界面文案。
func inviteStatusLabel(status string) string {
	switch status {
	case store.InviteActive:
		return "可用"
	case store.InviteExhausted:
		return "已用尽"
	case store.InviteExpired:
		return "已过期"
	case store.InviteRevoked:
		return "已作废"
	default:
		return status
	}
}

// inviteReason 是页面上的原因说明（比「不可用」有用得多）。
func inviteReason(status string) string {
	switch status {
	case store.InviteExhausted:
		return "该邀请码的可用次数已经用完。"
	case store.InviteExpired:
		return "该邀请码已超过有效期。"
	case store.InviteRevoked:
		return "该邀请码已被管理员作废。"
	default:
		return "该邀请码当前不可用。"
	}
}

// remainingLabel 是剩余次数的人话表示。
func remainingLabel(inv *store.Invite) string {
	if inv.MaxUses == 0 {
		return "不限"
	}
	left := inv.MaxUses - inv.UseCount
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("%d 次", left)
}

// remainingUses 是剩余次数的数值表示（0 表示不限）。
func remainingUses(inv *store.Invite) int {
	if inv.MaxUses == 0 {
		return 0
	}
	left := inv.MaxUses - inv.UseCount
	if left < 0 {
		left = 0
	}
	return left
}

// usesLabel 把 max_uses 转成日志里的人话。
func usesLabel(maxUses int) string {
	if maxUses == 0 {
		return "不限次数"
	}
	return fmt.Sprintf("%d 次", maxUses)
}

// fallback 返回首个非空字符串。
func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
