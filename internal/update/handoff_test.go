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

// resetHandoffEnv 清掉 init 继承下来的交接身份，避免一个测试的 pending 状态
// 漏到下一个测试里。
func resetHandoffEnv(t *testing.T) {
	t.Helper()
	handoffEnvDir, handoffEnvID = "", ""
	t.Cleanup(func() { handoffEnvDir, handoffEnvID = "", "" })
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

// loaded 只说明二进制加载了，不能当成交接成功。旧版只认 ready。
func TestMarkLoadedDoesNotMarkReady(t *testing.T) {
	dir, h := newTestHandoff(t)

	if err := MarkLoaded(dir, h.InvocationID); err != nil {
		t.Fatalf("MarkLoaded: %v", err)
	}
	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if got.Phase != handoffLoaded {
		t.Errorf("phase = %q, want %q", got.Phase, handoffLoaded)
	}
	if got.NewPID != os.Getpid() {
		t.Errorf("NewPID = %d, want %d", got.NewPID, os.Getpid())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond); err == nil {
		t.Fatal("loaded 状态不能让 WaitReady 返回 nil")
	}
}

// 状态只能往前走：init 晚到的 loaded 不能把已经写好的 ready 倒退回去。
func TestMarkLoadedDoesNotRegressReady(t *testing.T) {
	dir, h := newTestHandoff(t)
	if err := MarkReady(dir, h.InvocationID); err != nil {
		t.Fatal(err)
	}
	if err := MarkLoaded(dir, h.InvocationID); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != handoffReady {
		t.Errorf("phase = %q, want %q", got.Phase, handoffReady)
	}
}

// 已经确认接手的交接不能被事后改写：旧版已经据此退出，再改只会让下一次启动
// 读到互相矛盾的历史。
func TestFailHandoffDoesNotOverwriteReady(t *testing.T) {
	dir, h := newTestHandoff(t)
	if err := MarkReady(dir, h.InvocationID); err != nil {
		t.Fatal(err)
	}
	if err := FailHandoff(dir, h.InvocationID, errors.New("稍后发生的错误")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != handoffReady {
		t.Errorf("phase = %q, want %q", got.Phase, handoffReady)
	}
}

// init 阶段从环境变量拿到交接信息时只写 loaded，ready 等 main 的 MarkStartupReady。
func TestConsumeHandoffEnvWritesLoadedNotReady(t *testing.T) {
	resetHandoffEnv(t)
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, dir)
	t.Setenv(handoffInvocationEnv, h.InvocationID)

	consumeHandoffEnv()

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if got.Phase != handoffLoaded || got.NewPID != os.Getpid() {
		t.Errorf("loaded 没写对: phase=%q pid=%d", got.Phase, got.NewPID)
	}
	if !HandoffPending() {
		t.Error("有交接环境变量时 HandoffPending 必须为 true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond); err == nil {
		t.Fatal("init 阶段写的状态不能让旧版以为交接成功")
	}
}

// MarkStartupReady 是旧版退出的唯一凭据：走完它之后 WaitReady 才返回 nil。
func TestMarkStartupReadyAfterEnv(t *testing.T) {
	resetHandoffEnv(t)
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, dir)
	t.Setenv(handoffInvocationEnv, h.InvocationID)

	consumeHandoffEnv()
	if err := MarkStartupReady(); err != nil {
		t.Fatalf("MarkStartupReady: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if got.NewPID != os.Getpid() {
		t.Errorf("NewPID = %d, want %d", got.NewPID, os.Getpid())
	}
}

// 普通双击启动（没有交接环境变量）时，ready / failed 都不能碰状态文件。
func TestMarkStartupReadyWithoutPendingHandoffIsNoop(t *testing.T) {
	resetHandoffEnv(t)
	if err := MarkStartupReady(); err != nil {
		t.Fatalf("没有待确认交接时不该报错: %v", err)
	}
	FailStartup(errors.New("无交接时的失败"))
}

// 新版没接手时写 failed，旧版要立刻收到原因而不是等到超时。
func TestFailStartupReportsFailedForPendingHandoff(t *testing.T) {
	resetHandoffEnv(t)
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, dir)
	t.Setenv(handoffInvocationEnv, h.InvocationID)
	consumeHandoffEnv()

	FailStartup(errors.New("没有拿到单实例互斥体"))

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != handoffFailed {
		t.Errorf("phase = %q, want %q", got.Phase, handoffFailed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, werr := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond)
	if !errors.Is(werr, ErrHandoffFailed) {
		t.Errorf("应返回 ErrHandoffFailed，实际: %v", werr)
	}
}

func TestConsumeHandoffEnvIgnoresMissingEnv(t *testing.T) {
	resetHandoffEnv(t)
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, "")
	t.Setenv(handoffInvocationEnv, h.InvocationID)

	consumeHandoffEnv()

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != handoffLaunching {
		t.Errorf("没有交接环境变量时不该改状态: phase=%q", got.Phase)
	}
	if HandoffPending() {
		t.Error("没有交接环境变量时 HandoffPending 必须为 false")
	}
}

func TestConsumeHandoffEnvIgnoresMismatchedInvocation(t *testing.T) {
	resetHandoffEnv(t)
	dir, h := newTestHandoff(t)
	t.Setenv(handoffDirEnv, dir)
	t.Setenv(handoffInvocationEnv, "not-"+h.InvocationID)

	consumeHandoffEnv()

	got, err := ReadHandoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != handoffLaunching {
		t.Errorf("InvocationID 对不上时不该写状态: phase=%q", got.Phase)
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

// TestHandoffHelperExitEarly 是被拉起的「新版」里最坏的那种：二进制确实加载了
// （update 包的 init 跑过），但 main 还没跑到「初始化完成」就以非零码退出。
// 真机复现是 exit 17。
func TestHandoffHelperExitEarly(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_EXIT_EARLY") != "1" {
		return
	}
	os.Exit(17)
}

// 复现第 1 个问题：新进程「加载了但没接手就死了」，旧进程不能把 init 写的状态
// 当成交接成功 —— 否则旧进程一退，用户手上一个能跑的客户端都没有。
func TestWaitReadyRejectsChildThatExitsBeforeReady(t *testing.T) {
	dir, h := newTestHandoff(t)

	cmd := exec.Command(os.Args[0], "-test.run=TestHandoffHelperExitEarly")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HANDOFF_EXIT_EARLY=1",
		handoffDirEnv+"="+dir,
		handoffInvocationEnv+"="+h.InvocationID)
	if err := cmd.Start(); err != nil {
		t.Fatalf("拉起子进程: %v", err)
	}
	// 子进程立刻退出，这里同步等它死，确保下面读到的是「死掉之后」的状态。
	_ = cmd.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond); err == nil {
		t.Fatal("新进程以非零码退出后 WaitReady 仍返回 nil：旧进程会以为更新成功然后退出")
	}
}

// 新版明确上报启动失败时，旧进程要立刻知道该恢复，而不是等满整个等待窗口。
func TestWaitReadyReturnsFastOnFailedHandoff(t *testing.T) {
	dir, h := newTestHandoff(t)
	if err := FailHandoff(dir, h.InvocationID, errors.New("拿不到单实例互斥体")); err != nil {
		t.Fatalf("FailHandoff: %v", err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := WaitReady(ctx, dir, h.InvocationID, 10*time.Millisecond)
	if err == nil {
		t.Fatal("failed 状态必须让 WaitReady 返回错误")
	}
	if !errors.Is(err, ErrHandoffFailed) {
		t.Errorf("应返回 ErrHandoffFailed，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "互斥体") {
		t.Errorf("原始失败原因丢了: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("failed 状态要立刻返回，实际等了 %v", elapsed)
	}
}

// TestHandoffHelperProcess 是被拉起的「新版」：update 包的 init 在它启动时
// 只写 loaded，这里模拟 main 走完关键初始化后写 ready。
func TestHandoffHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HANDOFF_HELPER") != "1" {
		return
	}
	if err := MarkStartupReady(); err != nil {
		os.Exit(3)
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
