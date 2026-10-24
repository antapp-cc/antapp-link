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

// applyPostReadyGrace 是拿到 ready 之后继续盯着新进程的时间。
//
// ready 只能证明界面和托盘建起来了，证明不了它站得住。新版确认后立刻退出时，
// 旧进程如果马上跟着走，就再没有人能恢复现场；多盯这一小会儿，它还在才真的放手。
// 代价是旧窗口要多留这么久（新版窗口此时已经出现），所以别拉长到用户能察觉。
var applyPostReadyGrace = 2 * time.Second

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
	return apply(self, newExePath)
}

// apply 是 Apply 里不依赖「当前进程是谁」的那部分：测试可以拿临时文件当程序路径，
// 不必真的去改名正在运行的测试二进制。
func apply(self, newExePath string) error {
	recordLifecycle(LifecycleEvent{
		Event:  EventUpdateStart,
		Detail: fmt.Sprintf("替换 %s", self),
	})

	// 交接状态建不起来就中止更新：没有它，旧进程无法确认新版是否真的接手，
	// 只能退回「Start() 成功即成功」—— 那正是这一轮要消灭的行为。临时目录不可写、
	// 空间不足都会走到这里；宁可这次不更新，也不能让旧版在无法确认的情况下退出。
	handoff, err := NewHandoff(applyHandoffDir(), "", "", applyWaitReady)
	if err != nil {
		recordLifecycle(LifecycleEvent{
			Event:  EventUpdateFailed,
			Detail: "无法建立更新交接状态，已中止更新",
			Err:    err.Error(),
		})
		return fmt.Errorf("无法建立更新交接状态，已中止更新（旧版继续运行）: %w", err)
	}

	// 交接状态建立成功、即将真正替换文件：旧版的健康标记对新版无效了。
	// 放在 NewHandoff 之后：交接建不起来时更新根本没发生，不能顺手清掉健康标记，
	// 否则当前版本会被后续启动误判成"未健康"而回滚。
	ClearHealthyMark()

	var launched *exec.Cmd
	err = swapExe(self, newExePath, func(p string) error {
		cmd := exec.Command(p)
		cmd.Env = append(os.Environ(),
			handoffDirEnv+"="+applyHandoffDir(),
			handoffInvocationEnv+"="+handoff.InvocationID)
		if err := cmd.Start(); err != nil {
			return err
		}
		launched = cmd
		return nil
	})
	if err != nil {
		_ = FailHandoff(applyHandoffDir(), handoff.InvocationID, err)
		return err
	}
	recordLifecycle(LifecycleEvent{Event: EventUpdateLaunched, Detail: "新版进程已创建"})

	// 旧进程此刻还握着单实例互斥体，而新版要拿到它才写 ready。等待之前必须先让出去，
	// 否则新版卡在单实例闸门、旧进程卡在这里，两边一起超时 —— 这不是保守，是死锁。
	relinquishInstance()

	return awaitHandoff(applyHandoffDir(), handoff, launched, self)
}

// awaitHandoff 等新版确认接手，并在确认之后再盯一小段时间。
//
// 等待失败（超时 / 新版自报失败）和「确认后立刻退出」都算交接没完成：旧进程必须
// 把单实例互斥体收回来继续跑，它一退出用户手上就什么都不剩。
func awaitHandoff(dir string, h *Handoff, newProc *exec.Cmd, self string) error {
	ctx, cancel := context.WithTimeout(context.Background(), applyWaitReady)
	defer cancel()
	if _, werr := WaitReady(ctx, dir, h.InvocationID, 50*time.Millisecond); werr != nil {
		// 新版没接手，旧进程还得继续跑：先把互斥体收回来，确认自己仍是唯一实例。
		// 收不回来通常意味着新版已经拿到锁并在跑，或者用户又起了一个实例。
		reclaimed := reclaimInstance()
		// 新版自己报过失败时保留第一手原因，别用「没收到确认」覆盖掉
		// 「拿不到互斥体」这类真正有用的信息。
		if !errors.Is(werr, ErrHandoffFailed) {
			_ = FailHandoff(dir, h.InvocationID,
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
			return fmt.Errorf("新版没有确认启动（%v），且单实例互斥体已被别的实例持有：本窗口停止接管网络，请关闭它（%w）",
				werr, ErrHandoffBlocked)
		}
		return fmt.Errorf("新版已拉起，但没有在 %v 内确认启动（回滚副本仍保留在 %s.old，可重试更新）",
			applyWaitReady, self)
	}

	// ready 只覆盖「图标、窗口、托盘都建好了」这一刻。新版确认后马上崩掉的话，
	// 旧进程如果已经走了，就没有任何人能恢复磁盘上的回滚副本。
	exited, waitErr := waitProcessExit(newProc, applyPostReadyGrace)
	if !exited {
		return nil
	}
	// 退出码 0 是正常退出（用户自己关掉了窗口），不是"新版没站住"：磁盘上的新版
	// 是好的，回滚反而会把用户刚确认可用的版本换回旧的。用户主动关掉的程序也不该
	// 被一个隐藏的旧窗口顶回来。
	if waitErr == nil {
		recordLifecycle(LifecycleEvent{Event: EventUpdateReady, Detail: "新版正常退出（用户主动关闭），保留新版"})
		return nil
	}

	reclaimed := reclaimInstance()
	if reclaimed {
		if rerr := restoreOldExe(self); rerr != nil {
			recordLifecycle(LifecycleEvent{Event: EventRollback, Detail: "新版确认后异常退出，恢复旧版失败", Err: rerr.Error()})
			return fmt.Errorf("新版在确认启动后 %v 内异常退出，且恢复旧版失败（旧版仍在 %s.old，可手动改回）: %w",
				applyPostReadyGrace, self, rerr)
		}
		recordLifecycle(LifecycleEvent{Event: EventRollback, Detail: "新版确认后异常退出，已恢复旧版"})
		return fmt.Errorf("新版在确认启动后 %v 内异常退出，已恢复旧版并继续运行", applyPostReadyGrace)
	}
	recordLifecycle(LifecycleEvent{Event: EventHandoffTimeout, Detail: "新版确认后异常退出，但单实例互斥体已被别的实例持有"})
	return fmt.Errorf("新版在确认启动后 %v 内异常退出，且单实例互斥体已被别的实例持有：本窗口停止接管网络，请关闭它（%w）",
		applyPostReadyGrace, ErrHandoffBlocked)
}

// waitProcessExit 在 grace 内观察新进程是否退出；返回 true 表示它已经死了。
//
// 第二个返回值是 cmd.Wait 的结果：nil = 正常退出（exit 0），*exec.ExitError =
// 崩溃或非零退出。调用方据此区分「用户主动关闭」和「新版没站住」。
func waitProcessExit(cmd *exec.Cmd, grace time.Duration) (bool, error) {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-done:
		return true, err
	case <-t.C:
		return false, nil
	}
}

