package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newInviteDB 为每个用例开独立临时库。
func newInviteDB(t *testing.T) {
	t.Helper()
	if err := InitDB(filepath.Join(t.TempDir(), "invites.db")); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	t.Cleanup(func() { DB.Close() })
}

// TestInviteCodeFormat 验证码格式与字符集：16 位、四组、不含易混字符。
func TestInviteCodeFormat(t *testing.T) {
	newInviteDB(t)

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		code, err := NewInviteCode()
		if err != nil {
			t.Fatalf("生成邀请码失败: %v", err)
		}
		normalized := NormalizeInviteCode(code)
		if len(normalized) != inviteLen {
			t.Fatalf("邀请码长度应为 %d，实际 %d（%s）", inviteLen, len(normalized), code)
		}
		if len(code) != inviteLen+3 {
			t.Fatalf("邀请码应带 3 个连字符，实际 %q", code)
		}
		for _, r := range normalized {
			if r == '0' || r == 'O' || r == '1' || r == 'I' || r == 'L' {
				t.Fatalf("邀请码含易混字符 %q: %s", r, code)
			}
		}
		if seen[normalized] {
			t.Fatalf("出现重复邀请码: %s", normalized)
		}
		seen[normalized] = true
	}
}

// TestNormalizeInviteCode 验证大小写与分隔符归一化。
func TestNormalizeInviteCode(t *testing.T) {
	cases := map[string]string{
		"abcd-efgh-jkmp-qrst": "ABCDEFGHJKMPQRST",
		" ABCD EFGH JKMP QRST ": "ABCDEFGHJKMPQRST",
		"abcdefghjkmpqrst":      "ABCDEFGHJKMPQRST",
	}
	for in, want := range cases {
		if got := NormalizeInviteCode(in); got != want {
			t.Errorf("归一化 %q 得到 %q，期望 %q", in, got, want)
		}
	}
}

// TestInviteCreateAndValidate 验证签发后可校验，且校验不消费名额。
func TestInviteCreateAndValidate(t *testing.T) {
	newInviteDB(t)

	code, inv, err := CreateInvite("前台工位", 30*time.Minute, 1)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if inv.Status != InviteActive {
		t.Fatalf("新签发应为 active，实际 %s", inv.Status)
	}

	// 校验多次都不应消费名额 —— 反复打开落地页不该烧掉装机名额。
	for i := 0; i < 3; i++ {
		got, err := ValidateInvite(code)
		if err != nil {
			t.Fatalf("第 %d 次校验失败: %v", i+1, err)
		}
		if got.UseCount != 0 {
			t.Fatalf("校验不应消费名额，实际 use_count=%d", got.UseCount)
		}
	}

	// 大小写与分隔符不同也应命中同一条。
	if _, err := ValidateInvite(NormalizeInviteCode(code)); err != nil {
		t.Fatalf("归一化后校验失败: %v", err)
	}
}

// TestInviteConsumeExhausts 验证一码一机：消费一次后即枯竭。
func TestInviteConsumeExhausts(t *testing.T) {
	newInviteDB(t)

	code, _, err := CreateInvite("", time.Hour, 1)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	inv, err := ConsumeInvite(code, "NODE-A")
	if err != nil {
		t.Fatalf("首次消费失败: %v", err)
	}
	if inv.UseCount != 1 {
		t.Fatalf("首次消费后 use_count 应为 1，实际 %d", inv.UseCount)
	}
	if inv.Status != InviteExhausted {
		t.Fatalf("名额用尽后状态应为 exhausted，实际 %s", inv.Status)
	}
	if inv.UsedBy != "NODE-A" {
		t.Fatalf("应记录使用者，实际 %q", inv.UsedBy)
	}

	if _, err := ConsumeInvite(code, "NODE-B"); !errors.Is(err, ErrInviteExhausted) {
		t.Fatalf("二次消费应报枯竭，实际 %v", err)
	}
	if _, err := ValidateInvite(code); !errors.Is(err, ErrInviteExhausted) {
		t.Fatalf("枯竭后校验应报枯竭，实际 %v", err)
	}
}

// TestInviteUnlimitedUses 验证最大次数为 0 时不限次数。
func TestInviteUnlimitedUses(t *testing.T) {
	newInviteDB(t)

	code, _, err := CreateInvite("机房批量", time.Hour, 0)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := ConsumeInvite(code, "NODE"); err != nil {
			t.Fatalf("第 %d 次消费失败: %v", i+1, err)
		}
	}
	inv, err := GetInviteByCode(code)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if inv.UseCount != 5 {
		t.Fatalf("应累计 5 次，实际 %d", inv.UseCount)
	}
	if inv.Status != InviteActive {
		t.Fatalf("不限次数的码应保持 active，实际 %s", inv.Status)
	}
}

