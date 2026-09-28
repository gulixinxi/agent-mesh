package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB 是全局 SQLite 句柄，由 InitDB 初始化后供 api 层使用。
var DB *sql.DB

// InitDB 打开（必要时新建）SQLite 中央数据底座，并建立两张核心表。
func InitDB(dbPath string) error {
	var err error
	// 驱动名统一用 "sqlite"（纯 Go），不要写成 mattn 的 "sqlite3"。
	DB, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 只有一个写者，多节点并发上报时靠 WAL + busy_timeout 兜住 "database is locked"。
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
	} {
		if _, err := DB.Exec(pragma); err != nil {
			return fmt.Errorf("设置 PRAGMA 失败 (%s): %w", pragma, err)
		}
	}

	deviceTable := `CREATE TABLE IF NOT EXISTS devices (
		client_id      TEXT PRIMARY KEY,
		client_name    TEXT,
		os             TEXT,
		ip_address     TEXT,
		status         TEXT,
		last_heartbeat INTEGER,
		agents         TEXT
	);`

	auditTable := `CREATE TABLE IF NOT EXISTS audit_logs (
		task_id       TEXT PRIMARY KEY,
		target_node   TEXT,
		agent_kind    TEXT,
		prompt        TEXT,
		result        TEXT,
		input_tokens  INTEGER,
		output_tokens INTEGER,
		timestamp     INTEGER
	);`

	// 下行任务表：控制台下发 -> 节点领取执行 -> 结果回传，构成完整闭环。
	taskTable := `CREATE TABLE IF NOT EXISTS tasks (
		task_id           TEXT PRIMARY KEY,
		target_node       TEXT,
		target_agent_kind TEXT,
		prompt            TEXT,
		status            TEXT,
		result            TEXT,
		error_msg         TEXT,
		created_at        INTEGER,
		claimed_at        INTEGER,
		updated_at        INTEGER
	);`

	if _, err := DB.Exec(deviceTable); err != nil {
		return fmt.Errorf("创建 devices 表失败: %w", err)
	}
	if _, err := DB.Exec(auditTable); err != nil {
		return fmt.Errorf("创建 audit_logs 表失败: %w", err)
	}
	if _, err := DB.Exec(taskTable); err != nil {
		return fmt.Errorf("创建 tasks 表失败: %w", err)
	}

	// 轻量迁移：老版本建的表没有 agents 列，这里补上；已存在时 SQLite 会报错，忽略即可。
	_, _ = DB.Exec(`ALTER TABLE devices ADD COLUMN agents TEXT;`)

	fmt.Println("[DB] 服务端 SQLite 中央数据底座初始化完成。")
	return nil
}

// CleanupStaleDevices 把超过 timeout 没有心跳的节点标记为 offline。
func CleanupStaleDevices(timeout time.Duration) error {
	cutoff := time.Now().Add(-timeout).Unix()
	_, err := DB.Exec(`UPDATE devices SET status = 'offline' WHERE status != 'offline' AND last_heartbeat < ?`, cutoff)
	return err
}

// CleanupOldAuditLogs 删除超过 retention 的审计流水，防止 SQLite 无限膨胀。
// 返回被删除的行数。
func CleanupOldAuditLogs(retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).Unix()
	res, err := DB.Exec(`DELETE FROM audit_logs WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CleanupFinishedTasks 删除已完结（completed / failed）且超过 retention 的历史任务。
// 未完结（pending / running）的任务绝不清理，避免误删正在处理的工单。
func CleanupFinishedTasks(retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).Unix()
	res, err := DB.Exec(
		`DELETE FROM tasks WHERE status IN ('completed','failed') AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}
