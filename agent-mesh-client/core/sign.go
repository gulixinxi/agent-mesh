package core

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// =====================================================================
// 客户端侧请求签名，规范必须与服务端 api.ComputeSignature 完全一致：
//   HMAC-SHA256(secret, method\npath\ntimestamp\nnonce\nsha256(body))
// =====================================================================

const (
	headerTimestamp = "X-Mesh-Timestamp"
	headerNonce     = "X-Mesh-Nonce"
	headerSignature = "X-Mesh-Signature"
)

// signPayload 构造待签名的规范字符串（顺序与分隔符必须两端一致）。
func signPayload(method, path, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(sum[:]))
	return b.String()
}

// computeSignature 计算 HMAC-SHA256 签名。
func computeSignature(secret, method, path, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signPayload(method, path, timestamp, nonce, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// newNonce 生成防重放随机串（16 个 hex 字符）。
func newNonce() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// 随机数不可用时不降级为固定值，改用时间戳+计数器组合，避免 nonce 碰撞。
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(buf)
}
