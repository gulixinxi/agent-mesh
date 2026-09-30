package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 按来源 IP 的固定窗口限流
//
// 为什么必须有：/join/* 是**唯一**一组不要求 HMAC 签名的写/取凭据端点。
// 邀请码只有 16 位、字母表 31 个字符，理论上是可以被暴力枚举的。
// 没有限流的话，攻击者在内网里跑个脚本就能把有效码试出来。
//
// 实现选固定窗口而非令牌桶：够用、无浮点误差、每分钟只做一次归零判断。
// 代价是窗口边界处允许两倍瞬时流量，对「防爆破」这个目的完全可接受。
// =====================================================================

// ipWindow 是单个 IP 在当前窗口内的计数。
type ipWindow struct {
	count   int
	resetAt time.Time
}

// ipLimiter 是全局的按 IP 固定窗口计数器。
type ipLimiter struct {
	mu      sync.Mutex
	windows map[string]*ipWindow
	limit   int
	period  time.Duration
	lastGC  time.Time
}

func newIPLimiter(limit int, period time.Duration) *ipLimiter {
	return &ipLimiter{
		windows: make(map[string]*ipWindow),
		limit:   limit,
		period:  period,
		lastGC:  time.Now(),
	}
}

// allow 判断该 IP 本次请求是否放行。
func (l *ipLimiter) allow(ip string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// 惰性回收：每 5 分钟扫一次，清掉已过期的窗口，避免 map 无限增长。
	if now.Sub(l.lastGC) > 5*time.Minute {
		for k, w := range l.windows {
			if now.After(w.resetAt) {
				delete(l.windows, k)
			}
		}
		l.lastGC = now
	}

	w, ok := l.windows[ip]
	if !ok || now.After(w.resetAt) {
		l.windows[ip] = &ipWindow{count: 1, resetAt: now.Add(l.period)}
		return true, 0
	}
	if w.count >= l.limit {
		return false, time.Until(w.resetAt)
	}
	w.count++
	return true, 0
}

// joinLimiter 作用于 /join/* 全组：默认每 IP 每分钟 60 次。
// 正常装机流程一次只会打几个请求（落地页 + 配置包 + 客户端包 + 脚本 + 回传），
// 60 次给了足够冗余，同时对枚举攻击形成了有效压制。
var joinLimiter = newIPLimiter(60, time.Minute)

// JoinRateLimitMiddleware 是 /join 路由组的限流中间件。
func JoinRateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := clientIP(c)
		ok, retryAfter := joinLimiter.allow(ip)
		if !ok {
			secs := int(retryAfter.Seconds())
			if secs < 1 {
				secs = 1
			}
			c.Header("Retry-After", itoa(secs))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate_limited",
				"message":     "请求过于频繁，请稍后再试",
				"retry_after": secs,
			})
			return
		}
		c.Next()
	}
}

// clientIP 取请求来源 IP。
// 注意：这里**不信任** X-Forwarded-For —— 它会破坏限流的有效性
// （攻击者随便填一个头就能绕过计数）。服务端若部署在反向代理之后，
// 应由代理做连接级限流，或在反代上开启 realip 后再改这里。
func clientIP(c *gin.Context) string {
	if c.Request == nil {
		return "unknown"
	}
	addr := c.Request.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		return strings.Trim(addr[:idx], "[]")
	}
	if addr == "" {
		return "unknown"
	}
	return addr
}

// itoa 是 strconv.Itoa 的内联替代，避免为一个调用引入额外 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
