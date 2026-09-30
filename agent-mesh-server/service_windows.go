//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "AgentMeshServer"
	serviceDesc = "Agent Mesh 中央汇总服务端"
)

// isServiceSession 判定当前是否运行在 Windows 服务会话里。
// 双击 exe 或在终端里跑都属于交互式会话，返回 false。
func isServiceSession() bool {
	interactive, err := svc.IsAnInteractiveSession()
	if err != nil {
		return false
	}
	return !interactive
}

type serviceHandler struct {
	run func(context.Context)
}

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	changes <- svc.Status{State: svc.Running, Accepts: accepts}

	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			cancel()
			return false, 0
		default:
		}
	}
	return false, 0
}

// runService 进入服务主循环，阻塞到服务被停止。
func runService(run func(context.Context)) error {
	return svc.Run(serviceName, &serviceHandler{run: run})
}

// installService 注册 Windows 服务，或在已注册时就地重启以加载新程序。
//
// 幂等是刻意为之：升级时用户只会把安装脚本再跑一遍，不会记得先卸载。
// 若沿用"已存在就报错让用户自己 uninstall"的旧行为，替换 exe 那一步会被
// 运行中的服务锁死（Copy-Item 报 file is being used by another process），
// 而这条报错完全看不出跟服务有什么关系。
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return fmt.Errorf("解析自身路径失败: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（需以管理员身份运行）: %w", err)
	}
	defer m.Disconnect()

	// 已存在：确认它指向的就是当前这个 exe，然后停旧实例、拉新实例。
	if existing, err := m.OpenService(serviceName); err == nil {
		defer existing.Close()

		if cfg, cfgErr := existing.Config(); cfgErr == nil && !sameBinaryPath(cfg.BinaryPathName, exe) {
			return fmt.Errorf("服务 %s 已存在且指向另一路径：%s\n当前程序位于：%s\n请先执行 uninstall 后再安装",
				serviceName, cfg.BinaryPathName, exe)
		}

		if err := stopService(existing); err != nil {
			return err
		}
		if err := existing.Start(); err != nil {
			return fmt.Errorf("已换用新程序但启动失败: %w", err)
		}
		fmt.Printf("服务 %s 已存在，已就地重启并加载新程序\n", serviceName)
		return nil
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: serviceDesc,
		Description: serviceDesc,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("服务已注册但启动失败: %w", err)
	}
	fmt.Printf("服务 %s 已注册并启动\n", serviceName)
	return nil
}

// stopService 优雅停止并等待到真正退出，避免后续操作撞上还没释放的文件句柄。
func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("查询服务状态失败: %w", err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("停止旧实例失败: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if st, err = s.Query(); err != nil || st.State == svc.Stopped {
			return nil
		}
	}
	return fmt.Errorf("等待旧实例停止超时（30s）")
}

// sameBinaryPath 比较服务里登记的可执行文件路径与当前程序路径。
// 服务管理器存的是命令行，通常带引号、可能还附加参数，不能直接字符串比对。
func sameBinaryPath(registered, current string) bool {
	norm := func(s string) string {
		// 顺序有讲究：先去首尾空白才剥得掉外层引号，
		// 剥完再 TrimSpace 一次，处理 "  path  " 这种两头都带的情况。
		s = strings.TrimSpace(s)
		s = strings.Trim(s, `"`)
		s = strings.TrimSpace(s)
		s = strings.ReplaceAll(s, "/", `\`)
		return strings.ToLower(s)
	}
	r := trimToExe(norm(registered))
	c := norm(current)
	return r != "" && r == c
}

// trimToExe 从可能带参数的命令行里截出可执行文件那一段。
//
// 关键是 .exe 之后必须是字符串末尾或分隔符：否则 service.exe.bak 会被当成
// service.exe，服务看着正常、实际指向一个备份文件。
func trimToExe(lower string) string {
	const ext = ".exe"
	for i := 0; i < len(lower); {
		idx := strings.Index(lower[i:], ext)
		if idx < 0 {
			return lower
		}
		end := i + idx + len(ext)
		if end == len(lower) || lower[end] == ' ' || lower[end] == '"' || lower[end] == '\t' {
			return lower[:end]
		}
		i = end
	}
	return lower
}

// uninstallService 停止并删除 Windows 服务。
func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（需以管理员身份运行）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("打开服务失败（可能尚未安装）: %w", err)
	}
	// 先尝试优雅停止，失败也无妨——删除前系统会强制终止。
	_, _ = s.Control(svc.Stop)

	if err := s.Delete(); err != nil {
		_ = s.Close()
		return fmt.Errorf("删除服务失败: %w", err)
	}
	_ = s.Close()
	return nil
}
