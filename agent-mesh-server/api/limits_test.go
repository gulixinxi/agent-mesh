package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestQueryLimit 固化 limit 参数的边界行为。
//
// 外部审计曾断言「limit=-1 会导致 SQLite 拉取全量数据进而 OOM」。
// 实际不会：负值与非数字在解析阶段就被拒绝，超大值收敛到 max。
// 这条测试的作用是把这个结论固定成可执行的事实，避免日后被改动悄悄破坏。
func TestQueryLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		q    string
		def  int
		max  int
		want int
	}{
		{"未传参取默认值", "", 30, 200, 30},
		{"正常值原样采纳", "10", 30, 200, 10},
		{"负数不采纳回退默认", "-1", 30, 200, 30},
		{"零值不采纳回退默认", "0", 30, 200, 30},
		{"超大值收敛到上限", "99999999", 30, 200, 200},
		{"非数字不采纳回退默认", "abc", 30, 200, 30},
		// "+" 在 query 里解码为空格：Sscanf 会跳过前导空白并解析为 5，
		// 这条验证「带空白的数字」不会被当成非法值丢掉。
		{"前导空白数字", "+5", 30, 200, 5},
		{"刚好等于上限", "200", 30, 200, 200},
		{"超过上限一位", "201", 30, 200, 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/x?limit="+tc.q, nil)
			if got := queryLimit(c, tc.def, tc.max); got != tc.want {
				t.Errorf("limit=%q: 得到 %d，期望 %d", tc.q, got, tc.want)
			}
		})
	}
}

// TestValidatePrompt 校验任务指令长度的两条边界：非空、不超限。
func TestValidatePrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
		return c
	}

	t.Run("空指令被拒", func(t *testing.T) {
		c := newCtx()
		if validatePrompt(c, "") {
			t.Fatal("空 prompt 应被拒绝")
		}
		if c.Writer.Status() != http.StatusBadRequest {
			t.Errorf("状态码应为 400，实际 %d", c.Writer.Status())
		}
	})

	t.Run("正常指令放行", func(t *testing.T) {
		c := newCtx()
		if !validatePrompt(c, "帮我总结这段话") {
			t.Fatal("正常 prompt 应被放行")
		}
	})

	t.Run("超长指令被拒", func(t *testing.T) {
		c := newCtx()
		long := strings.Repeat("A", MaxPromptBytes+1)
		if validatePrompt(c, long) {
			t.Fatal("超长 prompt 应被拒绝")
		}
		if c.Writer.Status() != http.StatusRequestEntityTooLarge {
			t.Errorf("状态码应为 413，实际 %d", c.Writer.Status())
		}
	})

	t.Run("刚好卡在上限放行", func(t *testing.T) {
		c := newCtx()
		if !validatePrompt(c, strings.Repeat("A", MaxPromptBytes)) {
			t.Fatal("恰好等于上限的 prompt 应被放行")
		}
	})
}

// TestBodyLimitMiddleware 验证超限请求在到达业务解析前就被拦下。
func TestBodyLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := BodyLimitMiddleware(1024)

	t.Run("声明的超大长度直接 413", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		c.Request.ContentLength = 4096
		handler(c)
		if c.Writer.Status() != http.StatusRequestEntityTooLarge {
			t.Errorf("状态码应为 413，实际 %d", c.Writer.Status())
		}
		if !c.IsAborted() {
			t.Error("应中断后续处理，实际未中断")
		}
	})

	t.Run("未超限放行", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("hi"))
		c.Request.ContentLength = 2
		handler(c)
		if c.IsAborted() {
			t.Error("未超限不应中断")
		}
		if c.Request.Body == nil {
			t.Error("未超限时 Body 应已被包装为 MaxBytesReader，不能为空")
		}
	})

	t.Run("上限为零表示不限制", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		c.Request.ContentLength = 1 << 30
		BodyLimitMiddleware(0)(c)
		if c.IsAborted() {
			t.Error("maxBytes=0 应表示不限制，实际被中断")
		}
	})
}
