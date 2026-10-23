//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// applyWaitReady 是旧版等新版确认接手的窗口。
//
// 正常情况新版会在毫秒级写 ready；这个上限只兜「新进程根本没起来、或起来就崩」。
// 超时后旧版不退出：磁盘上虽然是新版，但让用户手上立刻什么都没有更糟。
var applyWaitReady = 15 * time.Second

// applyHandoffDir 是交接状态目录。用临时目录而不是安装目录：它是跨进程的短命
// 状态，不该跟 Program Files 的权限和清理策略绑在一起。
func applyHandoffDir() string {
	return filepath.Join(os.TempDir(), "AntAppLink-handoff")
}

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
	recordLifecycle(LifecycleEvent{
		Event:  EventUpdateStart,
		Detail: fmt.Sprintf("替换 %s", self),
	})

	// 交接状态建不起来也照常更新：它只影响「能否确认新版真的起来了」，
	// 没有它时退回原行为（调用方退出，由用户或下一次启动兜底）。
	handoff, handoffErr := NewHandoff(applyHandoffDir(), "", "", applyWaitReady)
	if handoffErr != nil {
		recordLifecycle(LifecycleEvent{Event: EventUpdateFailed, Detail: "写交接状态失败，跳过就绪确认", Err: handoffErr.Error()})
	}

	err = swapExe(self, newExePath, func(p string) error {
		cmd := exec.Command(p)
		if handoffErr == nil {
			cmd.Env = append(os.Environ(),
				handoffDirEnv+"="+applyHandoffDir(),
				handoffInvocationEnv+"="+handoff.InvocationID)
		}
		return cmd.Start()
	})
	if err != nil {
		if handoffErr == nil {
			_ = FailHandoff(applyHandoffDir(), handoff.InvocationID, err)
		}
		return err
	}
	recordLifecycle(LifecycleEvent{Event: EventUpdateLaunched, Detail: "新版进程已创建"})

	if handoffErr != nil {
		return nil
	}

	// 旧进程此刻还握着单实例互斥体，而新版要拿到它才写 ready。等待之前必须先让出去，
	// 否则新版卡在单实例闸门、旧进程卡在这里，两边一起超时 —— 这不是保守，是死锁。
	relinquishInstance()

	// 只确认进程起来了还不够：Start 成功之后新版仍可能在初始化阶段退出。
	ctx, cancel := context.WithTimeout(context.Background(), applyWaitReady)
	defer cancel()
	if _, werr := WaitReady(ctx, applyHandoffDir(), handoff.InvocationID, 50*time.Millisecond); werr != nil {
		// 新版没接手，旧进程还得继续跑：先把互斥体收回来，确认自己仍是唯一实例。
		// 收不回来通常意味着新版已经拿到锁并在跑，或者用户又起了一个实例。
		reclaimed := reclaimInstance()
		// 新版自己报过失败时保留第一手原因，别用「没收到确认」覆盖掉
		// 「拿不到互斥体」这类真正有用的信息。
		if !errors.Is(werr, ErrHandoffFailed) {
			_ = FailHandoff(applyHandoffDir(), handoff.InvocationID,
				fmt.Errorf("%v 内没有收到新版的启动确认: %w", applyWaitReady, werr))
		}
		detail := "新版没有确认启动，保留回滚副本"
		if errors.Is(werr, ErrHandoffFailed) {
			detail = "新版报告启动失败，保留回滚副本"
		}
		recordLifecycle(LifecycleEvent{
			Event:  EventHandoffTimeout,
			Detail: detail,
			Err:    werr.Error(),
		})
		// 不退出、也不回滚：新旧两版里只有旧版在跑，磁盘上的文件名属于新版，
		// 回滚需要用户重新启动时才安全。
		if !reclaimed {
			return fmt.Errorf("新版没有确认启动（%v），且单实例互斥体已被别的实例持有：请从托盘确认当前在跑的是哪一个", werr)
		}
		return fmt.Errorf("新版已拉起，但没有在 %v 内确认启动（回滚副本仍保留在 %s.old，可重试更新）",
			applyWaitReady, self)
	}
	return nil
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
		recordLifecycle(LifecycleEvent{Event: EventRollback, Detail: "写入新版本失败，已回滚", Err: err.Error()})
		return fmt.Errorf("写入新版本失败（已回滚）: %w", err)
	}
	if err := launch(self); err != nil {
		// 新版起不来就把旧版改回原名。不恢复的话，用户下次从快捷方式启动跑的还是
		// 这个起不来的版本，而能用的旧版只剩在一个 .old 文件里 —— 他根本不会想到去
		// 手动改名。恢复失败才只能报错并指明路径，同时**保留** .old。
		if rerr := os.Rename(old, self); rerr != nil {
			return fmt.Errorf("新版本启动失败，且回滚也失败（旧版仍在 %s，可手动改回）: %v / %w",
				old, rerr, err)
		}
		recordLifecycle(LifecycleEvent{Event: EventRollback, Detail: "新版本启动失败，已回滚", Err: err.Error()})
		return fmt.Errorf("新版本启动失败（已回滚到旧版）: %w", err)
	}
	recordLifecycle(LifecycleEvent{Event: EventUpdateSwapped, Detail: "新版本已写入程序路径"})
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
	// scheduleCleanup 留成变量：测试不能真的等两分钟才验证「延迟删除」。
	scheduleCleanup = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
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
	removeOldNow()
}

// CleanupOldAfterHealthy 由调用方在确认新版健康后调用。
//
// 但「控制连接握上手」不等于数据面健康（第 13 章第 7 项），所以这里不立即删：
// 先记住这次确认，等进程本身也跑够观察窗口再删。调用方只报一次健康也不会丢清理：
// 删除已经排进定时器。
func CleanupOldAfterHealthy() {
	if elapsed := time.Since(processStarted); elapsed < minHealthyUptime {
		scheduleCleanup(minHealthyUptime-elapsed, removeOldNow)
		return
	}
	removeOldNow()
}

func removeOldNow() {
	self, err := executablePath()
	if err != nil {
		return
	}
	old := self + ".old"
	if err := os.Remove(old); err != nil {
		return
	}
	recordLifecycle(LifecycleEvent{Event: EventBackupRemoved, Detail: old})
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
