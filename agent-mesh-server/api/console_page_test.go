package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-mesh-server/web"

	"github.com/gin-gonic/gin"
)

// TestConsolePageServesInvitePanels 守住控制台页面的「接入侧」接线。
//
// 控制台是内嵌单页（web.ConsoleHTML），没有后端模板渲染，页面里少一块
// DOM 或漏一个 JS 加载函数，编译期完全看不出来——只有人肉打开浏览器才会发现。
// 这里用一次真实 HTTP 渲染把三块面板 + 它们的加载函数钉死：
// 一旦被误删/改名，测试立刻红。
func TestConsolePageServesInvitePanels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/console", HandleConsole)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("控制台页面状态码 = %d, 期望 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, 期望 text/html", ct)
	}

	body := w.Body.String()

	// 三块接入侧面板 + 表单 + 两个加载函数（refresh() 里被调用）。
	for _, want := range []string{
		"接入与设备 · 签发邀请",
		`id="inviteForm"`,
		"邀请记录",
		"入网记录",
		"loadInvites",
		"loadEnrollments",
		`id="invites"`,
		`id="enrollments"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("控制台页面缺少 %q（接入侧 UI 接线断了）", want)
		}
	}

	// 概览卡要能展示「有效邀请码」——它与 ConsoleOverview 的 active_invites 成对。
	if !strings.Contains(body, "有效邀请码") {
		t.Error("控制台页面缺少概览卡「有效邀请码」")
	}

	// 页面必须与内嵌常量逐字节一致，防止有人改了 web 常量却没走 HandleConsole。
	if body != web.ConsoleHTML {
		t.Error("HandleConsole 返回的页面与 web.ConsoleHTML 不一致")
	}
}
