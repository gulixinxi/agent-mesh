package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"agent-mesh-server/api"
	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dbPath := flag.String("db", "agent_mesh_center.db", "SQLite 数据文件路径")
	debug := flag.Bool("debug", false, "开启 gin 调试模式（含请求日志）")
	secret := flag.String("secret", os.Getenv("AGENT_MESH_SECRET"),
		"集群共享密钥；非空时对 /api/v1 启用 HMAC 签名鉴权（客户端需配置同一密钥）")
	retention := flag.Duration("retention", 30*24*time.Hour,
		"审计日志与已完结任务的保留时长，超期自动清理（如 720h、30d）")
	flag.Parse()

	fmt.Println("==================================================")
	fmt.Println("   Agent Mesh 本地 AI 协作中枢 - 中央汇总服务端")
	fmt.Println("==================================================")

	if err := store.InitDB(*dbPath); err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer store.DB.Close()

	if *debug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// 留存策略：启动时先清一次，之后每 6 小时滚动截断，
	// 避免审计流水与历史任务把 SQLite 撑爆。
	go func() {
		runCleanup := func() {
			if n, err := store.CleanupOldAuditLogs(*retention); err == nil && n > 0 {
				fmt.Printf("[清理] 已清除 %d 条超过 %s 的审计日志\n", n, *retention)
			}
			if n, err := store.CleanupFinishedTasks(*retention); err == nil && n > 0 {
				fmt.Printf("[清理] 已清除 %d 条已完结的历史任务\n", n)
			}
		}
		runCleanup()
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			runCleanup()
		}
	}()

	r := gin.Default()

	// 健康检查：给本机联调脚本一个快速探针。
	r.GET("/healthz", func(c *gin.Context) {
		if err := store.DB.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db unreachable", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 业务接口统一挂在 /api/v1 下，鉴权中间件只作用于该组：
	// /healthz 必须保持免鉴权，否则监控探活会全部 401。
	apiGroup := r.Group("/api/v1")
	if *secret != "" {
		apiGroup.Use(api.SecurityAuthMiddleware(*secret))
		fmt.Println("[安全] 已启用 HMAC 签名鉴权（时间戳窗口 300s + nonce 防重放）")
	} else {
		fmt.Println("[安全] 警告：未配置 -secret，/api/v1 处于无鉴权状态，内网任何主机均可伪造上报")
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

	fmt.Printf("[Server] 中央控制中枢监听中: %s\n", *addr)
	if err := r.Run(*addr); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
