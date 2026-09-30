package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// =====================================================================
// 邀请码（入网凭证）存储层
//
// 设计要点（对齐对标产品的自助入网模型，但适配我们现有的「集群共享密钥」鉴权）：
//   1. 邀请码只是「把配置安全送到机器上」的一次性凭证，不引入第二套鉴权体系。
//      集群密钥本身没有变，也就没有破坏既有 HMAC 协议的风险。
//   2. 库里只存 sha256 哈希 —— 数据库被拖走也拿不到可用的码。
//      明文只在签发那一刻返回一次，之后任何接口都查不出来。
//   3. 消费是事务内原子递增，天然处理并发：多台机器同时用同一个码时，
//      只有名额之内的那次能成功，不会出现「超发」。
//   4. 状态不做持久化改写（除作废），读取时按 expires_at / use_count 现算，
//      避免依赖定时任务才能显示正确状态。
// =====================================================================

// inviteAlphabet 是邀请码字符集：剔除 0/O/1/I/L 等肉眼易混字符。
// 用户是要照着念、照着敲的，少一个歧义字符就少一通售后电话。
const inviteAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// inviteLen 是邀请码有效字符数（分组展示为 XXXX-XXXX-XXXX-XXXX）。
const inviteLen = 16

// 邀请码状态。
const (
	InviteActive    = "active"
	InviteRevoked   = "revoked"
	InviteExhausted = "exhausted"
	InviteExpired   = "expired"
)

// 邀请码相关的可判别错误。调用方据此返回不同的 HTTP 语义。
var (
	ErrInviteNotFound  = errors.New("邀请码不存在")
	ErrInviteRevoked   = errors.New("邀请码已被作废")
	ErrInviteExpired   = errors.New("邀请码已过期")
	ErrInviteExhausted = errors.New("邀请码可用次数已用尽")
)

// Invite 是一条邀请码的元数据。注意：不含明文码。
type Invite struct {
	CodeHash    string `json:"-"`
	Label       string `json:"label"`
	MaxUses     int    `json:"max_uses"` // 0 表示不限次数
	UseCount    int    `json:"use_count"`
	Status      string `json:"status"`
	ExpiresAt   int64  `json:"expires_at"`
	CreatedAt   int64  `json:"created_at"`
	FirstUsedAt int64  `json:"first_used_at"`
	LastUsedAt  int64  `json:"last_used_at"`
	UsedBy      string `json:"used_by"`
}

// ID 返回可对外展示的短标识（哈希前缀）。
// 作废接口既接受它，也接受明文码，管理员不必额外记录。
func (i Invite) ID() string {
	if len(i.CodeHash) < 12 {
		return i.CodeHash
	}
	return i.CodeHash[:12]
}

// Enrollment 是一次入网自检的记录，用于「装完了没有」的闭环可见性。
type Enrollment struct {
	ID         int64  `json:"id"`
	CodeLabel  string `json:"code_label"`
	ClientID   string `json:"client_id"`
	Hostname   string `json:"hostname"`
	OS         string `json:"os"`
	IPAddress  string `json:"ip_address"`
	OK         bool   `json:"ok"`
	Steps      string `json:"steps"` // JSON 数组：各步骤的成败与详情
	CreatedAt  int64  `json:"created_at"`
	CodeMasked string `json:"code_masked"`
}

