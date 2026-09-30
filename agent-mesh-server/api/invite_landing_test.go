package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-mesh-server/internal/netutil"
	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// 落地页与控制台的地址来源都是「请求里带的 Host」。这组用例守住一个真实踩到的坑：
// 管理者从 127.0.0.1 打开页面时，页面给出的命令里地址也是 127.0.0.1，
// 发给客户机后那台机器只会连它自己，报「连接被拒绝」——
// 看起来像网络问题，实际是地址拿错。页面必须当场说清楚。

const loopbackWarningMarker = "只有在「中枢这台机器」上才有效"

// landingWithHost 以指定 Host 请求落地页，返回「归一化后的码」与页面正文。
func landingWithHost(t *testing.T, host string) (string, string) {
	t.Helper()
	r := newInviteRouter(t)
	code, _ := issueInvite(t, r, "落地页地址", 30, nil)
	norm := store.NormalizeInviteCode(code) // 命令里出现的是归一化码，不是带横线的展示码

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/join/"+norm, nil)
	req.Host = host
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("落地页状态码 = %d, 期望 200", w.Code)
	}
	return norm, w.Body.String()
}

// TestJoinLandingWarnsOnLoopbackHost 回环地址下必须显式警告。
func TestJoinLandingWarnsOnLoopbackHost(t *testing.T) {
	norm, body := landingWithHost(t, "127.0.0.1:4024")

	if !strings.Contains(body, loopbackWarningMarker) {
		t.Fatal("从 127.0.0.1 打开落地页时，没有给出「该地址只对本机有效」的警告")
	}
	// 命令仍按原样渲染（便于对照），但必须同时出现警告。
	if !strings.Contains(body, "http://127.0.0.1:4024/join/"+norm) {
		t.Error("落地页应保留原始回环地址的命令，便于管理员对照差异")
	}

	// 本机探测到的每个地址都要列出，并标注来源网卡 ——
	// 一台机器常有真实网卡 + 虚拟网卡，只给 IP 会让人挑错。
	addrs := netutil.LocalAddrs()
	if len(addrs) == 0 {
		t.Log("本机未探测到内网地址，跳过转发链接断言")
	}
	for _, a := range addrs {
		want := "http://" + netutil.FormatHostPort(a.IP, "4024") + "/join/" + norm
		if !strings.Contains(body, want) {
			t.Errorf("落地页没有列出可转发的内网链接 %s", want)
		}
		if a.Iface != "" && !strings.Contains(body, a.Iface) {
			t.Errorf("落地页没有标注网卡名 %s（管理员据此判断选哪条）", a.Iface)
		}
	}
	// 虚拟网卡必须被点名，否则它排在真实网卡后面也照样会被误选。
	for _, a := range addrs {
		if a.Virtual && !strings.Contains(body, "虚拟网卡") {
			t.Error("存在疑似虚拟网卡，但落地页没有标注「虚拟网卡，客户机大概率访问不到」")
			break
		}
	}

	// 光列链接不够，必须说清「让客户自己打开」这个关键动作。
	if !strings.Contains(body, "把下面的链接发给客户") {
		t.Error("落地页缺少「把链接发给客户，让客户自己打开」的操作指引")
	}
}

// TestJoinLandingNoWarningOnRoutableHost 可路由地址下不得误报，
// 否则真正的告警会因为天天出现而被无视。
func TestJoinLandingNoWarningOnRoutableHost(t *testing.T) {
	for _, host := range []string{"192.168.2.131:4024", "10.8.0.3:4024", "mesh.example.com"} {
		t.Run(host, func(t *testing.T) {
			norm, body := landingWithHost(t, host)
			if strings.Contains(body, loopbackWarningMarker) {
				t.Errorf("Host=%s 是可路由地址，不应出现回环警告", host)
			}
			if !strings.Contains(body, "http://"+host+"/join/"+norm) {
				t.Errorf("Host=%s 时命令里应使用该地址", host)
			}
		})
	}
}

// TestJoinLandingPublicBaseURLBeatsHost 配了 -public-url 时，请求 Host 不再影响结果。
// 这是现场最省事的做法：管理者在开发机上随便怎么打开，给出的命令都固定正确。
func TestJoinLandingPublicBaseURLBeatsHost(t *testing.T) {
	r := newInviteRouter(t)
	t.Cleanup(func() { PublicBaseURL = "" })
	PublicBaseURL = "http://192.168.2.131:4024"

	code, _ := issueInvite(t, r, "固定基址", 30, nil)
	norm := store.NormalizeInviteCode(code)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/join/"+norm, nil)
	req.Host = "127.0.0.1:4024" // 即使从回环打开
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, loopbackWarningMarker) {
		t.Error("配了 -public-url 后，即使从回环打开也不该警告")
	}
	if !strings.Contains(body, "http://192.168.2.131:4024/join/"+norm) {
		t.Error("命令里应使用 -public-url 指定的基址")
	}
}

