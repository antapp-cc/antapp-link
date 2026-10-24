package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 更新后的健康/启动/失败记录都放在用户配置目录（AppData），不放安装目录：
// 客户端装在 Program Files 时普通权限写不进去，而这些文件必须跨进程、跨重启存活
// （.old 的回滚判据、失败版本的跳过记录都不能随程序退出丢失）。
var (
	healthyMarkPathFn   = func() string { return defaultRolloutPath("healthy") }
	failedVersionPathFn = func() string { return defaultRolloutPath("update-failed") }
	startupMarkPathFn   = func() string { return defaultRolloutPath("update-starting") }
)

func defaultRolloutPath(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return filepath.Join(os.TempDir(), "AntAppLink", name)
	}
	return filepath.Join(dir, "AntAppLink", name)
}

// startupCrashWindow 内同一版本连续两次启动都没确认健康，就认定它启动即崩。
//
// 为什么用「两次启动」而不是「一次进入健康检查窗口」：更新后的第一次启动必须给
// 新版机会，否则正常更新会被自己回滚掉；而一直不写健康标记的版本，第二次启动
// 就该被拦下来。窗口限制防止用户很久以前的一次异常退出把几天后的正常启动误判。
const startupCrashWindow = 10 * time.Minute

// startupMark 记录「这个版本正在启动、还没确认健康」。MarkHealthy 会清掉它。
type startupMark struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
	At      string `json:"at"`
}

func readStartupMark() (startupMark, bool) {
	raw, err := os.ReadFile(startupMarkPathFn())
	if err != nil {
		return startupMark{}, false
	}
	var m startupMark
	if err := json.Unmarshal(raw, &m); err != nil {
		return startupMark{}, false
	}
	return m, true
}

func writeStartupMark(m startupMark) {
	path := startupMarkPathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(raw, '\n'), 0o600)
}

func clearStartupMark() { _ = os.Remove(startupMarkPathFn()) }

// MarkHealthy 记录「版本 version 已经健康运行过」，并清掉启动中标记。
//
// 它同时清掉失败版本记录：同一个版本这次健康了，就不该再被 CheckUpdate 跳过。
func MarkHealthy(version string) error {
	path := healthyMarkPathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(version+"\n"), 0o600); err != nil {
		return err
	}
	clearStartupMark()
	ClearFailedVersion()
	return nil
}

// ClearHealthyMark 在真正替换文件之前调用：旧版的健康标记对新版无效。
func ClearHealthyMark() { _ = os.Remove(healthyMarkPathFn()) }

func healthyMarkedVersion() string {
	raw, err := os.ReadFile(healthyMarkPathFn())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// MarkFailedVersion 记录「这个版本更新后没健康运行过、已回滚」。
//
// CheckUpdate 会跳过它，避免旧版把同一个崩溃版本反复装回去。用户确认问题解决后
// 可删掉记录重试。
func MarkFailedVersion(version string) {
	path := failedVersionPathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(version+"\n"), 0o600)
}

// FailedVersion 返回已记录的失败版本，没有则空字符串。
func FailedVersion() string {
	raw, err := os.ReadFile(failedVersionPathFn())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ClearFailedVersion 清掉失败版本记录（更新成功或用户手动重试时）。
func ClearFailedVersion() { _ = os.Remove(failedVersionPathFn()) }

// HealthyMarked 报告 version 是否被记录为健康过（主要给测试用）。
func HealthyMarked(version string) bool { return healthyMarkedVersion() == version }
