package server

import (
	"os"
	"path/filepath"
	"testing"
)

// 失败记录是「同版本不再重试」的唯一依据，读写与清除必须成对。
func TestFailedVersionRoundTrip(t *testing.T) {
	old := failedVersionPath
	failedVersionPath = filepath.Join(t.TempDir(), "update-failed")
	t.Cleanup(func() { failedVersionPath = old })

	if v, err := readFailedVersion(); err == nil {
		t.Errorf("还没有记录时应返回错误，实际读到 %q", v)
	}
	if err := writeFailedVersion("0.2.9"); err != nil {
		t.Fatalf("writeFailedVersion: %v", err)
	}
	v, err := readFailedVersion()
	if err != nil {
		t.Fatalf("readFailedVersion: %v", err)
	}
	if v != "0.2.9" {
		t.Errorf("读回 %q，期望 0.2.9", v)
	}
	clearFailedVersion()
	if _, err := readFailedVersion(); err == nil {
		t.Error("clear 之后不该还能读到")
	}
}

// 目录不存在时要自己建出来：首次更新就失败的情况最需要这条记录。
func TestWriteFailedVersionCreatesDir(t *testing.T) {
	old := failedVersionPath
	failedVersionPath = filepath.Join(t.TempDir(), "nested", "dir", "update-failed")
	t.Cleanup(func() { failedVersionPath = old })

	if err := writeFailedVersion("0.3.0"); err != nil {
		t.Fatalf("writeFailedVersion: %v", err)
	}
	if _, err := os.Stat(failedVersionPath); err != nil {
		t.Errorf("文件没建出来: %v", err)
	}
}
