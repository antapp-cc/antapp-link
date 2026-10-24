package update

import (
	"os"
	"path/filepath"
	"strings"
)

// 更新后的健康标记与失败版本记录都放在用户配置目录（AppData），不放安装目录：
// 客户端装在 Program Files 时普通权限写不进去，而这两个文件必须跨进程、跨重启存活
// （.old 的回滚判据、失败版本的跳过记录都不能随程序退出丢失）。
var (
	healthyMarkPathFn   = func() string { return defaultRolloutPath("healthy") }
	failedVersionPathFn = func() string { return defaultRolloutPath("update-failed") }
)

func defaultRolloutPath(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return filepath.Join(os.TempDir(), "AntAppLink", name)
	}
	return filepath.Join(dir, "AntAppLink", name)
}

// MarkHealthy 记录「版本 version 已经健康运行过」。
//
// 它是「.old 要不要回滚」的判据：有标记说明上次更新后的新版不是崩溃版；没有标记
// 而 .old 还在，说明新版启动后从未确认健康，下次启动应该恢复旧版。
func MarkHealthy(version string) error {
	path := healthyMarkPathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(version+"\n"), 0o600); err != nil {
		return err
	}
	// 健康了就不再需要失败版本记录：同一个版本以后还能正常更新。
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
// CheckUpdate 会跳过它，避免旧版把同一个崩溃版本反复装回去（装一次崩一次、回滚、
// 再装）。用户确认问题解决后可删掉记录重试。
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
