//go:build windows

package main

import "testing"

// 服务升级时要靠这个判断决定「就地重启」还是「拒绝并让用户先卸载」。
// 判错两个方向都有害：漏判会让服务一直跑旧路径的程序，
// 误判则每次重装都报路径冲突。
func TestSameBinaryPath(t *testing.T) {
	cases := []struct {
		name       string
		registered string
		current    string
		want       bool
	}{
		{"完全一致", `C:\Program Files\AgentMesh\Server\server.exe`, `C:\Program Files\AgentMesh\Server\server.exe`, true},
		{"服务管理器存的通常带引号", `"C:\Program Files\AgentMesh\Server\server.exe"`, `C:\Program Files\AgentMesh\Server\server.exe`, true},
		{"大小写不敏感", `C:\PROGRAM FILES\AGENTMESH\SERVER\SERVER.EXE`, `C:\Program Files\AgentMesh\Server\server.exe`, true},
		{"斜杠方向相反", `C:/Program Files/AgentMesh/Server/server.exe`, `C:\Program Files\AgentMesh\Server\server.exe`, true},
		{"带启动参数", `"C:\Program Files\AgentMesh\Server\server.exe" -addr :8080`, `C:\Program Files\AgentMesh\Server\server.exe`, true},
		{"前后有空白", `  "C:\Program Files\AgentMesh\Server\server.exe"  `, `C:\Program Files\AgentMesh\Server\server.exe`, true},
		{"换成旧目录", `C:\Program Files\AgentMesh\server.exe`, `C:\Program Files\AgentMesh\Server\server.exe`, false},
		{"不同文件名", `C:\Program Files\AgentMesh\Server\client.exe`, `C:\Program Files\AgentMesh\Server\server.exe`, false},
		{"注册值为空", ``, `C:\Program Files\AgentMesh\Server\server.exe`, false},
		{"前缀包含关系不得误判", `C:\Program Files\AgentMesh\Server\server.exe.bak`, `C:\Program Files\AgentMesh\Server\server.exe`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameBinaryPath(c.registered, c.current); got != c.want {
				t.Errorf("sameBinaryPath(%q, %q) = %v, 期望 %v", c.registered, c.current, got, c.want)
			}
		})
	}
}
