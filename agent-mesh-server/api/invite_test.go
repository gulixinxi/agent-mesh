package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// newInviteRouter 建一个与生产同构的路由表。
//
// 关键点：路由注册走的是 api.RegisterJoinRoutes / RegisterConsoleInviteRoutes，
// 与 main.go 里用的是同一份代码。若测试自己另抄一份路由，那测的就是抄件，不是生产件。
func newInviteRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	if err := store.InitDB(filepath.Join(t.TempDir(), "api-invite.db")); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })

	// 每个用例重置进程级上下文与限流器，避免互相污染。
	ClusterSecret = "test-cluster-secret"
	PublicBaseURL = ""
	ClientPackDir = ""
	TLSCAPEM = ""
	EnrollDefaultTTL = 30 * time.Minute
	joinLimiter = newIPLimiter(60, time.Minute)

	r := gin.New()
	RegisterJoinRoutes(r)
	RegisterConsoleInviteRoutes(r.Group("/console"))
	return r
}

// do 发一个请求并返回记录器。
func doJoin(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// issueInvite 通过控制台接口签发一条邀请码，返回明文码与响应体。
func issueInvite(t *testing.T, r *gin.Engine, label string, ttlMinutes int, maxUses *int) (string, map[string]any) {
	t.Helper()
	w := doJoin(r, http.MethodPost, "/console/api/invites", map[string]any{
		"label": label, "ttl_minutes": ttlMinutes, "max_uses": maxUses,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("签发邀请码失败: HTTP %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析签发响应失败: %v", err)
	}
	code, _ := resp["code"].(string)
	if code == "" {
		t.Fatal("签发响应里没有 code")
	}
	return code, resp
}

// decoded 把响应体解成 map。
func decoded(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析响应失败: %v（原文 %s）", err, w.Body.String())
	}
	return m
}

// TestJoinRoutesRegisteredAndNoConflict 验证路由形状成立。
//
// /join/:code 与 /join/:code/playbook.json 同时存在时，若路由树不支持
// 「参数段 + 静态子路径」共存，gin 会在注册阶段 panic。这个用例就是那道闸。
func TestJoinRoutesRegisteredAndNoConflict(t *testing.T) {
	r := newInviteRouter(t)

	// 不存在的码：必须是规整的错误码契约，而不是 404 空体或 panic。
	w := doJoin(r, http.MethodGet, "/join/ZZZZ-ZZZZ-ZZZZ-ZZZZ/playbook.json", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的码应返回 404，实际 %d", w.Code)
	}
	if got := decoded(t, w)["error"]; got != "invite_not_found" {
		t.Fatalf("错误码应为 invite_not_found，实际 %v", got)
	}
}

// TestJoinFullFlow 走完整条链路：签发 -> 落地页 -> 取配置包（消费）-> 自检回传。
func TestJoinFullFlow(t *testing.T) {
	r := newInviteRouter(t)

	code, issue := issueInvite(t, r, "前台工位", 30, nil)
	norm := strings.ReplaceAll(code, "-", "")

	if issue["client_ready"] != false {
		t.Fatal("未配置客户端目录时应标记 client_ready=false")
	}
	for _, key := range []string{"windows_cmd", "linux_cmd", "landing_url"} {
		if s, _ := issue[key].(string); !strings.Contains(s, norm) {
			t.Fatalf("%s 里应含邀请码，实际 %q", key, s)
		}
	}
	if s, _ := issue["windows_cmd"].(string); !strings.Contains(s, "install.ps1") {
		t.Fatalf("Windows 命令应指向 install.ps1，实际 %q", s)
	}

	// 1) 落地页：不消费名额
	w := doJoin(r, http.MethodGet, "/join/"+norm, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("落地页应 200，实际 %d", w.Code)
	}
	page := w.Body.String()
	if !strings.Contains(page, code) {
		t.Fatal("落地页应展示邀请码")
	}
	if !strings.Contains(page, "可用") {
		t.Fatal("落地页应显示「可用」状态")
	}

	// 2) 取配置包：这一次才消费名额
	w = doJoin(r, http.MethodGet, "/join/"+norm+"/playbook.json?client_id=NODE-A", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("取配置包应 200，实际 %d %s", w.Code, w.Body.String())
	}
	pb := decoded(t, w)
	if pb["secret"] != "test-cluster-secret" {
		t.Fatalf("配置包应含集群密钥，实际 %v", pb["secret"])
	}
	if pb["server_url"] == "" {
		t.Fatal("配置包应含服务端地址")
	}
	if left, _ := pb["remaining_uses"].(float64); left != 0 {
		t.Fatalf("一码一机消费后剩余应为 0，实际 %v", left)
	}
	checks, _ := pb["checks"].([]any)
	if len(checks) == 0 {
		t.Fatal("配置包应含自检项清单")
	}

	// 3) 同一台机器重取：不重复扣次数、也不该被拒
	w = doJoin(r, http.MethodGet, "/join/"+norm+"/playbook.json?client_id=NODE-A", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("同一台机器重取应放行，实际 %d %s", w.Code, w.Body.String())
	}

	// 4) 另一台机器再取：名额已尽，必须明确拒绝
	w = doJoin(r, http.MethodGet, "/join/"+norm+"/playbook.json?client_id=NODE-B", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("名额用尽应返回 409，实际 %d %s", w.Code, w.Body.String())
	}
	if got := decoded(t, w)["error"]; got != "invite_exhausted" {
		t.Fatalf("错误码应为 invite_exhausted，实际 %v", got)
	}

	// 5) 自检回传：即使码已用尽也必须允许，否则失败信息会静默丢失
	w = doJoin(r, http.MethodPost, "/join/"+norm+"/report", map[string]any{
		"client_id": "NODE-A", "hostname": "pc-1", "os": "windows",
		"ip_address": "10.0.0.9", "ok": true,
		"steps": []map[string]any{{"name": "heartbeat_ok", "ok": true}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("自检回传应 200，实际 %d %s", w.Code, w.Body.String())
	}

	// 6) 控制台的入网记录应能看到这条
	w = doJoin(r, http.MethodGet, "/console/api/enrollments", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("读入网记录应 200，实际 %d", w.Code)
	}
	var recs []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &recs); err != nil {
		t.Fatalf("解析入网记录失败: %v", err)
	}
	if len(recs) != 1 || recs[0]["client_id"] != "NODE-A" || recs[0]["ok"] != true {
		t.Fatalf("入网记录不符: %+v", recs)
	}

	// 7) 落地页此时应显示已用尽
	w = doJoin(r, http.MethodGet, "/join/"+norm, nil)
	if !strings.Contains(w.Body.String(), "已用尽") {
		t.Fatal("名额用尽后落地页应显示「已用尽」")
	}

	// 8) 控制台列表应能列出这一条，且只暴露哈希前缀
	w = doJoin(r, http.MethodGet, "/console/api/invites", nil)
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析邀请码列表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条邀请码，实际 %d", len(list))
	}
	if id, _ := list[0]["id"].(string); len(id) != 12 {
		t.Fatalf("列表应只暴露 12 位哈希前缀，实际 %q", id)
	}
	if _, leaked := list[0]["code"]; leaked {
		t.Fatal("列表不得返回明文邀请码")
	}
}

// TestJoinUnlimitedUses 验证不限次数的邀请码可以装多台。
func TestJoinUnlimitedUses(t *testing.T) {
	r := newInviteRouter(t)

	zero := 0
	code, _ := issueInvite(t, r, "机房批量", 60, &zero)
	norm := strings.ReplaceAll(code, "-", "")

	for i, node := range []string{"NODE-1", "NODE-2", "NODE-3"} {
		w := doJoin(r, http.MethodGet, "/join/"+norm+"/playbook.json?client_id="+node, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 台应可接入，实际 %d %s", i+1, w.Code, w.Body.String())
		}
		if left, _ := decoded(t, w)["remaining_uses"].(float64); left != 0 {
			t.Fatalf("不限次数时 remaining_uses 应为 0（表示不限），实际 %v", left)
		}
	}

	// 落地页应显示「不限」
	if !strings.Contains(doJoin(r, http.MethodGet, "/join/"+norm, nil).Body.String(), "不限") {
		t.Fatal("不限次数的码在落地页应显示「不限」")
	}
}

// TestJoinExpiredAndRevoked 验证过期与作废两条失效路径。
func TestJoinExpiredAndRevoked(t *testing.T) {
	r := newInviteRouter(t)

	// --- 过期 ---
	code, _ := issueInvite(t, r, "过期", 60, nil)
	norm := strings.ReplaceAll(code, "-", "")
	if _, err := store.DB.Exec(
		`UPDATE invites SET expires_at = ? WHERE code_hash = ?`,
		time.Now().Add(-time.Minute).Unix(), store.HashInviteCode(norm)); err != nil {
		t.Fatalf("改写过期时间失败: %v", err)
	}

	w := doJoin(r, http.MethodGet, "/join/"+norm+"/playbook.json", nil)
	if w.Code != http.StatusGone || decoded(t, w)["error"] != "invite_expired" {
		t.Fatalf("过期码应 410/invite_expired，实际 %d %s", w.Code, w.Body.String())
	}
	// 落地页对人也应说清原因
	if !strings.Contains(doJoin(r, http.MethodGet, "/join/"+norm, nil).Body.String(), "有效期") {
		t.Fatal("过期码落地页应说明原因")
	}

	// --- 作废 ---
	code2, issue2 := issueInvite(t, r, "作废", 60, nil)
	norm2 := strings.ReplaceAll(code2, "-", "")
	w = doJoin(r, http.MethodPost, "/console/api/invites/revoke", map[string]any{
		"selector": issue2["id"],
	})
	if w.Code != http.StatusOK {
		t.Fatalf("作废应 200，实际 %d %s", w.Code, w.Body.String())
	}

	w = doJoin(r, http.MethodGet, "/join/"+norm2+"/playbook.json", nil)
	if w.Code != http.StatusGone || decoded(t, w)["error"] != "invite_revoked" {
		t.Fatalf("作废码应 410/invite_revoked，实际 %d %s", w.Code, w.Body.String())
	}
}

// TestJoinInstallScripts 验证两条引导脚本的内容与协议判定。
func TestJoinInstallScripts(t *testing.T) {
	r := newInviteRouter(t)
	code, _ := issueInvite(t, r, "", 30, nil)
	norm := strings.ReplaceAll(code, "-", "")

	// HTTP 场景：脚本里不应出现禁用证书校验的分支
	w := doJoin(r, http.MethodGet, "/join/"+norm+"/install.ps1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PS 脚本应 200，实际 %d", w.Code)
	}
	ps1 := w.Body.String()
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("脚本必须声明 text/plain，实际 %q", ct)
	}
	if !strings.Contains(ps1, code) {
		t.Fatal("PS 脚本应含邀请码")
	}
	if !strings.Contains(ps1, "/join/"+norm+"/client") {
		t.Fatal("PS 脚本应指向本中枢的客户端下载地址")
	}
	if strings.Contains(ps1, "MeshTrustAll") {
		t.Fatal("HTTP 部署下不应生成禁用证书校验的分支")
	}
	if !strings.Contains(ps1, "enroll --server") {
		t.Fatal("PS 脚本应调用客户端的 enroll 子命令")
	}

	w = doJoin(r, http.MethodGet, "/join/"+norm+"/install.sh", nil)
	sh := w.Body.String()
	if !strings.Contains(sh, code) || !strings.Contains(sh, "enroll --server") {
		t.Fatal("sh 脚本应含邀请码并调用 enroll")
	}
	if !strings.Contains(sh, "id -u") {
		t.Fatal("sh 脚本应做 root 检查并给出提权用法")
	}

	// HTTPS 场景：必须给出 TOFU 说明
	PublicBaseURL = "https://mesh.example.com"
	t.Cleanup(func() { PublicBaseURL = "" })
	w = doJoin(r, http.MethodGet, "/join/"+norm+"/install.ps1", nil)
	ps1 = w.Body.String()
	if !strings.Contains(ps1, "MeshTrustAll") {
		t.Fatal("HTTPS 部署下 PS 脚本应处理自签证书")
	}
	if strings.Contains(ps1, "http://") {
		t.Fatal("配置了 PublicBaseURL 后不应再出现 http 地址")
	}
}

