package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"agent-mesh-client/adapters"
	"agent-mesh-client/config"
	"agent-mesh-client/core"
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
	)
	flag.Parse()

	// 纯 MCP 模式：不启动常驻引擎，只把 stdin/stdout 交给 MCP 协议栈。
	if *mcpMode {
		ctx, cancel := signalCtx()
		defer cancel()
		core.NewMCPServerEntry().StartStdioLoop(ctx)
		return
	}

	if *clientID == "" {
		host, err := os.Hostname()
		if err != nil {
			log.Fatalf("获取主机名失败: %v", err)
		}
		*clientID = host
	}

	ctx, cancel := signalCtx()
	defer cancel()

	fmt.Println("==================================================")
	fmt.Println("   Agent Mesh 本地 AI 协作中枢 - 客户端常驻端")
	fmt.Println("==================================================")

	if *p2pPort > 0 {
		mgr, err := core.NewP2PTransferManager(*p2pPort, *downloadDir)
		if err != nil {
			log.Fatalf("P2P 初始化失败: %v", err)
		}
		defer mgr.Close()
		fmt.Printf("[P2P] 本地 PeerID: %s\n", mgr.Host.ID().String())
		for _, addr := range mgr.ListenAddresses() {
			fmt.Printf("[P2P] 监听地址: %s\n", addr)
		}
	}

	engine := core.NewMeshEngine(*serverURL, *clientID)
	engine.RegisterAdapter(adapters.NewOllamaAdapter())
	engine.RegisterAdapter(adapters.NewDoubaoAdapter(*doubaoDB))
	engine.Start(ctx)

	if abs, err := filepath.Abs(*doubaoDB); err == nil {
		fmt.Printf("[Doubao] 监听会话库: %s\n", abs)
	}
	fmt.Printf("[Engine] 节点就绪，上报目标 %s，节点 ID %s\n", *serverURL, *clientID)

	<-ctx.Done()
	fmt.Println("\n[Client] 收到退出信号，正在收尾…")
	cancel()
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
