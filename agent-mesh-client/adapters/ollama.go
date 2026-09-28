package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"agent-mesh-client/core"
)

// OllamaAdapter 探测本机 Ollama 服务的可用性，并可执行下发任务。
type OllamaAdapter struct {
	endpoint string
	model    string
}

// NewOllamaAdapter 创建指向本地 11434 端口的适配器。
// 模型留空时，执行任务会自动挑选本机第一个可用模型。
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{endpoint: "http://127.0.0.1:11434"}
}

// WithModel 指定执行任务时使用的模型名。
func (o *OllamaAdapter) WithModel(model string) *OllamaAdapter {
	o.model = model
	return o
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

// Execute 调用本地 Ollama 的 /api/generate 执行一条指令。
// 未指定模型时，先查 /api/tags 取本机第一个可用模型，避免硬编码模型名导致执行失败。
func (o *OllamaAdapter) Execute(ctx context.Context, prompt string) (string, error) {
	if prompt == "" {
		return "", fmt.Errorf("prompt 不能为空")
	}

	model := o.model
	if model == "" {
		picked, err := o.firstAvailableModel(ctx)
		if err != nil {
			return "", err
		}
		model = picked
	}

	payload := map[string]interface{}{
		"model":  model,
		"prompt": prompt,
		"stream": false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 Ollama 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Ollama 返回 HTTP %d: %s", resp.StatusCode, truncateText(string(raw), 200))
	}

	var out struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 Ollama 响应失败: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("Ollama 报错: %s", out.Error)
	}
	return out.Response, nil
}

// firstAvailableModel 取本机第一个已安装的模型名。
func (o *OllamaAdapter) firstAvailableModel(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint+"/api/tags", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Ollama 不可用: %w", err)
	}
	defer resp.Body.Close()

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", fmt.Errorf("解析模型列表失败: %w", err)
	}
	if len(tags.Models) == 0 {
		return "", fmt.Errorf("本机 Ollama 未安装任何模型")
	}
	return tags.Models[0].Name, nil
}

func truncateText(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
