package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// FileTransferProtocolID 是 Mesh 内部文件传输的私有协议标识。
const FileTransferProtocolID = protocol.ID("/agentmesh/file/1.0.0")

// DiscoveryServiceTag 是局域网 mDNS 发现的服务名，同网段节点靠它互相发现。
const DiscoveryServiceTag = "agentmesh-p2p"

// maxStreams 限制并发接收的文件流数量，避免被同一节点刷爆磁盘句柄。
const maxStreams = 8

// maxFileSize 限制单个接收文件的大小，防止对端声明超大长度把磁盘写满。
const maxFileSize = 2 << 30 // 2GiB

// P2PTransferManager 负责节点之间的点对点文件直传。
type P2PTransferManager struct {
	Host         host.Host
	downloadDir  string
	peerRegistry sync.Map
	allowedPeers sync.Map // peer.ID -> struct{}；非空时只接受白名单内节点
	streamSem    chan struct{}
}

// NewP2PTransferManager 在指定端口启动 libp2p 主机，并注册文件流处理器与 mDNS 发现。
func NewP2PTransferManager(listenPort int, downloadDir string) (*P2PTransferManager, error) {
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return nil, fmt.Errorf("创建下载目录失败: %w", err)
	}

	// 注意：libp2p 自 v0.33 起把 Noise()/Yamux() 移出了根包。
	// 这里依赖 libp2p.New 的默认配置（已包含 Noise/TLS 安全通道与 Yamux 多路复用），
	// 如需显式指定，改用 libp2p.Security(noise.ID, noise.New) 与 libp2p.Muxer(yamux.ID, yamux.DefaultTransport)。
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", listenPort)),
	)
	if err != nil {
		return nil, fmt.Errorf("启动 libp2p 主机失败: %w", err)
	}

	mgr := &P2PTransferManager{
		Host:        h,
		downloadDir: downloadDir,
		streamSem:   make(chan struct{}, maxStreams),
	}
	h.SetStreamHandler(FileTransferProtocolID, mgr.handleIncomingFileStream)
	_ = mdns.NewMdnsService(h, DiscoveryServiceTag, &mdnsNotifee{mgr: mgr}).Start()
	return mgr, nil
}

// ListenAddresses 返回本机可对外公布的 libp2p 地址，方便日志排障。
func (p *P2PTransferManager) ListenAddresses() []string {
	addrs := make([]string, 0, len(p.Host.Addrs()))
	for _, a := range p.Host.Addrs() {
		addrs = append(addrs, fmt.Sprintf("%s/p2p/%s", a.String(), p.Host.ID().String()))
	}
	return addrs
}

// Close 关闭 libp2p 主机。
func (p *P2PTransferManager) Close() error { return p.Host.Close() }

// AllowPeer 把指定 PeerID 加入文件传输白名单。
// 调用过本方法即进入严格模式：只有白名单内的节点能推送文件。
func (p *P2PTransferManager) AllowPeer(peerIDStr string) error {
	id, err := peer.Decode(peerIDStr)
	if err != nil {
		return fmt.Errorf("非法的 PeerID %q: %w", peerIDStr, err)
	}
	p.allowedPeers.Store(id, struct{}{})
	return nil
}

