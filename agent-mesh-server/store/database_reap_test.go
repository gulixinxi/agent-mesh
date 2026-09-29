package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestReapTimedOutTasks 验证僵尸 running 任务的回收规则：
// 超时且还有重试余量的退回 pending，次数耗尽的判死，未超时的和未领取的都不动。
func TestReapTimedOutTasks(t *testing.T) {
	dir := t.TempDir()
	if err := InitDB(filepath.Join(dir, "reap.db")); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	defer DB.Close()

	now := time.Now().Unix()
	past := now - 60
	future := now + 3600

	insert := func(id, status string, attempts, maxAttempts, timeoutAt int64) {
		if _, err := DB.Exec(
			`INSERT INTO tasks (task_id, prompt, status, created_at, updated_at, attempts, max_attempts, timeout_at, claim_token, claimed_by)
			 VALUES (?, 'prompt', ?, ?, ?, ?, ?, ?, ?, 'node-A')`,
			id, status, now, now, attempts, maxAttempts, timeoutAt, "tok-"+id); err != nil {
			t.Fatalf("插入 %s 失败: %v", id, err)
		}
	}

	insert("t-requeue", "running", 1, 3, past)   // 超时 + 有余量 -> 退回 pending
	insert("t-dead", "running", 3, 3, past)      // 超时 + 次数耗尽 -> failed
	insert("t-running", "running", 1, 3, future) // 没超时 -> 保持 running
	insert("t-pending", "pending", 0, 3, 0)      // 未领取 -> 不受影响

	requeued, dead, err := ReapTimedOutTasks()
	if err != nil {
		t.Fatalf("回收超时任务失败: %v", err)
	}
	if requeued != 1 {
		t.Errorf("重投数 = %d，期望 1", requeued)
	}
	if dead != 1 {
		t.Errorf("判死数 = %d，期望 1", dead)
	}

	statusOf := func(id string) string {
		var s string
		if err := DB.QueryRow(`SELECT status FROM tasks WHERE task_id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("查询 %s 失败: %v", id, err)
		}
		return s
	}

	if got := statusOf("t-requeue"); got != "pending" {
		t.Errorf("t-requeue 状态 = %s，期望 pending", got)
	}
	if got := statusOf("t-dead"); got != "failed" {
		t.Errorf("t-dead 状态 = %s，期望 failed", got)
	}
	if got := statusOf("t-running"); got != "running" {
		t.Errorf("t-running 状态 = %s，期望保持 running", got)
	}
	if got := statusOf("t-pending"); got != "pending" {
		t.Errorf("t-pending 状态 = %s，期望保持 pending", got)
	}

	// 重投后的任务必须换发新的认领凭据：旧凭据作废，
	// 上一轮那个卡死的节点迟到的回传才会被拒，而不是覆盖新一轮的结果。
	var token, claimer string
	if err := DB.QueryRow(`SELECT COALESCE(claim_token,''), COALESCE(claimed_by,'') FROM tasks WHERE task_id = ?`,
		"t-requeue").Scan(&token, &claimer); err != nil {
		t.Fatalf("查询认领凭据失败: %v", err)
	}
	if token != "" {
		t.Errorf("重投后认领凭据应清空，实际 = %q", token)
	}
	if claimer != "" {
		t.Errorf("重投后领取节点应清空，实际 = %q", claimer)
	}

	// 幂等：再跑一次不应重复计数。
	requeued2, dead2, err := ReapTimedOutTasks()
	if err != nil {
		t.Fatalf("二次回收失败: %v", err)
	}
	if requeued2 != 0 || dead2 != 0 {
		t.Errorf("二次回收应为空操作，实际 requeued=%d dead=%d", requeued2, dead2)
	}
}
