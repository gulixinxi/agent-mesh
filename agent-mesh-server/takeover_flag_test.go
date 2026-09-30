package main

import "testing"

// 接管的默认行为必须被测试钉住：它决定了机器上到底留一个还是两个实例。
// 注册服务的真实效果没法在 CI 里断言，但"开关有没有被正确解析"可以，
// 而这恰恰是最容易写反的地方（双重否定）。
func TestApplyInstallFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"默认接管", nil, true},
		{"显式关闭接管", []string{"--no-takeover"}, false},
		{"无关参数不影响默认", []string{"--addr", ":8099"}, true},
		{"开关藏在参数之后", []string{"--force", "--no-takeover"}, false},
		{"不完全匹配的写法不算数", []string{"--no-takeover=true", "-no-takeover"}, true},
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
