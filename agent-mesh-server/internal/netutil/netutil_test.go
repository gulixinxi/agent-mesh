package netutil

import (
	"net"
	"testing"
)

func TestIsLoopbackBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"回环带端口", "http://127.0.0.1:4024", true},
		{"回环不带端口", "http://127.0.0.1", true},
		{"回环无 scheme", "127.0.0.1:4024", true},
		{"127/8 整段都算", "http://127.0.0.2:8080", true},
		{"localhost", "http://localhost:4024", true},
		{"IPv6 回环带方括号", "http://[::1]:4024", true},
		{"监听通配地址不是可路由地址", "http://0.0.0.0:4024", true},
		{"空串没有可用地址", "", true},

		{"内网 IP", "http://192.168.1.20:4024", false},
		{"10 段内网", "http://10.0.0.5", false},
		{"172 段内网", "https://172.16.3.9:4024", false},
		{"公网域名", "https://mesh.example.com", false},
		{"主机名（无法判断，按可路由处理）", "http://mesh-server:4024", false},
		{"隧道地址", "https://abc.trycloudflare.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLoopbackBaseURL(tc.in); got != tc.want {
				t.Errorf("IsLoopbackBaseURL(%q) = %v, 期望 %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:4024", true},
		{"[::1]:4024", true},
		{"::1", true},
		{"localhost", true},
		{"LOCALHOST:4024", true},
		{"0.0.0.0", true},
		{" 127.0.0.1 ", true},
		{"192.168.1.20:4024", false},
		{"example.com:443", false},
		{"8.8.8.8", false},
	}

	for _, tc := range cases {
		if got := IsLoopbackHost(tc.in); got != tc.want {
			t.Errorf("IsLoopbackHost(%q) = %v, 期望 %v", tc.in, got, tc.want)
		}
	}
}

// TestLocalIPv4sFiltersUnusable 只做「不返回不可用地址」的硬断言：
// 具体有哪些网卡依赖运行环境，断言具体值会在别的机器上误报。
func TestLocalIPv4sFiltersUnusable(t *testing.T) {
	for _, s := range LocalIPv4s() {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("返回了无法解析的地址: %q", s)
		}
		if ip.To4() == nil {
			t.Errorf("返回了非 IPv4 地址: %q", s)
		}
		if ip.IsLoopback() {
			t.Errorf("返回了回环地址: %q", s)
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("返回了链路本地地址: %q", s)
		}
		if ip.IsUnspecified() {
			t.Errorf("返回了未指定地址: %q", s)
		}
	}
}

// TestLocalIPv4sPrivateFirst 私网地址必须排在前面，否则厂区内网场景下
// 管理者看到的第一个候选地址可能是虚拟网卡。
func TestLocalIPv4sPrivateFirst(t *testing.T) {
	list := LocalIPv4s()
	seenPublic := false
	for _, s := range list {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip.IsPrivate() {
			if seenPublic {
				t.Fatalf("私网地址 %s 排在了公网段地址之后: %v", s, list)
			}
			continue
		}
		seenPublic = true
	}
}
