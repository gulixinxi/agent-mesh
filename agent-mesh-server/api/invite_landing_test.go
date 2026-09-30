package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-mesh-server/internal/netutil"
	"agent-mesh-server/store"
)

// 落地页的地址来源是「请求里带的 Host」。这组用例守住一个真实踩到的坑：
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

	// 本机探测到的内网地址必须全部列出来，供直接转发给客户机。
	ips := netutil.LocalIPv4s()
	if len(ips) == 0 {
		t.Log("本机未探测到内网地址，跳过转发链接断言")
	}
	for _, ip := range ips {
		want := "http://" + ip + ":4024/join/" + norm
		if !strings.Contains(body, want) {
			t.Errorf("落地页没有列出可转发的内网链接 %s", want)
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
	for _, host := range []string{"192.168.1.20:4024", "10.8.0.3:4024", "mesh.example.com"} {
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
	PublicBaseURL = "http://192.168.1.20:4024"

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
	if !strings.Contains(body, "http://192.168.1.20:4024/join/"+norm) {
		t.Error("命令里应使用 -public-url 指定的基址")
	}
}
