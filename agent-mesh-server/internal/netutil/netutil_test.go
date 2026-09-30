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

// TestLocalAddrsRealBeforeVirtual 是本包最要紧的一条排序规则：
// 客户机只能访问真实网卡地址，虚拟网卡（WSL/VMware/Docker…）必须排在后面。
// 现场踩到的正是这个：列表头一个是 172.28.160.1（虚拟），
// 真实地址 192.168.2.131 被挤到后面，管理员很容易挑错。
func TestLocalAddrsRealBeforeVirtual(t *testing.T) {
	addrs := LocalAddrs()
	seenVirtual := false
	for _, a := range addrs {
		if a.Virtual {
			seenVirtual = true
			continue
		}
		if seenVirtual {
			t.Fatalf("真实网卡地址 %s(%s) 排在了虚拟地址之后: %+v", a.IP, a.Iface, addrs)
		}
	}
}

// TestLocalAddrsPrivateBeforePublicWithinGroup 同组内私网优先。
func TestLocalAddrsPrivateBeforePublicWithinGroup(t *testing.T) {
	addrs := LocalAddrs()
	seenPublic := [2]bool{}
	for _, a := range addrs {
		idx := 0
		if a.Virtual {
			idx = 1
		}
		if isPrivateIP(a.IP) {
			if seenPublic[idx] {
				t.Fatalf("私网地址 %s 排在同组公网段地址之后: %+v", a.IP, addrs)
			}
			continue
		}
		seenPublic[idx] = true
	}
}

func TestLooksVirtualIface(t *testing.T) {
	virtual := []string{
		"vEthernet (WSL)", "vEthernet (Default Switch)", "VMware Network Adapter VMnet8",
		"VirtualBox Host-Only Network", "docker0", "docker_gwbridge", "veth1234",
		"Tailscale", "ZeroTier One [abc]", "utun3", "TAP-Windows Adapter V9",
	}
	for _, n := range virtual {
		if !LooksVirtualIface(n) {
			t.Errorf("LooksVirtualIface(%q) = false, 期望 true", n)
		}
	}
	// 真实网卡名里可能含 bridge 之类的词，不能误杀；
	// 特别注意 "br-lan"（OpenWrt 上的 LAN 网桥）就是真实业务网卡，故意不按虚拟处理。
	real := []string{"以太网", "WLAN", "Ethernet", "Wi-Fi", "en0", "eth0", "本地连接", "br-lan"}
	for _, n := range real {
		if LooksVirtualIface(n) {
			t.Errorf("LooksVirtualIface(%q) = true, 期望 false", n)
		}
	}
}

func TestLessIPv4Numeric(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10.0.0.9", "10.0.0.10", true}, // 字符串比较会判错，必须按数值
		{"10.0.0.10", "10.0.0.9", false},
		{"192.168.2.131", "192.168.2.20", false},
		{"192.168.2.20", "192.168.2.131", true},
	}
	for _, tc := range cases {
		if got := lessIPv4(tc.a, tc.b); got != tc.want {
			t.Errorf("lessIPv4(%q, %q) = %v, 期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFormatHostPort(t *testing.T) {
	cases := []struct {
		ip, port, want string
	}{
		{"192.168.2.131", "4024", "192.168.2.131:4024"},
		{"192.168.2.131", "", "192.168.2.131"},
		{"192.168.2.131", "abc", "192.168.2.131"},
		{"192.168.2.131", "0", "192.168.2.131"},
		{"::1", "80", "[::1]:80"},
	}
	for _, tc := range cases {
		if got := FormatHostPort(tc.ip, tc.port); got != tc.want {
			t.Errorf("FormatHostPort(%q, %q) = %q, 期望 %q", tc.ip, tc.port, got, tc.want)
		}
	}
}
