package main

import "testing"

// enroll 与 install 共用同一套开关解析：接管与否决定机器上到底留一个还是两个实例，
// 双重否定最容易写反，所以必须由测试钉住。
func TestApplyInstallFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"默认接管", nil, true},
		{"显式关闭接管", []string{"--no-takeover"}, false},
		{"enroll 常规参数不影响默认", []string{"--server", "http://10.0.0.5:8080", "--code-stdin"}, true},
		{"开关藏在参数之后", []string{"--server", "http://x", "--no-takeover"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installTakeover = true
			applyInstallFlags(c.args)
			if installTakeover != c.want {
				t.Fatalf("applyInstallFlags(%v) 后 installTakeover=%v，期望 %v",
					c.args, installTakeover, c.want)
			}
		})
	}
	installTakeover = true
}

func TestHasFlag(t *testing.T) {
	if !hasFlag([]string{"a", "--x"}, "--x") {
		t.Fatal("应当找到位于尾部的开关")
	}
	if hasFlag([]string{"a", "b"}, "--x") {
		t.Fatal("不该把普通参数当成开关")
	}
	if hasFlag(nil, "--x") {
		t.Fatal("空参数集不该命中")
	}
}
