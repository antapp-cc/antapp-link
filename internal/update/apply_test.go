//go:build windows

package update

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubExecutable 把「当前程序在哪」指到临时目录里的假程序，免得测试删到别处。
// 顺带把生命周期日志也指到临时目录：这些用例都会经过 Apply/cleanup 的日志点。
func stubExecutable(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })

	prevPath := lifecyclePathFn
	lifecyclePathFn = func() string { return filepath.Join(t.TempDir(), "lifecycle.log") }
	t.Cleanup(func() { lifecyclePathFn = prevPath })
}

// stubLifecycle 把生命周期日志指到临时目录，测试不去污染真实用户目录。
func stubLifecycle(t *testing.T) {
	t.Helper()
	prev := lifecyclePathFn
	lifecyclePathFn = func() string { return filepath.Join(t.TempDir(), "lifecycle.log") }
	t.Cleanup(func() { lifecyclePathFn = prev })
}

func TestCleanupOldAfterHealthyRemovesBackup(t *testing.T) {
	stubLifecycle(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	if err := os.WriteFile(self, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := self + ".old"
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, self)
	prevStarted := processStarted
	t.Cleanup(func() { processStarted = prevStarted })
	// 已经跑够观察窗口：这时调用方才算真的确认了健康
	processStarted = time.Now().Add(-minHealthyUptime - time.Second)

	CleanupOldAfterHealthy()

	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("确认健康后该删掉 .old，实际 err=%v", err)
	}
	if got, err := os.ReadFile(self); err != nil || string(got) != "new" {
		t.Errorf("不该动到程序自己：%q (err=%v)", got, err)
	}
}

// 新版还没证明自己活得下来之前，回滚副本必须留着。
func TestCleanupOldWaitsForHealthyUptime(t *testing.T) {
	stubLifecycle(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	backup := self + ".old"
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, self)
	prevStarted := processStarted
	t.Cleanup(func() { processStarted = prevStarted })

	processStarted = time.Now()
	CleanupOld()
	if _, err := os.Stat(backup); err != nil {
		t.Error("启动早期不该删掉回滚副本")
	}

	processStarted = time.Now().Add(-minHealthyUptime - time.Second)
	CleanupOld()
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("跑够观察窗口后该清掉回滚副本，实际 err=%v", err)
	}
}

