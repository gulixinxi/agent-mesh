package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"agent-mesh-client/adapters"
	"agent-mesh-client/config"
	"agent-mesh-client/core"
	"agent-mesh-client/internal/filelog"
)

// installTakeover 决定 install / enroll 在「同名服务指向另一路径」时是否接管。
// 默认接管：服务名本机唯一，放着不管就等于把旧版本留在开机自启动序列里。
var installTakeover = true

// applyInstallFlags 按子命令参数更新安装行为开关（install / enroll 共用）。
func applyInstallFlags(args []string) {
	installTakeover = !hasFlag(args, "--no-takeover")
}

// hasFlag 在子命令的剩余参数里查找开关。
// 单独抽出来是为了能写单测：服务注册的真实效果断言不了，
// 但"用户打了开关到底生效没有"必须有人验证。
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

	var (
		serverURL   = flag.String("server", cfg.ServerURL, "中央服务端地址")
		clientID    = flag.String("id", cfg.ClientID, "本节点 ID（空则取主机名）")
		p2pPort     = flag.Int("p2p-port", cfg.P2PPort, "libp2p 监听端口，0 表示不启用 P2P")
		downloadDir = flag.String("dl-dir", cfg.DownloadDir, "P2P 文件落地目录")
		doubaoDB    = flag.String("doubao-db", cfg.DoubaoDB, "豆包本地会话库路径")
		mcpMode     = flag.Bool("mcp", cfg.MCPOnly, "仅以 MCP stdio 模式运行（供 Cursor 等宿主接入）")
		secret      = flag.String("secret", cfg.Secret, "集群共享密钥，非空时对上报请求做 HMAC 签名")
		p2pAllow    = flag.String("p2p-allow", cfg.P2PAllow, "逗号分隔的 PeerID 白名单，非空时只接受名单内节点传文件")
		tlsCA       = flag.String("tls-ca", cfg.TLSCA, "信任的自签 CA 证书路径（服务端启用 HTTPS 时必填）")
		tlsInsecure = flag.Bool("tls-insecure", cfg.TLSInsecure, "跳过服务端证书校验，仅供联调，生产禁用")
		logDir      = flag.String("log-dir", cfg.LogDir, "日志落盘目录；服务模式下必填，否则日志无人接收")
	)
	flag.Parse()

	// 纯 MCP 模式：不启动常驻引擎，只把 stdin/stdout 交给 MCP 协议栈。
	// 注意：该模式下 stdout 被 JSON-RPC 独占，所有日志必须走 stderr，
	// 因此这里注册适配器要用静默版本，也不能打印任何启动横幅。
	// 同理，日志接管绝不能在此处启用——一旦把 stdout 接走，协议就断了。
	if *mcpMode {
		ctx, cancel := signalCtx()
		defer cancel()

		mcpID := *clientID
		if mcpID == "" {
			if host, err := os.Hostname(); err == nil {
				mcpID = host
			} else {
				mcpID = "mcp-local"
			}
		}

		entry := core.NewMCPServerEntry()
		engine := core.NewMeshEngine(*serverURL, mcpID, *secret)
		if c, err := core.NewHTTPClient(*tlsCA, *tlsInsecure); err == nil {
			engine.SetHTTPClient(c)
		}
		engine.SetDownloadDir(*downloadDir)
		engine.RegisterAdapterSilent(adapters.NewOllamaAdapter())
		engine.RegisterAdapterSilent(adapters.NewDoubaoAdapter(*doubaoDB))
		entry.Handler().SetEngine(engine)

		if *p2pPort > 0 {
			if mgr, err := core.NewP2PTransferManager(*p2pPort, *downloadDir); err == nil {
				defer mgr.Close()
				entry.Handler().SetP2P(mgr)
			}
		}

		entry.StartStdioLoop(ctx)
		return
	}

	// 安装 / 卸载子命令：必须在接管日志之前处理，
	// 否则执行结果全写进日志文件，操作者在控制台里什么都看不到。
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
		case "enroll":
			// 自助接入：这条路径必须自己解析参数（不能用全局 flag.Parse 的结果），
			// 因为它只是子命令，顶层 flag 已经在前面被 Parse 过了。
			os.Exit(runEnroll(args[1:]))
		case "status":
			ok, state := serviceStatus()
			fmt.Printf("服务状态：%s（就绪=%v）\n", state, ok)
			if !ok {
				os.Exit(1)
			}
			return
		default:
			fmt.Printf("[提示] 未知子命令 %q，可用：enroll / install / uninstall / status\n", args[0])
			os.Exit(2)
		}
	}

	inService := isServiceSession()

	// 服务模式：接管 stdout/stderr 到日志文件。
	// Windows 服务没有控制台，之前所有打印都会丢进黑洞。
	var logHandle *filelog.Handle
	if inService {
		if *logDir == "" {
			*logDir = filepath.Join(os.Getenv("ProgramData"), "AgentMesh", "logs")
		}
		h, err := filelog.Redirect(filelog.Options{Dir: *logDir, Name: "agent-mesh-client"})
		if err != nil {
			os.Exit(1)
		}
		logHandle = h
		defer h.Close()
		log.SetOutput(os.Stderr)
	}

	// 服务进程的当前工作目录是 System32，相对路径会写到完全错误的地方。
	*downloadDir = resolvePath(*downloadDir)
	*doubaoDB = resolvePath(*doubaoDB)

	run := func(ctx context.Context) {
		runNode(ctx, nodeOpts{
			serverURL:   *serverURL,
			clientID:    *clientID,
			p2pPort:     *p2pPort,
			downloadDir: *downloadDir,
			doubaoDB:    *doubaoDB,
			secret:      *secret,
			p2pAllow:    *p2pAllow,
			tlsCA:       *tlsCA,
			tlsInsecure: *tlsInsecure,
			logPath:     logFilePath(logHandle),
		})
	}

	if inService {
		if err := runService(run); err != nil {
			fmt.Printf("[服务] 退出: %v\n", err)
		}
		return
	}

	ctx, cancel := signalCtx()
	defer cancel()
	run(ctx)
}

