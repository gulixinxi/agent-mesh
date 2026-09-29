package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agent-mesh-server/store"
	"agent-mesh-server/web"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 中央控制台：面向「老总看板」的单页界面与配套只读/写接口。
//
// 鉴权说明：控制台接口**不能**走 /api/v1 的 HMAC 签名中间件——
// 浏览器无法持有集群密钥去做 HMAC 签名。因此这里单独挂在 /console 下，
// 由 main.go 用 Basic Auth 保护（配置 -console-user / -console-pass）。
// 未配置口令时启动会打印警告，仅适用于完全可信的内网。
// =====================================================================

// HandleConsole 返回内嵌的控制台页面。
func HandleConsole(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(web.ConsoleHTML))
}

// ConsoleOverview 返回顶部概览卡片所需的统计数字。
func ConsoleOverview(c *gin.Context) {
	var (
		totalDevices, onlineDevices int
		pendingTasks, todayAudit    int
		runnableAgents              int
	)

	_ = store.DB.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&totalDevices)
	_ = store.DB.QueryRow(`SELECT COUNT(*) FROM devices WHERE status='online'`).Scan(&onlineDevices)
	_ = store.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status='pending'`).Scan(&pendingTasks)
	_ = store.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE timestamp >= ?`, startOfToday()).Scan(&todayAudit)

	// 统计「可用适配器」需要解析每个节点上报的 agents JSON。
	rows, err := store.DB.Query(`SELECT agents FROM devices WHERE status='online'`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var raw sql.NullString
			if err := rows.Scan(&raw); err != nil || !raw.Valid || raw.String == "" {
				continue
			}
			var agents []struct {
				Runnable bool `json:"runnable"`
			}
			if err := json.Unmarshal([]byte(raw.String), &agents); err != nil {
				continue
			}
			for _, a := range agents {
				if a.Runnable {
					runnableAgents++
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total_devices":   totalDevices,
		"online_devices":  onlineDevices,
		"runnable_agents": runnableAgents,
		"pending_tasks":   pendingTasks,
		"today_audit":     todayAudit,
	})
}

// ConsoleDevices 返回节点拓扑，附带解析后的适配器名称便于前端展示。
func ConsoleDevices(c *gin.Context) {
	rows, err := store.DB.Query(
		`SELECT client_id, client_name, os, ip_address, status, last_heartbeat, agents
		 FROM devices ORDER BY last_heartbeat DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type deviceOut struct {
		ClientID      string   `json:"client_id"`
		ClientName    string   `json:"client_name"`
		OS            string   `json:"os"`
		IPAddress     string   `json:"ip_address"`
		Status        string   `json:"status"`
		LastHeartbeat int64    `json:"last_heartbeat"`
		AgentNames    []string `json:"agent_names"`
	}

	list := make([]deviceOut, 0)
	for rows.Next() {
		var (
			d      deviceOut
			raw    sql.NullString
			name   sql.NullString
			osName sql.NullString
			ip     sql.NullString
			status sql.NullString
			lastHB sql.NullInt64
		)
		if err := rows.Scan(&d.ClientID, &name, &osName, &ip, &status, &lastHB, &raw); err != nil {
			continue
		}
		d.ClientName, d.OS, d.IPAddress, d.Status = name.String, osName.String, ip.String, status.String
		d.LastHeartbeat = lastHB.Int64
		d.AgentNames = []string{}

		if raw.Valid && raw.String != "" {
			var agents []struct {
				Name     string `json:"name"`
				Runnable bool   `json:"runnable"`
			}
			if err := json.Unmarshal([]byte(raw.String), &agents); err == nil {
				for _, a := range agents {
					// 可用标记前置，界面上一眼能看出哪些工具真的能用
					if a.Runnable {
						d.AgentNames = append(d.AgentNames, "● "+a.Name)
					} else {
						d.AgentNames = append(d.AgentNames, "○ "+a.Name)
					}
				}
			}
		}
		list = append(list, d)
	}
	c.JSON(http.StatusOK, list)
}

// ConsoleTasks 返回任务流水。
func ConsoleTasks(c *gin.Context) {
	limit := queryLimit(c, 30, 200)
	rows, err := store.DB.Query(
		`SELECT task_id, target_node, target_agent_kind, prompt, status, result, error_msg, created_at, updated_at
		 FROM tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type taskOut struct {
		TaskID          string `json:"task_id"`
		TargetNode      string `json:"target_node"`
		TargetAgentKind string `json:"target_agent_kind"`
		Prompt          string `json:"prompt"`
		Status          string `json:"status"`
		Result          string `json:"result"`
		Error           string `json:"error"`
		CreatedAt       int64  `json:"created_at"`
		UpdatedAt       int64  `json:"updated_at"`
	}

	list := make([]taskOut, 0, limit)
	for rows.Next() {
		var t taskOut
		var result, errMsg sql.NullString
		if err := rows.Scan(&t.TaskID, &t.TargetNode, &t.TargetAgentKind, &t.Prompt,
			&t.Status, &result, &errMsg, &t.CreatedAt, &t.UpdatedAt); err != nil {
			continue
		}
		t.Result, t.Error = result.String, errMsg.String
		list = append(list, t)
	}
	c.JSON(http.StatusOK, list)
}

// ConsoleAudit 返回审计流水。
func ConsoleAudit(c *gin.Context) {
	limit := queryLimit(c, 30, 200)
	rows, err := store.DB.Query(
		`SELECT task_id, target_node, agent_kind, prompt, result, timestamp
		 FROM audit_logs ORDER BY timestamp DESC LIMIT ?`, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type auditOut struct {
		TaskID     string `json:"task_id"`
		TargetNode string `json:"target_node"`
		AgentKind  string `json:"agent_kind"`
		Prompt     string `json:"prompt"`
		Result     string `json:"result"`
		Timestamp  int64  `json:"timestamp"`
	}

	list := make([]auditOut, 0, limit)
	for rows.Next() {
		var a auditOut
		var result sql.NullString
		if err := rows.Scan(&a.TaskID, &a.TargetNode, &a.AgentKind, &a.Prompt, &result, &a.Timestamp); err != nil {
			continue
		}
		a.Result = result.String
		list = append(list, a)
	}
	c.JSON(http.StatusOK, list)
}

// ConsoleCreateTask 供控制台页面下发任务。
// 与 /api/v1/tasks/create 逻辑一致，但不要求 HMAC 签名（浏览器无法签名）。
func ConsoleCreateTask(c *gin.Context) {
	var req CreateTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad JSON"})
		return
	}
	if !validatePrompt(c, req.Prompt) {
		return
	}

	taskID := newTaskID()
	now := time.Now().Unix()
	if _, err := store.DB.Exec(
		`INSERT INTO tasks (task_id, target_node, target_agent_kind, prompt, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?)`,
		taskID, req.TargetNode, req.TargetAgentKind, req.Prompt, now, now,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	fmt.Printf("[控制台下发] %s -> 节点:%s | 工具:%s | %s\n",
		taskID, displayNode(req.TargetNode), req.TargetAgentKind, truncate(req.Prompt, 60))
	c.JSON(http.StatusOK, gin.H{"status": "task created", "task_id": taskID})
}

// startOfToday 返回今天零点的时间戳，用于「今日审计」统计。
func startOfToday() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}

// queryLimit 解析并约束分页参数。
func queryLimit(c *gin.Context, def, max int) int {
	limit := def
	if v := c.Query("limit"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > max {
		limit = max
	}
	return limit
}
