//go:build windows

// Package schtask 管理 AntAppLink 计划任务的运行策略。
//
// 自启任务由 schtasks 创建时没有运行时限设置，Windows 默认给 72 小时
// （ExecutionTimeLimit=PT72H）且 AllowHardTerminate=True：到点后任务调度器会
// 强制结束进程，界面和后台一起消失，日志里连「已断开」都留不下。这是 2026-11
// 批量「程序自己关了」的根因。
package schtask

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// TaskName 是自启任务名。和 setup/客户端托盘用的是同一个，改一处会全部对不上。
const TaskName = "AntAppLink"

// ErrNotFound 表示任务不存在（用户关闭过自启，或从未安装）。
var ErrNotFound = errors.New("AntAppLink 计划任务不存在")

// ApplySettings 把已有任务的运行策略改成「无限运行、电池不停、异常重启」。
//
// 只改已有任务，不创建、不删除。任务不存在时返回 ErrNotFound，调用方据此决定
// 是否忽略——绝不能把用户主动关掉的自启重新打开。
func ApplySettings() error {
	name := strings.ReplaceAll(TaskName, "'", "''")
	script := fmt.Sprintf(`
$task = Get-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue
if (-not $task) { Write-Output '__NOT_FOUND__'; exit 2 }
$s = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Set-ScheduledTask -TaskName '%s' -Settings $s | Out-Null
`, name, name)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(text, "__NOT_FOUND__") {
			return ErrNotFound
		}
		return fmt.Errorf("设置计划任务运行策略失败: %w（%s）", err, text)
	}
	return nil
}