// =====================================================================
// enroll 子命令：用一枚邀请码完成「换配置 -> 落盘 -> 装自启 -> 自检 -> 回传」
// =====================================================================

// runEnroll 执行自助接入，返回进程退出码。
func runEnroll(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "中央服务端地址，如 https://10.0.0.5:8443（必填）")
	code := fs.String("code", "", "邀请码，形如 ABCD-EFGH-IJKL-MNOP")
	codeStdin := fs.Bool("code-stdin", false,
		"从标准输入读取邀请码；比写在命令行上安全（Linux 下命令行对同机其他用户可见）")
	nodeID := fs.String("id", "", "本节点 ID；留空取主机名")
	installDir := fs.String("install-dir", defaultInstallDir(), "安装目录（程序与配置的落地位置）")
	dataDir := fs.String("data-dir", defaultDataDir(), "数据目录（日志与下载）")
	p2pPort := fs.Int("p2p-port", 6001, "libp2p 监听端口，0 表示关闭 P2P")
	noService := fs.Bool("no-service", false, "只写配置，不注册系统服务（排障用）")
	fs.Bool("no-takeover", false,
		"机器上已存在同名服务但装在其他目录时，报错退出而不是由本次安装接管（取值由 applyInstallFlags 读取）")
	timeout := fs.Duration("timeout", 3*time.Minute, "整体超时")
	_ = fs.Parse(args)
	applyInstallFlags(args)

	out := os.Stdout
	fmt.Println("==================================================")
	fmt.Println("   Agent Mesh 节点自助接入")
	fmt.Println("==================================================")

	// 邀请码优先从 stdin 读：命令行参数会出现在进程列表里，
	// 同机其他用户（Linux 上 /proc/*/cmdline 默认可读）能直接看到。
	inviteCode := strings.TrimSpace(*code)
	if *codeStdin {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<10))
		if err != nil {
			fmt.Printf("读取标准输入失败: %v\n", err)
			return 2
		}
		inviteCode = strings.TrimSpace(string(raw))
	}
	if inviteCode == "" {
		fmt.Println("错误：缺少邀请码。请用 --code <码> 或 --code-stdin 传入（后者从标准输入读取）。")
		return 2
	}
	if strings.TrimSpace(*server) == "" {
		fmt.Println("错误：缺少 --server，请填中央服务端的地址。")
		return 2
	}

	clientID := strings.TrimSpace(*nodeID)
	if clientID == "" {
		host, err := os.Hostname()
		if err != nil {
			fmt.Printf("获取主机名失败: %v\n", err)
			return 2
		}
		clientID = host
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res := core.Enroll(ctx, core.EnrollConfig{
		ServerURL:   strings.TrimSpace(*server),
		Code:        inviteCode,
		ClientID:    clientID,
		InstallDir:  *installDir,
		DataDir:     *dataDir,
		P2PPort:     *p2pPort,
		SkipService: *noService,
		Service: core.ServiceHooks{
			Install:        installServiceAt,
			Restart:        restartService,
			Stop:           stopService,
			Status:         serviceStatus,
			PrivilegeCheck: requirePrivilege,
			// 「已注册」在平台层的措辞各不相同，判断留在平台文件旁边，
			// 免得这里散落一堆字符串匹配。
			AlreadyExists: isServiceExistsErr,
		},
	}, out)

	fmt.Println()
	if res.OK {
		fmt.Println("接入完成：本机已注册为常驻节点，重启后会自动拉起。")
		fmt.Printf("日志目录：%s\n", filepath.Join(*dataDir, "logs"))
		fmt.Println("若控制台未显示本节点，请检查中枢地址与网络连通性。")
		return 0
	}

	fmt.Println("接入未完成。请把上面标 ✗ 的步骤连同原因发给管理员。")
	fmt.Println("排障提示：")
	fmt.Println("  · 提示需要管理员/root —— 换用提权终端重跑同一条命令（不会重复消耗邀请码）")
	fmt.Println("  · 提示邀请码过期/已用尽 —— 让管理员重新签发")
	fmt.Println("  · 心跳失败 —— 确认中枢地址可达、双方时间偏差在 5 分钟内")
	return 1
}

