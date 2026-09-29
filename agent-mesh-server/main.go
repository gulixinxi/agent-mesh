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
	taskTimeout := flag.Duration("task-timeout", 5*time.Minute,
		"任务从被节点领走到必须回传结果的上限，超时即回收重投（须大于客户端 3 分钟的执行超时）")
	flag.Parse()

	// 小于客户端执行超时会让正在跑的任务被误判超时并重投，导致同一条指令被执行两遍。
	if *taskTimeout <= 3*time.Minute {
		fmt.Printf("[警告] -task-timeout=%s 不大于客户端执行超时 3 分钟，可能导致重复执行，已回落为 5 分钟\n", *taskTimeout)
		*taskTimeout = 5 * time.Minute
	}
	api.TaskTimeout = *taskTimeout

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

	// 中央控制台：挂在 /console 下，用 Basic Auth 保护。
	// 不能复用 /api/v1 的 HMAC 中间件——浏览器拿不到集群密钥，做不了签名。
	consoleUser := os.Getenv("CONSOLE_USER")
	consolePass := os.Getenv("CONSOLE_PASS")
	console := r.Group("/console")
	if consoleUser != "" && consolePass != "" {
		console.Use(gin.BasicAuth(gin.Accounts{consoleUser: consolePass}))
		fmt.Printf("[控制台] 已启用口令保护，访问 http://<本机IP>%s/console\n", *addr)
	} else {
		fmt.Println("[控制台] 警告：未设置 CONSOLE_USER / CONSOLE_PASS，控制台处于无口令状态")
	}
	console.GET("", api.HandleConsole)
	console.GET("/", api.HandleConsole)
	console.GET("/api/overview", api.ConsoleOverview)
	console.GET("/api/devices", api.ConsoleDevices)
	console.GET("/api/tasks", api.ConsoleTasks)
	console.GET("/api/audit", api.ConsoleAudit)
	console.POST("/api/tasks/create", api.ConsoleCreateTask)

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
