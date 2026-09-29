package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 中转文件接口的端到端单测
//
// 覆盖三条最容易出事、且只在真实请求里才暴露的路径：
//   1. 签名只覆盖「声明的摘要」，实体是否相符必须另行校验 —— 否则签名形同虚设；
//   2. 落盘用的是随机 ID，文件名是不可信输入，必须归一化后再进响应头；
//   3. 定向投递的文件不能被其它节点看到或取走。
// =====================================================================

const testSecret = "test-secret"

// newFileTestRouter 搭一套最小可用的中转路由：真实 SQLite + 临时存储目录。
func newFileTestRouter(t *testing.T) *gin.Engine {
	t.Helper()

	dir := t.TempDir()
	if err := store.InitDB(filepath.Join(dir, "files.db")); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}

	FileStoreDir = filepath.Join(dir, "store")
	if err := os.MkdirAll(FileStoreDir, 0o755); err != nil {
		t.Fatalf("创建存储目录失败: %v", err)
	}

	t.Cleanup(func() {
		store.DB.Close()
		FileStoreDir = ""
	})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 与 main.go 的注册方式保持一致：上传走独立鉴权，其余在 /api/v1 组内。
	r.POST(FileUploadPath, FileUploadAuthMiddleware(testSecret), UploadFile)
	g := r.Group("/api/v1")
	g.GET("/files", ListFiles)
	g.GET("/files/download", DownloadFile)
	g.POST("/files/delete", DeleteFile)
	return r
}

// uploadReq 组装并发送一次签名上传。body 与 declaredDigest 分开传，
// 是为了能构造出「签名合法但实体被替换」这种中间人场景。
func uploadReq(t *testing.T, r *gin.Engine, name, target string, body []byte, declaredDigest, signature string) *httptest.ResponseRecorder {
	t.Helper()

	if declaredDigest == "" {
		sum := sha256.Sum256(body)
		declaredDigest = hex.EncodeToString(sum[:])
	}
	// 时间戳与 nonce 始终照常携带：这样「签名错误」用例命中的是 403 分支，
	// 而不是因为缺头掉进 401，混淆了被测行为。
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := fmt.Sprintf("n-%d", time.Now().UnixNano())
	if signature == "" {
		signature = ComputeSignatureWithDigest(testSecret, http.MethodPost, FileUploadPath, ts, nonce, declaredDigest)
	}

	req := httptest.NewRequest(http.MethodPost, FileUploadPath, bytes.NewReader(body))
	req.Header.Set(HeaderFileName, url.QueryEscape(name))
	req.Header.Set(HeaderContentSHA256, declaredDigest)
	req.Header.Set(HeaderFileUploader, url.QueryEscape("NODE-A"))
	if target != "" {
		req.Header.Set(HeaderFileTarget, url.QueryEscape(target))
	}
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, signature)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeMeta(t *testing.T, w *httptest.ResponseRecorder) store.FileMeta {
	t.Helper()
	var m store.FileMeta
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析上传响应失败: %v (body=%s)", err, w.Body.String())
	}
	return m
}

