package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 外层 TLS 握手必须有独立上限：relay 的 256 条限制在转发连接上，管不到隧道监听。
// 没有它，大量半开握手能把 fd 和内存吃光，而 accept 循环看起来一切正常。
func TestHandshakeLimiterCapsConcurrent(t *testing.T) {
	l := newHandshakeLimiter(1, 20*time.Millisecond)
	release, ok := l.acquire(context.Background())
	if !ok {
		t.Fatal("空位时应该能拿到名额")
	}

	start := time.Now()
	if _, ok := l.acquire(context.Background()); ok {
		t.Fatal("超上限时必须拒绝，而不是无限排队")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Errorf("拒绝前应该排满等待预算，实际只等了 %v", elapsed)
	}
	if inFlight, peak, refused := l.snapshot(); inFlight != 1 || peak != 1 || refused != 1 {
		t.Errorf("统计不对: inFlight=%d peak=%d refused=%d, want 1/1/1", inFlight, peak, refused)
	}

	release()
	again, ok := l.acquire(context.Background())
	if !ok {
		t.Fatal("释放后应能再拿到名额")
	}
	again()
	if inFlight, _, _ := l.snapshot(); inFlight != 0 {
		t.Errorf("全部释放后 inFlight 应为 0，实际 %d", inFlight)
	}
}

// 重复 release 会把名额多还一次，最终让并发上限形同虚设。
func TestHandshakeLimiterReleaseIsIdempotent(t *testing.T) {
	l := newHandshakeLimiter(2, time.Millisecond)
	release, ok := l.acquire(context.Background())
	if !ok {
		t.Fatal("应该拿到名额")
	}
	release()
	release()

	a, ok1 := l.acquire(context.Background())
	b, ok2 := l.acquire(context.Background())
	if !ok1 || !ok2 {
		t.Fatalf("重复释放后名额应仍然只有 2 个: %v %v", ok1, ok2)
	}
	if _, ok := l.acquire(context.Background()); ok {
		t.Error("第三个名额不该存在（重复 release 不能凭空造名额）")
	}
	a()
	b()
}