// defaultInstallDir 返回各平台的默认安装目录。
func defaultInstallDir() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\AgentMesh\Client`
	}
	return "/opt/agent-mesh/client"
}

// defaultDataDir 返回各平台的默认数据目录（日志与下载）。
func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "AgentMesh")
		}
		return `C:\ProgramData\AgentMesh`
	}
	return "/var/lib/agent-mesh"
}

type nodeOpts struct {
	serverURL   string
	clientID    string
	p2pPort     int
	downloadDir string
	doubaoDB    string
	secret      string
	p2pAllow    string
	tlsCA       string
	tlsInsecure bool
	logPath     string
}

func runNode(ctx context.Context, o nodeOpts) {
	if o.clientID == "" {
		host, err := os.Hostname()
		if err != nil {
			log.Fatalf("获取主机名失败: %v", err)
		}
		o.clientID = host
	}

	fmt.Println("==================================================")
	fmt.Println("   Agent Mesh 本地 AI 协作中枢 - 客户端常驻端")
	fmt.Println("==================================================")

	if o.logPath != "" {
		fmt.Printf("[日志] 已接管输出，落盘于 %s\n", o.logPath)
	}

	if o.p2pPort > 0 {
		mgr, err := core.NewP2PTransferManager(o.p2pPort, o.downloadDir)
		if err != nil {
			log.Fatalf("P2P 初始化失败: %v", err)
		}
		defer mgr.Close()
		fmt.Printf("[P2P] 本地 PeerID: %s\n", mgr.Host.ID().String())
		for _, addr := range mgr.ListenAddresses() {
			fmt.Printf("[P2P] 监听地址: %s\n", addr)
		}

		// 白名单：配置了则只接受名单内节点的文件流，否则处于宽松模式（仅靠内网边界）。
		if o.p2pAllow != "" {
			for _, id := range strings.Split(o.p2pAllow, ",") {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				if err := mgr.AllowPeer(id); err != nil {
					log.Fatalf("P2P 白名单含非法 PeerID %q: %v", id, err)
				}
			}
			fmt.Printf("[P2P] 已启用节点白名单，仅接受 %d 个授权节点的文件传输\n", mgr.AllowedPeerCount())
		} else {
			fmt.Println("[P2P] 警告：未配置白名单，当前接受局域网内任何节点的文件传输")
		}
	}

	engine := core.NewMeshEngine(o.serverURL, o.clientID, o.secret)

	// 服务端若是 HTTPS + 自签证书，这里必须换成信任该 CA 的客户端，否则握手必失败。
	if strings.HasPrefix(strings.ToLower(o.serverURL), "https://") {
		c, err := core.NewHTTPClient(o.tlsCA, o.tlsInsecure)
		if err != nil {
			log.Fatalf("构造 HTTPS 客户端失败: %v", err)
		}
		engine.SetHTTPClient(c)
		if o.tlsCA != "" {
			fmt.Printf("[安全] 已信任自签 CA: %s\n", o.tlsCA)
		} else if !o.tlsInsecure {
			fmt.Println("[安全] 警告：服务端为 HTTPS 但未配置 tls_ca，将仅信任系统根证书")
		}
	}

	engine.RegisterAdapter(adapters.NewOllamaAdapter())

	// 中转文件的落盘目录与 P2P 下载目录保持一致，运维只需关心一处。
	engine.SetDownloadDir(o.downloadDir)

	// 会话库路径为空说明这台机器上没有要监听的豆包客户端，
	// 此时注册适配器只会得到一个永远扫不到东西的空壳，不如直接跳过。
	if o.doubaoDB == "" {
		fmt.Println("[Doubao] 未配置会话库路径，跳过豆包适配器")
	} else {
		engine.RegisterAdapter(adapters.NewDoubaoAdapter(o.doubaoDB))
		fmt.Printf("[Doubao] 监听会话库: %s\n", o.doubaoDB)
	}
	engine.Start(ctx)

	fmt.Printf("[Engine] 节点就绪，上报目标 %s，节点 ID %s\n", o.serverURL, o.clientID)

	<-ctx.Done()
	fmt.Println("\n[Client] 收到退出信号，正在收尾…")
	engine.Stop()
}

// signalCtx 返回一个在收到 SIGINT / SIGTERM 时自动取消的 context。
func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	return ctx, cancel
}

// resolvePath 把相对路径解析到可执行文件同目录。
// 服务进程的工作目录是 System32，相对路径会写到完全错误的地方。
func resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return p
	}
	return filepath.Join(filepath.Dir(exe), p)
}

func logFilePath(h *filelog.Handle) string {
	if h == nil {
		return ""
	}
	return h.Path()
}