// TestFileUploadDownloadRoundTrip 打通「上传 -> 按节点可见 -> 下载 -> 删除」闭环。
func TestFileUploadDownloadRoundTrip(t *testing.T) {
	r := newFileTestRouter(t)

	content := []byte("这是一份任务产出的报表内容-report-v1")
	w := uploadReq(t, r, "月度报表.xlsx", "NODE-B", content, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("上传应成功，实际 %d: %s", w.Code, w.Body.String())
	}
	meta := decodeMeta(t, w)
	if meta.FileID == "" || meta.Size != int64(len(content)) {
		t.Fatalf("上传响应异常: %+v", meta)
	}
	if meta.FileName != "月度报表.xlsx" {
		t.Errorf("文件名 = %q，期望 %q", meta.FileName, "月度报表.xlsx")
	}

	// 目标节点可见，其它节点不可见 —— 定向投递的基本要求。
	if got := listFor(t, r, "NODE-B", false); len(got) != 1 {
		t.Errorf("NODE-B 应看到 1 个文件，实际 %d", len(got))
	}
	if got := listFor(t, r, "NODE-C", false); len(got) != 0 {
		t.Errorf("NODE-C 不应看到定向给 NODE-B 的文件，实际看到 %d 个", len(got))
	}

	// 目标节点可下载，内容与摘要都要对得上。
	dl := download(t, r, meta.FileID, "NODE-B")
	if dl.Code != http.StatusOK {
		t.Fatalf("下载应成功，实际 %d: %s", dl.Code, dl.Body.String())
	}
	if !bytes.Equal(dl.Body.Bytes(), content) {
		t.Error("下载内容与原文件不一致")
	}
	if got := dl.Header().Get("X-Mesh-File-SHA256"); got != meta.SHA256 {
		t.Errorf("响应头摘要 = %q，期望 %q", got, meta.SHA256)
	}
	// 文件名含中文，必须经 RFC5987 编码，不能是裸字节。
	if cd := dl.Header().Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''") {
		t.Errorf("Content-Disposition 未按 RFC5987 编码: %q", cd)
	}

	// 领取后应被标记，不再出现在待领取列表里。
	if got := listFor(t, r, "NODE-B", true); len(got) != 0 {
		t.Errorf("领取后待领取列表应为空，实际 %d", len(got))
	}

	// 越权下载：定向给 NODE-B 的文件，NODE-C 不能取。
	if dl := download(t, r, meta.FileID, "NODE-C"); dl.Code != http.StatusForbidden {
		t.Errorf("越权下载应 403，实际 %d", dl.Code)
	}

	// 删除后实体与元数据都应消失。
	if w := postJSON(t, r, "/api/v1/files/delete", map[string]string{"file_id": meta.FileID}); w.Code != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d: %s", w.Code, w.Body.String())
	}
	if dl := download(t, r, meta.FileID, "NODE-B"); dl.Code != http.StatusNotFound {
		t.Errorf("删除后下载应 404，实际 %d", dl.Code)
	}
	if _, err := os.Stat(filepath.Join(FileStoreDir, meta.FileID)); !os.IsNotExist(err) {
		t.Error("删除后磁盘实体仍存在")
	}
}

// TestFileUploadRejectsTamperedBody 是这套设计里最关键的一条：
// 签名只覆盖客户端声明的摘要，因此服务端必须自己边收边算、比对实体。
// 少了这步，中间人替换传输内容不会被发现。
func TestFileUploadRejectsTamperedBody(t *testing.T) {
	r := newFileTestRouter(t)

	original := []byte("原始内容")
	sum := sha256.Sum256(original)
	declared := hex.EncodeToString(sum[:])

	// 用原始内容的摘要去签名，却发送另一份内容 —— 模拟传输中被替换。
	tampered := []byte("被中间人替换过的内容")
	w := uploadReq(t, r, "a.txt", "", tampered, declared, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("摘要与实体不一致时应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "内容校验和不匹配") {
		t.Errorf("错误信息应点明摘要不匹配，实际: %s", w.Body.String())
	}

	// 失败的上传不能在磁盘上留下任何残留。
	assertStoreEmpty(t)
}

// TestFileUploadAuth 覆盖上传专用鉴权的三个拒绝分支。
func TestFileUploadAuth(t *testing.T) {
	r := newFileTestRouter(t)
	body := []byte("hello")

	t.Run("签名错误被拒", func(t *testing.T) {
		w := uploadReq(t, r, "a.txt", "", body, "", "deadbeef")
		if w.Code != http.StatusForbidden {
			t.Errorf("错误签名应 403，实际 %d", w.Code)
		}
	})

	t.Run("缺少摘要头被拒", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, FileUploadPath, bytes.NewReader(body))
		req.Header.Set(HeaderFileName, url.QueryEscape("a.txt"))
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
		req.Header.Set(HeaderNonce, "n-missing-digest")
		req.Header.Set(HeaderSignature, "whatever")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("缺摘要头应 401，实际 %d", w.Code)
		}
	})

	t.Run("时间戳偏移过大被拒", func(t *testing.T) {
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		ts := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
		nonce := "n-skewed"
		sig := ComputeSignatureWithDigest(testSecret, http.MethodPost, FileUploadPath, ts, nonce, digest)

		req := httptest.NewRequest(http.MethodPost, FileUploadPath, bytes.NewReader(body))
		req.Header.Set(HeaderFileName, url.QueryEscape("a.txt"))
		req.Header.Set(HeaderContentSHA256, digest)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderNonce, nonce)
		req.Header.Set(HeaderSignature, sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("过期时间戳应 401，实际 %d", w.Code)
		}
	})

	t.Run("nonce 重放被拒", func(t *testing.T) {
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := fmt.Sprintf("n-replay-%d", time.Now().UnixNano())
		sig := ComputeSignatureWithDigest(testSecret, http.MethodPost, FileUploadPath, ts, nonce, digest)

		send := func() int {
			req := httptest.NewRequest(http.MethodPost, FileUploadPath, bytes.NewReader(body))
			req.Header.Set(HeaderFileName, url.QueryEscape("r.txt"))
			req.Header.Set(HeaderContentSHA256, digest)
			req.Header.Set(HeaderTimestamp, ts)
			req.Header.Set(HeaderNonce, nonce)
			req.Header.Set(HeaderSignature, sig)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w.Code
		}

		if code := send(); code != http.StatusOK {
			t.Fatalf("首次上传应成功，实际 %d", code)
		}
		if code := send(); code != http.StatusUnauthorized {
			t.Errorf("重放同一 nonce 应 401，实际 %d", code)
		}
	})
}

