package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// installBinary 要在同目录里落下替换品：内容换成新的、权限带可执行位、不留临时文件。
func TestInstallBinaryReplacesContent(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "antapp-linkd")
	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "download")
	if err := os.WriteFile(newBin, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installBinary(self, newBin); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary" {
		t.Errorf("内容 = %q，期望 new-binary", got)
	}
	// 换上去的必须可执行，否则 systemd 重启会 EACCES。
	// Windows 没有 POSIX 权限位（WriteFile 的 perm 和 Chmod 都不反映在 Mode 上），
	// 只在本平台之外断言这条。
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(self); err != nil {
			t.Fatal(err)
		} else if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("权限 %v 不带可执行位", fi.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "antapp-linkd" && e.Name() != "download" {
			t.Errorf("目录里留下了多余文件 %s", e.Name())
		}
	}
}

// 源文件不存在时要报错，而且不能动目标文件 —— 否则现场会多出一个空的"新版本"。
func TestInstallBinaryMissingSourceKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "antapp-linkd")
	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(self, filepath.Join(dir, "nope")); err == nil {
		t.Fatal("源文件不存在时应该报错")
	}
	got, _ := os.ReadFile(self)
	if string(got) != "old" {
		t.Errorf("失败时不该改动目标，实际 %q", got)
	}
}
