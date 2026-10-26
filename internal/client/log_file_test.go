package client

import (
	"os"
	"path/filepath"
	"testing"
)

// CurrentLogFile 返回当前会话日志文件；NewFileLogger 之后应指向 client.log，
// SwitchLogSession 之后应切到 <配置名>.log。
func TestCurrentLogFile(t *testing.T) {
	dir := t.TempDir()
	_, _, closeLog, err := NewFileLogger(dir)
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	t.Cleanup(closeLog)

	got := CurrentLogFile()
	if want := filepath.Join(LogsDir(dir), "client.log"); got != want {
		t.Fatalf("初始日志文件 = %q，want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("日志文件应存在: %v", err)
	}

	SwitchLogSession("pinode.antapp")
	if got, want := CurrentLogFile(), filepath.Join(LogsDir(dir), "pinode.log"); got != want {
		t.Fatalf("切换后日志文件 = %q，want %q", got, want)
	}
}
