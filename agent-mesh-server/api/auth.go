package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 内网轻量签名鉴权：HMAC-SHA256 + 时间戳窗口 + Nonce 防重放
//
// 与 Google AI 原方案的关键差异（修正其三个缺陷）：
//   1. 签名覆盖 method + path + timestamp + nonce + body 摘要，
//      而不是只签时间戳 —— 否则攻击者可任意篡改请求体内容重放。
//   2. 时间窗取绝对值双向校验 —— 否则未来时间戳（差值为负）能绕过。
//   3. 中间件只挂在 /api/v1 路由组 —— 不能连 /healthz 一起拦，监控探活会全 401。
// =====================================================================

const (
	// HeaderTimestamp 请求发起的 Unix 秒级时间戳。
	HeaderTimestamp = "X-Mesh-Timestamp"
	// HeaderNonce 每次请求唯一的随机串，用于防重放。
	HeaderNonce = "X-Mesh-Nonce"
	// HeaderSignature HMAC-SHA256 签名（hex）。
	HeaderSignature = "X-Mesh-Signature"

	// maxSkewSeconds 允许的服务端与客户端时钟偏移（双向）。
	maxSkewSeconds = 300
	// nonceTTL Nonce 在内存中的保留时长，超过即视为过期可复用（已被时间窗挡住）。
	nonceTTL = 10 * time.Minute
)

// canonicalString 构造待签名的规范字符串。
// 双端必须严格一致：method\npath\ntimestamp\nnonce\nsha256(body)
//
// 最后一个字段是「实体字节 SHA-256 的十六进制文本」，而不是实体本身。
// 把规范串的构造独立出来，是为了让文件上传能复用同一套签名规则——
// 上传是流式的，服务端不可能先把整个文件读进内存只为算一次摘要。
func canonicalString(method, path, timestamp, nonce, digestHex string) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(digestHex)
	return b.String()
}

// ComputeSignature 计算请求签名。服务端校验与客户端签发共用同一规范。
func ComputeSignature(secret, method, path, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return ComputeSignatureWithDigest(secret, method, path, timestamp, nonce, hex.EncodeToString(sum[:]))
}

// ComputeSignatureWithDigest 直接以「已算好的实体摘要」参与签名，
// 不接触实体字节本身。
//
// 用途：文件上传路径。客户端先流式算出文件的 SHA-256 放进请求头，
// 服务端据此重建规范串完成鉴权，随后边落盘边算摘要并与声明值比对。
// 这样签名校验阶段的内存占用是常数级，几百 MB 的文件也不会撑爆进程。
func ComputeSignatureWithDigest(secret, method, path, timestamp, nonce, digestHex string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonicalString(method, path, timestamp, nonce, digestHex)))
	return hex.EncodeToString(mac.Sum(nil))
}

// nonceStore 记录已使用过的 nonce，防止同一请求在窗口内被重放。
// 单机内存实现即可：本服务是内网单机中枢；多实例部署时需换成共享存储。
type nonceStore struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	lastGC time.Time
}

var nonces = &nonceStore{
	seen:   make(map[string]time.Time),
	lastGC: time.Now(),
}

// consume 返回 false 表示该 nonce 已被使用过（重放攻击）。
func (s *nonceStore) consume(nonce string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// 惰性清理：每分钟扫一次，避免 map 无限增长。
	if now.Sub(s.lastGC) > time.Minute {
		for k, t := range s.seen {
			if now.Sub(t) > nonceTTL {
				delete(s.seen, k)
			}
		}
		s.lastGC = now
	}

	if _, exists := s.seen[nonce]; exists {
		return false
	}
	s.seen[nonce] = now
	return true
}

// SecurityAuthMiddleware 校验内网请求的 HMAC 签名与时间戳窗口。
// secret 为空时不应挂载本中间件（调用方负责判断）。
func SecurityAuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		tsStr := c.GetHeader(HeaderTimestamp)
		nonce := c.GetHeader(HeaderNonce)
		signature := c.GetHeader(HeaderSignature)

		if tsStr == "" || nonce == "" || signature == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少签名请求头"})
			return
		}

		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "时间戳格式非法"})
			return
		}

		// 双向窗口：过去太远和未来太远都要拒绝（只判单侧会被未来时间戳绕过）。
		skew := time.Now().Unix() - ts
		if skew > maxSkewSeconds || skew < -maxSkewSeconds {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请求已过期或时间戳偏移过大"})
			return
		}

		if !nonces.consume(nonce) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "检测到重放请求（nonce 已使用）"})
			return
		}

		// 读取 body 计算摘要后回填，保证后续 handler 仍能正常解析。
		var body []byte
		if c.Request.Body != nil {
			body, err = io.ReadAll(c.Request.Body)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "读取请求体失败"})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}

		expected := ComputeSignature(secret, c.Request.Method, c.Request.URL.Path, tsStr, nonce, body)
		if !hmac.Equal([]byte(signature), []byte(expected)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "签名校验失败"})
			return
		}

		c.Next()
	}
}
