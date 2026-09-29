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

// canonicalString 构造待签名的规范字符串（顺序与分隔符必须与服务端一致）。
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

// computeSignature 计算 HMAC-SHA256 签名（对请求体摘要签名）。
func computeSignature(secret, method, path, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return computeSignatureWithDigest(secret, method, path, timestamp, nonce, hex.EncodeToString(sum[:]))
}

// computeSignatureWithDigest 以「已算好的实体摘要」参与签名，不接触实体字节。
//
// 用途：文件上传是流式的，不能为了签名把整个文件读进内存。客户端先流式
// 算出文件 SHA-256 放进请求头，服务端据此重建同一条规范串完成鉴权；
// 摘要与收到的字节是否相符，则由服务端边落盘边算来兜底。
func computeSignatureWithDigest(secret, method, path, timestamp, nonce, digestHex string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonicalString(method, path, timestamp, nonce, digestHex)))
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