// TestFileUploadRejectsOversize 声明的长度超限时直接 413，不落盘。
func TestFileUploadRejectsOversize(t *testing.T) {
	r := newFileTestRouter(t)

	body := []byte("small")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n-oversize"

	req := httptest.NewRequest(http.MethodPost, FileUploadPath, bytes.NewReader(body))
	req.ContentLength = MaxFileBytes + 1 // 伪造一个超限的声明长度
	req.Header.Set(HeaderFileName, url.QueryEscape("big.bin"))
	req.Header.Set(HeaderContentSHA256, digest)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, ComputeSignatureWithDigest(testSecret, http.MethodPost, FileUploadPath, ts, nonce, digest))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限上传应 413，实际 %d: %s", w.Code, w.Body.String())
	}
	assertStoreEmpty(t)
}

// TestSanitizeFileName 固化文件名归一化规则。
// 落盘用的是随机 ID，所以这里的重点是「不把路径成分带进响应头与展示」。
func TestSanitizeFileName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"report.xlsx", "report.xlsx", false},
		{"月度报表.xlsx", "月度报表.xlsx", false},
		{"../../etc/passwd", "passwd", false},   // 收敛为纯文件名
		{`..\..\windows\system32\a.dll`, "a.dll", false},
		{"a/b/c.txt", "c.txt", false},
		{"..", "", true},
		{"", "", true},
		{"  ", "", true},
		{"..hidden", "", true}, // 含 ".." 一律拒绝，宁可严一点
		{strings.Repeat("x", 256), "", true},
	}
	for _, tc := range cases {
		got, err := sanitizeFileName(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("sanitizeFileName(%q) 应报错，实际得到 %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("sanitizeFileName(%q) 意外报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("sanitizeFileName(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestFileVisibleToAllWhenBroadcast 广播文件（未指定目标）对所有节点可见。
func TestFileVisibleToAllWhenBroadcast(t *testing.T) {
	r := newFileTestRouter(t)

	w := uploadReq(t, r, "公告.pdf", "", []byte("broadcast"), "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("广播上传应成功，实际 %d: %s", w.Code, w.Body.String())
	}
	meta := decodeMeta(t, w)

	for _, node := range []string{"NODE-A", "NODE-B", "NODE-C"} {
		if got := listFor(t, r, node, false); len(got) != 1 {
			t.Errorf("广播文件应对 %s 可见，实际看到 %d 个", node, len(got))
		}
	}
	// 任何节点都能取广播文件。
	if dl := download(t, r, meta.FileID, "NODE-C"); dl.Code != http.StatusOK {
		t.Errorf("广播文件应可被任意节点下载，实际 %d", dl.Code)
	}
}

// TestUploadDisabledWithoutDir 未配置存储目录时上传返回 503 而不是静默丢文件。
func TestUploadDisabledWithoutDir(t *testing.T) {
	r := newFileTestRouter(t)
	saved := FileStoreDir
	FileStoreDir = ""
	defer func() { FileStoreDir = saved }()

	w := uploadReq(t, r, "a.txt", "", []byte("x"), "", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("未启用存储时应 503，实际 %d", w.Code)
	}
}

// --- 测试辅助 ---

func listFor(t *testing.T, r *gin.Engine, node string, pendingOnly bool) []store.FileMeta {
	t.Helper()
	q := "/api/v1/files?node=" + url.QueryEscape(node)
	if pendingOnly {
		q += "&pending=1"
	}
	req := httptest.NewRequest(http.MethodGet, q, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("列表请求失败 %d: %s", w.Code, w.Body.String())
	}
	var list []store.FileMeta
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	return list
}

func download(t *testing.T, r *gin.Engine, fileID, node string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/files/download?file_id="+url.QueryEscape(fileID)+"&node="+url.QueryEscape(node), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postJSON(t *testing.T, r *gin.Engine, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertStoreEmpty 确认存储目录里除了 tmp 子目录没有任何残留文件。
func assertStoreEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(FileStoreDir)
	if err != nil {
		t.Fatalf("读取存储目录失败: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "tmp" {
			continue
		}
		t.Errorf("存储目录不应有残留实体: %s", e.Name())
	}
}
