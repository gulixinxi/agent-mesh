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
	//
	// 可靠性相关的四个字段：
	//   attempts     已被领取的次数，用于判断还能不能重投
	//   max_attempts 该任务允许的最大领取次数，超出即判死，避免无限重投
	//   timeout_at   本次领取的到期时刻；节点宕机时由 reaper 依据它回收
	//   claim_token  本次领取的认领凭据；重投后旧凭据作废，
	//                迟到的旧节点带着旧 token 回传结果会被拒，避免结果错配
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
		updated_at        INTEGER,
		attempts          INTEGER DEFAULT 0,
		max_attempts      INTEGER DEFAULT 3,
		timeout_at        INTEGER DEFAULT 0,
		claimed_by        TEXT,
		claim_token       TEXT
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

	// 轻量迁移：老版本建的表没有这些列，这里补上；已存在时 SQLite 会报错，忽略即可。
	_, _ = DB.Exec(`ALTER TABLE devices ADD COLUMN agents TEXT;`)
	for _, alter := range []string{
		`ALTER TABLE tasks ADD COLUMN attempts INTEGER DEFAULT 0;`,
		`ALTER TABLE tasks ADD COLUMN max_attempts INTEGER DEFAULT 3;`,
		`ALTER TABLE tasks ADD COLUMN timeout_at INTEGER DEFAULT 0;`,
		`ALTER TABLE tasks ADD COLUMN claimed_by TEXT;`,
		`ALTER TABLE tasks ADD COLUMN claim_token TEXT;`,
	} {
		_, _ = DB.Exec(alter)
	}

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

// ReapTimedOutTasks 回收「超时未回传」的任务：还有余量的退回 pending，次数耗尽的判死。
//
// 这是僵尸 running 任务的唯一回收路径：节点一旦宕机就再也不会有回传，
// 而 CleanupFinishedTasks 只处理 completed/failed，永远碰不到它们。
//
// 两条 UPDATE 的条件互斥（attempts < max_attempts 与 attempts >= max_attempts），
// 放在同一个事务里执行，确保同一条任务不会被既重投又判死。
func ReapTimedOutTasks() (requeued int64, dead int64, err error) {
	tx, err := DB.Begin()
	if err != nil {
		return 0, 0, err
	}

	now := time.Now().Unix()

	res, err := tx.Exec(
		`UPDATE tasks SET status='pending', claimed_by='', claim_token='', timeout_at=0, updated_at=?
		 WHERE status='running' AND timeout_at > 0 AND timeout_at <= ?
		   AND attempts < COALESCE(max_attempts, 3)`, now, now)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	if requeued, err = res.RowsAffected(); err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}

	res, err = tx.Exec(
		`UPDATE tasks SET status='failed', error_msg=?, claimed_by='', claim_token='', timeout_at=0, updated_at=?
		 WHERE status='running' AND timeout_at > 0 AND timeout_at <= ?
		   AND attempts >= COALESCE(max_attempts, 3)`,
		"执行超时且重试次数已耗尽（节点可能已宕机）", now, now)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	if dead, err = res.RowsAffected(); err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return requeued, dead, nil
}
