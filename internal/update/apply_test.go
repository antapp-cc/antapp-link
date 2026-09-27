//go:build windows

package update

import (
	"os"
	"path/filepath"
	"testing"
)

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
