package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
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

// TaskTimeout 是单条任务从「被节点领走」到「必须回传结果」的时长上限，由 main 依 -task-timeout 注入。
//
// 必须大于客户端的 taskExecTimeout（3 分钟）：否则一个正在正常执行的任务会被误判超时、
// 重投给另一个节点，造成同一条指令被执行两遍。
var TaskTimeout = 5 * time.Minute

// DefaultMaxAttempts 是任务默认的最大领取次数，超出即判死，避免无限重投。
const DefaultMaxAttempts = 3

// CreateTaskReq 是控制台下发任务的入参。
type CreateTaskReq struct {
	TargetNode      string `json:"target_node"`       // 空表示任意在线节点均可领取
	TargetAgentKind string `json:"target_agent_kind"` // 如 ollama / doubao_local
	Prompt          string `json:"prompt"`            // 要执行的指令
	MaxAttempts     int    `json:"max_attempts"`      // 可选，最大领取次数；非法值回落默认
}

// TaskResultReq 是节点执行完任务后的回传载荷。
type TaskResultReq struct {
	TaskID     string `json:"task_id"`
	ClaimToken string `json:"claim_token"` // 领取时下发的认领凭据，用于识别迟到的旧结果
	Status     string `json:"status"`      // completed / failed / timeout
	Result     string `json:"result"`
	Error      string `json:"error"`
}

// taskRow 是 tasks 表的一行。
//
// 可空列统一在 SQL 里用 COALESCE 兜成零值，避免用 sql.NullString 这类包装类型：
// 它们序列化出来是 {"String":..,"Valid":..} 对象，客户端拿到手还得再解一层。
type taskRow struct {
	TaskID          string `json:"task_id"`
	TargetNode      string `json:"target_node"`
	TargetAgentKind string `json:"target_agent_kind"`
	Prompt          string `json:"prompt"`
	Status          string `json:"status"`
	Result          string `json:"-"`
	ErrorMsg        string `json:"-"`
	CreatedAt       int64  `json:"created_at"`
	ClaimedAt       int64  `json:"-"`
	UpdatedAt       int64  `json:"updated_at"`
	Attempts        int64  `json:"attempts"`
	MaxAttempts     int64  `json:"max_attempts"`
	TimeoutAt       int64  `json:"timeout_at"`
	ClaimedBy       string `json:"-"`
	ClaimToken      string `json:"claim_token"`
}

