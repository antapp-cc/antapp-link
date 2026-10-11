//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Apply 用下载好的新程序替换当前可执行文件，并拉起新版本。
//
// Windows 不允许覆盖正在运行的 exe，但**允许给它改名** —— 所以先把当前文件改成
// .old，再把新版写回原名。写失败会立刻回滚，不能让用户手上既没有旧版也没有新版。
//
// 调用方在 Apply 返回 nil 之后必须马上退出自己：此刻跑着的还是旧版代码，
// 而磁盘上的文件名已经被新版占用了。
func Apply(newExePath string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取当前程序路径: %w", err)
	}
	return swapExe(self, newExePath, func(p string) error {
		return exec.Command(p).Start()
	})
}

// swapExe 是 Apply 里可测的那部分：改名 → 写入 → 拉起。
// self 和 newExe 都由调用方给，测试才能拿临时文件验证失败时的回滚。
func swapExe(self, newExe string, launch func(string) error) error {
	old := self + ".old"
	_ = os.Remove(old) // 清掉上一次留下的

	if err := os.Rename(self, old); err != nil {
		return fmt.Errorf("把当前程序改名为 %s 失败: %w", old, err)
	}
	if err := copyFile(newExe, self); err != nil {
		if rerr := os.Rename(old, self); rerr != nil {
			return fmt.Errorf("写入新版本失败，且回滚也失败（旧版仍在 %s，可手动改回）: %v / %w", old, rerr, err)
		}
		return fmt.Errorf("写入新版本失败（已回滚）: %w", err)
	}
	if err := launch(self); err != nil {
		return fmt.Errorf("新版本已就位但启动失败；旧版留在 %s，可手动改回: %w", old, err)
	}
	return nil
}

// minHealthyUptime 是判断「新版没在启动时崩掉」的观察窗口。
//
// 启动即崩和被杀软隔离都发生在头几分钟内，熬过去再删回滚副本才不算赌。
const minHealthyUptime = 2 * time.Minute

var (
	// 取当前程序路径留成变量：测试要把它指到临时目录，否则删的是别处的东西
	executablePath = os.Executable
	processStarted = time.Now()
)

// CleanupOld 删除上次更新留下的 .old，但只在本进程已经跑够 minHealthyUptime 之后。
//
// 保留这个名字是给启动最早期就调它的调用点用的：那个时点新版什么都还没自检过，
// 直接删就等于「能跑的还没证明，能退的已经没了」。要「确认新版健康后立刻清」，
// 用 CleanupOldAfterHealthy。
func CleanupOld() {
	if time.Since(processStarted) < minHealthyUptime {
		return
	}
	CleanupOldAfterHealthy()
}

// CleanupOldAfterHealthy 立刻删除上次更新留下的 .old，调用方负责确认新版已经能
// 正常工作（比如已经连上隧道、界面起来了）。
//
// 那个 .old 是新版崩掉或被杀软隔离时唯一能退回上一版的副本，删早了节点机上就既
// 没有能跑的也没有可回滚的。
func CleanupOldAfterHealthy() {
	self, err := executablePath()
	if err != nil {
		return
	}
	_ = os.Remove(self + ".old")
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
