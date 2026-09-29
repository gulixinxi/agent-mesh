package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrExecUnsupported 表示适配器只能只读监听，无法被注入执行指令。
var ErrExecUnsupported = errors.New("该适配器为只读监听型，不支持注入执行")

// AIAdapter 是所有本地 AI 运行时需要实现的统一接口。
// 云端 README 要求：严禁在主引擎中写特定工具的定制代码，新增工具必须在 adapters/ 下实现本接口。
type AIAdapter interface {
	Name() string
	Kind() string
	InspectStatus() (bool, error)
	StartLogTailing(ctx context.Context, logChan chan<- *TaskPayload) error
}

// TaskExecutor 是「可执行下发任务」这一可选能力接口。
// 只读监听型适配器（如豆包本地库）不实现它，调度侧据此跳过，
// 而不是靠试执行去探测能力（那会触发真实副作用）。
type TaskExecutor interface {
	Execute(ctx context.Context, prompt string) (string, error)
}

const (
	heartbeatInterval = 10 * time.Second
	auditWorkers      = 8
	queueSize         = 256
	// taskPollInterval 下行任务轮询间隔。
	taskPollInterval = 5 * time.Second
	// taskExecTimeout 单条任务的执行超时。
	// 中枢的 -task-timeout 必须大于这个值，否则正常执行的任务会被误判超时并重投。
	taskExecTimeout = 3 * time.Minute
	// maxConcurrentTasks 单节点同时执行的任务数上限。
	// AI 生成是重活，全放开会互相拖慢；但也不能串行，否则一条卡住的任务会堵住后面所有任务。
	maxConcurrentTasks = 2
)

// taskSem 是任务执行的并发槽位。
var taskSem = make(chan struct{}, maxConcurrentTasks)

// MeshEngine 是客户端常驻引擎，负责心跳上报与审计日志汇聚上报。
type MeshEngine struct {
	adapters   []AIAdapter
	logChan    chan *TaskPayload
	wg         sync.WaitGroup
	cancel     context.CancelFunc
	serverURL  string
	clientID   string
	secret     string
	httpClient *http.Client
}

// NewMeshEngine 创建引擎。serverURL 形如 http://192.168.1.10:8080。
// secret 为集群共享密钥，非空时对所有上报请求附加 HMAC 签名（服务端开启鉴权时必填）。
func NewMeshEngine(serverURL, clientID, secret string) *MeshEngine {
	return &MeshEngine{
		adapters:   make([]AIAdapter, 0),
		logChan:    make(chan *TaskPayload, queueSize),
		serverURL:  strings.TrimRight(serverURL, "/"),
		clientID:   clientID,
		secret:     secret,
		httpClient: http.DefaultClient,
	}
}

// SetHTTPClient 替换引擎使用的 HTTP 客户端。
// 服务端启用 HTTPS 且用的是内网自签证书时，必须换成信任该 CA 的客户端，否则握手失败。
func (e *MeshEngine) SetHTTPClient(c *http.Client) {
	if c != nil {
		e.httpClient = c
	}
}

// RegisterAdapter 注册一个本地 AI 适配器。
func (e *MeshEngine) RegisterAdapter(a AIAdapter) {
	e.adapters = append(e.adapters, a)
	fmt.Printf("[Engine] 适配器注册成功: %s\n", a.Name())
}

// RegisterAdapterSilent 注册适配器但不打印任何日志。
// MCP stdio 模式下 stdout 被 JSON-RPC 协议独占，任何额外输出都会破坏协议，
// 因此该模式必须用本方法注册。
func (e *MeshEngine) RegisterAdapterSilent(a AIAdapter) {
	e.adapters = append(e.adapters, a)
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

	// 下行任务轮询：定时向中枢领取派给本节点的任务并执行。
	e.startTaskPoller(ctx)
}

// startTaskPoller 每 5 秒拉一次待执行任务。
func (e *MeshEngine) startTaskPoller(ctx context.Context) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(taskPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.pollAndRunTasks(ctx)
			}
		}
	}()
}

