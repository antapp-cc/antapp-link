package update

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 生命周期日志独立于按会话清空的 client.log：升级、恢复、退出原因必须能跨会话查。
func TestLifecycleRecordsStructuredEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	l := newLifecycleLog(path, 1<<20)

	if err := l.Record(LifecycleEvent{
		Event:   EventUpdateStart,
		Version: "0.4.10",
		Detail:  "开始替换程序",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Record(LifecycleEvent{
		Event: EventUpdateFailed,
		Err:   "CreateProcess: %1 is not a valid Win32 application.",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var events []LifecycleEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e LifecycleEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("行不是结构化 JSON: %q (%v)", sc.Text(), err)
		}
		events = append(events, e)
	}
	if len(events) != 2 {
		t.Fatalf("事件数 = %d, want 2", len(events))
	}
	if events[0].Event != EventUpdateStart || events[1].Event != EventUpdateFailed {
		t.Errorf("事件顺序不对: %+v", events)
	}
	if events[0].PID != os.Getpid() {
		t.Errorf("事件应带 PID，实际 %d", events[0].PID)
	}
	if events[0].Time.IsZero() {
		t.Error("事件应带时间")
	}
	if !strings.Contains(events[1].Err, "Win32") {
		t.Errorf("原始系统错误必须保留: %q", events[1].Err)
	}
}

// 有容量上限：超了就轮转，只留一份备份，磁盘不会被日志撑爆。
func TestLifecycleRotatesAtCapacity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.log")
	l := newLifecycleLog(path, 512)

	for i := 0; i < 20; i++ {
		if err := l.Record(LifecycleEvent{Event: EventUpdateReady, Detail: strings.Repeat("x", 40)}); err != nil {
			t.Fatalf("第 %d 条: %v", i, err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 512 {
		t.Errorf("当前日志 %d 字节，超过上限 512", info.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("轮转后应留下 .1 备份: %v", err)
	}
}

func TestLifecycleSurvivesUnwritableDirectory(t *testing.T) {
	// 目录不存在时 Record 自己建；父路径是一个文件时建不出来，必须静默返回错误
	// 而不是让更新流程崩掉。
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newLifecycleLog(filepath.Join(blocker, "lifecycle.log"), 1024)
	if err := l.Record(LifecycleEvent{Event: EventUpdateStart}); err == nil {
		t.Error("写不进去时应返回错误，交给调用方决定记不记")
	}
}

// 生命周期路径可由测试替换；默认路径必须落在用户可写的位置，而不是安装目录
// （Program Files 对普通用户只读，启动期的生命周期事件会全部丢掉）。
func TestLifecyclePathIsWritableLocation(t *testing.T) {
	path := defaultLifecyclePath()
	if path == "" {
		t.Fatal("默认生命周期日志路径不能为空")
	}
	if !filepath.IsAbs(path) {
		t.Errorf("默认路径应是绝对路径: %q", path)
	}
	if strings.Contains(strings.ToLower(path), "program files") {
		t.Errorf("不能写安装目录: %q", path)
	}
}

// recordLifecycle 是更新流程用的 best-effort 入口：写失败不能影响更新。
func TestRecordLifecycleIsBestEffort(t *testing.T) {
	prev := lifecyclePathFn
	t.Cleanup(func() { lifecyclePathFn = prev })

	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	lifecyclePathFn = func() string { return filepath.Join(blocker, "lifecycle.log") }

	// 不应 panic，也不该返回错误给更新路径
	recordLifecycle(LifecycleEvent{Event: EventUpdateStart})
}

// LogLifecycle 是 client/main 记录启动、恢复、退出原因的入口：原始系统错误必须保留。
func TestLogLifecycleRecordsCallerEvents(t *testing.T) {
	prev := lifecyclePathFn
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	lifecyclePathFn = func() string { return path }
	t.Cleanup(func() { lifecyclePathFn = prev })

	LogLifecycle(EventProcessExit, "用户从托盘退出", errors.New("WSAECONNRESET: connection reset by peer"))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("生命周期日志没写出来: %v", err)
	}
	var e LifecycleEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
		t.Fatalf("不是结构化 JSON: %q", raw)
	}
	if e.Event != EventProcessExit {
		t.Errorf("event = %q, want %q", e.Event, EventProcessExit)
	}
	if !strings.Contains(e.Err, "WSAECONNRESET") {
		t.Errorf("原始系统错误被吞掉了: %q", e.Err)
	}
	if e.PID != os.Getpid() || e.Invocation == "" {
		t.Errorf("事件应带 PID 与 invocation: pid=%d inv=%q", e.PID, e.Invocation)
	}
}