// TestJoinClientPack 验证客户端分发的三条分支：未配置 / 找不到 / 正常下发。
func TestJoinClientPack(t *testing.T) {
	r := newInviteRouter(t)
	code, _ := issueInvite(t, r, "", 30, nil)
	norm := strings.ReplaceAll(code, "-", "")

	// 未配置目录
	w := doJoin(r, http.MethodGet, "/join/"+norm+"/client?os=windows", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置客户端目录应 503，实际 %d", w.Code)
	}
	if got := decoded(t, w)["error"]; got != "client_pack_unavailable" {
		t.Fatalf("错误码应为 client_pack_unavailable，实际 %v", got)
	}

	// 配置了目录但没有对应平台的文件
	dir := t.TempDir()
	ClientPackDir = dir
	t.Cleanup(func() { ClientPackDir = "" })
	if err := os.WriteFile(filepath.Join(dir, "client-windows-amd64.exe"), []byte("MZ-fake"), 0o644); err != nil {
		t.Fatalf("写入假客户端失败: %v", err)
	}

	w = doJoin(r, http.MethodGet, "/join/"+norm+"/client?os=linux", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺 Linux 包应 503，实际 %d", w.Code)
	}

	// 正常下发
	w = doJoin(r, http.MethodGet, "/join/"+norm+"/client?os=windows", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应能下发客户端，实际 %d", w.Code)
	}
	if body := w.Body.String(); body != "MZ-fake" {
		t.Fatalf("下发内容不符: %q", body)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "client-windows-amd64.exe") {
		t.Fatalf("应带附件文件名，实际 %q", cd)
	}

	// 有包之后，落地页应认为客户端已就绪（不再展示放置指引）
	if !strings.Contains(doJoin(r, http.MethodGet, "/join/"+norm, nil).Body.String(), "install.ps1") {
		t.Fatal("客户端就绪后落地页应直接给出安装命令")
	}
}