// restoreOldExe 把 .old 改回程序路径。
//
// 只用在「新版已经确认接手却又退出」之后：旧进程还在内存里跑，但磁盘上的正常路径
// 已经被新版占了。不换回来的话，用户下次启动拿到的还是那个刚崩过的版本。
func restoreOldExe(self string) error {
	old := self + ".old"
	if _, err := os.Stat(old); err != nil {
		return err
	}
	failed := self + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(self, failed); err != nil {
		return err
	}
	if err := os.Rename(old, self); err != nil {
		// 退回去：正常路径上必须留着一个能启动的程序。
		_ = os.Rename(failed, self)
		return err
	}
	// 崩掉的那一版没有保留价值，下次更新会重新下载。
	_ = os.Remove(failed)
	return nil
}

// RollbackIfUnhealthyUpdate 在启动早期检查「上次更新是否留下了启动即崩的新版」。
//
// 判据：安装目录旁边有 .old（上次更新替换掉旧版的副本），且同一版本在
// startupCrashWindow 内连续两次启动都没确认健康（没有 MarkHealthy 清掉启动标记）。
// 第一次启动总是放行：正常更新后的新版刚起来，还没机会证明自己；只有它「启动后
// 没健康、下次又起来」才回滚，这样才不会把正常更新自己回滚掉。
//
// 返回 true 表示执行了回滚。当前进程是内存里的新版，回滚只影响下次启动。
func RollbackIfUnhealthyUpdate(self, currentVersion string) (bool, error) {
	old := self + ".old"
	if _, err := os.Stat(old); err != nil {
		return false, nil // 没有回滚副本
	}
	// 这个版本已经确认过健康：.old 只是还没清理的备份，不能因为后续两次启动
	// 异常就把它当崩溃版回滚掉。
	if healthyMarkedVersion() == currentVersion {
		clearStartupMark()
		return false, nil
	}

	count := 1
	if prev, ok := readStartupMark(); ok && prev.Version == currentVersion {
		if at, err := time.Parse(time.RFC3339, prev.At); err == nil && time.Since(at) <= startupCrashWindow {
			count = prev.Count + 1
		}
	}
	// 先记下「本次启动正在进行、尚未确认健康」。MarkHealthy 会清掉它。
	//
	// 写失败必须 fail-closed：没有可靠的启动计数，崩溃版永远不会被认出来，
	// 用户会一直对着一个每次启动都崩的版本。恢复旧版比继续跑更安全。
	if err := writeStartupMark(startupMark{Version: currentVersion, Count: count, At: time.Now().Format(time.RFC3339)}); err != nil {
		return rollbackToOld(self, old, currentVersion, "启动状态无法持久化，已恢复旧版: "+err.Error())
	}
	if count < 2 {
		return false, nil // 更新后的第一次启动：给新版机会
	}
	return rollbackToOld(self, old, currentVersion, "同一版本连续两次启动都未确认健康，已恢复旧版")
}

// rollbackToOld 把磁盘恢复成 .old，并记录失败版本、清启动标记。
func rollbackToOld(self, old, currentVersion, reason string) (bool, error) {
	failed := self + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(self, failed); err != nil {
		return false, err
	}
	if err := os.Rename(old, self); err != nil {
		// 退回去：正常路径上必须留着一个能启动的程序。
		_ = os.Rename(failed, self)
		return false, err
	}
	clearStartupMark()
	if err := MarkFailedVersion(currentVersion); err != nil {
		// 回滚本身完成了，但「这个版本别再装」的记录写不进去：旧版下次可能又把它
		// 装回来。发出告警让用户/日志知道，而不是静默返回「回滚完成」。
		recordLifecycle(LifecycleEvent{
			Event:   EventUpdateFailed,
			Version: currentVersion,
			Detail:  "已恢复旧版，但失败版本记录写入失败（旧版可能再次尝试同一版本）",
			Err:     err.Error(),
		})
		return true, fmt.Errorf("已恢复旧版，但失败版本记录写入失败（旧版可能再次尝试同一版本）: %w", err)
	}
	recordLifecycle(LifecycleEvent{
		Event:   EventRollback,
		Version: currentVersion,
		Detail:  reason,
	})
	return true, nil
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
