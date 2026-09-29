package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
// 经中枢中转的文件收发
//
// 为什么需要：P2P 只有 mDNS 发现（p2p_transfer.go），而 mDNS 是二层组播、
// 路由器不转发 —— 跨网段（哪怕同一个厂区）文件直传必然失败；任务产出的
// 文件更是只留在执行机本地。这里改走中枢：上传 → 落库落盘 → 目标节点拉取，
// 彻底不依赖点对点可达性。
// =====================================================================

const (
	// relayUploadPath 必须与服务端 api.FileUploadPath 逐字一致。
	// 它是签名规范串里的 path，差一个字符都会变成「签名校验失败」。
	relayUploadPath   = "/api/v1/files/upload"
	relayListPath     = "/api/v1/files"
	relayDownloadPath = "/api/v1/files/download"

	// maxRelayFileBytes 是客户端侧的单文件上限，与服务端 MaxFileBytes 对齐。
	// 先拦一道是为了避免把几百 MB 传完才被服务端 413。
	maxRelayFileBytes = 512 << 20

	// 大文件跨网传输耗时长，超时给得比普通接口宽松得多；
	// 元数据接口仍然按小请求给短超时，避免轮询被卡住。
	relayTransferTimeout = 30 * time.Minute
	relayMetaTimeout     = 10 * time.Second
)

