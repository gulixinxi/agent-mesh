package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// do 发起一次测试请求，返回状态码与解析后的响应体。
func do(t *testing.T, r *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body != nil {
		buf, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestTaskClaimLifecycle 覆盖任务从下发到判死的完整可靠性链路：
// 认领凭据、重复回传被拒、超时重投、旧凭据作废、重试耗尽判死。
func TestTaskClaimLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	if err := store.InitDB(filepath.Join(dir, "dispatch.db")); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	defer store.DB.Close()

	// 把超时压到 1 秒，好在测试里观察完整的重投过程。
	TaskTimeout = 1 * time.Second

	r := gin.New()
	r.POST("/tasks/create", CreateTask)
	r.GET("/tasks/pending", GetPendingTasks)
	r.POST("/tasks/result", ReportTaskResult)
	r.GET("/tasks", ListTasks)

	// 1. 下发任务
	code, res := do(t, r, http.MethodPost, "/tasks/create", map[string]any{"prompt": "可靠性测试指令"})
	if code != http.StatusOK {
		t.Fatalf("下发任务失败: %d %v", code, res)
	}
	taskID, _ := res["task_id"].(string)
	if taskID == "" {
		t.Fatalf("下发任务未返回 task_id: %v", res)
	}

	// 2. 节点领取，必须拿到认领凭据
	code, res = do(t, r, http.MethodGet, "/tasks/pending?node=node-A", nil)
	if code != http.StatusOK {
		t.Fatalf("领取任务失败: %d %v", code, res)
	}
	tasks, _ := res["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("领取到 %d 条任务，期望 1 条", len(tasks))
	}
	first := tasks[0].(map[string]any)
	token1, _ := first["claim_token"].(string)
	if token1 == "" {
		t.Fatalf("领取结果缺少 claim_token: %v", first)
	}
	if attempts, _ := first["attempts"].(float64); attempts != 1 {
		t.Errorf("首次领取 attempts = %v，期望 1", attempts)
	}

	// 3. 用假凭据回传必须被拒
	code, _ = do(t, r, http.MethodPost, "/tasks/result", map[string]any{
		"task_id": taskID, "claim_token": "forged", "status": "completed", "result": "ok",
	})
	if code != http.StatusConflict {
		t.Errorf("伪造凭据回传应返回 409，实际 %d", code)
	}

	// 4. 节点宕机：超时后由回收逻辑退回待派发
	time.Sleep(1200 * time.Millisecond)
	requeued, dead, err := store.ReapTimedOutTasks()
	if err != nil {
		t.Fatalf("回收超时任务失败: %v", err)
	}
	if requeued != 1 || dead != 0 {
		t.Fatalf("回收结果不符预期: requeued=%d dead=%d", requeued, dead)
	}

	// 5. 上一轮那个卡死节点的迟到回传，必须因凭据失效被拒
	code, _ = do(t, r, http.MethodPost, "/tasks/result", map[string]any{
		"task_id": taskID, "claim_token": token1, "status": "completed", "result": "迟到的结果",
	})
	if code != http.StatusConflict {
		t.Errorf("旧凭据回传应返回 409，实际 %d", code)
	}

	// 6. 重新领取：次数累加，且换发新凭据
	code, res = do(t, r, http.MethodGet, "/tasks/pending?node=node-B", nil)
	if code != http.StatusOK {
		t.Fatalf("二次领取失败: %d %v", code, res)
	}
	tasks, _ = res["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("二次领取到 %d 条任务，期望 1 条", len(tasks))
	}
	second := tasks[0].(map[string]any)
	token2, _ := second["claim_token"].(string)
	if token2 == "" || token2 == token1 {
		t.Fatalf("重投后应换发新凭据，旧=%q 新=%q", token1, token2)
	}
	if attempts, _ := second["attempts"].(float64); attempts != 2 {
		t.Errorf("二次领取 attempts = %v，期望 2", attempts)
	}

	// 7. 节点主动报超时：还有余量，应退回待派发而不是判死
	code, res = do(t, r, http.MethodPost, "/tasks/result", map[string]any{
		"task_id": taskID, "claim_token": token2, "status": "timeout",
	})
	if code != http.StatusOK || res["status"] != "requeued" {
		t.Fatalf("报超时应重投，实际 %d %v", code, res)
	}

	// 8. 第三次领取后报超时：次数耗尽，判死
	code, res = do(t, r, http.MethodGet, "/tasks/pending?node=node-C", nil)
	if code != http.StatusOK {
		t.Fatalf("三次领取失败: %d %v", code, res)
	}
	tasks, _ = res["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("三次领取到 %d 条任务，期望 1 条", len(tasks))
	}
	token3 := tasks[0].(map[string]any)["claim_token"].(string)

	code, res = do(t, r, http.MethodPost, "/tasks/result", map[string]any{
		"task_id": taskID, "claim_token": token3, "status": "timeout",
	})
	if code != http.StatusOK || res["status"] != "dead" {
		t.Fatalf("重试耗尽应判死，实际 %d %v", code, res)
	}

	// 9. 判死后不应再出现在待派发列表里
	code, res = do(t, r, http.MethodGet, "/tasks/pending?node=node-D", nil)
	if code != http.StatusOK {
		t.Fatalf("查询待派发失败: %d %v", code, res)
	}
	if count, _ := res["count"].(float64); count != 0 {
		t.Errorf("判死后待派发数 = %v，期望 0", count)
	}

	// 10. 正常完成的路径依然可用
	code, res = do(t, r, http.MethodPost, "/tasks/create", map[string]any{"prompt": "正常任务"})
	if code != http.StatusOK {
		t.Fatalf("下发正常任务失败: %d %v", code, res)
	}
	okID, _ := res["task_id"].(string)
	_, res = do(t, r, http.MethodGet, "/tasks/pending?node=node-A", nil)
	tasks, _ = res["tasks"].([]any)
	tokenOK := tasks[0].(map[string]any)["claim_token"].(string)
	code, res = do(t, r, http.MethodPost, "/tasks/result", map[string]any{
		"task_id": okID, "claim_token": tokenOK, "status": "completed", "result": "done",
	})
	if code != http.StatusOK || res["status"] != "result accepted" {
		t.Fatalf("正常回传应被接受，实际 %d %v", code, res)
	}
}