// TestInviteExpired 验证过期码不可校验、不可消费。
func TestInviteExpired(t *testing.T) {
	newInviteDB(t)

	code, _, err := CreateInvite("", time.Hour, 1)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	// 直接把过期时间改到过去，避免用例真的等一小时。
	if _, err := DB.Exec(
		`UPDATE invites SET expires_at = ? WHERE code_hash = ?`,
		time.Now().Add(-time.Minute).Unix(), HashInviteCode(code)); err != nil {
		t.Fatalf("改写过期时间失败: %v", err)
	}

	if _, err := ValidateInvite(code); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("过期码校验应报过期，实际 %v", err)
	}
	if _, err := ConsumeInvite(code, "NODE"); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("过期码消费应报过期，实际 %v", err)
	}
	inv, err := GetInviteByCode(code)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if inv.Status != InviteExpired {
		t.Fatalf("状态应为 expired，实际 %s", inv.Status)
	}
}

// TestInviteRevoke 验证作废：明文码、完整哈希、哈希前缀三种写法都能命中。
func TestInviteRevoke(t *testing.T) {
	newInviteDB(t)

	code, inv, err := CreateInvite("", time.Hour, 1)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	// 前缀写法（控制台列表里显示的就是它）
	n, err := RevokeInvite(inv.ID())
	if err != nil {
		t.Fatalf("按前缀作废失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应作废 1 条，实际 %d", n)
	}
	if _, err := ValidateInvite(code); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("作废后校验应报作废，实际 %v", err)
	}

	// 已作废的码再作废应报找不到（RowsAffected=0）。
	if _, err := RevokeInvite(code); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("重复作废应报找不到，实际 %v", err)
	}

	// 明文与完整哈希两条路径也各验一次
	code2, inv2, _ := CreateInvite("", time.Hour, 1)
	if _, err := RevokeInvite(HashInviteCode(code2)); err != nil {
		t.Fatalf("按完整哈希作废失败: %v", err)
	}
	if _, err := GetInviteByHash(inv2.CodeHash); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
}

// TestInviteNotFound 验证不存在的码给出明确错误。
func TestInviteNotFound(t *testing.T) {
	newInviteDB(t)

	if _, err := ValidateInvite("ZZZZ-ZZZZ-ZZZZ-ZZZZ"); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("不存在的码应报未找到，实际 %v", err)
	}
	if _, err := RevokeInvite("ZZZZ-ZZZZ-ZZZZ-ZZZZ"); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("作废不存在的码应报未找到，实际 %v", err)
	}
}

// TestInvitePlaintextNotStored 验证库里不出现明文码，只存哈希。
func TestInvitePlaintextNotStored(t *testing.T) {
	newInviteDB(t)

	code, _, err := CreateInvite("审计", time.Hour, 1)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	var stored string
	if err := DB.QueryRow(`SELECT code_hash FROM invites LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("读取 code_hash 失败: %v", err)
	}
	if stored == code {
		t.Fatal("库里不应存明文邀请码")
	}
	if stored != HashInviteCode(code) {
		t.Fatalf("应存 sha256 哈希，实际 %q", stored)
	}
	if len(stored) != 64 {
		t.Fatalf("sha256 hex 长度应为 64，实际 %d", len(stored))
	}
}

// TestInviteListAndEnrollments 验证列表与入网记录的读写。
func TestInviteListAndEnrollments(t *testing.T) {
	newInviteDB(t)

	if _, _, err := CreateInvite("甲", time.Hour, 1); err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, _, err := CreateInvite("乙", time.Hour, 2); err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	list, err := ListInvites()
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条邀请码，实际 %d", len(list))
	}
	// 最近签发的在前
	if list[0].Label != "乙" {
		t.Fatalf("应按 created_at 倒序，首条为 %q", list[0].Label)
	}

	if err := RecordEnrollment("AB••OP", "前台", "NODE-A", "pc-1", "windows", "10.0.0.9", `[{"name":"heartbeat_ok","ok":true}]`, true); err != nil {
		t.Fatalf("写入入网记录失败: %v", err)
	}
	recs, err := ListEnrollments(10)
	if err != nil {
		t.Fatalf("读入网记录失败: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("应有 1 条入网记录，实际 %d", len(recs))
	}
	if !recs[0].OK || recs[0].ClientID != "NODE-A" || recs[0].Steps == "" {
		t.Fatalf("入网记录字段不符: %+v", recs[0])
	}
}

// TestCleanupInvites 验证按保留期清理两张表。
func TestCleanupInvites(t *testing.T) {
	newInviteDB(t)

	code, _, err := CreateInvite("旧", time.Hour, 1)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if err := RecordEnrollment("AB••OP", "旧", "NODE", "pc", "linux", "10.0.0.1", "[]", true); err != nil {
		t.Fatalf("写入入网记录失败: %v", err)
	}

	// 把创建时间挪到很久以前
	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := DB.Exec(`UPDATE invites SET created_at = ?`, old); err != nil {
		t.Fatalf("改时间失败: %v", err)
	}
	if _, err := DB.Exec(`UPDATE enrollments SET created_at = ?`, old); err != nil {
		t.Fatalf("改时间失败: %v", err)
	}

	n, err := CleanupInvites(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应清理 2 条，实际 %d", n)
	}
	if _, err := GetInviteByCode(code); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("清理后应查不到，实际 %v", err)
	}
	recs, _ := ListEnrollments(10)
	if len(recs) != 0 {
		t.Fatalf("清理后入网记录应为空，实际 %d", len(recs))
	}
}