// TestJoinClientPackFallbackName 验证兜底命名匹配（运维不一定按约定命名）。
func TestJoinClientPackFallbackName(t *testing.T) {
	r := newInviteRouter(t)
	code, _ := issueInvite(t, r, "", 30, nil)
	norm := strings.ReplaceAll(code, "-", "")

	dir := t.TempDir()
	ClientPackDir = dir
	t.Cleanup(func() { ClientPackDir = "" })
	if err := os.WriteFile(filepath.Join(dir, "my-linux-build"), []byte("ELF"), 0o755); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	w := doJoin(r, http.MethodGet, "/join/"+norm+"/client?os=linux", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应按文件名关键字兜底命中，实际 %d %s", w.Code, w.Body.String())
	}
}

// TestJoinRateLimit 验证限流真的拦得住爆破。
func TestJoinRateLimit(t *testing.T) {
	r := newInviteRouter(t)
	joinLimiter = newIPLimiter(10, time.Minute)

	var last int
	for i := 0; i < 12; i++ {
		w := doJoin(r, http.MethodGet, "/join/ZZZZ-ZZZZ-ZZZZ-ZZZZ", nil)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("超过配额应返回 429，实际 %d", last)
	}

	w := doJoin(r, http.MethodGet, "/join/ZZZZ-ZZZZ-ZZZZ-ZZZZ", nil)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After 头")
	}
	if got := decoded(t, w)["error"]; got != "rate_limited" {
		t.Fatalf("错误码应为 rate_limited，实际 %v", got)
	}
}

