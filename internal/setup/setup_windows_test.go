//go:build windows

package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseClientFreshInstall(t *testing.T) {
	dst := filepath.Join(t.TempDir(), AppExeName)

	if err := releaseClient(dst, []byte("new")); err != nil {
		t.Fatalf("releaseClient: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "new" {
		t.Errorf("装完 dst 内容 = %q (err=%v), want \"new\"", got, err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Error("临时文件该被改名成 dst，不该留下")
	}
}

// 写新文件失败时旧版必须改回原名：用户手上不能既没有旧版也没有新版。
func TestReleaseClientRollsBackWhenWriteFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, AppExeName)
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 临时文件名被目录占住，写新文件必然失败
	if err := os.Mkdir(dst+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	err := releaseClient(dst, []byte("new"))
	if err == nil {
		t.Fatal("写不进去时必须报错")
	}

	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("回滚后应该还能读到旧版: %v", rerr)
	}
	if string(got) != "old" {
		t.Errorf("失败后 dst 内容 = %q, want \"old\"", got)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Error("回滚后不该还留着 .old")
	}
}

func TestReleaseClientOverwritesExistingInstall(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, AppExeName)
	for file, body := range map[string]string{
		dst:          "old",
		dst + ".old": "上一次安装留下的垃圾",
	} {
		if err := os.WriteFile(file, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := releaseClient(dst, []byte("new")); err != nil {
		t.Fatalf("releaseClient: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "new" {
		t.Errorf("装完 dst 内容 = %q (err=%v), want \"new\"", got, err)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Error("装成功后不该留下 .old")
	}
}
