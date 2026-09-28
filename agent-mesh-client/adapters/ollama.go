package adapters

import (
	"context"
	"net/http"
	"time"

	"agent-mesh-client/core"
)

// OllamaAdapter 探测本机 Ollama 服务的可用性。
type OllamaAdapter struct {
	endpoint string
}

// NewOllamaAdapter 创建指向本地 11434 端口的适配器。
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{endpoint: "http://127.0.0.1:11434"}
}

// Name 返回适配器的可读名称。
func (o *OllamaAdapter) Name() string { return "Ollama 本地引擎" }

// Kind 返回适配器类型标识，会作为 target_agent_kind 上报。
func (o *OllamaAdapter) Kind() string { return "ollama" }

// InspectStatus 通过 /api/tags 判断服务是否存活。
func (o *OllamaAdapter) InspectStatus() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint+"/api/tags", nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil // 服务没启动属于常态，不算错误
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// StartLogTailing Ollama 没有结构化任务日志，这里先占位：收到 ctx 取消即退出。
// 后续可改为读取 Ollama 自身的 server.log 或直接拦截 /api/chat 调用。
func (o *OllamaAdapter) StartLogTailing(ctx context.Context, logChan chan<- *core.TaskPayload) error {
	<-ctx.Done()
	return ctx.Err()
}