// AllowedPeerCount 返回白名单中的节点数量；0 表示处于宽松模式。
func (p *P2PTransferManager) AllowedPeerCount() int {
	n := 0
	p.allowedPeers.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// isAllowed 判断对端是否有权推送文件。白名单为空时放行（仅依赖内网边界）。
func (p *P2PTransferManager) isAllowed(id peer.ID) bool {
	if p.AllowedPeerCount() == 0 {
		return true
	}
	_, ok := p.allowedPeers.Load(id)
	return ok
}

// sanitizeFileName 把对端传来的文件名收敛为纯文件名。
// 不做这步的话，形如 "../../Desktop/x.exe" 的名字经 filepath.Join 规范化后
// 会跳出下载目录，变成任意路径写入漏洞。
func sanitizeFileName(raw string) (string, error) {
	// 统一分隔符后只取 basename，天然去掉目录前缀。
	name := filepath.Base(strings.ReplaceAll(raw, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.Contains(name, "..") {
		return "", fmt.Errorf("非法文件名: %q", raw)
	}
	if len([]byte(name)) > 255 {
		return "", fmt.Errorf("文件名过长: %q", raw)
	}
	return name, nil
}

// SendFileToPeer 把本地文件推送给指定 PeerID 的节点。
func (p *P2PTransferManager) SendFileToPeer(ctx context.Context, targetPeerIDStr, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开待发送文件失败: %w", err)
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return err
	}

	name := filepath.Base(filePath)
	if len(name) > 255 {
		return fmt.Errorf("文件名过长（超过 255 字节）: %s", filePath)
	}

	targetPeerID, err := peer.Decode(targetPeerIDStr)
	if err != nil {
		return fmt.Errorf("非法的 PeerID: %w", err)
	}

	raw, ok := p.peerRegistry.Load(targetPeerID)
	if !ok {
		return fmt.Errorf("目标节点未在局域网内发现或已离线: %s", targetPeerIDStr)
	}
	addrInfo := raw.(peer.AddrInfo)

	if err := p.Host.Connect(ctx, addrInfo); err != nil {
		return fmt.Errorf("连接目标节点失败: %w", err)
	}

	stream, err := p.Host.NewStream(ctx, targetPeerID, FileTransferProtocolID)
	if err != nil {
		return fmt.Errorf("建立文件流失败: %w", err)
	}
	defer stream.Close()

	// 传输帧：[1 字节文件名长度][文件名][8 字节小端文件大小][文件内容][32 字节 SHA256]
	if _, err := stream.Write([]byte{byte(len(name))}); err != nil {
		return err
	}
	if _, err := stream.Write([]byte(name)); err != nil {
		return err
	}
	sizeBuf := make([]byte, 8)
	size := fi.Size()
	for i := 0; i < 8; i++ {
		sizeBuf[i] = byte(size >> (i * 8))
	}
	if _, err := stream.Write(sizeBuf); err != nil {
		return err
	}

	hasher := sha256.New()
	if _, err := io.CopyBuffer(io.MultiWriter(stream, hasher), file, make([]byte, 64*1024)); err != nil {
		return fmt.Errorf("发送文件内容失败: %w", err)
	}
	if _, err := stream.Write(hasher.Sum(nil)); err != nil {
		return err
	}
	return nil
}

// handleIncomingFileStream 处理对端推来的文件，SHA256 校验不一致则删除。
func (p *P2PTransferManager) handleIncomingFileStream(stream network.Stream) {
	select {
	case p.streamSem <- struct{}{}:
		defer func() { <-p.streamSem }()
	default:
		fmt.Println("[P2P] 并发文件流已达上限，拒绝本次传输")
		_ = stream.Reset()
		return
	}
	defer stream.Close()

	remotePeer := stream.Conn().RemotePeer()
	remote := remotePeer.String()

	// 节点鉴权：白名单非空时，非授权节点一律斩断连接。
	if !p.isAllowed(remotePeer) {
		fmt.Printf("[P2P] 拒绝未授权节点的文件传输: %s\n", remote)
		_ = stream.Reset()
		return
	}

	lenBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		fmt.Printf("[P2P] 读取文件名长度失败 (%s): %v\n", remote, err)
		return
	}
	nameBuf := make([]byte, int(lenBuf[0]))
	if _, err := io.ReadFull(stream, nameBuf); err != nil {
		fmt.Printf("[P2P] 读取文件名失败 (%s): %v\n", remote, err)
		return
	}
	sizeBuf := make([]byte, 8)
	if _, err := io.ReadFull(stream, sizeBuf); err != nil {
		fmt.Printf("[P2P] 读取文件大小失败 (%s): %v\n", remote, err)
		return
	}
	var fileSize int64
	for i := 0; i < 8; i++ {
		fileSize |= int64(sizeBuf[i]) << (i * 8)
	}
	if fileSize < 0 {
		fmt.Printf("[P2P] 非法文件大小 (%s): %d\n", remote, fileSize)
		return
	}
	if fileSize > maxFileSize {
		fmt.Printf("[P2P] 文件大小超过上限 %d 字节 (%s)，拒绝接收\n", maxFileSize, remote)
		_ = stream.Reset()
		return
	}

	// 文件名必须收敛为纯文件名，杜绝 ../ 路径穿越。
	safeName, err := sanitizeFileName(string(nameBuf))
	if err != nil {
		fmt.Printf("[P2P] %v (%s)，拒绝接收\n", err, remote)
		_ = stream.Reset()
		return
	}
	// 名字被改写说明对端带了路径成分，落盘安全但仍留一条可审计痕迹。
	if safeName != string(nameBuf) {
		fmt.Printf("[P2P] 注意：对端文件名 %q 含路径成分，已收敛为 %q (%s)\n",
			string(nameBuf), safeName, remote)
	}

	dstPath := filepath.Join(p.downloadDir, fmt.Sprintf("mesh_%d_%s", time.Now().Unix(), safeName))
	// 兜底校验：即使上面的清洗有遗漏，也确保落盘路径不越出下载目录。
	if rel, relErr := filepath.Rel(p.downloadDir, dstPath); relErr != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Printf("[P2P] 落盘路径越出下载目录 (%s)，拒绝接收\n", remote)
		_ = stream.Reset()
		return
	}

	dstFile, err := os.Create(dstPath)
	if err != nil {
		fmt.Printf("[P2P] 创建落盘文件失败 %s: %v\n", dstPath, err)
		return
	}
	defer dstFile.Close()

	hasher := sha256.New()
	if _, err := io.CopyBuffer(io.MultiWriter(dstFile, hasher), io.LimitReader(stream, fileSize), make([]byte, 64*1024)); err != nil {
		os.Remove(dstPath)
		fmt.Printf("[P2P] 接收文件失败 %s: %v\n", dstPath, err)
		return
	}

	remoteChecksum := make([]byte, 32)
	if _, err := io.ReadFull(stream, remoteChecksum); err != nil {
		os.Remove(dstPath)
		fmt.Printf("[P2P] 读取校验和失败 (%s): %v\n", remote, err)
		return
	}

	if hex.EncodeToString(hasher.Sum(nil)) != hex.EncodeToString(remoteChecksum) {
		os.Remove(dstPath)
		fmt.Printf("[P2P] 校验和不匹配，已丢弃: %s\n", dstPath)
		return
	}
	fmt.Printf("[P2P] 内网大文件字节直传验证落盘成功: %s (%d bytes, from %s)\n", dstPath, fileSize, remote)
}

// mdnsNotifee 维护已发现节点的地址表；未被发现不代表离线，仅表示 mDNS 未通告。
type mdnsNotifee struct{ mgr *P2PTransferManager }

func (m *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == m.mgr.Host.ID() {
		return
	}
	m.mgr.peerRegistry.Store(pi.ID, pi)
	fmt.Printf("[P2P] 发现邻居节点: %s\n", pi.ID.String())
}
