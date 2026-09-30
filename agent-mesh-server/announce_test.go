package main

import "testing"

// portFromAddr 决定启动日志里给客户机看的内网地址带不带端口。
// 取错会打出 "http://192.168.1.20/join/<码>" 这种少一节端口的地址。
func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{":4024", "4024"},
		{"127.0.0.1:8080", "8080"},
		{"0.0.0.0:80", "80"},
		{"[::]:4024", "4024"},
		{"4024", "4024"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := portFromAddr(tc.in); got != tc.want {
			t.Errorf("portFromAddr(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}
