package client

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// 文件日志跟界面行一样简单：10-26 15:35:14 . 出网自检通过（不带 time=/level=/msg=）。
func TestTeeHandlerFileLine(t *testing.T) {
	var buf strings.Builder
	h := &teeHandler{w: &buf, buf: NewLogBuffer(10)}
	rec := slog.NewRecord(time.Date(2026, 10, 26, 15, 35, 14, 0, time.Local), slog.LevelInfo, "出网自检通过", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	want := "10-26 15:35:14 . 出网自检通过\n"
	if got := buf.String(); got != want {
		t.Fatalf("文件日志 = %q，want %q", got, want)
	}
}

// 文件要写 Debug（补拨失败、丢包、UDP 失败等关键现场都在 Debug 里），
// 界面只显示 Info 及以上，保持简洁。
func TestTeeHandlerDebug(t *testing.T) {
	var buf strings.Builder
	h := &teeHandler{w: &buf, buf: NewLogBuffer(10)}
	rec := slog.NewRecord(time.Date(2026, 10, 26, 15, 36, 0, 0, time.Local), slog.LevelDebug, "成员连接补拨失败", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "成员连接补拨失败") {
		t.Fatalf("文件应写 Debug 日志，实际 %q", got)
	}
	lines, _ := h.buf.Since(0)
	if len(lines) != 0 {
		t.Fatalf("界面不该显示 Debug 日志，实际 %q", lines)
	}
}
