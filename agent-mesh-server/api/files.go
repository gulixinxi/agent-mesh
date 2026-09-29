package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-mesh-server/store"

	"github.com/gin-gonic/gin"
)

// =====================================================================
// 中转文件接口：把「文件」从点对点直传解耦出来
//
// 为什么需要它：
//   1. P2P 只有 mDNS 发现（client/core/p2p_transfer.go），而 mDNS 是二层组播，
//      路由器不转发 —— 跨网段（哪怕同一个厂区）文件直传必然失败。
//   2. 任务执行产出的文件原本只留在执行机本地，中枢只收到一句文字结果，
//      控制台看不到也拿不回来。
//
// 做法：文件先上传到中枢（实体落磁盘、元数据入库），目标节点再按 file_id 拉走。
// 于是不再依赖任何点对点可达性，也让「任务产出的文件」第一次可被取回。
// =====================================================================

// FileStoreDir 是中转文件实体的落盘根目录，由 main 在启动时注入。
// 为空表示未启用文件存储：上传接口直接 503，而不是静默把文件丢掉。
var FileStoreDir string

// FileUploadPath 是文件上传的完整路由路径。
//
// 抽成常量，是因为它同时被三处引用：路由注册、体积上限中间件（要按路径
// 放行到文件上限）、以及上传鉴权（规范串里的 path 必须与它逐字一致）。
// 任何一处改动而其它两处没跟上，都会表现为「签名校验失败」这种极难定位的症状。
const FileUploadPath = "/api/v1/files/upload"

const (
	// MaxFileBytes 是单个中转文件的大小上限。
	//
	// 上传路径是流式的（边落盘边算摘要），内存占用与文件大小无关，
	// 所以这里的约束是磁盘与带宽，而不是内存。512MiB 足以覆盖报表、
	// 导出件、素材这类产出物。
	MaxFileBytes int64 = 512 << 20

	// MaxFileStoreBytes 是中转文件占用的磁盘总量上限。
	// 超过后拒绝新上传（507），避免长期运行把磁盘写满导致整个中枢不可用。
	MaxFileStoreBytes int64 = 8 << 30
)

// 上传请求用到的头。文件名等可能含非 ASCII 的字段一律先做 URL 编码，
// HTTP 头只能装 Latin-1。
const (
	// HeaderFileName 是 URL 编码后的文件名。
	HeaderFileName = "X-Mesh-File-Name"
	// HeaderFileTarget 是目标节点 ID，URL 编码；留空表示广播给所有节点。
	HeaderFileTarget = "X-Mesh-File-Target"
	// HeaderFileTask 是可选的关联任务 ID，便于把「任务产出的文件」归位。
	HeaderFileTask = "X-Mesh-File-Task"
	// HeaderFileUploader 是上传者节点 ID，URL 编码，仅用于审计展示。
	HeaderFileUploader = "X-Mesh-File-Uploader"
	// HeaderContentSHA256 是文件实体字节的 SHA-256（hex）。
	//
	// 它有两个作用：一是参与签名，让服务端无需读 body 就能重建规范串；
	// 二是服务端边落盘边算摘要后与它比对，确保收到的字节没被中途篡改。
	HeaderContentSHA256 = "X-Mesh-Content-SHA256"
)

// newFileID 生成中转文件的对外标识。用 128 位随机数而非序号：
// 文件名与投递目标都可能是敏感的，ID 不可枚举是这类中转接口的最低要求。
func newFileID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("file-%d", time.Now().UnixNano())
	}
	return "file-" + hex.EncodeToString(buf)
}

// decodeHeader 解出 URL 编码的头值；解码失败时按原样返回，
// 让后续的文件名校验去拒绝它，而不是把编码错误伪装成正常值。
func decodeHeader(v string) string {
	if v == "" {
		return ""
	}
	if s, err := url.QueryUnescape(v); err == nil {
		return s
	}
	return v
}

