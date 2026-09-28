package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// HeartbeatReq 是客户端心跳上报体，字段与 client/core/engine.go 发出的 JSON 严格对应。
type HeartbeatReq struct {
	ClientID   string          `json:"client_id"`
	ClientName string          `json:"client_name"`
	OS         string          `json:"os"`
	IPAddress  string          `json:"ip_address"`
	Status     string          `json:"status"`
	Agents     json.RawMessage `json:"agents"` // 适配器清单，原样存 JSON，不去解释具体结构
}

// DeviceResponse 是控制台视角下的节点视图。
type DeviceResponse struct {
	ClientID      string          `json:"client_id"`
	ClientName    string          `json:"client_name"`
	OS            string          `json:"os"`
	IPAddress     string          `json:"ip_address"`
	Status        string          `json:"status"`
	LastHeartbeat int64           `json:"last_heartbeat"`
	Agents        json.RawMessage `json:"agents,omitempty"`
}

// staleAfter 心跳超过这个时长没刷新，读取时就判定为离线。
const staleAfter = 30 * time.Second

// HandleHeartbeat 接收节点心跳并 UPSERT 进 devices 表。
func HandleHeartbeat(c *gin.Context) {
	var req HeartbeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad JSON"})
		return
	}
	if req.ClientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_id 不能为空"})
		return
	}

	query := `INSERT INTO devices (client_id, client_name, os, ip_address, status, last_heartbeat, agents)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET
			client_name    = excluded.client_name,
			os             = excluded.os,
			ip_address     = excluded.ip_address,
			status         = excluded.status,
			last_heartbeat = excluded.last_heartbeat,
			agents         = excluded.agents;`

	agents := "[]"
	if len(req.Agents) > 0 {
		agents = string(req.Agents)
	}

	if _, err := store.DB.Exec(query,
		req.ClientID, req.ClientName, req.OS, req.IPAddress, req.Status, time.Now().Unix(), agents,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "heartbeat ok"})
}

// GetDevices 返回全部已登记节点；心跳超时的会被标记为 offline。
func GetDevices(c *gin.Context) {
	rows, err := store.DB.Query(`SELECT client_id, client_name, os, ip_address, status, last_heartbeat, agents FROM devices ORDER BY last_heartbeat DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	devices := make([]DeviceResponse, 0)
	now := time.Now()
	for rows.Next() {
		var d DeviceResponse
		var agents sql.NullString
		if err := rows.Scan(&d.ClientID, &d.ClientName, &d.OS, &d.IPAddress, &d.Status, &d.LastHeartbeat, &agents); err != nil {
			continue
		}
		if now.Sub(time.Unix(d.LastHeartbeat, 0)) > staleAfter {
			d.Status = "offline"
		}
		if agents.Valid && agents.String != "" {
			d.Agents = json.RawMessage(agents.String)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, devices)
}
