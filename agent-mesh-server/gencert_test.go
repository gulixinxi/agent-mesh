package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func loadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取证书失败 %s: %v", path, err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("PEM 解析失败: %s", path)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("证书解析失败 %s: %v", path, err)
	}
	return cert
}

// containsIP 判断 IP SAN 里是否含指定地址。
func containsIP(cert *x509.Certificate, want string) bool {
	for _, ip := range cert.IPAddresses {
		if ip.String() == want {
			return true
		}
	}
	return false
}

func containsDNS(cert *x509.Certificate, want string) bool {
	for _, n := range cert.DNSNames {
		if n == want {
			return true
		}
	}
	return false
}

// TestGenerateCertsExtraHosts 跨网部署的核心保障：
// -host 给的域名要进 DNS SAN，IP 要进 IP SAN，且签名链能被 CA 验通。
func TestGenerateCertsExtraHosts(t *testing.T) {
	dir := t.TempDir()
	extra := []string{"mesh.example.com", "203.0.113.9", "  ", "10.20.30.40"}

	caPath, certPath, keyPath, err := generateCerts(dir, extra)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	for _, p := range []string{caPath, certPath, keyPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("应生成文件 %s: %v", p, err)
		}
	}

	srv := loadCert(t, certPath)
	ca := loadCert(t, caPath)

	if !containsDNS(srv, "mesh.example.com") {
		t.Errorf("域名未写入 SAN: %v", srv.DNSNames)
	}
	if !containsIP(srv, "203.0.113.9") {
		t.Errorf("公网 IP 未写入 SAN: %v", srv.IPAddresses)
	}
	if !containsIP(srv, "10.20.30.40") {
		t.Errorf("第二个 IP 未写入 SAN: %v", srv.IPAddresses)
	}
	// 空串不应污染 SAN
	for _, n := range srv.DNSNames {
		if n == "" {
			t.Errorf("空字符串混入 DNS SAN: %v", srv.DNSNames)
		}
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := srv.Verify(x509.VerifyOptions{DNSName: "mesh.example.com", Roots: roots}); err != nil {
		t.Errorf("按域名验签失败（客户端会握手失败）: %v", err)
	}
	if certPath == keyPath {
		t.Error("证书与私钥不应是同一路径")
	}
}

// TestGenerateCertsNoExtra 不传 -host 时仍需保持原有行为：本机身份可用。
func TestGenerateCertsNoExtra(t *testing.T) {
	dir := t.TempDir()
	_, certPath, _, err := generateCerts(dir, nil)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}

	srv := loadCert(t, certPath)
	if !containsDNS(srv, "localhost") {
		t.Errorf("默认身份 localhost 丢失: %v", srv.DNSNames)
	}
	if !containsDNS(srv, "agent-mesh") {
		t.Errorf("默认身份 agent-mesh 丢失: %v", srv.DNSNames)
	}
	if !containsIP(srv, "127.0.0.1") {
		t.Errorf("默认回环 IP 丢失: %v", srv.IPAddresses)
	}
	if srv.NotAfter.Before(srv.NotBefore) {
		t.Error("证书有效期区间异常")
	}
	if _, err := os.Stat(filepath.Join(dir, "server-key.pem")); err != nil {
		t.Fatalf("私钥文件缺失: %v", err)
	}
}

// TestLocalIdentitiesSplit 校验分类逻辑本身：IP 归 IP、其余归域名。
func TestLocalIdentitiesSplit(t *testing.T) {
	hosts, ips := localIdentities([]string{"a.example.com", "198.51.100.7", "b.example.com"})

	found := map[string]bool{}
	for _, h := range hosts {
		found[h] = true
	}
	if !found["a.example.com"] || !found["b.example.com"] {
		t.Errorf("域名未进 hosts: %v", hosts)
	}
	hasIP := false
	for _, ip := range ips {
		if ip.String() == "198.51.100.7" {
			hasIP = true
		}
	}
	if !hasIP {
		t.Errorf("IP 未进 ips: %v", ips)
	}
}