// sanitizeFileName 把文件名收敛为纯文件名。
//
// 落盘用的是随机 file_id，路径穿越本来就走不通；但这个名字会进入
// Content-Disposition 头与控制台展示，仍是不可信输入，必须归一化。
func sanitizeFileName(raw string) (string, error) {
	name := filepath.Base(strings.ReplaceAll(raw, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.Contains(name, "..") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("非法文件名: %q", raw)
	}
	if len([]byte(name)) > 255 {
		return "", fmt.Errorf("文件名过长: %q", raw)
	}
	return name, nil
}

// FileUploadAuthMiddleware 是文件上传专用的 HMAC 鉴权。
//
// 为什么不复用 SecurityAuthMiddleware：后者为保证签名覆盖请求体内容，
// 会先把整个 body 读进内存再回填。对几百 MB 的文件上传，这等于把
// 「防篡改」做成了「自我 OOM」。这里改为让客户端事先声明实体摘要，
// 服务端据此重建同一套规范串，校验阶段内存占用是常数级；
// 摘要与实体是否一致，则由 UploadFile 边落盘边算来兜底。
func FileUploadAuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if secret == "" {
			// 未配置密钥时 /api/v1 整体处于无鉴权状态，与其余接口保持一致。
			c.Next()
			return
		}

		tsStr := c.GetHeader(HeaderTimestamp)
		nonce := c.GetHeader(HeaderNonce)
		signature := c.GetHeader(HeaderSignature)
		digest := strings.ToLower(strings.TrimSpace(c.GetHeader(HeaderContentSHA256)))

		if tsStr == "" || nonce == "" || signature == "" || digest == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "缺少签名请求头或 " + HeaderContentSHA256,
			})
			return
		}

		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "时间戳格式非法"})
			return
		}
		skew := time.Now().Unix() - ts
		if skew > maxSkewSeconds || skew < -maxSkewSeconds {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请求已过期或时间戳偏移过大"})
			return
		}
		if !nonces.consume(nonce) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "检测到重放请求（nonce 已使用）"})
			return
		}

		expected := ComputeSignatureWithDigest(secret, c.Request.Method, c.Request.URL.Path, tsStr, nonce, digest)
		if !hmac.Equal([]byte(signature), []byte(expected)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "签名校验失败"})
			return
		}
		c.Next()
	}
}

// UploadFile 接收节点上传的中转文件。
//
// 全程流式：请求体直接落到临时文件并同步计算摘要，最后与声明值比对后
// 原子改名为 file_id。任何一步失败都会清掉临时文件，不留垃圾。
func UploadFile(c *gin.Context) {
	if FileStoreDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "服务端未启用文件中转（未配置文件存储目录）"})
		return
	}

	rawName := decodeHeader(c.GetHeader(HeaderFileName))
	name, err := sanitizeFileName(rawName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	target := decodeHeader(c.GetHeader(HeaderFileTarget))
	taskID := decodeHeader(c.GetHeader(HeaderFileTask))
	uploader := decodeHeader(c.GetHeader(HeaderFileUploader))
	declared := strings.ToLower(strings.TrimSpace(c.GetHeader(HeaderContentSHA256)))

	// 声明了长度且超限就直接拒，连磁盘都不用碰。
	if c.Request.ContentLength > MaxFileBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": fmt.Sprintf("文件超过 %d 字节上限", MaxFileBytes),
		})
		return
	}

	used, err := store.TotalFileBytes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if used >= MaxFileStoreBytes ||
		(c.Request.ContentLength > 0 && used+c.Request.ContentLength > MaxFileStoreBytes) {
		c.JSON(http.StatusInsufficientStorage, gin.H{
			"error": fmt.Sprintf("中转存储已达上限（%d 字节），请清理历史文件后重试", MaxFileStoreBytes),
			"used":  used,
		})
		return
	}

	tmpDir := filepath.Join(FileStoreDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tmpName := tmp.Name()

	// 多读 1 字节即可判断是否超限，无需先读完整个文件。
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(c.Request.Body, MaxFileBytes+1))
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传内容失败: " + err.Error()})
		return
	}
	if written > MaxFileBytes {
		tmp.Close()
		os.Remove(tmpName)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": fmt.Sprintf("文件超过 %d 字节上限", MaxFileBytes),
		})
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 摘要比对：签名只覆盖「声明的摘要」，实体是否与之相符必须在这里验证。
	// 少了这一步，中间人可以在不改签名的情况下替换传输中的文件内容。
	got := hex.EncodeToString(hasher.Sum(nil))
	if declared == "" || got != declared {
		os.Remove(tmpName)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":    "内容校验和不匹配",
			"expected": declared,
			"actual":   got,
		})
		return
	}

	fileID := newFileID()
	final := filepath.Join(FileStoreDir, fileID)
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	meta := store.FileMeta{
		FileID:     fileID,
		FileName:   name,
		Size:       written,
		SHA256:     got,
		Uploader:   uploader,
		TargetNode: target,
		TaskID:     taskID,
		CreatedAt:  time.Now().Unix(),
	}
	if err := store.InsertFile(meta); err != nil {
		os.Remove(final)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	fmt.Printf("[文件中转] 已接收 %s (%d 字节) 上传者:%s -> 目标:%s\n",
		name, written, displayNode(uploader), displayNode(target))
	c.JSON(http.StatusOK, meta)
}

// ListFiles 返回调用节点可见的中转文件：投递给它的，或广播的。
func ListFiles(c *gin.Context) {
	node := c.Query("node")
	pendingOnly := c.Query("pending") == "1"
	limit := queryLimit(c, 50, 500)

	list, err := store.ListFilesForNode(node, pendingOnly, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.FileMeta{}
	}
	c.JSON(http.StatusOK, list)
}

