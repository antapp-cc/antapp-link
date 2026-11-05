package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 崩溃日志必须落到 logs\ 下，且带上版本、panic 值和堆栈。
func TestWritePanicLog(t *testing.T) {
	dir := t.TempDir()
	stack := []byte("goroutine 1 [running]:\nmain.run()")
	if err := WritePanicLog(dir, "boom", stack); err != nil {
		t.Fatalf("WritePanicLog: %v", err)
	}
	entries, err := os.ReadDir(LogsDir(dir))
	if err != nil {
		t.Fatalf("读日志目录: %v", err)
	}
	var name string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "panic-") {
			name = e.Name()
			break
		}
	}
	if name == "" {
		t.Fatal("没有生成 panic 日志")
	}
	raw, err := os.ReadFile(filepath.Join(LogsDir(dir), name))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"version: " + Version, "panic: boom", "main.run()"} {
		if !strings.Contains(body, want) {
			t.Fatalf("崩溃日志缺少 %q：\n%s", want, body)
		}
	}
}
