//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// =====================================================================
// 非 Windows 平台的服务化实现
//
// 原先是「一律返回错误」的空壳，后果是 Linux 上只能前台跑 —— 一关终端就断心跳。
// 这正是对标产品在安装页上用加粗警告提醒用户的那件事：
//   「勿只前台跑 supervisor：关掉窗口或重启就会断心跳」
// 与其在文档里警告，不如把自启做出来。这里用 systemd 补齐。
//
// 没有 systemd 的环境（容器、Alpine、老发行版）会明确报错并给出前台运行指引，
// 不做「静默降级成不生效」这种事后极难排查的处理。
// =====================================================================

const (
	serviceName = "agent-mesh-client"
	serviceDesc = "Agent Mesh 本地 AI 节点常驻客户端"
	// serviceUnitPath 是 systemd 单元文件位置。
	serviceUnitPath = "/etc/systemd/system/agent-mesh-client.service"
)

// isServiceSession 判定当前是否由 systemd 拉起。
// systemd 会为每个 unit 实例注入 INVOCATION_ID；终端里手工执行不会有。
func isServiceSession() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

// runService 在 systemd 下直接进入主循环。
//
// 单元用 Type=simple，进程即服务本体，signal 由我们的 signalCtx 正常处理，
// 不需要额外的 sd_notify 协议。
func runService(run func(context.Context)) error {
	run(context.Background())
	return nil
}

// installService 把「当前进程自己的可执行文件」装成 systemd 服务。
// 兼容既有 client.exe install / agent-mesh-client install 子命令。
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败: %w", err)
	}
	return installServiceAt(exe)
}

// installServiceAt 写入 systemd 单元并启用。
//
// exePath 必须指向**最终安装位置的副本**：enroll 会把程序先复制到安装目录再注册，
// 否则服务会指向临时目录里那份，重启或临时目录被清理后就再也起不来。
func installServiceAt(exePath string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	exe, err := filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("解析可执行文件路径失败: %w", err)
	}
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("可执行文件不存在: %s", exe)
	}
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限才能注册系统服务（请用 sudo 执行）")
	}

	unit := fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
WorkingDirectory=%s
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`, serviceDesc, exe, filepath.Dir(exe))

	if err := os.WriteFile(serviceUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入单元文件失败: %w", err)
	}
	if out, err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %v（%s）", err, out)
	}
	if out, err := runSystemctl("enable", "--now", serviceName); err != nil {
		return fmt.Errorf("启用服务失败: %v（%s）", err, out)
	}
	return nil
}

// isServiceExistsErr 判断错误是否表示「服务已经注册」。
//
// Linux 侧 installServiceAt 是幂等覆盖，正常情况下不会走到这里；
// 保留它是因为 enroll 的「已注册就重启」逻辑是跨平台共用的。
func isServiceExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "已存在")
}

// requirePrivilege 探测当前进程是否有 root 权限。
func requirePrivilege() error {
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限：请用 sudo 执行接入命令（例如 sudo sh -c \"$(curl ...)\"）")
	}
	return nil
}

// stopService 停止服务（不删除）。
// enroll 覆盖安装目录里的程序文件前会调用它，避免「旧进程还在跑、新文件已替换」的错觉。
func stopService() error {
	if err := requireSystemd(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限（请用 sudo 执行）")
	}
	if out, err := runSystemctl("stop", serviceName); err != nil {
		return fmt.Errorf("停止服务失败: %v（%s）", err, out)
	}
	return nil
}

// restartService 重启服务，让新写入的配置生效。
func restartService() error {
	if err := requireSystemd(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限（请用 sudo 执行）")
	}
	if out, err := runSystemctl("restart", serviceName); err != nil {
		return fmt.Errorf("重启服务失败: %v（%s）", err, out)
	}
	return nil
}

// serviceStatus 查询服务是否处于 active。
func serviceStatus() (bool, string) {
	if err := requireSystemd(); err != nil {
		return false, err.Error()
	}
	out, err := exec.Command("systemctl", "is-active", serviceName).Output()
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil {
		return false, "服务尚未注册"
	}
	if state == "active" {
		return true, "运行中"
	}
	if state == "" {
		state = "未知"
	}
	return false, state
}

// uninstallService 停止、禁用并删除 systemd 单元。
func uninstallService() error {
	if err := requireSystemd(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限（请用 sudo 执行）")
	}
	// 服务可能本来就没启用，失败不是错误。
	_, _ = runSystemctl("disable", "--now", serviceName)
	if err := os.Remove(serviceUnitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除单元文件失败: %w", err)
	}
	_, _ = runSystemctl("daemon-reload")
	return nil
}

// requireSystemd 确认 systemd 可用。
func requireSystemd() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("本机没有 systemctl，无法注册为系统服务；" +
			"请用 nohup 或进程守护工具前台常驻运行（注意：关掉终端就会断心跳）")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("本机不是由 systemd 引导的（容器环境常见），无法注册系统服务；" +
			"请用 nohup 或进程守护工具前台常驻运行（注意：关掉终端就会断心跳）")
	}
	return nil
}

// runSystemctl 执行 systemctl 并把输出一并返回，便于把失败原因原样带给用户。
func runSystemctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
