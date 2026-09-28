package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 下行任务通道：控制台下发 -> 节点轮询领取 -> 执行 -> 结果回传
//
// 选型说明：此处采用 HTTP 轮询而非 P2P 反向直连。
// 反向直连要求服务端也作为 libp2p 节点，并且服务端要主动连回客户端，
// 在 NAT / 防火墙 / 笔记本休眠场景下失败率不可控；轮询虽然有几秒延迟，
// 但依赖现状即可落地，且天然穿透这些网络障碍。
// =====================================================================

// CreateTaskReq 是控制台下发任务的入参。
type CreateTaskReq struct {
	TargetNode       string `json:"target_node"`        // 空表示任意在线节点均可领取
	TargetAgentKind  string `json:"target_agent_kind"`  // 如 ollama / doubao_local
	Prompt           string `json:"prompt"`             // 要执行的指令
}

// TaskResultReq 是节点执行完任务后的回传载荷。
type TaskResultReq struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // completed / failed
	Result string `json:"result"`
	Error  string `json:"error"`
}

// taskRow 是 tasks 表的一行。
type taskRow struct {
	TaskID           string         `json:"task_id"`
	TargetNode       string         `json:"target_node"`
	TargetAgentKind  string         `json:"target_agent_kind"`
	Prompt           string         `json:"prompt"`
	Status           string         `json:"status"`
	Result           sql.NullString `json:"-"`
	ErrorMsg         sql.NullString `json:"-"`
	CreatedAt        int64          `json:"created_at"`
	ClaimedAt        sql.NullInt64  `json:"-"`
	UpdatedAt        int64          `json:"updated_at"`
}

// CreateTask 供控制台创建一条待执行任务。
func CreateTask(c *gin.Context) {
	var req CreateTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad JSON"})
		return
	}
	if req.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt 不能为空"})
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

	fmt.Printf("[任务下发] %s -> 节点:%s | 工具:%s | %s\n",
		taskID, displayNode(req.TargetNode), req.TargetAgentKind, truncate(req.Prompt, 60))
	c.JSON(http.StatusOK, gin.H{"status": "task created", "task_id": taskID})
}

// GetPendingTasks 返回派给该节点的待执行任务，并原子地把它置为 running。
// 原子领取是关键：多个节点同时轮询时，靠 UPDATE 的 WHERE status='pending'
// 保证同一条任务只会被一个节点领走。
func GetPendingTasks(c *gin.Context) {
	node := c.Query("node")
	limit := 5
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}

	rows, err := store.DB.Query(
		`SELECT task_id, target_node, target_agent_kind, prompt, status, result, error_msg, created_at, claimed_at, updated_at
		 FROM tasks
		 WHERE status = 'pending' AND (target_node = '' OR target_node = ?)
		 ORDER BY created_at ASC LIMIT ?`, node, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	candidates := make([]taskRow, 0, limit)
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.TaskID, &r.TargetNode, &r.TargetAgentKind, &r.Prompt, &r.Status,
			&r.Result, &r.ErrorMsg, &r.CreatedAt, &r.ClaimedAt, &r.UpdatedAt); err != nil {
			continue
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	claimed := make([]taskRow, 0, len(candidates))
	now := time.Now().Unix()
	for _, r := range candidates {
		res, err := store.DB.Exec(
			`UPDATE tasks SET status='running', claimed_at=?, updated_at=? WHERE task_id=? AND status='pending'`,
			now, now, r.TaskID)
		if err != nil {
			continue
		}
		affected, err := res.RowsAffected()
		if err != nil || affected == 0 {
			continue // 已被别的节点抢走
		}
		r.Status = "running"
		claimed = append(claimed, r)
	}

	c.JSON(http.StatusOK, gin.H{"tasks": claimed, "count": len(claimed)})
}

// ReportTaskResult 接收节点回传的执行结果。
func ReportTaskResult(c *gin.Context) {
	var req TaskResultReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad JSON"})
		return
	}
	if req.TaskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id 不能为空"})
		return
	}
	if req.Status != "completed" && req.Status != "failed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status 只能是 completed 或 failed"})
		return
	}

	res, err := store.DB.Exec(
		`UPDATE tasks SET status=?, result=?, error_msg=?, updated_at=? WHERE task_id=?`,
		req.Status, req.Result, req.Error, time.Now().Unix(), req.TaskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在: " + req.TaskID})
		return
	}

	fmt.Printf("[任务回传] %s -> %s | 结果:%s\n",
		req.TaskID, req.Status, truncate(req.Result, 60))
	c.JSON(http.StatusOK, gin.H{"status": "result accepted"})
}

// ListTasks 返回任务列表，供控制台查看闭环状态。
func ListTasks(c *gin.Context) {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	status := c.Query("status")

	query := `SELECT task_id, target_node, target_agent_kind, prompt, status, result, error_msg, created_at, claimed_at, updated_at FROM tasks`
	args := make([]interface{}, 0, 2)
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := store.DB.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type outTask struct {
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

	list := make([]outTask, 0, limit)
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.TaskID, &r.TargetNode, &r.TargetAgentKind, &r.Prompt, &r.Status,
			&r.Result, &r.ErrorMsg, &r.CreatedAt, &r.ClaimedAt, &r.UpdatedAt); err != nil {
			continue
		}
		list = append(list, outTask{
			TaskID:          r.TaskID,
			TargetNode:      r.TargetNode,
			TargetAgentKind: r.TargetAgentKind,
			Prompt:          r.Prompt,
			Status:          r.Status,
			Result:          r.Result.String,
			Error:           r.ErrorMsg.String,
			CreatedAt:       r.CreatedAt,
			UpdatedAt:       r.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, list)
}

// newTaskID 生成任务 ID。
func newTaskID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(buf)
}

// displayNode 让空的目标节点在日志里可读。
func displayNode(node string) string {
	if node == "" {
		return "任意节点"
	}
	return node
}
