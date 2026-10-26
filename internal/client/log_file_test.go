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

// 日志会话切换（启动/连接/切配置）后 epoch 递增——界面靠它清空日志控件，
// 避免旧会话的日志和新会话的混在一起。
func TestLogResetEpoch(t *testing.T) {
	dir := t.TempDir()
	_, _, closeLog, err := NewFileLogger(dir)
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	t.Cleanup(closeLog)

	before := LogResetEpoch()
	SwitchLogSession("pinode.antapp")
	if got := LogResetEpoch(); got != before+1 {
		t.Fatalf("切换会话后 epoch = %d, want %d", got, before+1)
	}
	SwitchLogSession("other.antapp")
	if got := LogResetEpoch(); got != before+2 {
		t.Fatalf("再次切换后 epoch = %d, want %d", got, before+2)
	}
}
