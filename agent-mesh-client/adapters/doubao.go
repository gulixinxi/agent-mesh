package adapters

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"agent-mesh-client/core"

	_ "modernc.org/sqlite"
)

// DoubaoAdapter 通过「只读复制隔离（Copy-on-Read）」机制监听豆包本地 SQLite 会话库，
// 避免直接打开豆包正在写入的库造成锁竞争或死锁（README 明确要求的隔离策略）。
type DoubaoAdapter struct {
	dbPath    string
	lastMsgID int64
}

// NewDoubaoAdapter 创建适配器，path 指向豆包本地会话数据库文件。
func NewDoubaoAdapter(path string) *DoubaoAdapter {
	return &DoubaoAdapter{dbPath: path, lastMsgID: 0}
}

// Name 返回适配器的可读名称。
func (d *DoubaoAdapter) Name() string { return "豆包客户端" }

// Kind 返回适配器类型标识，会作为 target_agent_kind 上报。
func (d *DoubaoAdapter) Kind() string { return "doubao_local" }

// InspectStatus 判断会话库文件是否存在且非空。
func (d *DoubaoAdapter) InspectStatus() (bool, error) {
	info, err := os.Stat(d.dbPath)
	if err != nil {
		return false, nil // 客户端没装或路径不对，属于常态
	}
	return info.Size() > 0, nil
}

// StartLogTailing 每 3 秒增量扫描一次会话记录并投递到审计队列。
func (d *DoubaoAdapter) StartLogTailing(ctx context.Context, logChan chan<- *core.TaskPayload) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d.scanLogs(ctx, logChan)
		}
	}
}

// scanLogs 复制一份快照到临时目录后按 id 增量读取，避免长时间持有源库句柄。
func (d *DoubaoAdapter) scanLogs(ctx context.Context, logChan chan<- *core.TaskPayload) {
	if ok, _ := d.InspectStatus(); !ok {
		return
	}

	tmpDB := filepath.Join(os.TempDir(), fmt.Sprintf("mesh_doubao_tail_%d.db", time.Now().UnixNano()))
	src, err := os.Open(d.dbPath)
	if err != nil {
		return
	}
	dst, err := os.Create(tmpDB)
	if err != nil {
		src.Close()
		return
	}
	if _, err := io.Copy(dst, src); err != nil {
		src.Close()
		dst.Close()
		os.Remove(tmpDB)
		return
	}
	src.Close()
	dst.Close()
	defer os.Remove(tmpDB)

	// 驱动名 "sqlite" 来自 modernc.org/sqlite；若切回 mattn/go-sqlite3 需改成 "sqlite3" 并装 gcc。
	db, err := sql.Open("sqlite", tmpDB)
	if err != nil {
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT id, query, response, prompt_tokens, completion_tokens FROM messages WHERE id > ? ORDER BY id ASC LIMIT 100`,
		d.lastMsgID)
	if err != nil {
		// 表结构不匹配（真实的豆包 schema 尚未确认）时不致命，等下一轮重试。
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id int64
			q  string
			r  string
			pt int
			ct int
		)
		if err := rows.Scan(&id, &q, &r, &pt, &ct); err != nil {
			continue
		}

		payload := &core.TaskPayload{
			TaskID:          fmt.Sprintf("db%d", id),
			TargetAgentKind: d.Kind(),
			Prompt:          q,
			Result:          r,
			InputTokens:     pt,
			OutputTokens:    ct,
			Status:          core.StatusCompleted,
			Metadata:        map[string]interface{}{"source": "doubao_sqlite"},
			Timestamp:       time.Now(),
		}

		select {
		case logChan <- payload:
		case <-ctx.Done():
			return
		}
		if id > d.lastMsgID {
			d.lastMsgID = id
		}
	}
}