// pollAndRunTasks 拉取待执行任务，并发执行。
//
// 逐条串行执行是明显的可靠性缺陷：一条卡住的任务会把后面所有任务一起堵住，
// 最长堵满整个执行超时。所以这里改成有上限的并发执行。
func (e *MeshEngine) pollAndRunTasks(ctx context.Context) {
	path := "/api/v1/tasks/pending"
	body, ok := e.getJSON(path, "node="+url.QueryEscape(e.clientID)+"&limit=2", 5*time.Second)
	if !ok {
		return
	}

	var resp struct {
		Tasks []struct {
			TaskID          string `json:"task_id"`
			TargetAgentKind string `json:"target_agent_kind"`
			Prompt          string `json:"prompt"`
			ClaimToken      string `json:"claim_token"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "[Engine] 解析任务列表失败: %v\n", err)
		return
	}
	for _, t := range resp.Tasks {
		t := t
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			// 并发槽位满了就让这条任务原地等；它会一直不回传，
			// 最终由中枢的超时回收逻辑退回待派发，不会丢。
			select {
			case taskSem <- struct{}{}:
				defer func() { <-taskSem }()
			case <-ctx.Done():
				return
			}
			e.runTask(ctx, t.TaskID, t.ClaimToken, t.TargetAgentKind, t.Prompt)
		}()
	}
}

// runTask 选中适配器执行任务，并把结果连同认领凭据一起回传中枢。
//
// claimToken 是领取任务时中枢下发的凭据。如果本节点执行太慢、中枢已把任务重投给别人，
// 这个凭据就失效了，中枢会拒掉这次回传——避免迟到的旧结果覆盖新一轮的结果。
func (e *MeshEngine) runTask(ctx context.Context, taskID, claimToken, kind, prompt string) {
	status, result, errMsg := "completed", "", ""

	executor := e.findExecutor(kind)
	switch {
	case executor == nil:
		status = "failed"
		if kind == "" {
			errMsg = "本机没有可执行的适配器"
		} else {
			errMsg = fmt.Sprintf("本机没有可执行的适配器（kind=%s）", kind)
		}
	default:
		execCtx, cancel := context.WithTimeout(ctx, taskExecTimeout)
		out, err := executor.Execute(execCtx, prompt)
		cancel()
		switch {
		case err != nil && errors.Is(err, context.DeadlineExceeded):
			// 执行超时不直接判死：模型偶尔会卡住，重投一轮可能就跑通了。
			// 报 timeout 让中枢按重试次数决定重投还是判死。
			status = "timeout"
			errMsg = fmt.Sprintf("执行超过 %s 未返回结果", taskExecTimeout)
		case err != nil:
			status = "failed"
			errMsg = err.Error()
		case ctx.Err() != nil:
			// 引擎正在关停，不回传结果：任务留着由中枢超时回收重投。
			return
		default:
			result = out
		}
	}

	fmt.Fprintf(os.Stderr, "[任务执行] %s | 适配器:%s | 结果:%s\n", taskID, kind, status)
	e.postJSON("/api/v1/tasks/result", map[string]interface{}{
		"task_id":     taskID,
		"claim_token": claimToken,
		"status":      status,
		"result":      result,
		"error":       errMsg,
	}, 5*time.Second)
}

// findExecutor 按 kind 挑一个当前可用、且实现了 TaskExecutor 的适配器。
// kind 为空时不限制类型；只读监听型适配器因未实现该接口会被自动跳过。
func (e *MeshEngine) findExecutor(kind string) TaskExecutor {
	for _, a := range e.adapters {
		if kind != "" && a.Kind() != kind {
			continue
		}
		runnable, err := a.InspectStatus()
		if err != nil || !runnable {
			continue
		}
		if executor, ok := a.(TaskExecutor); ok {
			return executor
		}
	}
	return nil
}

// getJSON 发起带签名的 GET 请求，返回响应体。
func (e *MeshEngine) getJSON(path, query string, timeout time.Duration) ([]byte, bool) {
	full := path
	if query != "" {
		full = path + "?" + query
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.serverURL+full, nil)
	if err != nil {
		return nil, false
	}

	// 签名必须用不含 query 的 path，与服务端 c.Request.URL.Path 保持一致。
	if e.secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := newNonce()
		req.Header.Set(headerTimestamp, ts)
		req.Header.Set(headerNonce, nonce)
		req.Header.Set(headerSignature,
			computeSignature(e.secret, http.MethodGet, path, ts, nonce, nil))
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[Engine] 拉取任务失败 %s: %v\n", path, err)
		return nil, false
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	if resp.StatusCode >= 300 {
		fmt.Printf("[Engine] 拉取任务异常 %s: HTTP %d\n", path, resp.StatusCode)
		return nil, false
	}
	return raw, true
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

	resp, err := e.httpClient.Do(req)
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
