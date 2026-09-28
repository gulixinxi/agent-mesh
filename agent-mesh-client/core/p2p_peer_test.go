package core

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestPeerExpiry 守卫 mDNS 邻居表的下线清理：
// 长时间未通告的邻居必须被剔除，新鲜的不能被误删。
func TestPeerExpiry(t *testing.T) {
	mgr := &P2PTransferManager{stopGC: make(chan struct{})}

	stale := peer.ID("12D3KooStaleNode")
	fresh := peer.ID("12D3KooFreshNode")

	mgr.peerRegistry.Store(stale, peerEntry{lastSeen: time.Now().Add(-peerTTL - time.Minute)})
	mgr.peerRegistry.Store(fresh, peerEntry{lastSeen: time.Now()})

	if mgr.KnownPeerCount() != 2 {
		t.Fatalf("初始应有 2 个邻居，实际 %d", mgr.KnownPeerCount())
	}

	mgr.expirePeers()

	if _, ok := mgr.peerRegistry.Load(stale); ok {
		t.Error("过期邻居未被清理，内存拓扑会持续膨胀")
	}
	if _, ok := mgr.peerRegistry.Load(fresh); !ok {
		t.Error("新鲜邻居被误删")
	}
	if n := mgr.KnownPeerCount(); n != 1 {
		t.Errorf("清理后应剩 1 个邻居，实际 %d", n)
	}
	if ids := mgr.KnownPeers(); len(ids) != 1 || ids[0] != fresh.String() {
		t.Errorf("KnownPeers 返回异常: %v", ids)
	}
}
