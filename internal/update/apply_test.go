//go:build windows

package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubExecutable 把「当前程序在哪」指到临时目录里的假程序，免得测试删到别处。
func stubExecutable(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
}

func TestCleanupOldAfterHealthyRemovesBackup(t *testing.T) {
	self := filepath.Join(t.TempDir(), "app.exe")
	if err := os.WriteFile(self, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := self + ".old"
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, self)

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
