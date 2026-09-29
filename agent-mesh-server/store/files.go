package store

import (
	"database/sql"
	"errors"
)

// =====================================================================
// 中转文件元数据
//
// 背景：客户端之间原本只有 libp2p mDNS 直传一条文件通道，而 mDNS 是二层
// 组播、路由器不转发 —— 跨网段（哪怕同一个厂区）就彻底不通；任务执行产出的
// 文件更是只留在执行机本地，中枢只能收到一句文字结果，取不回来。
//
// 这里把文件的「元数据」放库里、「实体字节」放磁盘，用随机 file_id 寻址，
// 于是文件可以先上传到中枢、再由目标节点拉走，不依赖任何点对点可达性。
// =====================================================================

// ErrFileNotFound 表示请求的中转文件不存在（从未上传，或已被过期清理）。
var ErrFileNotFound = errors.New("文件不存在")

// FileMeta 是 files 表的一行。
//
// 实体字节不入库：把 SQLite 当二进制仓库会同时拖慢审计写入与文件读写，
// 且备份与清理都变得难以控制。库里只留指向磁盘文件的指针。
type FileMeta struct {
	FileID       string `json:"file_id"`
	FileName     string `json:"file_name"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Uploader     string `json:"uploader"`
	TargetNode   string `json:"target_node"` // 空 = 广播，所有节点可见
	TaskID       string `json:"task_id"`     // 可选：关联的任务，便于把「任务产出的文件」归位
	CreatedAt    int64  `json:"created_at"`
	DownloadedAt int64  `json:"downloaded_at"` // 0 表示尚未被目标节点领取
}

const fileTable = `CREATE TABLE IF NOT EXISTS files (
	file_id       TEXT PRIMARY KEY,
	file_name     TEXT,
	size          INTEGER,
	sha256        TEXT,
	uploader      TEXT,
	target_node   TEXT,
	task_id       TEXT,
	created_at    INTEGER,
	downloaded_at INTEGER DEFAULT 0
);`

// fileIndex 让「按目标节点捞未领取文件」这一最热查询走索引。
// 客户端每次轮询都会打这条路径，不加索引会随文件数线性变慢。
const fileIndex = `CREATE INDEX IF NOT EXISTS idx_files_target ON files(target_node, downloaded_at);`

// initFileTable 建立中转文件表与索引，由 InitDB 调用。
func initFileTable() error {
	if _, err := DB.Exec(fileTable); err != nil {
		return err
	}
	_, err := DB.Exec(fileIndex)
	return err
}

// InsertFile 写入一条文件元数据。
func InsertFile(m FileMeta) error {
	_, err := DB.Exec(
		`INSERT INTO files
		 (file_id, file_name, size, sha256, uploader, target_node, task_id, created_at, downloaded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.FileID, m.FileName, m.Size, m.SHA256, m.Uploader, m.TargetNode, m.TaskID, m.CreatedAt, m.DownloadedAt,
	)
	return err
}

// GetFileMeta 按 file_id 取单条元数据；不存在时返回 ErrFileNotFound。
func GetFileMeta(fileID string) (FileMeta, error) {
	var m FileMeta
	err := DB.QueryRow(
		`SELECT file_id, COALESCE(file_name,''), COALESCE(size,0), COALESCE(sha256,''),
		        COALESCE(uploader,''), COALESCE(target_node,''), COALESCE(task_id,''),
		        COALESCE(created_at,0), COALESCE(downloaded_at,0)
		 FROM files WHERE file_id = ?`, fileID,
	).Scan(&m.FileID, &m.FileName, &m.Size, &m.SHA256, &m.Uploader, &m.TargetNode,
		&m.TaskID, &m.CreatedAt, &m.DownloadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FileMeta{}, ErrFileNotFound
	}
	return m, err
}

// ListFilesForNode 返回某节点可见的中转文件：目标是它本人的，或广播给所有人的。
//
// 按节点过滤是必要的：若不区分，任何持密钥的节点都能枚举出别人被定向投递的
// 文件名与大小 —— 那是元数据泄露。
func ListFilesForNode(node string, pendingOnly bool, limit int) ([]FileMeta, error) {
	query := `SELECT file_id, COALESCE(file_name,''), COALESCE(size,0), COALESCE(sha256,''),
	                 COALESCE(uploader,''), COALESCE(target_node,''), COALESCE(task_id,''),
	                 COALESCE(created_at,0), COALESCE(downloaded_at,0)
	          FROM files
	          WHERE (target_node = '' OR target_node = ?)`
	if pendingOnly {
		query += ` AND COALESCE(downloaded_at,0) = 0`
	}
	query += ` ORDER BY created_at DESC LIMIT ?`

	return scanFiles(query, node, limit)
}

// ListAllFiles 返回全部中转文件，供控制台总览（控制台已由 Basic Auth 保护）。
func ListAllFiles(limit int) ([]FileMeta, error) {
	query := `SELECT file_id, COALESCE(file_name,''), COALESCE(size,0), COALESCE(sha256,''),
	                 COALESCE(uploader,''), COALESCE(target_node,''), COALESCE(task_id,''),
	                 COALESCE(created_at,0), COALESCE(downloaded_at,0)
	          FROM files ORDER BY created_at DESC LIMIT ?`
	return scanFiles(query, limit)
}

// scanFiles 是 ListFilesForNode / ListAllFiles 共用的行扫描逻辑。
// limit 参数始终是最后一个占位符，与上面两条 SQL 的写法保持一致。
func scanFiles(query string, args ...interface{}) ([]FileMeta, error) {
	rows, err := DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]FileMeta, 0, 16)
	for rows.Next() {
		var m FileMeta
		if err := rows.Scan(&m.FileID, &m.FileName, &m.Size, &m.SHA256, &m.Uploader,
			&m.TargetNode, &m.TaskID, &m.CreatedAt, &m.DownloadedAt); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// MarkFileDownloaded 记录该文件已被领取。只写 0 → now，
// 重复下载不会把时间戳越刷越新（避免掩盖「何时首次领取」这个事实）。
func MarkFileDownloaded(fileID string, at int64) error {
	_, err := DB.Exec(
		`UPDATE files SET downloaded_at = ? WHERE file_id = ? AND COALESCE(downloaded_at,0) = 0`,
		at, fileID)
	return err
}

// DeleteFileMeta 删除一条元数据；磁盘实体由调用方负责清理。返回是否真的删掉了行。
func DeleteFileMeta(fileID string) (bool, error) {
	res, err := DB.Exec(`DELETE FROM files WHERE file_id = ?`, fileID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// TotalFileBytes 返回中转文件的磁盘占用总和，用于配额判断。
func TotalFileBytes() (int64, error) {
	var total sql.NullInt64
	if err := DB.QueryRow(`SELECT SUM(size) FROM files`).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// ExpiredFiles 返回创建时间早于 cutoff 的文件元数据，供过期清理使用。
func ExpiredFiles(cutoff int64, limit int) ([]FileMeta, error) {
	query := `SELECT file_id, COALESCE(file_name,''), COALESCE(size,0), COALESCE(sha256,''),
	                 COALESCE(uploader,''), COALESCE(target_node,''), COALESCE(task_id,''),
	                 COALESCE(created_at,0), COALESCE(downloaded_at,0)
	          FROM files WHERE created_at < ? ORDER BY created_at ASC LIMIT ?`
	return scanFiles(query, cutoff, limit)
}
