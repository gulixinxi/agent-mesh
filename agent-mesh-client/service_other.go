//go:build !windows

package main

import (
	"context"
	"errors"
)

// 非 Windows 平台没有 Windows 服务这一说，这里给出一致的空实现，
// 免得业务代码到处加平台判断。
const serviceName = "AgentMeshClient"

func isServiceSession() bool { return false }

func runService(run func(context.Context)) error {
	return errors.New("Windows 服务模式仅在 Windows 上可用")
}

func installService() error {
	return errors.New("安装为 Windows 服务仅在 Windows 上可用")
}

func uninstallService() error {
	return errors.New("卸载 Windows 服务仅在 Windows 上可用")
}
