package core

import (
	"encoding/json"
	"fmt"
)

// JSONRPCRequest 是 JSON-RPC 2.0 请求结构（MCP 基于其构建）。
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

// JSONRPCResponse 是 JSON-RPC 2.0 响应结构。
type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
	ID      interface{} `json:"id"`
}

// RPCError 描述 JSON-RPC 错误对象。
type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// ToolSpec 描述一个对外暴露的 MCP 工具。
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// ToolFunc 是 tools/call 的具体实现签名。
type ToolFunc func(args map[string]any) (any, error)

// MCPServerHandler 实现 AgentMesh 侧的 MCP 报文处理。
type MCPServerHandler struct {
	isInitialized bool
	tools         map[string]ToolFunc
	toolSpecs     map[string]ToolSpec
}

// NewMCPServerHandler 创建处理器并注册内置工具。
func NewMCPServerHandler() *MCPServerHandler {
	m := &MCPServerHandler{
		tools:     make(map[string]ToolFunc),
		toolSpecs: make(map[string]ToolSpec),
	}
	m.RegisterTool("execute_workbody_task", ToolSpec{
		Name:        "execute_workbody_task",
		Description: "调度本地 Agent Mesh 节点执行自动化任务",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "要下发的指令"},
			},
			"required": []string{"prompt"},
		},
	}, func(args map[string]any) (any, error) {
		prompt, _ := args["prompt"].(string)
		if prompt == "" {
			return nil, fmt.Errorf("参数 prompt 不能为空")
		}
		content := map[string]any{"type": "text", "text": "已接收任务: " + prompt}
		return map[string]any{"content": []any{content}, "isError": false}, nil
	})
	return m
}

// RegisterTool 注册一个可被 tools/call 调用的工具。
func (m *MCPServerHandler) RegisterTool(name string, spec ToolSpec, fn ToolFunc) {
	m.tools[name] = fn
	m.toolSpecs[name] = spec
}

// HandleMessage 处理一行 JSON-RPC 报文；返回 nil 表示无需回包（notification）。
func (m *MCPServerHandler) HandleMessage(input []byte) ([]byte, error) {
	var req JSONRPCRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return m.makeError(nil, -32700, "JSON Parse Error"), nil
	}
	if req.ID == nil {
		if req.Method == "notifications/initialized" {
			m.isInitialized = true
		}
		return nil, nil
	}
	if !m.isInitialized && req.Method != "initialize" {
		return m.makeError(req.ID, -32600, "Server not initialized"), nil
	}

	var (
		result any
		rpcErr *RPCError
	)
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]string{"name": "AgentMesh-Harness", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
		}
	case "tools/list":
		result = map[string]any{"tools": m.toolList()}
	case "tools/call":
		result, rpcErr = m.callTool(req.Params)
	case "ping":
		result = map[string]any{}
	default:
		rpcErr = &RPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)}
	}

	resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	return json.Marshal(resp)
}

func (m *MCPServerHandler) callTool(params json.RawMessage) (any, *RPCError) {
	var payload struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
	}
	fn, ok := m.tools[payload.Name]
	if !ok {
		return nil, &RPCError{Code: -32602, Message: "Unknown tool: " + payload.Name}
	}
	out, err := fn(payload.Arguments)
	if err != nil {
		return nil, &RPCError{Code: -32603, Message: err.Error()}
	}
	return out, nil
}

func (m *MCPServerHandler) toolList() []ToolSpec {
	list := make([]ToolSpec, 0, len(m.tools))
	for name := range m.tools {
		if spec, ok := m.toolSpecs[name]; ok {
			list = append(list, spec)
			continue
		}
		list = append(list, ToolSpec{Name: name})
	}
	return list
}

func (m *MCPServerHandler) makeError(id any, code int, msg string) []byte {
	resp := JSONRPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
	b, _ := json.Marshal(resp)
	return b
}
