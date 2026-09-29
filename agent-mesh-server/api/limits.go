package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 请求体与字段长度上限
//
// 起因：此前全库没有任何体积限制，ShouldBindJSON 会把整个请求体读进内存
// 再解析。POST 一个几百 MB 的 JSON 就能把服务端进程撑爆。
// =====================================================================

// 体积上限相关的常量。
const (
	// MaxBodyBytes 是单个请求体的字节上限。
	//
	// 审计上报的载荷含整段 AI 对话，单条通常几十 KB，批量上报时更大；
	// 8MB 对正常流量有充足余量，同时足以挡住超大载荷。
	MaxBodyBytes = 8 << 20

	// MaxPromptBytes 是单条任务 prompt 的长度上限。
	//
	// 它必须比请求体上限小两个数量级，因为 prompt 的破坏力被放大了两级：
	// 写进 SQLite 之后，会被每一个符合条件的节点领走执行。
	// 一条 500MB 的 prompt 不是打爆服务端，是打爆整个集群。
	// 正常任务指令远小于此，64KB 已属宽松。
	MaxPromptBytes = 64 << 10
)

// BodyLimitMiddleware 限制请求体大小，超限直接 413，不再进入业务解析。
//
// 用 http.MaxBytesReader 而非「先读完再判断长度」：后者必须把超大载荷
// 完整读进内存才能拒绝，等于没挡住。
//
// 文件上传是唯一例外：它的上限是 MaxFileBytes（数百 MB），且路径本身是
// 流式落盘的，不存在「读进内存」的问题。所以这里按路径切换上限，
// 而不是把全局上限抬高——那会让所有 JSON 接口一起失去 8MB 的保护。
func BodyLimitMiddleware(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := maxBytes
		if strings.HasPrefix(c.Request.URL.Path, FileUploadPath) {
			limit = MaxFileBytes
		}
		if limit <= 0 {
			c.Next()
			return
		}

		// 声明了 Content-Length 的请求直接拒，连包都不用拆。
		// 绝大多数客户端（Go http.Client、浏览器 fetch）都会带这个头。
		if c.Request.ContentLength > limit {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": fmt.Sprintf("请求体超过 %d 字节上限", limit),
			})
			return
		}

		// chunked 传输没有 Content-Length，交给 MaxBytesReader 边读边截断。
		// 这条路径下超限会表现为解析失败（400）而不是 413——够用了，
		// 真正要防的内存爆掉已经被挡住，状态码只是精确度问题。
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}

// validatePrompt 校验任务指令：非空且不超过长度上限。
// 返回 false 时已写入响应，调用方应直接 return。
func validatePrompt(c *gin.Context, prompt string) bool {
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt 不能为空"})
		return false
	}
	if len(prompt) > MaxPromptBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": fmt.Sprintf("prompt 超过 %d 字节上限（当前 %d 字节）", MaxPromptBytes, len(prompt)),
		})
		return false
	}
	return true
}
