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
	"strings"
)

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

// LocalIPv4s 列出本机可用于局域网访问的 IPv4 地址。
//
// 过滤：回环、未指定、链路本地（169.254/16）、组播；跳过已 down 与非回环以外的网卡。
// 排序：私网地址（10./172.16-31./192.168.）优先靠前 —— 厂区内网接入要的就是它们，
// 而虚拟机 / 容器网卡上那些公网段地址通常不是目标。
func LocalIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	var private, other []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
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
			if ip.IsPrivate() {
				private = append(private, s)
			} else {
				other = append(other, s)
			}
		}
	}

	sort.Strings(private)
	sort.Strings(other)
	return append(private, other...)
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
