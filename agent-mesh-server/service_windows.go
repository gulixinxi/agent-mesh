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
//
// 服务名是本机唯一的。所以"装到新目录、旧注册项还指向老路径"不是可以并存
// 的两份安装——它只会让注册表里那一条始终指向旧程序：安装全程看着成功，
// 重启后先把旧版本拉起来，新装的这份永远轮不到开机自启，两个实例抢同一个端口。
// 这种情况默认由当前程序接管（见 takeOverService），--no-takeover 可退回报错。
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

	// 已存在：先确认它指向哪里，同路径就地重启，异路径则接管。
	if existing, err := m.OpenService(serviceName); err == nil {
		registered := ""
		if cfg, cfgErr := existing.Config(); cfgErr == nil {
			registered = cfg.BinaryPathName
		}
		if registered != "" && !sameBinaryPath(registered, exe) {
			return takeOverService(m, existing, exe, registered)
		}
		defer existing.Close()

		if err := stopService(existing); err != nil {
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

// takeOverService 把指向另一路径的同名服务改成由当前程序接管。
//
// 步骤顺序是硬约束：先让旧进程真正退出（否则端口和文件句柄都被占着），
// 再注销注册项，最后才用新路径重建。跳过任何一步都会留下幽灵。
func takeOverService(m *mgr.Mgr, existing *mgr.Service, exe, registered string) error {
	if !installTakeover {
		return fmt.Errorf("服务 %s 已存在且指向另一路径：%s\n当前程序位于：%s\n请先执行 uninstall 后再安装；确认要接管可去掉 --no-takeover",
			serviceName, registered, exe)
	}

	fmt.Printf("[安装] 服务 %s 已存在，但指向的不是当前路径：\n", serviceName)
	fmt.Printf("        旧注册：%s\n", strings.TrimSpace(registered))
	fmt.Printf("        新程序：%s\n", exe)
	fmt.Printf("        接管：停止旧实例 → 注销旧注册项 → 改指当前路径（旧文件保留）\n")

	if err := stopService(existing); err != nil {
		// 停不下来也必须腾地方：进程不死，注册项就删不干净，重启后照旧起来。
		if pid := servicePID(existing); pid > 0 {
			fmt.Printf("        优雅停止失败（%v），强制结束旧进程 PID %d\n", err, pid)
			if kerr := killProcess(pid); kerr != nil {
				return fmt.Errorf("旧服务无法停止且不响应强制结束（PID %d）：%w", pid, kerr)
			}
		} else {
			return err
		}
	}
	// Close 之后再 Delete：句柄没关的话删除会被拒绝。
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
// 删完旧注册项立刻重建时，SCM 可能还处于"标记删除"的中间态，
// CreateService 会短暂失败，所以要重试几次而不是直接判死。
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
			fmt.Printf("服务 %s 已注册并启动\n", serviceName)
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("创建服务失败: %w", lastErr)
}

// installTakeover 在 main.go 里定义，由 install 子命令按开关设置。

// servicePID 取服务当前进程号，取不到返回 0。
func servicePID(s *mgr.Service) uint32 {
	st, err := s.Query()
	if err != nil {
		return 0
	}
	return st.ProcessId
}

// killProcess 强制结束指定进程。
// 走 taskkill 而不是 TerminateProcess：自己实现要是漏了权限提升，报出来的
// 错更难懂；而这里已经是管理员身份，taskkill 的语义更直观也更容易手工复现。
func killProcess(pid uint32) error {
	out, err := exec.Command("taskkill", "/F", "/PID", strconv.FormatUint(uint64(pid), 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill PID %d 失败: %w（%s）", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitServiceGone 轮询到服务注册项彻底消失。
// Delete 返回成功不代表注册项已移除——还有句柄没放干净时它只是被标记删除，
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
