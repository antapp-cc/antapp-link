package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

// 外层 TLS 握手必须有独立上限：relay 的 256 条限制在转发连接上，管不到隧道监听。
// 没有它，大量半开握手能把 fd 和内存吃光，而 accept 循环看起来一切正常。
func TestHandshakeLimiterCapsConcurrent(t *testing.T) {
	l := newHandshakeLimiter(1)
	release, ok := l.acquire()
	if !ok {
		t.Fatal("空位时应该能拿到名额")
	}

	start := time.Now()
	if _, ok := l.acquire(); ok {
		t.Fatal("超上限时必须拒绝，而不是无限排队")
	}
	// 等待队列不能缓冲：否则每个被拒连接都要先在内存里挂一会儿，
	// 洪峰时 goroutine/fd 仍然冲高。
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("拒绝应该立刻返回，实际等了 %v", elapsed)
	}
	if inFlight, peak, refused := l.snapshot(); inFlight != 1 || peak != 1 || refused != 1 {
		t.Errorf("统计不对: inFlight=%d peak=%d refused=%d, want 1/1/1", inFlight, peak, refused)
	}

	release()
	if inFlight, _, _ := l.snapshot(); inFlight != 0 {
		t.Errorf("释放后 inFlight 应为 0，实际 %d", inFlight)
	}
	again, ok := l.acquire()
	if !ok {
		t.Fatal("释放后应能再拿到名额")
	}
	again()
}

// 重复 release 会把名额多还一次，最终让并发上限形同虚设。
func TestHandshakeLimiterReleaseIsIdempotent(t *testing.T) {
	l := newHandshakeLimiter(2)
	release, ok := l.acquire()
	if !ok {
		t.Fatal("应该拿到名额")
	}
	release()
	release()

	a, ok1 := l.acquire()
	b, ok2 := l.acquire()
	if !ok1 || !ok2 {
		t.Fatalf("重复释放后名额应仍然只有 2 个: %v %v", ok1, ok2)
	}
	if _, ok := l.acquire(); ok {
		t.Error("第三个名额不该存在（重复 release 不能凭空造名额）")
	}
	a()
	b()
}

// 洪峰时不能在派生处理协程之后才排队：名额之外直接关连接，
// 否则 accept 每个连接都先起一个 goroutine，等待队列没有硬上限。
func TestServeConnRefusesBeyondLimitWithoutSpawning(t *testing.T) {
	s := &Server{log: discardLogger(), hs: newHandshakeLimiter(4), tlsCfg: &tls.Config{}}

	var clients []net.Conn
	t.Cleanup(func() {
		for _, c := range clients {
			_ = c.Close()
		}
	})

	// 占满名额：这 4 条连接保持「正在握手」——对端始终不发 ClientHello。
	for i := 0; i < 4; i++ {
		srv, cli := net.Pipe()
		clients = append(clients, cli)
		if !s.serveConn(context.Background(), srv) {
			t.Fatalf("第 %d 条应被接纳", i)
		}
	}
	if inFlight, _, _ := s.hs.snapshot(); inFlight != 4 {
		t.Fatalf("握手中的连接 = %d, want 4", inFlight)
	}

	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		srv, cli := net.Pipe()
		if s.serveConn(context.Background(), srv) {
			t.Fatalf("第 %d 条超出上限，应被拒绝", i)
		}
		_ = cli.Close()
	}
	if after := runtime.NumGoroutine(); after-before > 8 {
		t.Errorf("拒绝路径不该派生 goroutine：%d → %d", before, after)
	}
	if _, _, refused := s.hs.snapshot(); refused != 100 {
		t.Errorf("拒绝计数 = %d, want 100", refused)
	}

	// 对端关闭 → TLS 握手结束 → 名额必须归还，而不是永久占着。
	for _, c := range clients[:4] {
		_ = c.Close()
	}
	waitFor(t, 2*time.Second, func() bool {
		inFlight, _, _ := s.hs.snapshot()
		return inFlight == 0
	}, "握手结束后名额没有归还")
}

// 握手完成、进入会话后必须归还名额：长连接占着握手槽，64 条会话就能把
// 新连接全部拒掉。同时这条验证 HandshakeInFlight 只统计真正在做握手的连接。
func TestHandshakeSlotReleasedBeforeSession(t *testing.T) {
	s, cliCfg := newTestServer(t, newHandshakeLimiter(1))

	// 用真实 TCP 而不是 net.Pipe：TLS 握手在无缓冲管道上会互相等，
	// 这里要验证的是服务端的名额归还，不是管道的调度行为。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	serverConn := <-accepted

	if !s.serveConn(context.Background(), serverConn) {
		t.Fatal("第一条连接应被接纳")
	}

	clientDone := make(chan error, 1)
	go func() {
		cli := tls.Client(clientConn, cliCfg)
		if err := cli.Handshake(); err != nil {
			clientDone <- err
			return
		}
		hello, err := json.Marshal(helloPayload{Version: protoVersion, Client: "test", Sid: "sid-1", Members: 1})
		if err != nil {
			clientDone <- err
			return
		}
		if err := proto.WriteFrame(cli, proto.TypeHello, hello); err != nil {
			clientDone <- err
			return
		}
		typ, _, err := proto.ReadFrame(cli)
		if err == nil && typ != proto.TypeHelloAck {
			err = fmt.Errorf("首帧回包类型 = %s, want HELLO_ACK", typ)
		}
		clientDone <- err
	}()

	if err := <-clientDone; err != nil {
		t.Fatalf("客户端握手/HELLO 失败: %v", err)
	}

	// HELLO_ACK 已经回来，说明服务端进入了会话；此刻名额必须已经还回去。
	waitFor(t, time.Second, func() bool {
		inFlight, _, _ := s.hs.snapshot()
		return inFlight == 0
	}, "握手完成后 HandshakeInFlight 应归零")

	// 会话仍在进行时（连接没关），新的握手请求不能被这条长连接挡住。
	release, ok := s.hs.acquire()
	if !ok {
		t.Error("长连接不该一直占着握手名额")
	}
	if ok {
		release()
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestServer 起一个只够握手与会话外壳的 Server：测试里不会出现数据帧，
// 所以不需要真实 TUN。证书走 pki，和线上同一条路径。
func newTestServer(t *testing.T, hs *handshakeLimiter) (*Server, *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	if err := pki.Init(dir); err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	srvCfg, err := pki.ServerTLSConfig(dir)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	inv, err := pki.Issue(dir, "test-client", "127.0.0.1:62233", pki.TunnelParams{
		TunnelIP: "10.10.0.2", Gateway: "10.10.0.1", Prefix: 24, MTU: 1400, Members: 1,
	})
	if err != nil {
		t.Fatalf("pki.Issue: %v", err)
	}
	cliCfg, err := pki.ClientTLSConfig(inv)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	s := &Server{
		cfg:    Default(),
		tlsCfg: srvCfg,
		log:    discardLogger(),
		hs:     hs,
		joins:  newJoinGuard(),
		fatal:  make(chan error, 1),
	}
	return s, cliCfg
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}
