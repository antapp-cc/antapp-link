//go:build windows

package client

import (
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// ensureNlaSvc 确保「网络位置感知」（NlaSvc）服务在运行。
//
// 这个服务停止时 Windows 无法判断网络状态，任务栏会显示「无 Internet」（小地球）
// 图标，而实际网络是通的。客户端本来就以管理员运行，检测到它没跑就直接拉起，
// 免得用户被图标误导（0.4.14 之前踩过一次：服务没启动，图标一直是小地球）。
func ensureNlaSvc(log *slog.Logger) {
	out, err := exec.Command("sc.exe", "query", "nlasvc").Output()
	if err != nil {
		return
	}
	if nlaRunning(string(out)) {
		return
	}
	if err := exec.Command("sc.exe", "start", "nlasvc").Run(); err != nil {
		return
	}
	log.Info("已启动「网络位置感知」服务（任务栏网络图标会恢复正常）")
}

// nlaRunning 解析 `sc query nlasvc` 的输出，判断服务是否在运行。
//
// sc 的状态词（RUNNING/STOPPED）在中文系统上也保持英文，只匹配这一行即可，
// 不依赖前面「STATE」还是「状态」这类本地化文本。
func nlaRunning(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "RUNNING") {
			return true
		}
	}
	return false
}

// keepNlaSvcAlive 启动时检查一次，之后每 10 分钟补一次：NlaSvc 可能在网络变化、
// 系统更新或某些软件操作时被停掉，定期兜底比只在启动时查一次更稳。
func keepNlaSvcAlive(log *slog.Logger) {
	ensureNlaSvc(log)
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		ensureNlaSvc(log)
	}
}
