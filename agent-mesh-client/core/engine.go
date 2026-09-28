package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AIAdapter 是所有本地 AI 运行时需要实现的统一接口。
// 云端 README 要求：严禁在主引擎中写特定工具的定制代码，新增工具必须在 adapters/ 下实现本接口。
type AIAdapter interface {
	Name() string
	Kind() string
	InspectStatus() (bool, error)
	StartLogTailing(ctx context.Context, logChan chan<- *TaskPayload) error
}

const (
	heartbeatInterval = 10 * time.Second
	auditWorkers      = 8
	queueSize         = 256
)

// MeshEngine 是客户端常驻引擎，负责心跳上报与审计日志汇聚上报。
type MeshEngine struct {
	adapters  []AIAdapter
	logChan   chan *TaskPayload
	wg        sync.WaitGroup
	cancel    context.CancelFunc
	serverURL string
	clientID  string
	secret    string
}

// NewMeshEngine 创建引擎。serverURL 形如 http://192.168.1.10:8080。
// secret 为集群共享密钥，非空时对所有上报请求附加 HMAC 签名（服务端开启鉴权时必填）。
func NewMeshEngine(serverURL, clientID, secret string) *MeshEngine {
	return &MeshEngine{
		adapters:  make([]AIAdapter, 0),
		logChan:   make(chan *TaskPayload, queueSize),
		serverURL: strings.TrimRight(serverURL, "/"),
		clientID:  clientID,
		secret:    secret,
	}
}

// RegisterAdapter 注册一个本地 AI 适配器。
func (e *MeshEngine) RegisterAdapter(a AIAdapter) {
	e.adapters = append(e.adapters, a)
	fmt.Printf("[Engine] 适配器注册成功: %s\n", a.Name())
}

// Agents 汇总当前所有适配器的在线状态，随心跳一并上报。
func (e *MeshEngine) Agents() []AgentInfo {
	list := make([]AgentInfo, 0, len(e.adapters))
	for _, a := range e.adapters {
		runnable, _ := a.InspectStatus()
		list = append(list, AgentInfo{
			AgentID:  e.clientID + "/" + a.Kind(),
			Name:     a.Name(),
			Kind:     a.Kind(),
			Runnable: runnable,
		})
	}
	return list
}

// Submit 供 adapter 或 MCP server 投递审计载荷；队列满时丢弃并返回 false。
func (e *MeshEngine) Submit(p *TaskPayload) bool {
	select {
	case e.logChan <- p:
		return true
	default:
		fmt.Println("[Engine] 审计队列已满，本次载荷被丢弃")
		return false
	}
}

// Start 启动心跳、审计上报与所有适配器的日志尾随协程。
func (e *MeshEngine) Start(ctx context.Context) {
	ctx, e.cancel = context.WithCancel(ctx)

	// 固定 worker 池消费审计队列，避免每条日志裸开一个 goroutine 打爆服务端。
	for i := 0; i < auditWorkers; i++ {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case payload := <-e.logChan:
					e.reportAuditLogToServer(payload)
				}
			}
		}()
	}

	for _, adapter := range e.adapters {
		e.wg.Add(1)
		go func(a AIAdapter) {
			defer e.wg.Done()
			if err := a.StartLogTailing(ctx, e.logChan); err != nil && ctx.Err() == nil {
				fmt.Printf("[Engine] 适配器 %s 日志尾随退出: %v\n", a.Name(), err)
			}
		}(adapter)
	}

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		hostname, _ := os.Hostname()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.sendHeartbeatToServer(hostname)
			}
		}
	}()
}

// Stop 取消内部 ctx 并等待所有协程退出。
func (e *MeshEngine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	close(e.logChan)
}

// Wait 阻塞等待外部取消后的自然收敛。
func (e *MeshEngine) Wait() {
	e.wg.Wait()
	close(e.logChan)
}

func (e *MeshEngine) sendHeartbeatToServer(hostname string) {
	// 先汇总本节点的适配器清单（含每个工具是否可用）。
	agents := e.Agents()

	// status 表示节点本身的连通性：只要能发出心跳就是 online。
	// 某个 AI 工具是否可用属于「能力」维度，放在 agents[].runnable 里单独上报，
	// 否则一个明明在线、只是没装 Ollama 的节点会被误判成 offline。
	data := map[string]interface{}{
		"client_id":   e.clientID,
		"client_name": hostname,
		"os":          "windows",
		"ip_address":  localIP(),
		"status":      "online",
		"agents":      agents,
	}
	e.postJSON("/api/v1/cluster/heartbeat", data, 3*time.Second)
}

func (e *MeshEngine) reportAuditLogToServer(payload *TaskPayload) {
	data := map[string]interface{}{
		"task_id":           payload.TaskID,
		"source_node":       payload.SourceNode,
		"target_node":       e.clientID,
		"target_agent_kind": payload.TargetAgentKind,
		"prompt":            payload.Prompt,
		"result":            payload.Result,
		"input_tokens":      payload.InputTokens,
		"output_tokens":     payload.OutputTokens,
		"timestamp":         payload.Timestamp.Unix(),
	}
	e.postJSON("/api/v1/audit/report", data, 3*time.Second)
}

func (e *MeshEngine) postJSON(path string, payload interface{}, timeout time.Duration) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.serverURL+path, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	// 配置了集群共享密钥时，对请求做 HMAC 签名（时间戳 + nonce 防重放）。
	if e.secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := newNonce()
		req.Header.Set(headerTimestamp, ts)
		req.Header.Set(headerNonce, nonce)
		req.Header.Set(headerSignature,
			computeSignature(e.secret, http.MethodPost, path, ts, nonce, body))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("[Engine] 上报失败 %s: %v\n", path, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		fmt.Printf("[Engine] 上报异常 %s: HTTP %d\n", path, resp.StatusCode)
	}
}

// localIP 取本机第一个非回环 IPv4 地址；取不到时退回 127.0.0.1。
func localIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return "127.0.0.1"
}

// newID 生成带前缀的随机 ID。
func newID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf)
}