// 排队期间 ctx 取消（进程正在收尾）要立刻返回，且不能泄漏名额。
func TestHandshakeLimiterStopsOnContextCancel(t *testing.T) {
	l := newHandshakeLimiter(1, time.Hour)
	release, ok := l.acquire(context.Background())
	if !ok {
		t.Fatal("应该拿到名额")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, ok := l.acquire(ctx); ok {
		t.Fatal("ctx 已取消不该拿到名额")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("取消后应立刻返回，实际 %v", elapsed)
	}

	release()
	if inFlight, _, _ := l.snapshot(); inFlight != 0 {
		t.Errorf("取消那次不该占着名额，inFlight=%d", inFlight)
	}
}

// /proc/self/limits 的 soft 上限是文件描述符预算的真实来源。
// LimitNPROC=infinity 不代表 fd 够用，两者是不同资源。
func TestParseMaxOpenFiles(t *testing.T) {
	sample := `Limit                     Soft Limit           Hard Limit           Units
Max cpu time              unlimited            unlimited            seconds
Max open files            1024                 524288               files
Max locked memory         65536                65536                bytes
`
	soft, ok := parseMaxOpenFiles(strings.NewReader(sample))
	if !ok || soft != 1024 {
		t.Errorf("解析结果 = %d/%v, want 1024/true", soft, ok)
	}

	unlimited := `Max open files            unlimited            unlimited            files
`
	soft, ok = parseMaxOpenFiles(strings.NewReader(unlimited))
	if !ok || soft != -1 {
		t.Errorf("unlimited 应解析成 -1，实际 %d/%v", soft, ok)
	}

	if _, ok := parseMaxOpenFiles(strings.NewReader("Limit Soft Limit Hard Limit Units\n")); ok {
		t.Error("没有 Max open files 行时不该假装解析成功")
	}
}

func TestReadProcFDUsage(t *testing.T) {
	root := t.TempDir()
	fdDir := filepath.Join(root, "self", "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0", "1", "2", "7"} {
		if err := os.WriteFile(filepath.Join(fdDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	limits := "Max open files            4096                 1048576              files\n"
	if err := os.WriteFile(filepath.Join(root, "self", "limits"), []byte(limits), 0o600); err != nil {
		t.Fatal(err)
	}

	used, limit, err := readProcFDUsage(root)
	if err != nil {
		t.Fatalf("读伪造的 /proc 失败: %v", err)
	}
	if used != 4 {
		t.Errorf("fd 数量 = %d, want 4", used)
	}
	if limit != 4096 {
		t.Errorf("fd 上限 = %d, want 4096", limit)
	}
}

// 读不到必须报错，不能被当成「用了 0 个 fd」——那会让 status 永远显示健康。
func TestReadProcFDUsageMissingReportsError(t *testing.T) {
	if used, limit, err := readProcFDUsage(t.TempDir()); err == nil {
		t.Errorf("没有 /proc/self/fd 时应返回错误，实际 used=%d limit=%d", used, limit)
	}
}

// ip -s link 的 RX/TX 行下面那排数字里第 4 列是 dropped。
func TestParseIPLinkDrops(t *testing.T) {
	out := `2: antapp0: <POINTOPOINT,MULTICAST,NOARP,UP,LOWER_UP> mtu 1400 qdisc fq_codel state UNKNOWN mode DEFAULT group default qlen 2000
    link/none
    RX:  bytes packets errors dropped  missed   mcast
    123456     789      0      17       0       0
    TX:  bytes packets errors dropped carrier collsns
    654321     987      0      23       0       0
`
	rx, tx, ok := parseIPLinkDrops(out)
	if !ok {
		t.Fatal("应该解析出丢包计数")
	}
	if rx != 17 || tx != 23 {
		t.Errorf("丢包 = rx %d / tx %d, want 17/23", rx, tx)
	}
}

// 设备不在或输出被截断时必须报「不知道」，而不是返回 0/0 冒充健康。
func TestParseIPLinkDropsRejectsTruncatedOutput(t *testing.T) {
	for _, out := range []string{
		"",
		"2: antapp0: <POINTOPOINT> mtu 1400\n    link/none\n",
		"RX:  bytes packets errors dropped  missed   mcast\n    123456     789      0      17       0       0\n",
	} {
		if _, _, ok := parseIPLinkDrops(out); ok {
			t.Errorf("残缺输出不该解析成功: %q", out)
		}
	}
}

// 外部命令要有预算：状态读取卡住时不能把状态刷新一起拖住。
func TestRunCommandHonoursBudget(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	start := time.Now()
	_, err := runCommand(context.Background(), 100*time.Millisecond,
		os.Args[0], "-test.run=TestHelperProcess", "--", "sleep")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("超时命令必须返回错误")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误应说明超时，实际: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("预算 100ms 的命令跑了 %v，说明 kill 没生效", elapsed)
	}
}

func TestRunCommandReturnsOutput(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	// 预算给 5 秒而不是 1 秒：子进程是测试二进制自己，-race 下加载和初始化经常
	// 超过 1 秒，那不是 runCommand 的问题。这里要验证的是「正常命令能拿到输出」，
	// 超时行为由 TestRunCommandHonoursBudget 单独覆盖。
	out, err := runCommand(context.Background(), 5*time.Second,
		os.Args[0], "-test.run=TestHelperProcess", "--", "echo", "hello")
	if err != nil {
		t.Fatalf("正常命令不该失败: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello" {
		t.Errorf("输出 = %q, want hello", got)
	}
}

func TestReadTunDropsParsesCommandOutput(t *testing.T) {
	prev := runStateCommand
	t.Cleanup(func() { runStateCommand = prev })

	runStateCommand = func(context.Context, time.Duration, string, ...string) ([]byte, error) {
		return []byte(`2: antapp0: <POINTOPOINT,MULTICAST,NOARP,UP,LOWER_UP> mtu 1400 qdisc fq_codel state UNKNOWN mode DEFAULT group default qlen 2000
    link/none
    RX:  bytes packets errors dropped  missed   mcast
    123456     789      0      17       0       0
    TX:  bytes packets errors dropped carrier collsns
    654321     987      0      23       0       0
`), nil
	}
	rx, tx, err := readTunDrops(context.Background(), "antapp0")
	if err != nil {
		t.Fatalf("readTunDrops: %v", err)
	}
	if rx != 17 || tx != 23 {
		t.Errorf("丢包 = %d/%d, want 17/23", rx, tx)
	}
}

// 命令失败必须向上报错：调用方会把 known 置 false，不能显示成 0 丢包。
func TestReadTunDropsReportsCommandFailure(t *testing.T) {
	prev := runStateCommand
	t.Cleanup(func() { runStateCommand = prev })

	runStateCommand = func(context.Context, time.Duration, string, ...string) ([]byte, error) {
		return nil, errors.New(`exec: "ip": executable file not found`)
	}
	if _, _, err := readTunDrops(context.Background(), "antapp0"); err == nil {
		t.Fatal("命令失败时必须返回错误")
	}
}

func TestReadTunDropsRejectsUnparsableOutput(t *testing.T) {
	prev := runStateCommand
	t.Cleanup(func() { runStateCommand = prev })

	runStateCommand = func(context.Context, time.Duration, string, ...string) ([]byte, error) {
		return []byte("no statistics here"), nil
	}
	if _, _, err := readTunDrops(context.Background(), "antapp0"); err == nil {
		t.Fatal("输出解析不出来时必须返回错误")
	}
}

// status 行必须把「读不到」和「真的是 0」分开：这是第 12 章第 3 项的核心。
func TestStatusResourceLinesDistinguishUnknownFromZero(t *testing.T) {
	unknown := Status{}
	if got := fdUsageLine(unknown); !strings.Contains(got, "未知") {
		t.Errorf("fd 读不到时该显示未知: %q", got)
	}
	if got := tunDropsLine(unknown); !strings.Contains(got, "未知") {
		t.Errorf("丢包读不到时该显示未知: %q", got)
	}
	if got := handshakeLine(unknown); got != "" {
		t.Errorf("没有握手数据时不该占一行: %q", got)
	}

	known := Status{
		FDKnown: true, FDUsed: 12, FDLimit: 65535,
		TUNDropsKnown: true, TUNRxDropped: 17, TUNTxDropped: 23,
		HandshakePeak: 5, HandshakeRefused: 2,
	}
	if got := fdUsageLine(known); !strings.Contains(got, "12 / 65535") {
		t.Errorf("fd 行没报出用量: %q", got)
	}
	if got := tunDropsLine(known); !strings.Contains(got, "17") || !strings.Contains(got, "23") {
		t.Errorf("丢包行没报出数字: %q", got)
	}
	if got := handshakeLine(known); !strings.Contains(got, "峰值 5") {
		t.Errorf("握手指标没报出峰值: %q", got)
	}

	// 真的是 0 时仍然要显示数字，不能退回「未知」
	zero := Status{FDKnown: true, FDUsed: 0, FDLimit: 1024, TUNDropsKnown: true}
	if got := fdUsageLine(zero); !strings.Contains(got, "0 / 1024") {
		t.Errorf("真的 0 该显示 0: %q", got)
	}
}

// TestHelperProcess 是给 runCommand 测试用的假外部命令。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) > 0 {
		switch args[0] {
		case "sleep":
			time.Sleep(time.Hour)
		case "echo":
			_, _ = os.Stdout.WriteString(strings.Join(args[1:], " ") + "\n")
		}
	}
	os.Exit(0)
}
