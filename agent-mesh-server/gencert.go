package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// 内网部署没有公网域名，无从申请公信证书，只能自签。
// 所以把证书生成做成服务端自带子命令：装了服务端就有工具，不依赖 openssl，也不依赖 Go 环境。
//
// 用法：
//   agent-mesh-server gencert [-out <目录>]
//
// 产出三个文件：
//   ca.pem          自签 CA 证书 —— 分发给每个客户端，配到 tls_ca
//   server.pem      服务端证书
//   server-key.pem  服务端私钥 —— 权限敏感，不要分发

const (
	caValidYears     = 10
	serverValidYears = 3
	rsaKeyBits       = 2048
)

// generateCerts 生成自签 CA 与服务端证书，写入 outDir。
func generateCerts(outDir string) (caPath, certPath, keyPath string, err error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("创建输出目录失败: %w", err)
	}

	caKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return "", "", "", fmt.Errorf("生成 CA 私钥失败: %w", err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "Agent Mesh 内网 CA", Organization: []string{"Agent Mesh"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(caValidYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", fmt.Errorf("签发 CA 证书失败: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", "", "", fmt.Errorf("解析 CA 证书失败: %w", err)
	}

	srvKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return "", "", "", fmt.Errorf("生成服务端私钥失败: %w", err)
	}

	hosts, ips := localIdentities()
	srvTpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject: pkix.Name{
			CommonName:   hosts[0],
			Organization: []string{"Agent Mesh"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(serverValidYears, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hosts,
		IPAddresses:           ips,
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", fmt.Errorf("签发服务端证书失败: %w", err)
	}

	caPath = filepath.Join(outDir, "ca.pem")
	certPath = filepath.Join(outDir, "server.pem")
	keyPath = filepath.Join(outDir, "server-key.pem")

	if err := writePEM(caPath, "CERTIFICATE", caDER, 0o644); err != nil {
		return "", "", "", err
	}
	if err := writePEM(certPath, "CERTIFICATE", srvDER, 0o644); err != nil {
		return "", "", "", err
	}
	// 私钥只给本机服务账户读取，避免同机其他用户窥探。
	if err := writePEM(keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(srvKey), 0o600); err != nil {
		return "", "", "", err
	}
	return caPath, certPath, keyPath, nil
}

// localIdentities 收集证书要写进 SAN 的主机名与本机组网 IP。
// 客户端按 IP 访问时，证书里没有对应 SAN 会直接握手失败——这是自签证书最常见的坑。
func localIdentities() (hosts []string, ips []net.IP) {
	hosts = []string{"localhost", "agent-mesh"}
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts = append(hosts, name)
	}
	ips = append(ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return hosts, ips
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			ips = append(ips, v4)
		}
	}
	return hosts, ips
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if buf == nil {
		return fmt.Errorf("PEM 编码失败: %s", path)
	}
	if err := os.WriteFile(path, buf, perm); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

func newSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
