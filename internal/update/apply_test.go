//go:build windows

package update

import (
	"errors"
	"os"
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