// DownloadFile 下载一个中转文件。
//
// 可见性校验：定向投递给某节点的文件，只有那个节点能下。
// 缺了这步，任何持集群密钥的节点只要猜到 ID 就能取走别人的文件。
func DownloadFile(c *gin.Context) {
	node := c.Query("node")
	meta, ok := lookupFile(c)
	if !ok {
		return
	}
	if meta.TargetNode != "" && meta.TargetNode != node {
		c.JSON(http.StatusForbidden, gin.H{"error": "该文件定向投递给了其它节点"})
		return
	}

	if serveFile(c, meta) && node != "" && node == meta.TargetNode {
		// 领取成功才标记；失败时保持未领取，下一轮轮询会重试。
		_ = store.MarkFileDownloaded(meta.FileID, time.Now().Unix())
	}
}

// DeleteFile 删除一个中转文件（元数据与磁盘实体一并清理）。
func DeleteFile(c *gin.Context) {
	var req struct {
		FileID string `json:"file_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.FileID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_id 不能为空"})
		return
	}

	meta, err := store.GetFileMeta(req.FileID)
	if errors.Is(err, store.ErrFileNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在: " + req.FileID})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 实体可能已被手工删除或清理，这里以元数据为准，删不掉就算了。
	_ = os.Remove(filepath.Join(FileStoreDir, meta.FileID))
	if _, err := store.DeleteFileMeta(meta.FileID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	fmt.Printf("[文件中转] 已删除 %s (%s)\n", meta.FileName, meta.FileID)
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "file_id": meta.FileID})
}

// PurgeExpiredFiles 清理超过保留期的中转文件，返回清理条数。
// 由 main 的滚动清理协程调用，避免中转目录随时间无限增长。
func PurgeExpiredFiles(retention time.Duration) (int, error) {
	if FileStoreDir == "" || retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention).Unix()
	metas, err := store.ExpiredFiles(cutoff, 2000)
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, m := range metas {
		_ = os.Remove(filepath.Join(FileStoreDir, m.FileID))
		if _, err := store.DeleteFileMeta(m.FileID); err != nil {
			continue
		}
		purged++
	}
	return purged, nil
}

// ConsoleFiles 是中转文件的总览，供控制台查看（已由 Basic Auth 保护）。
func ConsoleFiles(c *gin.Context) {
	limit := queryLimit(c, 100, 500)
	list, err := store.ListAllFiles(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if list == nil {
		list = []store.FileMeta{}
	}
	c.JSON(http.StatusOK, list)
}

// ConsoleDownloadFile 让控制台直接取回文件实体（不限投递目标，凭 Basic Auth 放行）。
//
// 这是「任务产出的文件取不回来」的直接解药：产物上传中转后，
// 在控制台点一下就能下载，不必登录到那台执行机。
func ConsoleDownloadFile(c *gin.Context) {
	meta, ok := lookupFile(c)
	if !ok {
		return
	}
	serveFile(c, meta)
}

// lookupFile 从 file_id 查询元数据，并把错误直接写成响应。返回 false 表示已响应。
func lookupFile(c *gin.Context) (store.FileMeta, bool) {
	fileID := strings.TrimSpace(c.Query("file_id"))
	if fileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_id 不能为空"})
		return store.FileMeta{}, false
	}
	meta, err := store.GetFileMeta(fileID)
	if errors.Is(err, store.ErrFileNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在: " + fileID})
		return store.FileMeta{}, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return store.FileMeta{}, false
	}
	return meta, true
}

// serveFile 把磁盘上的文件实体写回响应。返回是否确实成功开始了传输。
func serveFile(c *gin.Context, meta store.FileMeta) bool {
	if FileStoreDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "服务端未启用文件中转"})
		return false
	}

	// file_id 来自数据库（自己生成的十六进制串），这里仍显式校验不越界：
	// 一旦哪天 ID 生成逻辑被改坏，也不会变成任意文件读取。
	path := filepath.Join(FileStoreDir, meta.FileID)
	if rel, err := filepath.Rel(FileStoreDir, path); err != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件路径非法"})
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件实体不存在（可能已被清理）"})
		return false
	}
	defer f.Close()

	// 强制二进制下载，避免浏览器把产出物当页面渲染。
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(meta.FileName))
	// 客户端据此还原文件名并校验完整性，无需先解析 Content-Disposition。
	c.Header("X-Mesh-File-Name", url.QueryEscape(meta.FileName))
	c.Header("X-Mesh-File-SHA256", meta.SHA256)
	c.Header("X-Mesh-File-ID", meta.FileID)

	http.ServeContent(c.Writer, c.Request, meta.FileName, time.Unix(meta.CreatedAt, 0), f)
	return true
}
