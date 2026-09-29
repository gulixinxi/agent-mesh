//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "AgentMeshClient"
	serviceDesc = "Agent Mesh 本地 AI 节点常驻客户端"
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

// installService 注册为自动启动的 Windows 服务并立即启动。
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

	if existing, err := m.OpenService(serviceName); err == nil {
		_ = existing.Close()
		return fmt.Errorf("服务 %s 已存在，请先执行 uninstall", serviceName)
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
	return nil
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
