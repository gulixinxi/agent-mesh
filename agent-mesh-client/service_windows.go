//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
//
// 幂等：服务已存在且指向同一路径时，停掉旧实例再拉起新实例，而不是报错让用户
// 先去 uninstall。装 ect 只会被再跑一遍安装脚本，升级不该是两步工序——
// 何况旧行为下替换 exe 会被运行中的服务锁死（file is being used by another process）。
//
// 指向另一路径时默认接管（见 takeOverService）：服务名本机唯一，
// 放着不管的结果就是重启后旧版本的注册项把旧程序拉起来，两套实例抢同一份身份。
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
		registered := ""
		if cfg, cfgErr := existing.Config(); cfgErr == nil {
			registered = cfg.BinaryPathName
		}
		if registered != "" && !sameBinaryPath(registered, exe) {
			return takeOverService(m, existing, exe, registered)
		}
		defer existing.Close()

		if err := stopInstance(existing); err != nil {
			return err
		}
		if err := existing.Start(); err != nil {
			return fmt.Errorf("已换用新程序但启动失败: %w", err)
		}
		fmt.Printf("服务 %s 已存在，已就地重启并加载新程序\n", serviceName)
		return nil
	}

	return createAndStart(m, exe)
}

// isServiceExistsErr 判断错误是否表示「服务已经注册」。
//
// 放在平台文件里：installServiceAt 与 takeOverService 都可能产生这类错误，
// 判断和产生它的地方挨着，改文案时不会漏掉判断。
func isServiceExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "已存在")
}

// takeOverService 把指向另一路径的同名服务改成由当前路径接管。
//
// 顺序是硬约束：先让旧进程真正退出（端口与文件句柄都占着），再注销注册项，
// 最后用新路径重建。任何一步省略都会留下幽灵实例。
func takeOverService(m *mgr.Mgr, existing *mgr.Service, exe, registered string) error {
	if !installTakeover {
		return fmt.Errorf("服务 %s 已注册但指向另一路径：%s\n当前程序位于：%s\n"+
			"路径不一致，请先执行 uninstall 卸载旧服务；确认要接管可去掉 --no-takeover",
			serviceName, registered, exe)
	}

	fmt.Printf("[安装] 服务 %s 已存在，但指向的不是当前路径：\n", serviceName)
	fmt.Printf("        旧注册：%s\n", strings.TrimSpace(registered))
	fmt.Printf("        新程序：%s\n", exe)
	fmt.Printf("        接管：停止旧实例 → 注销旧注册项 → 改指当前路径（旧文件保留）\n")

	if err := stopInstance(existing); err != nil {
		if pid := servicePID(existing); pid > 0 {
			fmt.Printf("        优雅停止失败（%v），强制结束旧进程 PID %d\n", err, pid)
			if kerr := killProcess(pid); kerr != nil {
				return fmt.Errorf("旧服务无法停止且不响应强制结束（PID %d）：%w", pid, kerr)
			}
		} else {
			return err
		}
	}
	existing.Close()
	if err := existing.Delete(); err != nil {
		return fmt.Errorf("注销旧服务注册失败: %w", err)
	}
	if err := waitServiceGone(m, 15*time.Second); err != nil {
		return err
	}
	if err := createAndStart(m, exe); err != nil {
		return err
	}
	fmt.Printf("服务 %s 已改指当前路径\n", serviceName)
	return nil
}

// createAndStart 用 exe 注册服务并立即启动。
//
// 刚删完旧注册项时 SCM 可能还处于"标记删除"中间态，CreateService 会短暂失败，
// 所以要重试若干次再判死。
func createAndStart(m *mgr.Mgr, exe string) error {
	var lastErr error
	for i := 0; i < 10; i++ {
		s, err := m.CreateService(serviceName, exe, mgr.Config{
			DisplayName: serviceDesc,
			Description: serviceDesc,
			StartType:   mgr.StartAutomatic,
		})
		if err == nil {
			defer s.Close()
			if startErr := s.Start(); startErr != nil {
				return fmt.Errorf("服务已注册但启动失败: %w", startErr)
			}
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("创建服务失败: %w", lastErr)
}

// installTakeover 在 client main.go 里定义，由 install / enroll 子命令共用。

// servicePID 取服务当前进程号，取不到返回 0。
func servicePID(s *mgr.Service) uint32 {
	st, err := s.Query()
	if err != nil {
		return 0
	}
	return st.ProcessId
}

// killProcess 强制结束指定进程。走 taskkill 而非 TerminateProcess：
// 这里已是管理员身份，用系统命令语义直观、报错可复现。
func killProcess(pid uint32) error {
	out, err := exec.Command("taskkill", "/F", "/PID", strconv.FormatUint(uint64(pid), 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill PID %d 失败: %w（%s）", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitServiceGone 轮询到注册项彻底消失。
// Delete 返回成功不等于移除完成：句柄没放干净时它只是被标记删除，
// 这时重建同名服务会撞上 ERROR_SERVICE_EXISTS。
func waitServiceGone(m *mgr.Mgr, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := m.OpenService(serviceName)
		if err != nil {
			return nil
		}
		s.Close()
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("等待旧服务注册项消失超时（%v）：疑似仍有残留进程未退出", timeout)
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

	// 停止失败不算致命：调用方大多只是想在覆盖文件前尽力释放句柄。
	if err := stopInstance(s); err != nil {
		return fmt.Errorf("停止服务失败: %w", err)
	}
	return nil
}

// stopInstance 优雅停止传入的服务并轮询到真正退出。
// 必须等到 Stopped 而非发完指令就返回——文件句柄要进程真正退出才释放。
func stopInstance(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("查询服务状态失败: %w", err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("停止服务失败: %w", err)
	}
	for i := 0; i < 60; i++ {
		time.Sleep(200 * time.Millisecond)
		if st, err = s.Query(); err != nil || st.State == svc.Stopped {
			return nil
		}
	}
	return fmt.Errorf("等待服务停止超时（约 12s）")
}

// sameBinaryPath 比较服务里登记的路径与当前程序路径。
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