// initInviteTables 建立邀请码与入网记录两张表。
func initInviteTables() error {
	inviteTable := `CREATE TABLE IF NOT EXISTS invites (
		code_hash     TEXT PRIMARY KEY,
		label         TEXT,
		max_uses      INTEGER NOT NULL DEFAULT 1,
		use_count     INTEGER NOT NULL DEFAULT 0,
		status        TEXT NOT NULL DEFAULT 'active',
		expires_at    INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL,
		first_used_at INTEGER NOT NULL DEFAULT 0,
		last_used_at  INTEGER NOT NULL DEFAULT 0,
		used_by       TEXT
	);`

	enrollTable := `CREATE TABLE IF NOT EXISTS enrollments (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		code_hash  TEXT,
		code_label TEXT,
		client_id  TEXT,
		hostname   TEXT,
		os         TEXT,
		ip_address TEXT,
		ok         INTEGER NOT NULL DEFAULT 0,
		steps      TEXT,
		created_at INTEGER NOT NULL
	);`

	if _, err := DB.Exec(inviteTable); err != nil {
		return fmt.Errorf("创建 invites 表失败: %w", err)
	}
	if _, err := DB.Exec(enrollTable); err != nil {
		return fmt.Errorf("创建 enrollments 表失败: %w", err)
	}
	if _, err := DB.Exec(`CREATE INDEX IF NOT EXISTS idx_invites_created_at ON invites(created_at);`); err != nil {
		return fmt.Errorf("创建 invites 索引失败: %w", err)
	}
	if _, err := DB.Exec(`CREATE INDEX IF NOT EXISTS idx_enrollments_created_at ON enrollments(created_at);`); err != nil {
		return fmt.Errorf("创建 enrollments 索引失败: %w", err)
	}
	return nil
}

// NewInviteCode 生成一个新的明文邀请码，格式 XXXX-XXXX-XXXX-XXXX。
//
// 用 crypto/rand 而非 math/rand：邀请码是能换取集群密钥的凭证，
// 可预测的随机数等于把密钥拱手让人。
func NewInviteCode() (string, error) {
	buf := make([]byte, inviteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成邀请码随机数失败: %w", err)
	}
	// 取模引入的偏置对我们的码长来说可以忽略（256 % 31 != 0 但偏差 < 0.4%），
	// 换取的是不引入额外依赖的简洁实现。
	var sb strings.Builder
	for i, b := range buf {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(inviteAlphabet[int(b)%len(inviteAlphabet)])
	}
	return sb.String(), nil
}

// NormalizeInviteCode 把用户输入的邀请码归一化：去掉空白与连字符、统一大写。
// 这样「abcd efgh-ijkl mnop」也能匹配上。
func NormalizeInviteCode(raw string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// HashInviteCode 计算归一化后邀请码的 SHA-256（hex）。入库与比对都用它。
func HashInviteCode(code string) string {
	sum := sha256.Sum256([]byte(NormalizeInviteCode(code)))
	return hex.EncodeToString(sum[:])
}

// MaskInviteCode 只保留首尾各 2 位用于界面展示，例如 "AB••••••••••••OP"。
func MaskInviteCode(code string) string {
	n := NormalizeInviteCode(code)
	if len(n) <= 4 {
		return strings.Repeat("•", len(n))
	}
	return n[:2] + strings.Repeat("•", len(n)-4) + n[len(n)-2:]
}

// CreateInvite 签发一条邀请码，返回明文码（仅此一次）与元数据。
// ttl <= 0 时默认 30 分钟；maxUses <= 0 视为不限次数。
func CreateInvite(label string, ttl time.Duration, maxUses int) (string, *Invite, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if maxUses < 0 {
		maxUses = 0
	}
	code, err := NewInviteCode()
	if err != nil {
		return "", nil, err
	}
	hash := HashInviteCode(code)
	now := time.Now()

	inv := &Invite{
		CodeHash:  hash,
		Label:     strings.TrimSpace(label),
		MaxUses:   maxUses,
		Status:    InviteActive,
		ExpiresAt: now.Add(ttl).Unix(),
		CreatedAt: now.Unix(),
	}

	if _, err := DB.Exec(
		`INSERT INTO invites (code_hash, label, max_uses, use_count, status, expires_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?, ?)`,
		inv.CodeHash, inv.Label, inv.MaxUses, InviteActive, inv.ExpiresAt, inv.CreatedAt,
	); err != nil {
		return "", nil, fmt.Errorf("写入邀请码失败: %w", err)
	}
	return code, inv, nil
}

// effectiveStatus 按当前时间与用量推导真实状态，不依赖定时任务。
func effectiveStatus(inv *Invite) string {
	if inv.Status == InviteRevoked {
		return InviteRevoked
	}
	if inv.ExpiresAt > 0 && time.Now().Unix() > inv.ExpiresAt {
		return InviteExpired
	}
	if inv.MaxUses > 0 && inv.UseCount >= inv.MaxUses {
		return InviteExhausted
	}
	return InviteActive
}

// GetInviteByCode 按明文码查询（内部换算成哈希）。
func GetInviteByCode(code string) (*Invite, error) {
	return GetInviteByHash(HashInviteCode(code))
}

// GetInviteByHash 按哈希查询单条邀请码。找不到返回 ErrInviteNotFound。
func GetInviteByHash(hash string) (*Invite, error) {
	row := DB.QueryRow(
		`SELECT code_hash, COALESCE(label,''), max_uses, use_count, status,
		        expires_at, created_at, first_used_at, last_used_at, COALESCE(used_by,'')
		 FROM invites WHERE code_hash = ?`, hash)
	inv, err := scanInvite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询邀请码失败: %w", err)
	}
	inv.Status = effectiveStatus(inv)
	return inv, nil
}

