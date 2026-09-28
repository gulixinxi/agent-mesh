package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// AuditLogReq 是客户端上报的任务审计载荷，字段与 client/core/engine.go 发出的 JSON 对应。
type AuditLogReq struct {
	TaskID          string `json:"task_id"`
	SourceNode      string `json:"source_node"`
	TargetNode      string `json:"target_node"`
	TargetAgentKind string `json:"target_agent_kind"`
	Prompt          string `json:"prompt"`
	Result          string `json:"result"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	Timestamp       int64  `json:"timestamp"`
}

// auditInsertSQL 用 UPSERT 兜住重试上报，避免主键冲突导致整个请求失败。
const auditInsertSQL = `INSERT INTO audit_logs
	(task_id, target_node, agent_kind, prompt, result, input_tokens, output_tokens, timestamp)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(task_id) DO UPDATE SET
		result = excluded.result,
		input_tokens = excluded.input_tokens,
		output_tokens = excluded.output_tokens;`

// ReportAuditLog 接收客户端的任务审计上报并落库。
func ReportAuditLog(c *gin.Context) {
	var req AuditLogReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad JSON"})
		return
	}
	if req.TaskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id 不能为空"})
		return
	}
	if req.Timestamp == 0 {
		req.Timestamp = time.Now().Unix()
	}

	if _, err := store.DB.Exec(auditInsertSQL,
		req.TaskID, req.TargetNode, req.TargetAgentKind,
		req.Prompt, req.Result, req.InputTokens, req.OutputTokens, req.Timestamp,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	fmt.Printf("[中央审计留痕] 节点:%s | 工具:%s | 指令:%s\n",
		req.TargetNode, req.TargetAgentKind, truncate(req.Prompt, 80))
	c.JSON(http.StatusOK, gin.H{"status": "audit report success"})
}

// ListAuditLogs 返回最近的审计日志，供控制台或排障脚本拉取。
func ListAuditLogs(c *gin.Context) {
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	node := c.Query("node")

	query := `SELECT task_id, target_node, agent_kind, prompt, result, input_tokens, output_tokens, timestamp
		FROM audit_logs`
	args := make([]interface{}, 0, 2)
	if node != "" {
		query += ` WHERE target_node = ?`
		args = append(args, node)
	}
	query += ` ORDER BY timestamp DESC LIMIT ?`
	args = append(args, limit)

	rows, err := store.DB.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type auditRow struct {
		TaskID          string `json:"task_id"`
		TargetNode      string `json:"target_node"`
		TargetAgentKind string `json:"target_agent_kind"`
		Prompt          string `json:"prompt"`
		Result          string `json:"result"`
		InputTokens     int    `json:"input_tokens"`
		OutputTokens    int    `json:"output_tokens"`
		Timestamp       int64  `json:"timestamp"`
	}

	list := make([]auditRow, 0, limit)
	for rows.Next() {
		var r auditRow
		var result sql.NullString
		if err := rows.Scan(&r.TaskID, &r.TargetNode, &r.TargetAgentKind, &r.Prompt,
			&result, &r.InputTokens, &r.OutputTokens, &r.Timestamp); err != nil {
			continue
		}
		r.Result = result.String
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
