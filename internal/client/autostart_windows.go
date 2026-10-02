//go:build windows

package client

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const autostartTask = "AntAppLink"

// EnableAutostart 注册「登录时自动启动」的计划任务。
//
// 必须用计划任务而不是注册表 Run 项：创建虚拟网卡和改路由都要管理员权限，
// 而 Run 项无法以最高权限启动。
func EnableAutostart() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取程序路径: %w", err)
	}
	target := exe
	if strings.ContainsAny(exe, " \t") {
		// schtasks 的 /tr 需要自带引号才能容纳带空格的路径
		target = `"` + exe + `"`
	}
	// /delay 登录后延迟 30 秒再拉起：给网络栈留出就绪时间（DHCP/默认路由），
	// 配合客户端自身的开机重试，双保险
	return runCommand(Command{"schtasks", []string{
		"/create", "/tn", autostartTask, "/tr", target,
		"/sc", "onlogon", "/delay", "0000:30", "/rl", "highest", "/f",
	}})
}

// DisableAutostart 删除计划任务。任务本来就不存在时不算失败。
func DisableAutostart() error {
	cmd := exec.Command("schtasks", "/delete", "/tn", autostartTask, "/f")
	cmd.SysProcAttr = hiddenProcAttr()
	_ = cmd.Run()
	return nil
}

func AutostartEnabled() bool {
	cmd := exec.Command("schtasks", "/query", "/tn", autostartTask)
	cmd.SysProcAttr = hiddenProcAttr()
	return cmd.Run() == nil
}