// ValidateInvite 校验邀请码当前可用，返回元数据。
// 它不消费名额 —— 「下载落地页/取配置包」这一步不该烧掉装机名额，
// 否则员工手滑刷新一次页面就得重新签发。
func ValidateInvite(code string) (*Invite, error) {
	inv, err := GetInviteByCode(code)
	if err != nil {
		return nil, err
	}
	switch inv.Status {
	case InviteRevoked:
		return nil, ErrInviteRevoked
	case InviteExpired:
		return nil, ErrInviteExpired
	case InviteExhausted:
		return nil, ErrInviteExhausted
	}
	return inv, nil
}

// ConsumeInvite 原子消费一次名额（代表「这台机器真的装好了」）。
//
// 并发安全靠「带条件的 UPDATE + RowsAffected」实现：
// WHERE 里同时约束状态、有效期与剩余次数，更新到多少行由数据库判定，
// 不需要在应用层加锁，多实例部署也不会超发。
func ConsumeInvite(code, usedBy string) (*Invite, error) {
	hash := HashInviteCode(code)
	now := time.Now()

	res, err := DB.Exec(
		`UPDATE invites
		    SET use_count     = use_count + 1,
		        last_used_at  = ?,
		        first_used_at = CASE WHEN first_used_at = 0 THEN ? ELSE first_used_at END,
		        used_by       = ?
		  WHERE code_hash  = ?
		    AND status      = 'active'
		    AND (expires_at = 0 OR expires_at >= ?)
		    AND (max_uses   = 0 OR use_count < max_uses)`,
		now.Unix(), now.Unix(), usedBy, hash, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("消费邀请码失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// 更新不到行 → 一定有个具体原因，回查一次把话说清楚，
		// 否则运维只会看到一句「失败」，还得反复试。
		inv, lookupErr := GetInviteByCode(code)
		if lookupErr != nil {
			return nil, lookupErr
		}
		switch inv.Status {
		case InviteRevoked:
			return nil, ErrInviteRevoked
		case InviteExpired:
			return nil, ErrInviteExpired
		case InviteExhausted:
			return nil, ErrInviteExhausted
		}
		return nil, ErrInviteNotFound
	}

	// 名额刚好用完时顺手落一次终态，便于列表页直接读字段即可。
	if _, err := DB.Exec(
		`UPDATE invites SET status = 'exhausted'
		  WHERE code_hash = ? AND status = 'active' AND max_uses > 0 AND use_count >= max_uses`,
		hash); err != nil {
		// 状态收尾失败不影响本次消费结果，仅记录不阻断。
		fmt.Printf("[邀请码] 收尾状态更新失败（不影响本次入库）: %v\n", err)
	}

	return GetInviteByCode(code)
}

// ListInvites 返回全部邀请码，最近签发的在前。
func ListInvites() ([]Invite, error) {
	rows, err := DB.Query(
		`SELECT code_hash, COALESCE(label,''), max_uses, use_count, status,
		        expires_at, created_at, first_used_at, last_used_at, COALESCE(used_by,'')
		 FROM invites ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil, fmt.Errorf("查询邀请码列表失败: %w", err)
	}
	defer rows.Close()

	list := make([]Invite, 0)
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			continue
		}
		inv.Status = effectiveStatus(inv)
		list = append(list, *inv)
	}
	return list, rows.Err()
}

// RevokeInvite 作废邀请码。selector 可以是明文码、完整哈希或哈希前缀。
// 返回实际作废的条数（前缀匹配时可能多于 1 条，正常为 1）。
func RevokeInvite(selector string) (int64, error) {
	sel := strings.TrimSpace(selector)
	if sel == "" {
		return 0, ErrInviteNotFound
	}

	var res sql.Result
	var err error
	switch {
	case len(sel) == 64 && isHex(sel):
		res, err = DB.Exec(`UPDATE invites SET status='revoked' WHERE code_hash = ? AND status != 'revoked'`, strings.ToLower(sel))
	case len(NormalizeInviteCode(sel)) == inviteLen:
		res, err = DB.Exec(`UPDATE invites SET status='revoked' WHERE code_hash = ? AND status != 'revoked'`, HashInviteCode(sel))
	default:
		// 当作哈希前缀处理；% 与 _ 会被 LIKE 当通配符，这里只接受纯十六进制，天然免疫。
		if !isHex(sel) {
			return 0, ErrInviteNotFound
		}
		res, err = DB.Exec(`UPDATE invites SET status='revoked' WHERE code_hash LIKE ? AND status != 'revoked'`, strings.ToLower(sel)+"%")
	}
	if err != nil {
		return 0, fmt.Errorf("作废邀请码失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrInviteNotFound
	}
	return n, nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return len(s) > 0
}

// RecordEnrollment 写入一条入网自检记录。
// codeMasked 只存打码后的码，避免记录里出现可用的明文凭证。
func RecordEnrollment(codeMasked, codeLabel, clientID, hostname, osName, ip, steps string, ok bool) error {
	okInt := 0
	if ok {
		okInt = 1
	}
	_, err := DB.Exec(
		`INSERT INTO enrollments (code_hash, code_label, client_id, hostname, os, ip_address, ok, steps, created_at)
		 VALUES ('', ?, ?, ?, ?, ?, ?, ?, ?)`,
		codeLabel, clientID, hostname, osName, ip, okInt, steps, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("写入入网记录失败: %w", err)
	}
	return nil
}

// ListEnrollments 返回最近的入网自检记录。
func ListEnrollments(limit int) ([]Enrollment, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := DB.Query(
		`SELECT id, COALESCE(code_label,''), COALESCE(client_id,''), COALESCE(hostname,''),
		        COALESCE(os,''), COALESCE(ip_address,''), ok, COALESCE(steps,''), created_at
		 FROM enrollments ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询入网记录失败: %w", err)
	}
	defer rows.Close()

	list := make([]Enrollment, 0)
	for rows.Next() {
		var e Enrollment
		var okInt int
		if err := rows.Scan(&e.ID, &e.CodeLabel, &e.ClientID, &e.Hostname, &e.OS,
			&e.IPAddress, &okInt, &e.Steps, &e.CreatedAt); err != nil {
			continue
		}
		e.OK = okInt == 1
		list = append(list, e)
	}
	return list, rows.Err()
}

// CleanupInvites 清理超过保留期的邀请码与入网记录，返回删除的总条数。
// 与审计日志一致地随滚动清理执行，避免这两张表无限增长。
func CleanupInvites(retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention).Unix()
	var total int64

	for _, q := range []string{
		`DELETE FROM invites WHERE created_at < ?`,
		`DELETE FROM enrollments WHERE created_at < ?`,
	} {
		res, err := DB.Exec(q, cutoff)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的公共 Scan。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanInvite 从一行结果扫描出 Invite。
func scanInvite(s rowScanner) (*Invite, error) {
	var inv Invite
	if err := s.Scan(&inv.CodeHash, &inv.Label, &inv.MaxUses, &inv.UseCount, &inv.Status,
		&inv.ExpiresAt, &inv.CreatedAt, &inv.FirstUsedAt, &inv.LastUsedAt, &inv.UsedBy); err != nil {
		return nil, err
	}
	return &inv, nil
}
