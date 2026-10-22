package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestHandoff(t *testing.T) (string, *Handoff) {
	t.Helper()
	dir := t.TempDir()
	h, err := NewHandoff(dir, "0.4.10", "0.4.11", 30*time.Second)
	if err != nil {
		t.Fatalf("NewHandoff: %v", err)
	}
	return dir, h
}

// 新旧进程必须靠 InvocationID + PID 认亲：同一次更新里两个进程的启动时刻
// 可能落在同一秒，时间戳分不出来。
func TestMarkReadyRecordsPIDAndInvocation(t *testing.T) {
	dir, h := newTestHandoff(t)

	if err := MarkReady(dir, h.InvocationID); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if got.Phase != handoffReady {
		t.Errorf("phase = %q, want %q", got.Phase, handoffReady)
	}
	if got.NewPID != os.Getpid() {
		t.Errorf("NewPID = %d, want %d", got.NewPID, os.Getpid())
	}
	if got.InvocationID != h.InvocationID {
		t.Errorf("InvocationID 被改了: %q, want %q", got.InvocationID, h.InvocationID)
	}
	if got.ReadyAt == nil {
		t.Error("ready 状态必须带 ready_at")
	}
}

// 别的交接留下的 ready 不能算到这次启动头上。
func TestMarkReadyRejectsOtherInvocation(t *testing.T) {
	dir, _ := newTestHandoff(t)
	if err := MarkReady(dir, "someone-else"); err == nil {
		t.Fatal("InvocationID 不匹配时必须拒绝写 ready")
	}
}

func TestWaitReadySeesConfirmation(t *testing.T) {
	dir, h := newTestHandoff(t)

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = MarkReady(dir, h.InvocationID)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := WaitReady(ctx, dir, h.InvocationID, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if got.NewPID != os.Getpid() {
		t.Errorf("NewPID = %d, want %d", got.NewPID, os.Getpid())
	}
}

// 新版没写 ready 时必须在窗口内放弃：旧版不能无限等，也不能把「拉起进程成功」
// 当成「新版已经能跑」。
func TestWaitReadyTimesOut(t *testing.T) {
	dir, h := newTestHandoff(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond)
	if err == nil {
		t.Fatal("没有 ready 时必须超时失败")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应返回 deadline 错误，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("超时等待拖了 %v", elapsed)
	}
}

// 第 8 项：不是比较秒级时间戳，而是比较这次启动的 invocation。
func TestHandoffDistinguishesSameSecondRestarts(t *testing.T) {
	dir, first := newTestHandoff(t)
	if err := MarkReady(dir, first.InvocationID); err != nil {
		t.Fatal(err)
	}

	// 立刻开始第二次交接（同一秒内）：第二次也确认了，但旧 invocation 必须失效。
	second, err := NewHandoff(dir, "0.4.11", "0.4.12", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.InvocationID == first.InvocationID {
		t.Fatal("两次交接不能共用 InvocationID")
	}
	if err := MarkReady(dir, second.InvocationID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := WaitReady(ctx, dir, first.InvocationID, 10*time.Millisecond); err == nil {
		t.Fatal("上一次交接的 ready 不能被当成本次启动")
	}
}

// 新版启动时从环境变量拿到交接信息，自己把 ready 写进状态文件。
func TestConsumeHandoffEnvWritesReady(t *testing.T) {
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, dir)
	t.Setenv(handoffInvocationEnv, h.InvocationID)

	consumeHandoffEnv()

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if got.Phase != handoffReady || got.NewPID != os.Getpid() {
		t.Errorf("ready 没写对: phase=%q pid=%d", got.Phase, got.NewPID)
	}
}

func TestConsumeHandoffEnvIgnoresMissingEnv(t *testing.T) {
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, "")
	t.Setenv(handoffInvocationEnv, h.InvocationID)

	consumeHandoffEnv()

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase == handoffReady {
		t.Error("没有交接环境变量时不该写 ready")
	}
}

func TestConsumeHandoffEnvIgnoresMismatchedInvocation(t *testing.T) {
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, dir)
	t.Setenv(handoffInvocationEnv, "not-"+h.InvocationID)

	consumeHandoffEnv()

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase == handoffReady {
		t.Error("InvocationID 对不上时不该写 ready")
	}
}

// 失败的交接必须留下原始系统错误，而不是一句「更新失败」。
func TestFailHandoffKeepsRawError(t *testing.T) {
	dir, h := newTestHandoff(t)
	if err := FailHandoff(dir, h.InvocationID, errors.New("CreateProcess: %1 is not a valid Win32 application.")); err != nil {
		t.Fatalf("FailHandoff: %v", err)
	}
	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != handoffFailed {
		t.Errorf("phase = %q, want %q", got.Phase, handoffFailed)
	}
	if !strings.Contains(got.Err, "Win32") {
		t.Errorf("原始错误被吞掉了: %q", got.Err)
	}
}

// 状态文件按原子写：读到的永远是完整 JSON。
func TestHandoffWriteIsAtomic(t *testing.T) {
	dir, h := newTestHandoff(t)
	raw, err := os.ReadFile(filepath.Join(dir, handoffFileName))
	if err != nil {
		t.Fatalf("状态文件不存在: %v", err)
	}
	if !strings.Contains(string(raw), h.InvocationID) {
		t.Errorf("状态文件里没有 invocation: %s", raw)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("临时文件没被清掉，目录里有 %d 个文件", len(entries))
	}
}

// TestHandoffHelperProcess 是被拉起的「新版」：update 包的 init 在它启动时
// 读到交接环境变量并写 ready，这里只负责立刻退出。
func TestHandoffHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_HELPER") != "1" {
		return
	}
	os.Exit(0)
}

// 端到端验证第 6 项的交接链路：旧版写状态 → 拉起真实新进程 → 新进程自己确认 →
// 旧版在超时前收到 ready（带新进程 PID，而不是旧版自己的 PID）。
func TestChildProcessConfirmsHandoff(t *testing.T) {
	dir, h := newTestHandoff(t)

	cmd := exec.Command(os.Args[0], "-test.run=TestHandoffHelperProcess")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HANDOFF_HELPER=1",
		handoffDirEnv+"="+dir,
		handoffInvocationEnv+"="+h.InvocationID)
	if err := cmd.Start(); err != nil {
		t.Fatalf("拉起子进程: %v", err)
	}
	childPID := cmd.Process.Pid

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := WaitReady(ctx, dir, h.InvocationID, 20*time.Millisecond)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("旧版没等到新进程确认: %v", err)
	}
	_ = cmd.Wait()

	if got.NewPID != childPID {
		t.Errorf("ready 里的 PID = %d, want 子进程 %d（不能是旧版自己）", got.NewPID, childPID)
	}
}