// TestIssueInviteValidation 验证签发参数被正确钳制与落库。
func TestIssueInviteValidation(t *testing.T) {
	r := newInviteRouter(t)

	// 超大 TTL 应被钳到 7 天
	w := doJoin(r, http.MethodPost, "/console/api/invites", map[string]any{
		"label": "超长", "ttl_minutes": 999999, "max_uses": 3,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("签发应 200，实际 %d %s", w.Code, w.Body.String())
	}
	resp := decoded(t, w)
	exp, _ := resp["expires_at"].(float64)
	maxAllowed := float64(time.Now().Add(EnrollMaxTTL + time.Minute).Unix())
	if exp > maxAllowed {
		t.Fatalf("有效期应被钳制到 7 天内，实际 %v", exp)
	}
	if mu, _ := resp["max_uses"].(float64); mu != 3 {
		t.Fatalf("max_uses 应为 3，实际 %v", mu)
	}

	// 非法 JSON 给出稳定错误码
	w = doJoin(r, http.MethodPost, "/console/api/invites", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空请求体应 400，实际 %d", w.Code)
	}
	if got := decoded(t, w)["error"]; got != "bad_json" {
		t.Fatalf("错误码应为 bad_json，实际 %v", got)
	}

	// 作废不存在的码 → 404
	w = doJoin(r, http.MethodPost, "/console/api/invites/revoke", map[string]any{"selector": "NOPE"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("作废不存在的码应 404，实际 %d %s", w.Code, w.Body.String())
	}
}

// TestJoinTLScaDelivered 验证 CA 会随配置包下发。
func TestJoinTLScaDelivered(t *testing.T) {
	r := newInviteRouter(t)
	TLSCAPEM = "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"
	t.Cleanup(func() { TLSCAPEM = "" })

	code, _ := issueInvite(t, r, "", 30, nil)
	norm := strings.ReplaceAll(code, "-", "")

	w := doJoin(r, http.MethodGet, "/join/"+norm+"/playbook.json", nil)
	pem, _ := decoded(t, w)["tls_ca_pem"].(string)
	if !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Fatalf("配置包应含 CA PEM，实际 %q", pem)
	}
}

// TestJoinClientIPIsRemoteAddr 验证限流键取的是连接地址，不吃 XFF。
// 否则攻击者随便填一个 X-Forwarded-For 就能把配额刷成无限。
func TestJoinClientIPIsRemoteAddr(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/join/X", nil)
	c.Request.RemoteAddr = "10.1.2.3:54321"
	c.Request.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientIP(c); got != "10.1.2.3" {
		t.Fatalf("限流键应为连接地址 10.1.2.3，实际 %q", got)
	}
}

// TestDisplayCodeGrouping 验证展示用的分组格式。
func TestDisplayCodeGrouping(t *testing.T) {
	cases := map[string]string{
		"abcdefghjkmpqrst":      "ABCD-EFGH-JKMP-QRST",
		"ABCD-EFGH-JKMP-QRST":   "ABCD-EFGH-JKMP-QRST",
		" abcd efgh jkmp qrst ": "ABCD-EFGH-JKMP-QRST",
	}
	for in, want := range cases {
		if got := displayCode(in); got != want {
			t.Errorf("displayCode(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 长度异常时原样返回，不做越界切分
	if got := displayCode("ABC"); got != "ABC" {
		t.Errorf("短码应原样返回，实际 %q", got)
	}
}

// TestInviteErrorStatusMapping 锁定错误码契约（控制台与客户端文案都依赖它）。
func TestInviteErrorStatusMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrInviteNotFound, http.StatusNotFound, "invite_not_found"},
		{store.ErrInviteRevoked, http.StatusGone, "invite_revoked"},
		{store.ErrInviteExpired, http.StatusGone, "invite_expired"},
		{store.ErrInviteExhausted, http.StatusConflict, "invite_exhausted"},
		{fmt.Errorf("其它错误"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		status, code := inviteErrorStatus(tc.err)
		if status != tc.status || code != tc.code {
			t.Errorf("%v 应映射为 %d/%s，实际 %d/%s", tc.err, tc.status, tc.code, status, code)
		}
	}
}
