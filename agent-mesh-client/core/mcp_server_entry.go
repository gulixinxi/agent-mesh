package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
)

// MCPServerEntry 把 MCP 处理器接到 stdio 传输层上。
type MCPServerEntry struct {
	handler *MCPServerHandler
}

// NewMCPServerEntry 创建 stdio 入口。
func NewMCPServerEntry() *MCPServerEntry {
	return &MCPServerEntry{handler: NewMCPServerHandler()}
}

// Handler 暴露底层处理器，便于外部注册额外的 tool。
func (e *MCPServerEntry) Handler() *MCPServerHandler { return e.handler }

// StartStdioLoop 从 stdin 逐行读取 JSON-RPC 报文，并把响应写回 stdout。
func (e *MCPServerEntry) StartStdioLoop(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	go func() {
		<-ctx.Done()
		os.Exit(0)
	}()

	for scanner.Scan() {
		inputBytes := scanner.Bytes()
		if len(inputBytes) == 0 {
			continue
		}
		outputBytes, err := e.handler.HandleMessage(inputBytes)
		if err != nil {
			fmt.Fprintln(os.Stderr, "MCP Error:", err)
			continue
		}
		if outputBytes != nil {
			if _, err := io.WriteString(os.Stdout, string(outputBytes)+"\n"); err != nil {
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "stdin 读取失败:", err)
	}
}
