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

// installService 把「当前进程自己的可执行文件」注册为自动启动的 Windows 服务并立即启动。
//
// 保留这个无参版本是为了兼容既有的 `client.exe install` 子命令；
// enroll 流程必须用 installServiceAt —— 它要把服务指向安装目录里的副本，
// 而不是引导脚本下载到临时目录的那一份。
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败: %w", err)
	}
	return installServiceAt(exe)
}

// installServiceAt 注册服务并把它的可执行文件路径固定为 exePath。
func installServiceAt(exePath string) error {
	exe, err := filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("解析可执行文件路径失败: %w", err)
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

// isServiceExistsErr 判断错误是否表示「服务已经注册」。
//
// 放在平台文件里：这句话是 installServiceAt 自己写出来的，
// 判断和产生它的地方挨着，改文案时不会漏掉判断。
func isServiceExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "已存在")
}

// requirePrivilege 探测当前进程是否有管理员权限。
//
// 实现方式是「连一下服务控制管理器」：没有管理员权限时它会直接拒绝，
// 比去查令牌组是否含 Administrators 更直接，也不必引入额外的 API。
func requirePrivilege() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("需要管理员权限：请右键 PowerShell 选「以管理员身份运行」后重试（%w）", err)
	}
	m.Disconnect()
	return nil
}

// stopService 停止服务（不删除）。
// enroll 覆盖安装程序文件前需要它：Windows 上正在运行的程序文件是被锁住的。
func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（需以管理员身份运行）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("服务未注册: %w", err)
	}
	defer s.Close()

	_, _ = s.Control(svc.Stop)
	for i := 0; i < 40; i++ {
		st, qerr := s.Query()
		if qerr != nil {
			break
		}
		if st.State == svc.Stopped {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

// restartService 重启已注册的服务。
// enroll 重新执行时用它让新写入的配置生效（服务只在启动时读一次配置）。
func restartService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败（需以管理员身份运行）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("服务未注册: %w", err)
	}
	defer s.Close()

	// 先停；已在停止中的服务会返回错误，忽略即可。
	_, _ = s.Control(svc.Stop)
	// 给 SCM 一点时间完成状态迁移，否则紧接着的 Start 可能报「服务正在停止」。
	for i := 0; i < 20; i++ {
		st, qerr := s.Query()
		if qerr != nil {
			break
		}
		if st.State == svc.Stopped {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("重启服务失败: %w", err)
	}
	return nil
}

// serviceStatus 查询服务当前是否在运行。
func serviceStatus() (bool, string) {
	m, err := mgr.Connect()
	if err != nil {
		return false, "无法连接服务控制管理器（需管理员权限）: " + err.Error()
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return false, "服务尚未注册"
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return false, "查询服务状态失败: " + err.Error()
	}
	if st.State == svc.Running {
		return true, "运行中"
	}
	return false, svcStateName(st.State)
}

// svcStateName 把 SCM 状态转成人话。
func svcStateName(st svc.State) string {
	switch st {
	case svc.Stopped:
		return "已停止"
	case svc.StartPending:
		return "启动中"
	case svc.StopPending:
		return "停止中"
	case svc.Running:
		return "运行中"
	case svc.ContinuePending:
		return "恢复中"
	case svc.PausePending:
		return "暂停中"
	case svc.Paused:
		return "已暂停"
	default:
		return fmt.Sprintf("未知状态(%d)", st)
	}
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
