package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"agent-mesh-client/adapters"
	"agent-mesh-client/config"
	"agent-mesh-client/core"
	"agent-mesh-client/internal/filelog"
)

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
		default:
			fmt.Printf("[提示] 未知子命令 %q，可用：install / uninstall\n", args[0])
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