// RelayFile 是中转文件的元数据，字段与服务端 store.FileMeta 一一对应。
type RelayFile struct {
	FileID       string `json:"file_id"`
	FileName     string `json:"file_name"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Uploader     string `json:"uploader"`
	TargetNode   string `json:"target_node"`
	TaskID       string `json:"task_id"`
	CreatedAt    int64  `json:"created_at"`
	DownloadedAt int64  `json:"downloaded_at"`
}

// UploadFileToServer 把本机文件经中枢中转。
//
// targetNode 留空表示广播给所有节点可见；非空表示只有该节点能取。
// taskID 可选，用于把「任务产出的文件」和任务归位。
func (e *MeshEngine) UploadFileToServer(ctx context.Context, filePath, targetNode, taskID string) (RelayFile, error) {
	var out RelayFile

	f, err := os.Open(filePath)
	if err != nil {
		return out, fmt.Errorf("打开待上传文件失败: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return out, err
	}
	if fi.IsDir() {
		return out, fmt.Errorf("不能上传目录: %s", filePath)
	}
	if fi.Size() > maxRelayFileBytes {
		return out, fmt.Errorf("文件 %d 字节，超过中转上限 %d 字节", fi.Size(), maxRelayFileBytes)
	}

	name, err := sanitizeRelayName(filepath.Base(filePath))
	if err != nil {
		return out, err
	}

	// 先算摘要：它既要参与签名（让服务端无需读 body 就能重建规范串），
	// 也是服务端校验实体完整性的依据。
	digest, err := hashFile(f)
	if err != nil {
		return out, fmt.Errorf("计算文件摘要失败: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return out, fmt.Errorf("重置读取位置失败: %w", err)
	}

	uploadCtx, cancel := context.WithTimeout(ctx, relayTransferTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, e.serverURL+relayUploadPath, f)
	if err != nil {
		return out, err
	}
	// *os.File 不会被自动推断长度，必须显式设置；
	// 否则请求退化成 chunked，服务端拿不到 Content-Length 就无法提前拒绝超限。
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	// 头字段只能装 Latin-1，文件名可能含中文，一律先 URL 编码。
	req.Header.Set("X-Mesh-File-Name", url.QueryEscape(name))
	req.Header.Set("X-Mesh-Content-SHA256", digest)
	req.Header.Set("X-Mesh-File-Uploader", url.QueryEscape(e.clientID))
	if targetNode != "" {
		req.Header.Set("X-Mesh-File-Target", url.QueryEscape(targetNode))
	}
	if taskID != "" {
		req.Header.Set("X-Mesh-File-Task", url.QueryEscape(taskID))
	}

	if e.secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := newNonce()
		req.Header.Set(headerTimestamp, ts)
		req.Header.Set(headerNonce, nonce)
		req.Header.Set(headerSignature,
			computeSignatureWithDigest(e.secret, http.MethodPost, relayUploadPath, ts, nonce, digest))
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("上传被拒（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("解析上传响应失败: %w", err)
	}
	return out, nil
}

// ListRelayFiles 拉取本节点可见的中转文件。
// pendingOnly 为真时只返回尚未领取过的。
func (e *MeshEngine) ListRelayFiles(ctx context.Context, node string, pendingOnly bool) ([]RelayFile, error) {
	q := url.Values{}
	q.Set("node", node)
	q.Set("limit", "100")
	if pendingOnly {
		q.Set("pending", "1")
	}

	listCtx, cancel := context.WithTimeout(ctx, relayMetaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(listCtx, http.MethodGet, e.serverURL+relayListPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	e.signRelayRequest(req, relayListPath, nil)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取中转文件列表失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("拉取中转文件列表异常（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var list []RelayFile
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("解析中转文件列表失败: %w", err)
	}
	return list, nil
}

// DownloadRelayFile 从中枢领取一个中转文件并落盘，返回保存路径。
//
// 全程流式写入，不把响应体读进内存；落盘后按响应头里的摘要校验完整性，
// 不一致直接删掉，避免半截文件混进下载目录。
func (e *MeshEngine) DownloadRelayFile(ctx context.Context, fileID string) (string, error) {
	dlCtx, cancel := context.WithTimeout(ctx, relayTransferTimeout)
	defer cancel()

	q := url.Values{}
	q.Set("file_id", fileID)
	q.Set("node", e.clientID)

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, e.serverURL+relayDownloadPath+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	e.signRelayRequest(req, relayDownloadPath, nil)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("下载被拒（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// 文件名和摘要都从响应头取，下载方无需先查元数据。
	name := decodeRelayName(resp.Header.Get("X-Mesh-File-Name"))
	if safe, err := sanitizeRelayName(name); err == nil {
		name = safe
	} else {
		// 名字不可信时退回 file_id：宁可名字难看，也不让路径成分落盘。
		name = fileID
	}

	dir := e.downloadDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "AgentMeshDownloads")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建下载目录失败: %w", err)
	}

	dst := filepath.Join(dir, fmt.Sprintf("mesh_relay_%d_%s", time.Now().Unix(), name))
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("创建落盘文件失败: %w", err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hasher), resp.Body); err != nil {
		out.Close()
		os.Remove(dst)
		return "", fmt.Errorf("写入文件失败: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("关闭落盘文件失败: %w", err)
	}

	want := strings.ToLower(strings.TrimSpace(resp.Header.Get("X-Mesh-File-SHA256")))
	if want != "" && hex.EncodeToString(hasher.Sum(nil)) != want {
		os.Remove(dst)
		return "", fmt.Errorf("文件校验和不匹配，已丢弃: %s", name)
	}
	return dst, nil
}

// signRelayRequest 给中转相关请求补上 HMAC 签名。
// 只签 path（不含 query），与服务端 c.Request.URL.Path 保持一致。
func (e *MeshEngine) signRelayRequest(req *http.Request, path string, digest []byte) {
	if e.secret == "" {
		return
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := newNonce()
	req.Header.Set(headerTimestamp, ts)
	req.Header.Set(headerNonce, nonce)
	req.Header.Set(headerSignature, computeSignature(e.secret, req.Method, path, ts, nonce, digest))
}

// hashFile 流式计算文件摘要。
func hashFile(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decodeRelayName 解出服务端 URL 编码的文件名。
func decodeRelayName(v string) string {
	if v == "" {
		return ""
	}
	if s, err := url.QueryUnescape(v); err == nil {
		return s
	}
	return v
}

// sanitizeRelayName 把文件名收敛为纯文件名，与服务端同名函数保持一致的判定，
// 避免「服务端认为合法、客户端认为非法」这类只在特定名字上出现的分歧。
func sanitizeRelayName(raw string) (string, error) {
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