// issueInviteWithHost 以指定 Host 调控制台签发接口，返回响应体。
func issueInviteWithHost(t *testing.T, r *gin.Engine, host, label string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"label": label, "ttl_minutes": 30})
	req := httptest.NewRequest(http.MethodPost, "/console/api/invites", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Host = host
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("签发失败: HTTP %d %s", w.Code, w.Body.String())
	}
	return decoded(t, w)
}

// TestConsoleIssueInviteFlagsLoopbackBase 控制台签发那一刻也必须提醒。
//
// 管理者实际是在控制台签码、复制链接的；只在落地页警告不够 ——
// 他很可能签完直接把链接转发出去，从不打开落地页。
func TestConsoleIssueInviteFlagsLoopbackBase(t *testing.T) {
	r := newInviteRouter(t)

	ok := issueInviteWithHost(t, r, "192.168.2.131:4024", "可路由")
	if ok["loopback"] == true {
		t.Error("可路由 Host 下不应标 loopback")
	}

	lb := issueInviteWithHost(t, r, "127.0.0.1:4024", "回环")
	if lb["loopback"] != true {
		t.Fatal("回环 Host 下应标 loopback，否则控制台无法提醒管理员")
	}

	links, _ := lb["alt_links"].([]any)
	if len(netutil.LocalAddrs()) > 0 && len(links) == 0 {
		t.Error("回环 Host 下应给出可转发链接")
	}
	for _, raw := range links {
		m, _ := raw.(map[string]any)
		u, _ := m["url"].(string)
		if !strings.HasPrefix(u, "http://") || !strings.Contains(u, "/join/") {
			t.Errorf("转发链接格式不对: %v", m)
		}
		if label, _ := m["label"].(string); label == "" {
			t.Errorf("转发链接缺少网卡名标注（管理员据此判断选哪条）: %v", m)
		}
	}
}

// TestJoinCommandsAvoidPS3OnlyCmdlets 接入命令与引导脚本不得依赖 PowerShell 3.0 才有的命令。
//
// 现场踩过：客户机是 Win7/2008R2（自带 PowerShell 2.0），irm 会直接报
// 「无法将 irm 项识别为 cmdlet」；引导脚本里的 Invoke-WebRequest 同理。
// WebClient.DownloadString / DownloadFile 从 2.0 起就有，一条命令通吃所有 Windows。
// 同时落地页必须写明客户端的系统要求 —— 老系统脚本跑通了也装不上（客户端二进制要 Win10+）。
func TestJoinCommandsAvoidPS3OnlyCmdlets(t *testing.T) {
	r := newInviteRouter(t)
	code, _ := issueInvite(t, r, "老系统兼容", 30, nil)
	norm := store.NormalizeInviteCode(code)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/join/"+norm, nil)
	req.Host = "192.168.2.131:4024"
	r.ServeHTTP(w, req)
	body := w.Body.String()

	if strings.Contains(body, `irm "`) {
		t.Error("落地页命令不应使用 irm（PowerShell 3.0 才有，Win7/2008R2 会直接报错）")
	}
	if !strings.Contains(body, "Net.WebClient") || !strings.Contains(body, "DownloadString") {
		t.Error("落地页命令应用 WebClient.DownloadString 换取脚本（PowerShell 2.0 起就有）")
	}
	if !strings.Contains(body, "Windows 10") {
		t.Error("落地页应注明客户端只支持 Windows 10 / Server 2016+，别让管理员把 Win7 也发过去")
	}

	w2 := doJoin(r, http.MethodGet, "/join/"+norm+"/install.ps1", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("引导脚本应 200，实际 %d", w2.Code)
	}
	ps1 := w2.Body.String()
	// 注意匹配的是调用用法而非裸词：脚本注释里会提到「为什么不用 Invoke-WebRequest」
	if strings.Contains(ps1, "Invoke-WebRequest ") {
		t.Error("引导脚本不应调用 Invoke-WebRequest（PowerShell 3.0 才有，Win7 会挂）")
	}
	if !strings.Contains(ps1, "WebClient") {
		t.Error("引导脚本应用 WebClient 下载客户端（PowerShell 2.0 兼容）")
	}
}
