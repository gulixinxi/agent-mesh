package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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
	engine        *MeshEngine        // 可选：提供节点状态与适配器清单
	p2p           *P2PTransferManager // 可选：提供 P2P 文件直传能力
}

// SetEngine 注入常驻引擎，使 mesh.status / mesh.agents 能读到真实状态。
// 未注入时这两个工具会明确返回「引擎未就绪」，而不是给出假数据。
func (m *MCPServerHandler) SetEngine(e *MeshEngine) { m.engine = e }

// SetP2P 注入 P2P 管理器，使 mesh.sendfile 能真正发起内网直传。
func (m *MCPServerHandler) SetP2P(p *P2PTransferManager) { m.p2p = p }

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
		return textResult("已接收任务: " + prompt)
	})

	// mesh.status：本机节点的内网健康状态与网卡信息。
	m.RegisterTool("mesh.status", ToolSpec{
		Name:        "mesh.status",
		Description: "获取本机 Agent Mesh 节点的内网健康状态、节点 ID、上报目标与 P2P 地址表",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (any, error) {
		status := map[string]any{
			"local_ip": localIP(),
			"p2p":      "未启用",
		}
		if m.engine != nil {
			status["node_id"] = m.engine.clientID
			status["server"] = m.engine.serverURL
			status["signed"] = m.engine.secret != ""
		} else {
			status["node_id"] = ""
			status["engine"] = "未就绪"
		}
		if m.p2p != nil {
			status["p2p"] = "已启用"
			status["peer_id"] = m.p2p.Host.ID().String()
			status["listen_addrs"] = m.p2p.ListenAddresses()
			status["known_peers"] = m.p2p.KnownPeerCount()
		}
		return textResult(status)
	})

	// mesh.agents：枚举本机可用的适配器。
	m.RegisterTool("mesh.agents", ToolSpec{
		Name:        "mesh.agents",
		Description: "枚举本机常驻的所有 AI 适配器清单及其可用状态（runnable）",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(args map[string]any) (any, error) {
		if m.engine == nil {
			return nil, fmt.Errorf("引擎未就绪，无法枚举适配器")
		}
		return textResult(m.engine.Agents())
	})

	// mesh.sendfile：向指定 PeerID 直传文件。
	m.RegisterTool("mesh.sendfile", ToolSpec{
		Name:        "mesh.sendfile",
		Description: "调用本地 libp2p 引擎，向内网另一 PeerID 直传实体文件（SHA-256 校验）",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"peer_id":   map[string]any{"type": "string", "description": "目标节点的 PeerID"},
				"file_path": map[string]any{"type": "string", "description": "本机待发送文件的绝对路径"},
			},
			"required": []string{"peer_id", "file_path"},
		},
	}, func(args map[string]any) (any, error) {
		if m.p2p == nil {
			return nil, fmt.Errorf("P2P 未启用，无法发送文件（启动时需指定 -p2p-port）")
		}
		peerID, _ := args["peer_id"].(string)
		filePath, _ := args["file_path"].(string)
		if peerID == "" || filePath == "" {
			return nil, fmt.Errorf("参数 peer_id 与 file_path 不能为空")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := m.p2p.SendFileToPeer(ctx, peerID, filePath); err != nil {
			// P2P 只有 mDNS 发现，而 mDNS 不跨网段 —— 跨网段时必然失败。
			// 与其让调用方看到一个无从下手的报错，不如直接给出可用的替代路径。
			return nil, fmt.Errorf("%w；若两台机器不在同一网段（mDNS 不跨网段），请改用 mesh.relayfile 经中枢中转", err)
		}
		return textResult(map[string]any{"status": "sent", "peer_id": peerID, "file": filePath})
	})

	// mesh.relayfile：经中枢中转把本机文件送达指定节点。
	// 这是跨网段场景下唯一可用的文件通道。
	m.RegisterTool("mesh.relayfile", ToolSpec{
		Name: "mesh.relayfile",
		Description: "经中央枢纽中转，把本机文件送达指定节点（跨网段或无法 P2P 直连时必须走这条路）。" +
			"target_node 留空表示所有节点可见；填写则只有该节点能取。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path":   map[string]any{"type": "string", "description": "本机待发送文件的绝对路径"},
				"target_node": map[string]any{"type": "string", "description": "目标节点 ID（可选；留空则广播给所有节点）"},
				"task_id":     map[string]any{"type": "string", "description": "关联的任务 ID（可选，便于把任务产出物归位）"},
			},
			"required": []string{"file_path"},
		},
	}, func(args map[string]any) (any, error) {
		if m.engine == nil {
			return nil, fmt.Errorf("引擎未就绪，无法上传文件")
		}
		filePath, _ := args["file_path"].(string)
		if filePath == "" {
			return nil, fmt.Errorf("参数 file_path 不能为空")
		}
		targetNode, _ := args["target_node"].(string)
		taskID, _ := args["task_id"].(string)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		meta, err := m.engine.UploadFileToServer(ctx, filePath, targetNode, taskID)
		if err != nil {
			return nil, err
		}
		return textResult(map[string]any{
			"status":      "relayed",
			"file_id":     meta.FileID,
			"file_name":   meta.FileName,
			"size":        meta.Size,
			"sha256":      meta.SHA256,
			"target_node": meta.TargetNode,
		})
	})

	// mesh.fetchfile：列出或领取中枢中转给本机的文件。
	m.RegisterTool("mesh.fetchfile", ToolSpec{
		Name:        "mesh.fetchfile",
		Description: "查看或领取经中枢中转给本机的文件。不带 file_id 时列出待领取清单，带 file_id 时只取那一个并落盘。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_id": map[string]any{"type": "string", "description": "要领取的文件 ID（可选；留空则只列出清单）"},
			},
		},
	}, func(args map[string]any) (any, error) {
		if m.engine == nil {
			return nil, fmt.Errorf("引擎未就绪，无法领取文件")
		}
		fileID, _ := args["file_id"].(string)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		if fileID != "" {
			saved, err := m.engine.DownloadRelayFile(ctx, fileID)
			if err != nil {
				return nil, err
			}
			return textResult(map[string]any{"status": "saved", "file_id": fileID, "path": saved})
		}

		files, err := m.engine.ListRelayFiles(ctx, m.engine.ClientID(), true)
		if err != nil {
			return nil, err
		}
		return textResult(map[string]any{"count": len(files), "files": files})
	})

	return m
}

// textResult 把任意结果包成 MCP 要求的 content 结构。
func textResult(v any) (any, error) {
	var text string
	switch t := v.(type) {
	case string:
		text = t
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": false,
	}, nil
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
