package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"agent-mesh-server/api"
	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dbPath := flag.String("db", "agent_mesh_center.db", "SQLite 数据文件路径")
	debug := flag.Bool("debug", false, "开启 gin 调试模式（含请求日志）")
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

	r := gin.Default()

	// 健康检查：给本机联调脚本一个快速探针。
	r.GET("/healthz", func(c *gin.Context) {
		if err := store.DB.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db unreachable", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.POST("/api/v1/cluster/heartbeat", api.HandleHeartbeat)
	r.GET("/api/v1/cluster/devices", api.GetDevices)
	r.POST("/api/v1/audit/report", api.ReportAuditLog)
	r.GET("/api/v1/audit/logs", api.ListAuditLogs)

	fmt.Printf("[Server] 中央控制中枢监听中: %s\n", *addr)
	if err := r.Run(*addr); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