// 交接状态建不起来（临时目录不可写 / 空间不足）时必须中止更新：旧版继续运行，
// 程序文件一个字节都不动。否则旧版只能退回「Start() 成功即成功」，正是这轮要
// 消灭的行为。
func TestApplyAbortsWhenHandoffCannotBeCreated(t *testing.T) {
	stubLifecycle(t)
	dir := t.TempDir()
	self := filepath.Join(dir, "app.exe")
	newExe := filepath.Join(dir, "app.exe.new")
	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newExe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 把系统临时目录指到一个普通文件上：交接目录必然建不出来，等价于磁盘不可写，
	// 又不用真的把机器上的临时目录弄坏。必须在 t.TempDir() 之后再改。
	blocker := filepath.Join(dir, "tmp-is-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", blocker)
	t.Setenv("TEMP", blocker)

	err := apply(self, newExe)
	if err == nil {
		t.Fatal("交接状态建不起来时必须报错，不能照常替换程序")
	}
	if !strings.Contains(err.Error(), "交接状态") {
		t.Errorf("错误应说明是交接状态建不起来，实际: %v", err)
	}
	if got, rerr := os.ReadFile(self); rerr != nil || string(got) != "old" {
		t.Errorf("中止更新时程序文件必须保持旧版，实际 %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(self + ".old"); !os.IsNotExist(serr) {
		t.Errorf("中止更新时不该改名程序文件，.old 实际 err=%v", serr)
	}
}

// writeFakeExePair 准备一对「新版在正常路径、旧版在 .old」的假程序文件，
// 让回滚测试不必碰真正的测试二进制。
func writeFakeExePair(t *testing.T, self string) {
	t.Helper()
	if err := os.WriteFile(self, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self+".old", []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// startHandoffChild 拉起一个真实子进程模拟「新版」，由子进程自己写交接状态。
func startHandoffChild(t *testing.T, dir, invocation, envKey, testName string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+testName)
	cmd.Env = append(os.Environ(),
		envKey+"=1",
		handoffDirEnv+"="+dir,
		handoffInvocationEnv+"="+invocation)
	if err := cmd.Start(); err != nil {
		t.Fatalf("拉起子进程: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// 模拟「界面刚建好就崩」的新版：确认接手后立刻以非零退出码退出。
func TestHandoffHelperReadyThenExit(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_READY_EXIT") != "1" {
		return
	}
	if err := MarkStartupReady(); err != nil {
		os.Exit(3)
	}
	os.Exit(4) // 崩溃（非零退出），不是用户主动关闭
}

// 模拟「确认接手后用户正常关闭」：退出码 0。
func TestHandoffHelperReadyThenNormalExit(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_NORMAL_EXIT") != "1" {
		return
	}
	if err := MarkStartupReady(); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// 模拟正常站得住的新版：确认接手后继续跑。
func TestHandoffHelperReadyThenStay(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_READY_STAY") != "1" {
		return
	}
	if err := MarkStartupReady(); err != nil {
		os.Exit(3)
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

// 模拟「根本没走到确认点」的新版。
func TestHandoffHelperNeverReady(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_NEVER_READY") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

// 新版确认接手后立刻退出（界面刚起来就崩）时，旧进程必须发现并恢复旧版，
// 而不是把 ready 当成更新成功然后退出。
func TestAwaitHandoffDetectsExitRightAfterReady(t *testing.T) {
	stubLifecycle(t)
	dir, h := newTestHandoff(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	writeFakeExePair(t, self)

	prevGrace := applyPostReadyGrace
	applyPostReadyGrace = time.Second
	t.Cleanup(func() { applyPostReadyGrace = prevGrace })

	reclaimed := false
	SetInstanceHandoffHooks(func() {}, func() bool { reclaimed = true; return true })
	t.Cleanup(func() { SetInstanceHandoffHooks(func() {}, func() bool { return true }) })

	cmd := startHandoffChild(t, dir, h.InvocationID, "GO_WANT_HANDOFF_READY_EXIT", "TestHandoffHelperReadyThenExit")
	err := awaitHandoff(dir, h, cmd, self)
	if err == nil {
		t.Fatal("新版确认后立刻退出时，旧进程不能认为更新成功")
	}
	if !reclaimed {
		t.Error("旧进程必须把单实例互斥体收回来")
	}
	if !strings.Contains(err.Error(), "已恢复旧版") {
		t.Errorf("应说明已恢复旧版，实际: %v", err)
	}
	got, rerr := os.ReadFile(self)
	if rerr != nil || string(got) != "old" {
		t.Errorf("程序路径上应恢复成旧版，实际 %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(self + ".old"); !os.IsNotExist(serr) {
		t.Errorf("恢复成功后 .old 不该还在，实际 err=%v", serr)
	}
}

// 新版确认后用户主动关闭（退出码 0）是正常退出：磁盘上的新版要保留，不回滚。
func TestAwaitHandoffKeepsNewOnNormalExit(t *testing.T) {
	stubLifecycle(t)
	dir, h := newTestHandoff(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	writeFakeExePair(t, self)

	prevGrace := applyPostReadyGrace
	applyPostReadyGrace = time.Second
	t.Cleanup(func() { applyPostReadyGrace = prevGrace })

	SetInstanceHandoffHooks(func() {}, func() bool {
		t.Error("正常退出不该回收互斥体")
		return true
	})
	t.Cleanup(func() { SetInstanceHandoffHooks(func() {}, func() bool { return true }) })

	cmd := startHandoffChild(t, dir, h.InvocationID, "GO_WANT_HANDOFF_NORMAL_EXIT", "TestHandoffHelperReadyThenNormalExit")
	if err := awaitHandoff(dir, h, cmd, self); err != nil {
		t.Fatalf("正常退出不该报错: %v", err)
	}
	got, rerr := os.ReadFile(self)
	if rerr != nil || string(got) != "new" {
		t.Errorf("正常退出应保留新版，实际 %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(self + ".old"); serr != nil {
		t.Errorf("正常退出应保留回滚副本，实际 err=%v", serr)
	}
}

// 确认之后新版还活着才是真的成功：旧进程什么都不该回收、不该回滚。
func TestAwaitHandoffAcceptsNewProcessThatStaysUp(t *testing.T) {
	stubLifecycle(t)
	dir, h := newTestHandoff(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	writeFakeExePair(t, self)

	prevGrace := applyPostReadyGrace
	applyPostReadyGrace = 300 * time.Millisecond
	t.Cleanup(func() { applyPostReadyGrace = prevGrace })

	SetInstanceHandoffHooks(func() {}, func() bool {
		t.Error("成功路径不该收回互斥体")
		return true
	})
	t.Cleanup(func() { SetInstanceHandoffHooks(func() {}, func() bool { return true }) })

	cmd := startHandoffChild(t, dir, h.InvocationID, "GO_WANT_HANDOFF_READY_STAY", "TestHandoffHelperReadyThenStay")
	if err := awaitHandoff(dir, h, cmd, self); err != nil {
		t.Fatalf("新版站住了就不该报错: %v", err)
	}
	if got, rerr := os.ReadFile(self); rerr != nil || string(got) != "new" {
		t.Errorf("成功路径不该动程序文件，实际 %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(self + ".old"); serr != nil {
		t.Errorf("成功路径要留着回滚副本给新进程清理，实际 err=%v", serr)
	}
}

// 新版没走确认点：旧进程照旧不退出、不回滚，保留回滚副本等用户重试。
func TestAwaitHandoffKeepsOldRunningWhenNoConfirmation(t *testing.T) {
	stubLifecycle(t)
	dir, h := newTestHandoff(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	writeFakeExePair(t, self)

	prevWait := applyWaitReady
	applyWaitReady = 300 * time.Millisecond
	t.Cleanup(func() { applyWaitReady = prevWait })

	reclaimed := false
	SetInstanceHandoffHooks(func() {}, func() bool { reclaimed = true; return true })
	t.Cleanup(func() { SetInstanceHandoffHooks(func() {}, func() bool { return true }) })

	cmd := startHandoffChild(t, dir, h.InvocationID, "GO_WANT_HANDOFF_NEVER_READY", "TestHandoffHelperNeverReady")
	if err := awaitHandoff(dir, h, cmd, self); err == nil {
		t.Fatal("没有 ready 时必须报错")
	}
	if !reclaimed {
		t.Error("旧进程必须把单实例互斥体收回来")
	}
	if got, rerr := os.ReadFile(self); rerr != nil || string(got) != "new" {
		t.Errorf("超时路径不该动程序文件，实际 %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(self + ".old"); serr != nil {
		t.Errorf("超时路径要保留回滚副本，实际 err=%v", serr)
	}
}

func TestSwapExeSucceeds(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "app.exe")
	newExe := filepath.Join(dir, "app.exe.new")

	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newExe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	var launched string
	err := swapExe(self, newExe, func(p string) error { launched = p; return nil })
	if err != nil {
		t.Fatalf("swapExe: %v", err)
	}

	got, err := os.ReadFile(self)
	if err != nil || string(got) != "new" {
		t.Errorf("替换后 self 内容 = %q (err=%v), want \"new\"", got, err)
	}
	backup, err := os.ReadFile(self + ".old")
	if err != nil || string(backup) != "old" {
		t.Errorf("备份内容 = %q (err=%v), want \"old\"", backup, err)
	}
	if launched != self {
		t.Errorf("应该拉起 %s，实际 %s", self, launched)
	}
}

// 拷贝失败时必须回滚：用户手上不能既没有旧版也没有新版。
func TestSwapExeRollsBackWhenCopyFails(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "app.exe")
	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := swapExe(self, filepath.Join(dir, "missing.exe"), func(string) error { return nil })
	if err == nil {
		t.Fatal("新版文件不存在时必须报错")
	}

	got, rerr := os.ReadFile(self)
	if rerr != nil {
		t.Fatalf("回滚后应该还能读到旧版: %v", rerr)
	}
	if string(got) != "old" {
		t.Errorf("失败后 self 内容 = %q, want \"old\"", got)
	}
}

// 新版本启动失败时，必须把旧版改回原名。
//
// 不恢复的话，用户下次从快捷方式启动跑的还是那个起不来的版本，而能用的旧版只剩在
// 一个 .old 文件里 —— 他根本不会想到去手动改名。这正是复审报告 7.1 指出的缺口。
func TestSwapExeRollsBackWhenLaunchFails(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "app.exe")
	newExe := filepath.Join(dir, "app.exe.new")

	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newExe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := swapExe(self, newExe, func(string) error {
		return errors.New("模拟启动失败")
	})
	if err == nil {
		t.Fatal("启动失败必须报错")
	}
	if !strings.Contains(err.Error(), "已回滚") {
		t.Errorf("错误信息应说明已回滚，实际: %v", err)
	}

	// 关键：正常路径上必须是旧版，而不是起不来的新版
	got, rerr := os.ReadFile(self)
	if rerr != nil {
		t.Fatalf("回滚后应该还能读到旧版: %v", rerr)
	}
	if string(got) != "old" {
		t.Errorf("启动失败后 self 内容 = %q, want \"old\"（旧版必须回到正常路径）", got)
	}
	// .old 已经被改回正常路径，不该还留着
	if _, serr := os.Stat(self + ".old"); !os.IsNotExist(serr) {
		t.Errorf("回滚成功后 .old 应该消失，实际 err=%v", serr)
	}
}

func TestSwapExeRemovesStaleBackup(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "app.exe")
	newExe := filepath.Join(dir, "app.exe.new")
	for file, body := range map[string]string{
		self:          "old",
		newExe:        "new",
		self + ".old": "上一次更新留下的垃圾",
	} {
		if err := os.WriteFile(file, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := swapExe(self, newExe, func(string) error { return nil }); err != nil {
		t.Fatalf("swapExe: %v", err)
	}
	backup, err := os.ReadFile(self + ".old")
	if err != nil || string(backup) != "old" {
		t.Errorf("备份应该是这一次的旧版，实际 %q (err=%v)", backup, err)
	}
}

// 第 7 项：调用方报「健康」时可能只是控制连接刚握上手，数据面还没被验证。
// 删 .old 必须等到进程跑够观察窗口，不能立即执行。
func TestCleanupOldAfterHealthyDefersUntilUptime(t *testing.T) {
	stubLifecycle(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	backup := self + ".old"
	if err := os.WriteFile(self, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, self)

	prevStarted := processStarted
	prevSchedule := scheduleCleanup
	t.Cleanup(func() {
		processStarted = prevStarted
		scheduleCleanup = prevSchedule
	})

	processStarted = time.Now()
	var scheduled time.Duration
	scheduleCleanup = func(d time.Duration, _ func()) { scheduled = d }

	CleanupOldAfterHealthy()

	if _, err := os.Stat(backup); err != nil {
		t.Error("启动早期确认的「健康」不足以删回滚副本")
	}
	if scheduled <= 0 {
		t.Fatal("应该延迟清理，而不是立刻删或永远不删")
	}
	if scheduled > minHealthyUptime {
		t.Errorf("延迟 %v 超过了观察窗口 %v", scheduled, minHealthyUptime)
	}
}

func TestCleanupOldAfterHealthyRemovesWhenAlreadyMature(t *testing.T) {
	stubLifecycle(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	backup := self + ".old"
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, self)

	prevStarted := processStarted
	prevSchedule := scheduleCleanup
	t.Cleanup(func() {
		processStarted = prevStarted
		scheduleCleanup = prevSchedule
	})

	processStarted = time.Now().Add(-minHealthyUptime - time.Second)
	scheduleCleanup = func(time.Duration, func()) {
		t.Error("已经跑够窗口就不该再调度")
	}

	CleanupOldAfterHealthy()

	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("跑够窗口后应清掉回滚副本，实际 err=%v", err)
	}
}

// 更新后的第一次启动必须放行（给新版机会），第二次仍未健康才回滚。
func TestRollbackIfUnhealthyUpdate(t *testing.T) {
	stubLifecycle(t)
	stubRolloutPaths(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	writeFakeExePair(t, self)

	// 第一次启动：不回滚，只记启动标记。
	rolled, err := RollbackIfUnhealthyUpdate(self, "0.5.0")
	if err != nil || rolled {
		t.Fatalf("第一次启动不该回滚: rolled=%v err=%v", rolled, err)
	}
	if got, _ := os.ReadFile(self); string(got) != "new" {
		t.Fatalf("第一次启动不该动程序文件，实际 %q", got)
	}

	// 第二次启动（窗口内、仍未健康）：回滚。
	rolled, err = RollbackIfUnhealthyUpdate(self, "0.5.0")
	if err != nil || !rolled {
		t.Fatalf("第二次启动应回滚: rolled=%v err=%v", rolled, err)
	}
	if got, _ := os.ReadFile(self); string(got) != "old" {
		t.Errorf("应恢复成旧版，实际 %q", got)
	}
	if got := FailedVersion(); got != "0.5.0" {
		t.Errorf("应记录失败版本 0.5.0，实际 %q", got)
	}

	// 确认健康会清掉启动标记：下次正常运行不会被回滚。
	writeFakeExePair(t, self)
	if err := MarkHealthy("0.5.0"); err != nil {
		t.Fatal(err)
	}
	rolled, err = RollbackIfUnhealthyUpdate(self, "0.5.0")
	if err != nil || rolled {
		t.Fatalf("确认健康后不该回滚: rolled=%v err=%v", rolled, err)
	}

	// 没有 .old：什么都不做。
	if err := os.Remove(self + ".old"); err != nil {
		t.Fatal(err)
	}
	rolled, err = RollbackIfUnhealthyUpdate(self, "0.5.0")
	if err != nil || rolled {
		t.Fatalf("没有回滚副本时不该动文件: rolled=%v err=%v", rolled, err)
	}
}

// 启动标记超出窗口不算连续崩溃：很久之前的一次异常退出不该回滚现在的新版。
func TestRollbackIfUnhealthyUpdateIgnoresStaleStartupMark(t *testing.T) {
	stubLifecycle(t)
	stubRolloutPaths(t)
	self := filepath.Join(t.TempDir(), "app.exe")
	writeFakeExePair(t, self)

	writeStartupMark(startupMark{
		Version: "0.5.0",
		Count:   1,
		At:      time.Now().Add(-2 * startupCrashWindow).Format(time.RFC3339),
	})
	rolled, err := RollbackIfUnhealthyUpdate(self, "0.5.0")
	if err != nil || rolled {
		t.Fatalf("过期启动标记不该触发回滚: rolled=%v err=%v", rolled, err)
	}
}
