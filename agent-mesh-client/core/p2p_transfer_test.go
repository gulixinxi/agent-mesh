package core

import "testing"

// TestSanitizeFileName 守卫 P0 路径穿越修复：
// 对端传来的文件名必须收敛为纯文件名，绝不允许跳出下载目录。
func TestSanitizeFileName(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"普通文件名", "report.xlsx", "report.xlsx", false},
		{"带子目录收敛为 basename", "sub/dir/report.xlsx", "report.xlsx", false},
		{"反斜杠同样收敛", `sub\dir\report.xlsx`, "report.xlsx", false},
		// 穿越尝试不需要"拒绝"，只需被中和成纯文件名后落在下载目录内。
		{"Unix 向上穿越被中和", "../../evil.exe", "evil.exe", false},
		{"Windows 向上穿越被中和", `..\..\evil.exe`, "evil.exe", false},
		{"深路径穿越被中和", "../../../../Users/Administrator/Desktop/x.exe", "x.exe", false},
		{"混合穿越被中和", "a/../../evil.exe", "evil.exe", false},
		{"纯点点", "..", "", true},
		{"当前目录", ".", "", true},
		{"空文件名", "", "", true},
		{"仅空格", "   ", "", true},
		{"内嵌点点保守拒绝", "..hidden", "", true},
	}

	for _, c := range cases {
		got, err := sanitizeFileName(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 期望拒绝 %q，实际通过并得到 %q", c.name, c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 期望通过 %q，实际报错: %v", c.name, c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: 期望 %q，实际 %q", c.name, c.want, got)
		}
	}
}
