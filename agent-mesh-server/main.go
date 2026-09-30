package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-mesh-server/api"
	"agent-mesh-server/config"
	"agent-mesh-server/internal/filelog"
	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// installTakeover 决定 install 子命令在「同名服务指向另一路径」时的行为。
// 默认接管：服务名本机唯一，不接管就等于把旧版本留在自启动序列里。
// --no-takeover 用于那种确实不想动现有服务的场合（比如同时维护两套实例）。
var installTakeover = true

// applyInstallFlags 按 install 子命令的参数更新安装行为开关。
func applyInstallFlags(args []string) {
	installTakeover = !hasFlag(args, "--no-takeover")
}

// hasFlag 在子命令的剩余参数里查找开关，找到返回 true。
// 单独抽出来是为了能写单测：Windows 服务注册的实际效果没法在 CI 里断言，
// 但"用户打了这个开关到底生效没有"必须有人验证。
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func main() {
	cfg := config.Load()

	addr := flag.String("addr", cfg.Addr, "HTTP 监听地址")
	dbPath := flag.String("db", cfg.DB, "SQLite 数据文件路径")
	debug := flag.Bool("debug", cfg.Debug, "开启 gin 调试模式（含请求日志）")
	secret := flag.String("secret", cfg.Secret,
		"集群共享密钥；非空时对 /api/v1 启用 HMAC 签名鉴权（客户端需配置同一密钥）")
	consoleUser := flag.String("console-user", cfg.ConsoleUser, "控制台 Basic Auth 用户名")
	consolePass := flag.String("console-pass", cfg.ConsolePass, "控制台 Basic Auth 口令")
	tlsCert := flag.String("tls-cert", cfg.TLSCert, "TLS 证书路径；与 -tls-key 同时非空则启用 HTTPS")
	tlsKey := flag.String("tls-key", cfg.TLSKey, "TLS 私钥路径")
	logDir := flag.String("log-dir", cfg.LogDir, "日志落盘目录；服务模式下必填，否则日志无人接收")
	retention := flag.Duration("retention", cfg.Retention,
		"审计日志与已完结任务的保留时长，超期自动清理（如 720h、30d）")
	taskTimeout := flag.Duration("task-timeout", cfg.TaskTimeout,
		"任务从被节点领走到必须回传结果的上限，超时即回收重投（须大于客户端 3 分钟的执行超时）")
	filesDir := flag.String("files-dir", cfg.FilesDir,
		"中转文件实体的落盘目录；留空时取数据库同级目录下的 files/")
	publicURL := flag.String("public-url", cfg.PublicURL,
		"邀请页与安装脚本里对外声明的基址（如 https://mesh.example.com）；"+
			"反向代理/隧道场景必填，留空则按请求 Host 推导")
	clientPack := flag.String("client-pack", cfg.ClientPack,
		"客户端二进制所在目录，供 /join/<码>/client 分发；"+
			"留空时该端点返回 503 并在落地页给出放置指引")
	flag.Parse()

	// 安装 / 卸载子命令：必须在接管日志之前处理，
	// 否则成功与失败信息会全部写进日志文件，执行者在控制台里什么都看不到。
	if args := flag.Args(); len(args) > 0 {
		switch args[0] {
		case "install":
			applyInstallFlags(args[1:])
			if err := installService(); err != nil {
				fmt.Printf("[安装] 失败: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("[安装] 服务已注册并启动")
			return
		case "uninstall":
			if err := uninstallService(); err != nil {
				fmt.Printf("[卸载] 失败: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("[卸载] 服务已移除")
			return
		case "gencert":
			fs := flag.NewFlagSet("gencert", flag.ExitOnError)
			outDir := fs.String("out", "", "证书输出目录（默认 exe 同目录下 certs/）")
			var extraHosts stringList
			fs.Var(&extraHosts, "host",
				"额外写入证书 SAN 的域名或 IP，可重复；跨网/隧道部署必填（如 -host mesh.example.com -host 1.2.3.4）")
			_ = fs.Parse(args[1:])

			dir := *outDir
			// 兼容旧写法：gencert <目录>（位置参数）
			if dir == "" && fs.NArg() > 0 {
				dir = fs.Arg(0)
			}
			if dir == "" {
				dir = filepath.Join(exeDir(), "certs")
			}

			caPath, certPath, keyPath, err := generateCerts(dir, extraHosts)
			if err != nil {
				fmt.Printf("[证书] 生成失败: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("[证书] 已生成自签证书：")
			fmt.Printf("  CA 证书（分发给每个客户端，配到 tls_ca）: %s\n", caPath)
			fmt.Printf("  服务端证书: %s\n", certPath)
			fmt.Printf("  服务端私钥（不要分发）: %s\n", keyPath)
			if len(extraHosts) > 0 {
				fmt.Printf("  额外 SAN: %s\n", strings.Join(extraHosts, ", "))
			}
			fmt.Println("  客户端若报证书校验失败，多半是没配 tls_ca，或证书 SAN 里没有实际访问的那个 IP/域名。")
			return
		default:
			fmt.Printf("[提示] 未知子命令 %q，可用：install / uninstall\n", args[0])
			os.Exit(2)
		}
	}

	inService := isServiceSession()

	// 服务模式：接管 stdout/stderr 到日志文件。
	// 这一步必须尽早做——Windows 服务没有控制台，之前的所有打印都会丢进黑洞。
	var logHandle *filelog.Handle
	if inService {
		if *logDir == "" {
			// 兜底到 ProgramData：Program Files 下写数据不规范，System32 更不行。
			*logDir = filepath.Join(os.Getenv("ProgramData"), "AgentMesh", "logs")
		}
		h, err := filelog.Redirect(filelog.Options{Dir: *logDir, Name: "agent-mesh-server"})
		if err != nil {
			// 日志都还没接上，只能硬退出；正常启动前应先验证目录可写。
			os.Exit(1)
		}
		logHandle = h
		defer h.Close()

		// gin 与标准库 log 在包初始化时就快照了 os.Stdout / os.Stderr，
		// 替换变量后必须显式重设，否则它们的日志仍流向原来的句柄。
		gin.DefaultWriter = os.Stdout
		gin.DefaultErrorWriter = os.Stderr
		log.SetOutput(os.Stderr)
	}

	// 小于客户端执行超时会让正在跑的任务被误判超时并重投，导致同一条指令被执行两遍。
	if *taskTimeout <= 3*time.Minute {
		fmt.Printf("[警告] -task-timeout=%s 不大于客户端执行超时 3 分钟，可能导致重复执行，已回落为 5 分钟\n", *taskTimeout)
		*taskTimeout = 5 * time.Minute
	}
	api.TaskTimeout = *taskTimeout

	// 服务进程的当前工作目录是 System32，相对路径会把数据库写到错误的地方。
	*dbPath = resolvePath(*dbPath)

	// 中转文件目录默认跟着数据库走：两者同处一个数据目录，
	// 备份、清理、迁移都只用处理一个位置。显式配置时以配置为准。
	if *filesDir != "" {
		*filesDir = resolvePath(*filesDir)
	} else {
		*filesDir = filepath.Join(filepath.Dir(*dbPath), "files")
	}

	// 客户端目录同样不能是相对路径：服务进程的工作目录是 System32。
	if *clientPack != "" {
		*clientPack = resolvePath(*clientPack)
	}

	run := func(ctx context.Context) {
		startServer(runOpts{
			addr:        *addr,
			dbPath:      *dbPath,
			debug:       *debug,
			secret:      *secret,
			consoleUser: *consoleUser,
			consolePass: *consolePass,
			tlsCert:     *tlsCert,
			tlsKey:      *tlsKey,
			retention:   *retention,
			filesDir:    *filesDir,
			publicURL:   *publicURL,
			clientPack:  *clientPack,
			logPath:     logFilePath(logHandle),
		})
	}

	if inService {
		if err := runService(run); err != nil {
			fmt.Printf("[服务] 退出: %v\n", err)
		}
		return
	}
	run(context.Background())
}

type runOpts struct {
	addr        string
	dbPath      string
	debug       bool
	secret      string
	consoleUser string
	consolePass string
	tlsCert     string
	tlsKey      string
	retention   time.Duration
	filesDir    string
	publicURL   string
	clientPack  string
	logPath     string
}

func startServer(o runOpts) {
	fmt.Println("==================================================")
	fmt.Println("   Agent Mesh 本地 AI 协作中枢 - 中央汇总服务端")
	fmt.Println("==================================================")

	if err := store.InitDB(o.dbPath); err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer store.DB.Close()

	if o.debug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	if o.logPath != "" {
		fmt.Printf("[日志] 已接管输出，落盘于 %s\n", o.logPath)
	}

	// 中转文件存储：目录建不出来就干脆禁用该能力（上传接口返回 503），
	// 而不是带着一个坏路径继续跑 —— 那会在第一次上传时才炸。
	if o.filesDir != "" {
		if err := os.MkdirAll(o.filesDir, 0o755); err != nil {
			fmt.Printf("[文件中转] 警告：无法创建存储目录 %s: %v，该能力已禁用\n", o.filesDir, err)
		} else {
			api.FileStoreDir = o.filesDir
			fmt.Printf("[文件中转] 已启用，文件实体落盘于 %s\n", o.filesDir)
		}
	}

	// 留存策略：启动时先清一次，之后每 6 小时滚动截断，
	// 避免审计流水与历史任务把 SQLite 撑爆。
	go func() {
		runCleanup := func() {
			if n, err := store.CleanupOldAuditLogs(o.retention); err == nil && n > 0 {
				fmt.Printf("[清理] 已清除 %d 条超过 %s 的审计日志\n", n, o.retention)
			}
			if n, err := store.CleanupFinishedTasks(o.retention); err == nil && n > 0 {
				fmt.Printf("[清理] 已清除 %d 条已完结的历史任务\n", n)
			}
			if n, err := api.PurgeExpiredFiles(o.retention); err == nil && n > 0 {
				fmt.Printf("[清理] 已清除 %d 个超过保留期的中转文件\n", n)
			}
			if n, err := store.CleanupInvites(o.retention); err == nil && n > 0 {
				fmt.Printf("[清理] 已清除 %d 条超过保留期的邀请码/入网记录\n", n)
			}
		}
		runCleanup()
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			runCleanup()
		}
	}()

	// 僵尸任务回收：节点领走任务后宕机就再也不会回传，
	// 只靠上面的完结清理永远碰不到这些 running 记录，必须单独定时扫。
	// 30 秒一轮是权衡——任务超时量级是分钟级，扫太密只是白烧 CPU。
	go func() {
		reap := func() {
			requeued, dead, err := store.ReapTimedOutTasks()
			if err != nil {
				fmt.Printf("[回收] 扫描超时任务失败: %v\n", err)
				return
			}
			if requeued > 0 {
				fmt.Printf("[回收] %d 条超时任务已重投\n", requeued)
			}
			if dead > 0 {
				fmt.Printf("[回收] %d 条任务重试次数耗尽，判死\n", dead)
			}
		}
		reap()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reap()
		}
	}()

	r := gin.Default()

	// 入网上下文注入：邀请页、安装脚本、配置包都依赖这几个进程级参数。
	// 与既有的 api.TaskTimeout / api.FileStoreDir 同一风格。
	api.ClusterSecret = o.secret
	api.PublicBaseURL = strings.TrimRight(strings.TrimSpace(o.publicURL), "/")
	api.ClientPackDir = o.clientPack
	api.TLSCAPEM = loadTLSCAPEM(o.tlsCert)

	// 请求体体积上限挂在全局：控制台与 /api/v1 都要管。
	// 后者虽需 HMAC 签名，但密钥是全体节点共享的，
	// 任何一个节点（或密钥泄露后的任何人）都能打超大载荷。
	r.Use(api.BodyLimitMiddleware(api.MaxBodyBytes))

	scheme := "http"
	useTLS := o.tlsCert != "" && o.tlsKey != ""
	if useTLS {
		scheme = "https"
	}

	// 中央控制台：挂在 /console 下，用 Basic Auth 保护。
	// 不能复用 /api/v1 的 HMAC 中间件——浏览器拿不到集群密钥，做不了签名。
	//
	// 收口规则：只要监听地址对外可达（非回环），就必须配好口令，否则干脆不注册这组路由。
	// 单机调试时绑 127.0.0.1 可以裸奔；一旦部署到内网，控制台能看设备拓扑、
	// 审计流水，还能下发任务，敞开着就是事故。
	switch {
	case o.consoleUser != "" && o.consolePass != "":
		registerConsole(r, o.consoleUser, o.consolePass)
		fmt.Printf("[控制台] 已启用口令保护，访问 %s://<本机IP>%s/console\n", scheme, o.addr)
	case !exposedAddr(o.addr):
		registerConsole(r, "", "")
		fmt.Printf("[控制台] 仅监听回环地址，未设口令仍可访问 %s/console\n", o.addr)
	default:
		// 不注册真实路由，但保留一个 503 兜底：
		// 直接 404 会让运维以为是路径写错而去查路由表，
		// 503 + 原因能当场说清「功能存在，被安全策略关掉了」。
		console := r.Group("/console")
		console.GET("", consoleDisabled)
		console.GET("/", consoleDisabled)
		console.Any("/api/*path", consoleDisabled)
		fmt.Println("[控制台] 已关闭：监听地址对外可达但未配置 console_user / console_pass。")
		fmt.Println("         请在配置文件 agent-mesh.json 里补上这两项后重启，控制台才会启用。")
		fmt.Println("         关闭期间访问 /console 会返回 503 并附带上述原因。")
	}

	// 健康检查：给本机联调脚本一个快速探针。
	r.GET("/healthz", func(c *gin.Context) {
		if err := store.DB.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db unreachable", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 自助入网：管理员签发邀请码 -> 员工一行命令 -> 自动落配置、装自启、自检回传。
	// 这组路由是**唯一**不要求 HMAC 签名的地方，因此单独挂限流中间件，
	// 否则 16 位邀请码在内网里是可以被暴力枚举的。
	api.RegisterJoinRoutes(r)
	if o.secret == "" {
		fmt.Println("[入网] 警告：未配置 secret，配置包里将下发空密钥，客户端不会被鉴权保护。")
	}
	if o.clientPack == "" {
		fmt.Println("[入网] 提示：未配置 -client-pack，/join/<码>/client 将返回 503（落地页会给出放置指引）。")
	} else if _, err := os.Stat(o.clientPack); err != nil {
		fmt.Printf("[入网] 警告：客户端目录不可用 %s: %v\n", o.clientPack, err)
	}

	// 业务接口统一挂在 /api/v1 下，鉴权中间件只作用于该组：
	// /healthz 必须保持免鉴权，否则监控探活会全部 401。
	apiGroup := r.Group("/api/v1")
	if o.secret != "" {
		apiGroup.Use(api.SecurityAuthMiddleware(o.secret))
		fmt.Println("[安全] 已启用 HMAC 签名鉴权（时间戳窗口 300s + nonce 防重放）")
	} else {
		fmt.Println("[安全] 警告：未配置 secret，/api/v1 处于无鉴权状态，内网任何主机均可伪造上报")
	}
	apiGroup.POST("/cluster/heartbeat", api.HandleHeartbeat)
	apiGroup.GET("/cluster/devices", api.GetDevices)
	apiGroup.POST("/audit/report", api.ReportAuditLog)
	apiGroup.GET("/audit/logs", api.ListAuditLogs)
	// 下行任务通道：控制台下发、节点轮询领取、结果回传、列表查询。
	apiGroup.POST("/tasks/create", api.CreateTask)
	apiGroup.GET("/tasks/pending", api.GetPendingTasks)
	apiGroup.POST("/tasks/result", api.ReportTaskResult)
	apiGroup.GET("/tasks", api.ListTasks)

	// 文件中转的读侧与删除侧：请求体很小，沿用缓冲式的 HMAC 中间件即可。
	apiGroup.GET("/files", api.ListFiles)
	apiGroup.GET("/files/download", api.DownloadFile)
	apiGroup.POST("/files/delete", api.DeleteFile)

	// 上传单独挂在引擎上，不复用 /api/v1 组的中间件链：
	// 一是上传体可达数百 MB，不能被缓冲式 HMAC 中间件整体读进内存；
	// 二是它需要用「声明摘要」而非「实体」参与签名。
	r.POST(api.FileUploadPath, api.FileUploadAuthMiddleware(o.secret), api.UploadFile)

	fmt.Printf("[Server] 中央控制中枢监听中: %s://%s\n", scheme, o.addr)
	if !useTLS && exposedAddr(o.addr) {
		fmt.Println("[安全] 警告：当前为 HTTP 明文传输，审计内容与控制台口令在网络上裸奔。")
		fmt.Println("         跨网段部署请配置 tls_cert / tls_key 启用 HTTPS。")
	}

	if useTLS {
		if err := r.RunTLS(o.addr, o.tlsCert, o.tlsKey); err != nil {
			log.Fatalf("服务退出: %v", err)
		}
		return
	}
	if err := r.Run(o.addr); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}

// exposedAddr 判断监听地址是否对外可达。
// 只绑回环（127.0.0.1 / localhost / [::1]）视为仅本机，其余都算对外暴露。
func exposedAddr(addr string) bool {
	host := addr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		host = addr[:idx]
		// IPv6 的 [::1]:8080 形式，去掉方括号
		host = strings.Trim(host, "[]")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return false
	}
	// 显式绑到了某块网卡的地址，一律按对外处理——宁可保守。
	return true
}

// registerConsole 注册控制台路由；传入非空口令时加 Basic Auth 保护。
func registerConsole(r *gin.Engine, user, pass string) {
	console := r.Group("/console")
	if user != "" && pass != "" {
		console.Use(gin.BasicAuth(gin.Accounts{user: pass}))
	}
	console.GET("", api.HandleConsole)
	console.GET("/", api.HandleConsole)
	console.GET("/api/overview", api.ConsoleOverview)
	console.GET("/api/devices", api.ConsoleDevices)
	console.GET("/api/tasks", api.ConsoleTasks)
	console.GET("/api/audit", api.ConsoleAudit)
	console.POST("/api/tasks/create", api.ConsoleCreateTask)
	// 中转文件：这是「任务产出的文件取不回来」的出口——
	// 产物上传后，在控制台点一下即可下载，不必登录到那台执行机。
	console.GET("/api/files", api.ConsoleFiles)
	console.GET("/api/files/download", api.ConsoleDownloadFile)
	// 自助入网：签发邀请、查看用量、作废、看自检结果。
	api.RegisterConsoleInviteRoutes(console)
}

// loadTLSCAPEM 读取与 leaf 证书同目录的 ca.pem，供配置包下发。
//
// 客户端拿到它之后就能严格校验服务端证书，不必再手工拷贝 CA。
// 读不到（HTTP 部署、或证书不是本工具生成的）时返回空串，配置包里就没有这一项。
func loadTLSCAPEM(certPath string) string {
	if certPath == "" {
		return ""
	}
	caPath := filepath.Join(filepath.Dir(certPath), "ca.pem")
	raw, err := os.ReadFile(caPath)
	if err != nil {
		return ""
	}
	return string(raw)
}

// consoleDisabled 是控制台被安全策略关闭时的兜底响应。
// 返回 503 而非 404：后者容易让运维误判成路由写错。
func consoleDisabled(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error":  "console disabled",
		"reason": "监听地址对外可达但未配置 console_user / console_pass",
		"action": "在 agent-mesh.json 中补上 console_user 与 console_pass 后重启服务",
	})
}

// resolvePath 把相对路径解析到可执行文件同目录。
// 服务进程的工作目录是 System32，相对路径会写到完全错误的地方。
func resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(exeDir(), p)
}

// exeDir 返回可执行文件所在目录；定位失败时退回当前工作目录。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	return filepath.Dir(exe)
}

func logFilePath(h *filelog.Handle) string {
	if h == nil {
		return ""
	}
	return h.Path()
}
