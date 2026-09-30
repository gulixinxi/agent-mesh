// Package netutil 收拢与「本机对外地址」有关的小工具。
//
// 起因（真实踩到）：邀请码落地页按「请求里带的 Host」拼接入命令。
// 管理者若从 127.0.0.1 打开页面，拼出来的命令就带 127.0.0.1，
// 发给客户机器必然连不通，而报错是「连接被拒绝」，很难一眼归因。
// 把「判断 + 候选地址」收到一处，页面与启动日志共用同一套口径。
package netutil

import (
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Addr 是一个候选访问地址及其来源网卡。
//
// 为什么要带网卡名：一台机器常常同时有真实网卡和虚拟网卡
// （WSL / VMware / VirtualBox / Hyper-V / Docker / VPN），
// 客户机只能访问真实网卡那个地址。只给一串 IP 让人挑，很容易挑错；
// 带上网卡名和「疑似虚拟网卡」标记，才是能用的提示。
type Addr struct {
	IP      string
	Iface   string // 网卡名，如 "以太网" / "vEthernet (WSL)"
	Virtual bool   // 疑似虚拟网卡，客户机大概率访问不到
}

// IsLoopbackBaseURL 判断基址是否「只在本机有效」。
//
// 命中：127.0.0.0/8、::1、localhost、0.0.0.0（监听通配，不是可路由地址）、空主机。
// 这类地址生成的接入命令，拿到别的机器上一定失败。
func IsLoopbackBaseURL(baseURL string) bool {
	return IsLoopbackHost(hostOf(baseURL))
}

// IsLoopbackHost 判断 host 是否是本机专用地址。
// 允许带端口（192.168.1.20:4024），也允许带方括号的 IPv6（[::1]:4024）。
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return true // 没写主机 = 没有可用地址，按「只在本机」处理更安全
	}

	// 剥端口。IPv6 是 [::1]:4024 这种写法：先剥方括号，再处理剩下的 :port。
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 0 {
			host = host[1:end]
		}
	} else if strings.Count(host, ":") == 1 {
		// 只有一个冒号才是 host:port；纯 IPv6 有多个冒号，保持原样。
		host = host[:strings.LastIndex(host, ":")]
	}

	switch strings.ToLower(host) {
	case "localhost", "0.0.0.0":
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false // 是个主机名，无法判断，按可路由处理
	}
	return ip.IsLoopback() || ip.IsUnspecified()
}

// virtualIfaceHints 是「疑似虚拟/隧道网卡」的名字特征（小写匹配）。
// 只用于排序与标注，不做剔除 —— 真实网卡都不在时，虚拟地址也是唯一线索。
var virtualIfaceHints = []string{
	"vethernet", "wsl", "vmware", "virtualbox", "vbox", "hyper-v",
	"docker", "veth", "tap-", "tun", "tailscale", "zerotier",
	"openvpn", "wireguard", "npcap", "loopback", "hamachi", "radmin",
}

// LooksVirtualIface 判断网卡名是否像虚拟/隧道网卡。
func LooksVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, h := range virtualIfaceHints {
		if strings.Contains(n, h) {
			return true
		}
	}
	return false
}

// LocalAddrs 列出本机可用于局域网访问的 IPv4 地址，**已按可用性排序**：
//
//	① 真实网卡优先，疑似虚拟网卡（WSL/VMware/…）靠后；
//	② 同组内私网地址（10./172.16-31./192.168.）优先；
//	③ 最后按 IP 数值升序，保证输出稳定可测。
//
// 过滤：回环、未指定、链路本地（169.254/16）、组播；跳过已 down 的网卡。
func LocalAddrs() []Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	var addrs []Addr
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		got, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range got {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil { // 只要 IPv4
				continue
			}
			if ip.IsLoopback() || ip.IsUnspecified() ||
				ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			s := ip.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			addrs = append(addrs, Addr{
				IP:      s,
				Iface:   ifi.Name,
				Virtual: LooksVirtualIface(ifi.Name),
			})
		}
	}

	sort.SliceStable(addrs, func(i, j int) bool {
		a, b := addrs[i], addrs[j]
		if a.Virtual != b.Virtual {
			return !a.Virtual // 真实网卡在前
		}
		if ap, bp := isPrivateIP(a.IP), isPrivateIP(b.IP); ap != bp {
			return ap // 私网在前
		}
		return lessIPv4(a.IP, b.IP)
	})
	return addrs
}

// LocalIPv4s 是 LocalAddrs 的便捷包装，只取地址串，顺序一致。
func LocalIPv4s() []string {
	addrs := LocalAddrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out
}

func isPrivateIP(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && ip.IsPrivate()
}

// lessIPv4 按四段数值比较，避免字符串比较把 "10.0.0.9" 排到 "10.0.0.10" 之后。
func lessIPv4(a, b string) bool {
	pa, pb := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if pa == nil || pb == nil {
		return a < b
	}
	for i := 0; i < 4; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// hostOf 从 baseURL 里取出主机（含端口）。解析不了时退回原始字符串。
func hostOf(baseURL string) string {
	s := strings.TrimSpace(baseURL)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	// 没带 scheme（例如直接填 "192.168.1.20:4024"）时 url.Parse 会报错，
	// 这里退化成手工剥离 scheme 分隔符之后的部分。
	s = strings.TrimPrefix(s, "//")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// FormatHostPort 把 IP 与端口拼成 host:port；端口非法或为空时只返回 IP。
func FormatHostPort(ip, port string) string {
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || n <= 0 {
		return ip
	}
	return net.JoinHostPort(ip, strconv.Itoa(n))
}
