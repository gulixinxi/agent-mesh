package core

import "time"

// TaskStatus 描述任务在 Mesh 中的生命周期状态。
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusCompleted TaskStatus = "completed"
	StatusFailed    TaskStatus = "failed"
)

// AgentInfo 是本机节点上单个 AI 适配器的能力摘要，随心跳上报到服务端。
type AgentInfo struct {
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Runnable bool   `json:"runnable"`
}

// TaskPayload 是贯穿 client 与 server 的审计 / 调度载荷。
// 云端 README 明确要求：心跳与审计交换必须严格遵循此结构。
type TaskPayload struct {
	TaskID          string                 `json:"task_id"`
	SourceNode      string                 `json:"source_node"`
	TargetNode      string                 `json:"target_node"`
	TargetAgentKind string                 `json:"target_agent_kind"`
	Prompt          string                 `json:"prompt"`
	Status          TaskStatus             `json:"status"`
	InputTokens     int                    `json:"input_tokens"`
	OutputTokens    int                    `json:"output_tokens"`
	Result          string                 `json:"result"`
	Metadata        map[string]interface{} `json:"metadata"`
	Timestamp       time.Time              `json:"timestamp"`
}

// NewTaskPayload 构造带默认值的载荷，避免各调用方重复初始化。
func NewTaskPayload(kind, prompt string) *TaskPayload {
	return &TaskPayload{
		TaskID:          newID("task"),
		TargetAgentKind: kind,
		Prompt:          prompt,
		Status:          StatusPending,
		Metadata:        map[string]interface{}{},
		Timestamp:       time.Now().UTC(),
	}
}
