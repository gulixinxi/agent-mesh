package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServer 模拟中枢侧与接入相关的几个端点。
type fakeServer struct {
	*httptest.Server

	playbookStatus int
	playbookBody   string

	heartbeatStatus int
	deviceStatus    int
	deviceJSON      string
	reportStatus    int

	// 记录收到的请求，用于断言「该发生的发生了、不该发生的没发生」
	playbookHits []string
	reportBody   string
	reportOK     bool
}

func newFakeServer(t *testing.T, clientID string) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		playbookStatus:  http.StatusOK,
		heartbeatStatus: http.StatusOK,
		deviceStatus:    http.StatusOK,
		reportStatus:    http.StatusOK,
	}
	fs.deviceJSON = deviceListJSON(clientID, "online", time.Now().Unix())

	mux := http.NewServeMux()
	mux.HandleFunc("/join/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/playbook.json"):
			fs.playbookHits = append(fs.playbookHits, r.URL.Query().Get("client_id"))
			if fs.playbookBody != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(fs.playbookStatus)
				_, _ = io.WriteString(w, fs.playbookBody)
				return
			}
			pb := map[string]any{
				"version":    1,
				"product":    "agent-mesh",
				"code":       "ABCDEFGHJKMPQRST",
				"server_url": fs.URL,
				"secret":     "cluster-secret-under-test",
				"checks":     []string{"heartbeat_ok"},
				"expires_at": time.Now().Add(20 * time.Minute).Unix(),
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fs.playbookStatus)
			_ = json.NewEncoder(w).Encode(pb)
		case strings.HasSuffix(r.URL.Path, "/report"):
			raw, _ := io.ReadAll(r.Body)
			fs.reportBody = string(raw)
			var payload struct {
				OK bool `json:"ok"`
			}
			_ = json.Unmarshal(raw, &payload)
			fs.reportOK = payload.OK
			w.WriteHeader(fs.reportStatus)
			_, _ = io.WriteString(w, `{"recorded":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/api/v1/cluster/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(fs.heartbeatStatus)
	})
	mux.HandleFunc("/api/v1/cluster/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fs.deviceStatus)
		_, _ = io.WriteString(w, fs.deviceJSON)
	})

	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func deviceListJSON(clientID, status string, lastHeartbeat int64) string {
	raw, _ := json.Marshal([]map[string]any{{
		"client_id":      clientID,
		"status":         status,
		"last_heartbeat": lastHeartbeat,
	}})
	return string(raw)
}

// okHooks 返回一组「一切正常」的平台服务钩子。
func okHooks() ServiceHooks {
	return ServiceHooks{
		Install:        func(string) error { return nil },
		Restart:        func() error { return nil },
		Stop:           func() error { return nil },
		Status:         func() (bool, string) { return true, "运行中" },
		AlreadyExists:  func(error) bool { return false },
		PrivilegeCheck: func() error { return nil },
	}
}

// stdConfig 构造一份最小可用的接入配置。
func stdConfig(t *testing.T, server string) EnrollConfig {
	t.Helper()
	dir := t.TempDir()
	return EnrollConfig{
		ServerURL:  server,
		Code:       "ABCD-EFGH-JKMP-QRST",
		ClientID:   "NODE-TEST",
		InstallDir: filepath.Join(dir, "install"),
		DataDir:    filepath.Join(dir, "data"),
		P2PPort:    6001,
		Service:    okHooks(),
	}
}

// shortPoll 把设备轮询压到毫秒级，避免负例用例真等 20 秒。
func shortPoll(t *testing.T) {
	t.Helper()
	oldT, oldI := devicePollTimeout, devicePollInterval
	devicePollTimeout, devicePollInterval = 600*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { devicePollTimeout, devicePollInterval = oldT, oldI })
}

// stepOf 取出指定步骤。
func stepOf(res *EnrollResult, name string) (EnrollStep, bool) {
	for _, s := range res.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return EnrollStep{}, false
}

// TestEnrollHappyPath 走通完整接入。
func TestEnrollHappyPath(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	cfg := stdConfig(t, fs.URL)

	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if !res.OK {
		t.Fatalf("接入应成功，实际失败。输出：\n%s", out.String())
	}
	for _, name := range []string{
		"privilege", "fetch_playbook", "install_binary", "write_config",
		"install_service", "service_running", "heartbeat_ok", "device_registered", "report",
	} {
		s, ok := stepOf(res, name)
		if !ok {
			t.Fatalf("缺少步骤 %s", name)
		}
		if !s.OK {
			t.Fatalf("步骤 %s 应通过，实际失败：%s", name, s.Detail)
		}
	}

	// 配置真的落盘了，且内容正确
	raw, err := os.ReadFile(filepath.Join(cfg.InstallDir, "agent-mesh.json"))
	if err != nil {
		t.Fatalf("配置文件未生成: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("配置文件不是合法 JSON: %v", err)
	}
	if persisted["server"] != fs.URL {
		t.Fatalf("配置里的 server 不正确: %v", persisted["server"])
	}
	if persisted["secret"] != "cluster-secret-under-test" {
		t.Fatalf("配置里应写入集群密钥，实际 %v", persisted["secret"])
	}
	if persisted["id"] != "NODE-TEST" {
		t.Fatalf("配置里的节点 ID 不正确: %v", persisted["id"])
	}
	if _, ok := persisted["doubao_db"]; !ok {
		t.Fatal("配置里应显式写出 doubao_db（空值也有意义）")
	}

	// 程序被复制进安装目录（而不是留在临时目录）
	if _, err := os.Stat(filepath.Join(cfg.InstallDir, "agent-mesh-client.exe")); err != nil {
		if _, err2 := os.Stat(filepath.Join(cfg.InstallDir, "agent-mesh-client")); err2 != nil {
			t.Fatalf("安装目录里没有程序文件: %v / %v", err, err2)
		}
	}

	// 取配置包时带上了本机 client_id（服务端据此做「同机重取不重复扣次数」）
	if len(fs.playbookHits) != 1 || fs.playbookHits[0] != "NODE-TEST" {
		t.Fatalf("取配置包应带 client_id=NODE-TEST，实际 %v", fs.playbookHits)
	}

	// 自检结果回传了，且判定为成功
	if fs.reportBody == "" {
		t.Fatal("应回传自检结果")
	}
	if !fs.reportOK {
		t.Fatal("回传的 ok 字段应为 true")
	}
	if !strings.Contains(fs.reportBody, "heartbeat_ok") {
		t.Fatal("回传内容应包含各步骤明细")
	}
}

// TestEnrollPrivilegeFailsBeforeConsumingInvite 验证权限不足时不会先烧掉邀请码。
//
// 这是最容易踩的坑：先取包（消费名额）再因为权限不足失败，
// 现场就得让管理员重新签发一次。
func TestEnrollPrivilegeFailsBeforeConsumingInvite(t *testing.T) {
	fs := newFakeServer(t, "NODE-TEST")
	cfg := stdConfig(t, fs.URL)
	hooks := okHooks()
	hooks.PrivilegeCheck = func() error { return errors.New("需要管理员权限") }
	cfg.Service = hooks

	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if res.OK {
		t.Fatal("权限不足时不应判定为接入成功")
	}
	if len(fs.playbookHits) != 0 {
		t.Fatalf("权限检查失败时不应去取配置包（会白烧邀请码），实际取了 %d 次", len(fs.playbookHits))
	}
	s, ok := stepOf(res, "privilege")
	if !ok || s.OK {
		t.Fatalf("应有未通过的 privilege 步骤，实际 %+v", res.Steps)
	}
	if !strings.Contains(out.String(), "尚未消耗邀请码") {
		t.Fatal("输出应明确告知「没消耗邀请码，修好后可直接重跑」")
	}
}

// TestEnrollInviteErrorsTranslated 验证服务端错误码被翻译成人话。
func TestEnrollInviteErrorsTranslated(t *testing.T) {
	cases := []struct {
		status int
		code   string
		expect string
	}{
		{http.StatusGone, "invite_expired", "已过期"},
		{http.StatusConflict, "invite_exhausted", "次数已用尽"},
		{http.StatusGone, "invite_revoked", "作废"},
		{http.StatusNotFound, "invite_not_found", "不存在"},
		{http.StatusTooManyRequests, "rate_limited", "限流"},
	}
	for _, tc := range cases {
		fs := newFakeServer(t, "NODE-TEST")
		fs.playbookStatus = tc.status
		body, _ := json.Marshal(map[string]string{"error": tc.code, "message": "raw"})
		fs.playbookBody = string(body)

		cfg := stdConfig(t, fs.URL)
		var out strings.Builder
		res := Enroll(context.Background(), cfg, &out)

		if res.OK {
			t.Fatalf("%s 场景不应成功", tc.code)
		}
		s, _ := stepOf(res, "fetch_playbook")
		if !strings.Contains(s.Detail, tc.expect) {
			t.Fatalf("%s 的提示应含 %q，实际 %q", tc.code, tc.expect, s.Detail)
		}
	}
}

// TestEnrollUnreachableServer 验证连不上中枢时的提示可用。
func TestEnrollUnreachableServer(t *testing.T) {
	// 起一个服务再立刻关掉，拿到一个确定没人监听的地址
	fs := newFakeServer(t, "NODE-TEST")
	addr := fs.URL
	fs.Close()

	cfg := stdConfig(t, addr)
	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if res.OK {
		t.Fatal("连不上中枢不应成功")
	}
	s, _ := stepOf(res, "fetch_playbook")
	if !strings.Contains(s.Detail, "连不上中枢") {
		t.Fatalf("应给出「连不上中枢」的提示，实际 %q", s.Detail)
	}
}

// TestEnrollHeartbeatRejected 验证密钥不符时心跳步骤报错，但结果仍会回传。
func TestEnrollHeartbeatRejected(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	fs.heartbeatStatus = http.StatusForbidden

	cfg := stdConfig(t, fs.URL)
	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if res.OK {
		t.Fatal("心跳被拒不应判定为成功")
	}
	hb, _ := stepOf(res, "heartbeat_ok")
	if hb.OK {
		t.Fatal("心跳步骤应为失败")
	}
	if !strings.Contains(hb.Detail, "集群密钥") {
		t.Fatalf("应提示密钥不符，实际 %q", hb.Detail)
	}
	// 失败也必须回传 —— 否则控制台看不到这次失败
	if fs.reportBody == "" {
		t.Fatal("失败场景也必须回传自检结果")
	}
	if fs.reportOK {
		t.Fatal("失败场景回传的 ok 应为 false")
	}
}

// TestEnrollDeviceNotRegistered 验证「心跳通了但节点没上线」也能识别出来。
func TestEnrollDeviceNotRegistered(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	fs.deviceJSON = deviceListJSON("OTHER-NODE", "online", time.Now().Unix())

	cfg := stdConfig(t, fs.URL)
	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if res.OK {
		t.Fatal("节点未上线不应判定为成功")
	}
	reg, _ := stepOf(res, "device_registered")
	if reg.OK {
		t.Fatal("device_registered 应为失败")
	}
	if !strings.Contains(reg.Detail, "NODE-TEST") {
		t.Fatalf("应指出找不到哪个节点，实际 %q", reg.Detail)
	}
}

// TestEnrollServiceRestartWhenAlreadyInstalled 验证「服务已注册」走重启而不是报错。
func TestEnrollServiceRestartWhenAlreadyInstalled(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	cfg := stdConfig(t, fs.URL)

	restarted := false
	hooks := okHooks()
	hooks.Install = func(string) error { return errors.New("服务 AgentMeshClient 已存在，请先执行 uninstall") }
	hooks.AlreadyExists = func(err error) bool { return strings.Contains(err.Error(), "已存在") }
	hooks.Restart = func() error { restarted = true; return nil }
	cfg.Service = hooks

	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if !res.OK {
		t.Fatalf("服务已注册应走重启并成功，实际失败：\n%s", out.String())
	}
	if !restarted {
		t.Fatal("应以重启方式让新配置生效")
	}
}

// TestEnrollStopsRunningServiceBeforeOverwrite 验证覆盖程序前会先停服务。
func TestEnrollStopsRunningServiceBeforeOverwrite(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	cfg := stdConfig(t, fs.URL)

	stopped := false
	hooks := okHooks()
	hooks.Status = func() (bool, string) { return true, "运行中" }
	hooks.Stop = func() error { stopped = true; return nil }
	cfg.Service = hooks

	var out strings.Builder
	if res := Enroll(context.Background(), cfg, &out); !res.OK {
		t.Fatalf("接入应成功，实际失败：\n%s", out.String())
	}
	if !stopped {
		t.Fatal("覆盖程序文件前应先停止在运行的服务")
	}
}

// TestEnrollSkipService 验证 --no-service 走「只落配置」路径。
func TestEnrollSkipService(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	cfg := stdConfig(t, fs.URL)
	cfg.SkipService = true

	var out strings.Builder
	res := Enroll(context.Background(), cfg, &out)

	if !res.OK {
		t.Fatalf("跳过服务时也应成功，实际失败：\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.InstallDir, "agent-mesh.json")); err != nil {
		t.Fatalf("配置仍应落盘: %v", err)
	}
}

// TestEnrollWritesCA 验证 CA 会落盘并被写进配置。
func TestEnrollWritesCA(t *testing.T) {
	shortPoll(t)
	fs := newFakeServer(t, "NODE-TEST")
	fs.playbookBody = `{"version":1,"server_url":"` + fs.URL +
		`","secret":"s","tls_ca_pem":"-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"}`
	cfg := stdConfig(t, fs.URL)

	var out strings.Builder
	if res := Enroll(context.Background(), cfg, &out); !res.OK {
		t.Fatalf("接入应成功，实际失败：\n%s", out.String())
	}

	caPath := filepath.Join(cfg.InstallDir, "ca.pem")
	if _, err := os.Stat(caPath); err != nil {
		t.Fatalf("CA 应落盘: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(cfg.InstallDir, "agent-mesh.json"))
	if !strings.Contains(string(raw), "ca.pem") {
		t.Fatal("配置里应指向落盘的 CA")
	}
}

// TestNormalizeServerURL 验证地址规整。
func TestNormalizeServerURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.0.0.5:8443", "http://10.0.0.5:8443"},
		{"http://10.0.0.5:8080/", "http://10.0.0.5:8080"},
		{"https://mesh.example.com", "https://mesh.example.com"},
		{"  10.0.0.5:8080  ", "http://10.0.0.5:8080"},
	}
	for _, tc := range cases {
		got, err := normalizeServerURL(tc.in)
		if err != nil {
			t.Errorf("normalizeServerURL(%q) 报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeServerURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", "http://"} {
		if _, err := normalizeServerURL(bad); err == nil {
			t.Errorf("normalizeServerURL(%q) 应报错", bad)
		}
	}
}

// TestNormalizeCode 验证邀请码归一化。
func TestNormalizeCode(t *testing.T) {
	cases := map[string]string{
		"abcd-efgh-jkmp-qrst":  "ABCDEFGHJKMPQRST",
		" ABCD EFGH JKMP QRST": "ABCDEFGHJKMPQRST",
		"abcdefghjkmpqrst\n":   "ABCDEFGHJKMPQRST",
	}
	for in, want := range cases {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestFindOnlineDevice 验证设备上线判定。
func TestFindOnlineDevice(t *testing.T) {
	now := time.Now().Unix()

	ok, _ := findOnlineDevice([]byte(deviceListJSON("NODE-A", "online", now)), "NODE-A")
	if !ok {
		t.Fatal("新鲜的 online 心跳应判定为已上线")
	}

	ok, detail := findOnlineDevice([]byte(deviceListJSON("NODE-A", "online", now-120)), "NODE-A")
	if ok {
		t.Fatal("心跳过期不应判定为已上线")
	}
	if !strings.Contains(detail, "不新鲜") {
		t.Fatalf("应说明心跳不新鲜，实际 %q", detail)
	}

	ok, _ = findOnlineDevice([]byte(deviceListJSON("NODE-A", "offline", now)), "NODE-A")
	if ok {
		t.Fatal("offline 不应判定为已上线")
	}

	ok, _ = findOnlineDevice([]byte(`[]`), "NODE-A")
	if ok {
		t.Fatal("空列表不应判定为已上线")
	}
}

// TestPlaybookErrorMessage 锁定错误码到人话的映射。
func TestPlaybookErrorMessage(t *testing.T) {
	cases := map[string]string{
		"invite_expired":          "过期",
		"invite_exhausted":        "次数已用尽",
		"invite_revoked":          "作废",
		"invite_not_found":        "不存在",
		"rate_limited":            "限流",
		"client_pack_unavailable": "客户端安装包",
	}
	for code, expect := range cases {
		got := playbookErrorMessage(http.StatusOK, code, "")
		if !strings.Contains(got, expect) {
			t.Errorf("错误码 %s 的文案应含 %q，实际 %q", code, expect, got)
		}
	}
	// 未知错误应带上状态码与原始信息，方便排查
	got := playbookErrorMessage(500, "", "内部错误")
	if !strings.Contains(got, "500") || !strings.Contains(got, "内部错误") {
		t.Errorf("未知错误应保留原始信息，实际 %q", got)
	}
}

// TestAllCriticalOK 验证关键步骤聚合逻辑。
func TestAllCriticalOK(t *testing.T) {
	allOK := []EnrollStep{
		{Name: "fetch_playbook", OK: true},
		{Name: "report", OK: false}, // 非关键，不该影响结论
	}
	if !allCriticalOK(allOK) {
		t.Fatal("非关键步骤失败不应影响结论")
	}

	withFailure := []EnrollStep{
		{Name: "fetch_playbook", OK: true},
		{Name: "heartbeat_ok", OK: false},
	}
	if allCriticalOK(withFailure) {
		t.Fatal("关键步骤失败必须判定为未成功")
	}
}