// newClaimToken 生成一次领取的认领凭据。重投时会换发新的，
// 于是「上一轮那个卡死的节点迟到的回传」会因凭据不匹配被拒，结果不会错配到新一轮。
func newClaimToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("claim-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// CreateTask 供控制台创建一条待执行任务。
func CreateTask(c *gin.Context) {
	var req CreateTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad JSON"})
		return
	}
	if !validatePrompt(c, req.Prompt) {
		return
	}

	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 || maxAttempts > 10 {
		maxAttempts = DefaultMaxAttempts
	}

	taskID := newTaskID()
	now := time.Now().Unix()
	if _, err := store.DB.Exec(
		`INSERT INTO tasks (task_id, target_node, target_agent_kind, prompt, status, created_at, updated_at, attempts, max_attempts)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?, 0, ?)`,
		taskID, req.TargetNode, req.TargetAgentKind, req.Prompt, now, now, maxAttempts,
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
		`SELECT task_id, target_node, target_agent_kind, prompt, status,
		        COALESCE(result,''), COALESCE(error_msg,''),
		        created_at, COALESCE(claimed_at,0), updated_at,
		        attempts, COALESCE(max_attempts,3), timeout_at,
		        COALESCE(claimed_by,''), COALESCE(claim_token,'')
		 FROM tasks
		 WHERE status = 'pending' AND (target_node = '' OR target_node = ?)
		   AND attempts < COALESCE(max_attempts, 3)
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
			&r.Result, &r.ErrorMsg, &r.CreatedAt, &r.ClaimedAt, &r.UpdatedAt,
			&r.Attempts, &r.MaxAttempts, &r.TimeoutAt, &r.ClaimedBy, &r.ClaimToken); err != nil {
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
	timeoutAt := time.Now().Add(TaskTimeout).Unix()
	for _, r := range candidates {
		token := newClaimToken()
		res, err := store.DB.Exec(
			`UPDATE tasks SET status='running', claimed_at=?, claimed_by=?, claim_token=?,
			        timeout_at=?, attempts=attempts+1, updated_at=?
			 WHERE task_id=? AND status='pending'`,
			now, node, token, timeoutAt, now, r.TaskID)
		if err != nil {
			continue
		}
		affected, err := res.RowsAffected()
		if err != nil || affected == 0 {
			continue // 已被别的节点抢走
		}
		r.Status = "running"
		r.Attempts++
		r.TimeoutAt = timeoutAt
		r.ClaimedBy = node
		r.ClaimToken = token
		claimed = append(claimed, r)
	}

	c.JSON(http.StatusOK, gin.H{"tasks": claimed, "count": len(claimed), "timeout_seconds": int64(TaskTimeout.Seconds())})
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
	// timeout 是第三种合法状态：节点执行超时后主动放弃，请求重新派发，
	// 而不是直接判死——超时通常是模型卡住，换一轮可能就跑通了。
	if req.Status != "completed" && req.Status != "failed" && req.Status != "timeout" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status 只能是 completed / failed / timeout"})
		return
	}
	if req.ClaimToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "claim_token 不能为空"})
		return
	}

	var curStatus, curToken string
	var attempts, maxAttempts int64
	if err := store.DB.QueryRow(
		`SELECT status, COALESCE(claim_token,''), attempts, COALESCE(max_attempts,3)
		 FROM tasks WHERE task_id = ?`, req.TaskID,
	).Scan(&curStatus, &curToken, &attempts, &maxAttempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在: " + req.TaskID})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 认领凭据不匹配 = 这次回传来自上一轮那个已经超时的节点。
	// 任务早已被重投给别人，此刻再写结果会覆盖新一轮的结果，必须拒掉。
	if curStatus != "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "任务已结束，不接受重复回传", "current_status": curStatus})
		return
	}
	if req.ClaimToken != curToken {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "认领凭据已失效（任务已被重新派发）",
			"stale":   true,
			"task_id": req.TaskID,
		})
		return
	}

	now := time.Now().Unix()

	// 节点主动报超时：还有余量就退回 pending 等人重领，没有余量才判死。
	if req.Status == "timeout" {
		if attempts < maxAttempts {
			if _, err := store.DB.Exec(
				`UPDATE tasks SET status='pending', claimed_by='', claim_token='', timeout_at=0,
				        error_msg=?, updated_at=? WHERE task_id=? AND claim_token=?`,
				fmt.Sprintf("第 %d 次执行超时，已退回待派发", attempts), now, req.TaskID, req.ClaimToken,
			); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			fmt.Printf("[任务重投] %s | 第 %d 次执行超时，退回 pending（上限 %d 次）\n",
				req.TaskID, attempts, maxAttempts)
			c.JSON(http.StatusOK, gin.H{"status": "requeued", "attempts": attempts, "max_attempts": maxAttempts})
			return
		}
		if _, err := store.DB.Exec(
			`UPDATE tasks SET status='failed', error_msg=?, claim_token='', timeout_at=0, updated_at=?
			 WHERE task_id=? AND claim_token=?`,
			fmt.Sprintf("执行超时且已达最大重试次数 %d", maxAttempts), now, req.TaskID, req.ClaimToken,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		fmt.Printf("[任务判死] %s | 重试 %d 次仍超时\n", req.TaskID, maxAttempts)
		c.JSON(http.StatusOK, gin.H{"status": "dead", "attempts": attempts})
		return
	}

	res, err := store.DB.Exec(
		`UPDATE tasks SET status=?, result=?, error_msg=?, claim_token='', timeout_at=0, updated_at=?
		 WHERE task_id=? AND claim_token=?`,
		req.Status, req.Result, req.Error, now, req.TaskID, req.ClaimToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "认领凭据已失效（任务已被重新派发）", "stale": true})
		return
	}

	fmt.Printf("[任务回传] %s -> %s | 第 %d 次 | 结果:%s\n",
		req.TaskID, req.Status, attempts, truncate(req.Result, 60))
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

	query := `SELECT task_id, target_node, target_agent_kind, prompt, status,
		        COALESCE(result,''), COALESCE(error_msg,''),
		        created_at, COALESCE(claimed_at,0), updated_at,
		        attempts, COALESCE(max_attempts,3), timeout_at,
		        COALESCE(claimed_by,''), COALESCE(claim_token,'')
		 FROM tasks`
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
		Attempts        int64  `json:"attempts"`
		MaxAttempts     int64  `json:"max_attempts"`
		ClaimedBy       string `json:"claimed_by"`
	}

	list := make([]outTask, 0, limit)
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.TaskID, &r.TargetNode, &r.TargetAgentKind, &r.Prompt, &r.Status,
			&r.Result, &r.ErrorMsg, &r.CreatedAt, &r.ClaimedAt, &r.UpdatedAt,
			&r.Attempts, &r.MaxAttempts, &r.TimeoutAt, &r.ClaimedBy, &r.ClaimToken); err != nil {
			continue
		}
		list = append(list, outTask{
			TaskID:          r.TaskID,
			TargetNode:      r.TargetNode,
			TargetAgentKind: r.TargetAgentKind,
			Prompt:          r.Prompt,
			Status:          r.Status,
			Result:          r.Result,
			Error:           r.ErrorMsg,
			CreatedAt:       r.CreatedAt,
			UpdatedAt:       r.UpdatedAt,
			Attempts:        r.Attempts,
			MaxAttempts:     r.MaxAttempts,
			ClaimedBy:       r.ClaimedBy,
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
