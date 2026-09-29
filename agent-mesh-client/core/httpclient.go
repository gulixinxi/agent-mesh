package core

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// NewHTTPClient 构造访问中枢用的 HTTP 客户端。
//
// 内网服务端一般用自签证书（没有公网域名，无从签发公信证书），
// 所以必须能把自签 CA 加进信任列表，否则 TLS 握手直接失败。
//
// caPath   非空时把该 CA 附加到信任池（可与系统根证书并存）
// insecure 为真时完全跳过证书校验，仅供联调，生产禁用
func NewHTTPClient(caPath string, insecure bool) (*http.Client, error) {
	if insecure {
		fmt.Fprintln(os.Stderr, "[安全] 警告：已开启跳过证书校验（tls_insecure），链路可被中间人窃听，仅限联调使用")
		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 - 显式由 tls_insecure 开关控制
			},
		}, nil
	}

	if caPath == "" {
		return http.DefaultClient, nil
	}

	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA 证书文件中没有有效证书: %s", caPath)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}, nil
}
