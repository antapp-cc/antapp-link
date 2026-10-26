package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealIfNeededIsNoopWithoutStateFile(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	if err := app.HealIfNeeded(); err != nil {
		t.Errorf("没有残留状态时不该报错: %v", err)
	}
}

func TestStatusBeforeConnect(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	st := app.Status()
	if st.Running || st.Online {
		t.Error("还没连上就报告已连接")
	}
	if st.Server != "103.143.11.34:62233" {
		t.Errorf("Server = %q", st.Server)
	}
	if st.TunnelIP != "" {
		t.Errorf("没连上时 TunnelIP 该是空的，实际 %q", st.TunnelIP)
	}
}

func TestDisconnectWithoutConnectIsNoop(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	if err := app.Disconnect(); err != nil {
		t.Errorf("没连接时断开应当直接返回，实际 %v", err)
	}
}

// 有待还原现场时，Status 必须报出来：UI 的自动重连守护要靠它改成「先重试恢复」，
// 否则会反复调用 Connect，而 Connect 在待还原状态下只会一直拒绝。
func TestStatusReportsPendingRestore(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	app.mu.Lock()
	app.pendingRestore = &pendingRestoreState{snap: testSnapshot(), cfg: testNetConfig()}
	app.mu.Unlock()

	if !app.Status().PendingRestore {
		t.Fatal("有待还原现场时 Status 必须报告 PendingRestore")
	}
}

// 更新交接期间不能再去接管网络：旧版此时已经让出单实例锁（复审第 4 项）。
func TestConnectRejectedWhileUpdating(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	app.mu.Lock()
	app.updating = true
	app.mu.Unlock()

	err := app.Connect()
	if err == nil || !strings.Contains(err.Error(), "正在更新") {
		t.Fatalf("更新期间 Connect 必须被拒绝，实际: %v", err)
	}
	if st := app.Status(); !st.Updating {
		t.Fatal("Status 必须报告更新中")
	}
}

func TestStatePathUnderRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	app := NewApp(testInvite(t), dir, nil)
	if got, want := app.RootDir(), dir; got != want {
		t.Errorf("RootDir = %q, want %q", got, want)
	}
	if got, want := StatePath(app.RootDir()), filepath.Join(dir, "data", "state.json"); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
}

// 三个子目录各司其职，别再把配置和日志混进 data。
func TestDirLayout(t *testing.T) {
	root := `C:\Program Files\AntApp Link`
	cases := []struct{ got, want, what string }{
		{ConfigDir(root), filepath.Join(root, "config"), "ConfigDir"},
		{LogsDir(root), filepath.Join(root, "logs"), "LogsDir"},
		{RuntimeDir(root), filepath.Join(root, "data"), "RuntimeDir"},
		{StatePath(root), filepath.Join(root, "data", "state.json"), "StatePath"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
		}
	}
}

// 不轮转：一次运行的日志完整落在一个文件里，不产生 .1 备份。
func TestOpenLogWritesWithoutRotate(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(StatePath(dir))
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer l.Close()

	line := []byte("0123456789abcdef0123456789abcdef\n")
	for i := 0; i < 10; i++ {
		if _, err := l.Write(line); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	raw, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat(string(line), 10); string(raw) != want {
		t.Fatalf("内容不完整：%q", raw)
	}
	if _, err := os.Stat(StatePath(dir) + ".1"); !os.IsNotExist(err) {
		t.Errorf("不该有 .1 轮转备份")
	}
}

// healthTestApp 造一个不写真实日志的 App，避免周期自检的日志盖住测试输出。
func healthTestApp(t *testing.T) *App {
	t.Helper()
	return NewApp(testInvite(t), t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 连续失败达到阈值必须触发一次重建：否则「显示已连接但没网」只能等 12 小时兜底。
func TestWatchHealthLoopRebuildsAfterConsecutiveFailures(t *testing.T) {
	oldInt, oldLimit, oldProbe, oldRebuild := healthProbeInterval, healthProbeFailLimit, healthProbe, healthRebuildHook
	healthProbeInterval = 10 * time.Millisecond
	healthProbeFailLimit = 3
	healthProbe = func(*App, context.Context) error { return errors.New("探测失败") }
	rebuilt := make(chan struct{}, 1)
	healthRebuildHook = func(*App) {
		select {
		case rebuilt <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		healthProbeInterval, healthProbeFailLimit = oldInt, oldLimit
		healthProbe, healthRebuildHook = oldProbe, oldRebuild
	})

	app := healthTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.watchHealthLoop(ctx)

	select {
	case <-rebuilt:
	case <-time.After(3 * time.Second):
		t.Fatal("连续 3 次自检失败后没有触发重建")
	}
}

// 失败没达到阈值就恢复（链路抖动）时不能重建：断一次网对用户是实打实的代价。
func TestWatchHealthLoopRecoversBeforeRebuild(t *testing.T) {
	oldInt, oldLimit, oldProbe, oldRebuild := healthProbeInterval, healthProbeFailLimit, healthProbe, healthRebuildHook
	healthProbeInterval = 10 * time.Millisecond
	healthProbeFailLimit = 3
	var calls atomic.Int32
	healthProbe = func(*App, context.Context) error {
		if calls.Add(1) <= 2 {
			return errors.New("探测失败")
		}
		return nil
	}
	rebuilt := make(chan struct{}, 1)
	healthRebuildHook = func(*App) {
		select {
		case rebuilt <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		healthProbeInterval, healthProbeFailLimit = oldInt, oldLimit
		healthProbe, healthRebuildHook = oldProbe, oldRebuild
	})

	app := healthTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.watchHealthLoop(ctx)

	select {
	case <-rebuilt:
		t.Fatal("失败两次后恢复，不该触发重建")
	case <-time.After(300 * time.Millisecond):
	}
}

// stubHealthProbes 替换三层自检的探针，注入成功/失败，避免测试碰真实网络。
func stubHealthProbes(t *testing.T, control, dns, public error) {
	t.Helper()
	oldControl, oldDNS, oldPublic := controlProbe, dnsProbe, publicProbe
	controlProbe = func(context.Context, string, time.Duration) error { return control }
	dnsProbe = func(context.Context, []string) error { return dns }
	publicProbe = func(context.Context) error { return public }
	t.Cleanup(func() { controlProbe, dnsProbe, publicProbe = oldControl, oldDNS, oldPublic })
}

// 控制面不通时直接报控制面，不去碰后面两层。
func TestHealthCheckReportsControlFailure(t *testing.T) {
	app := healthTestApp(t)
	stubHealthProbes(t, errors.New("注入：控制面超时"), errors.New("不该走到 DNS"), errors.New("不该走到数据面"))

	err := app.healthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "隧道内不通") {
		t.Fatalf("应报控制面失败，实际: %v", err)
	}
}

// DNS 正常但公网数据面不通：必须报数据面，不能因为前两层通过就判健康。
func TestHealthCheckReportsDataPathFailure(t *testing.T) {
	app := healthTestApp(t)
	stubHealthProbes(t, nil, nil, errors.New("注入：公网 HTTP 不通"))

	err := app.healthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "公网数据面") {
		t.Fatalf("DNS 正常但公网不通时必须报数据面，实际: %v", err)
	}
}

// 三层都通才返回 nil。
func TestHealthCheckPassesAllLayers(t *testing.T) {
	app := healthTestApp(t)
	stubHealthProbes(t, nil, nil, nil)

	if err := app.healthCheck(context.Background()); err != nil {
		t.Fatalf("三层都通时不该报错: %v", err)
	}
}

// 控制面失败必须立刻返回，不能继续去解析 DNS、发公网请求 —— 那两层在隧道断掉时
// 只会白等两个超时，把一次自检拖到一分多钟。
func TestHealthCheckStopsAfterControlFailure(t *testing.T) {
	app := healthTestApp(t)
	oldControl, oldDNS, oldPublic := controlProbe, dnsProbe, publicProbe
	controlProbe = func(context.Context, string, time.Duration) error {
		return errors.New("注入：控制面失败")
	}
	var dnsCalled, publicCalled bool
	dnsProbe = func(context.Context, []string) error { dnsCalled = true; return nil }
	publicProbe = func(context.Context) error { publicCalled = true; return nil }
	t.Cleanup(func() { controlProbe, dnsProbe, publicProbe = oldControl, oldDNS, oldPublic })

	if err := app.healthCheck(context.Background()); err == nil {
		t.Fatal("控制面失败必须报错")
	}
	if dnsCalled || publicCalled {
		t.Fatalf("控制面失败后不该继续探测：dns=%v public=%v", dnsCalled, publicCalled)
	}
}

// 断开失败（网络没还原干净）时绝不能再 Connect：Connect 会拒绝，而且下一次
// Capture 会把半接管现场写盘、覆盖待恢复依据。
func TestRebuildTunnelStopsWhenDisconnectFails(t *testing.T) {
	connected := false
	err := rebuildTunnel(
		func() error { return errors.New("注入：还原失败") },
		func() error { connected = true; return nil },
	)
	if err == nil {
		t.Fatal("断开失败必须报错")
	}
	if connected {
		t.Fatal("断开失败后不该再 Connect")
	}
}

// 断开成功才连接，顺序不能反。
func TestRebuildTunnelConnectsAfterDisconnect(t *testing.T) {
	var order []string
	err := rebuildTunnel(
		func() error { order = append(order, "disconnect"); return nil },
		func() error { order = append(order, "connect"); return nil },
	)
	if err != nil {
		t.Fatalf("正常重建不该报错: %v", err)
	}
	if len(order) != 2 || order[0] != "disconnect" || order[1] != "connect" {
		t.Fatalf("重建顺序应为 断开→连接，实际 %v", order)
	}
}

// 健康检查必须探测「实际配给系统的 DNS」，不是连接码里那份：接管时
// resolveClientDNS 默认把系统 DNS 换成公共解析器，拿隧道网关去测等于测了一条
// 用户不用的路 —— 公共 DNS 坏了自检照样通过，用户却解析不了。
func TestHealthCheckUsesConfiguredDNS(t *testing.T) {
	app := healthTestApp(t)
	app.mu.Lock()
	app.cfg = NetConfig{DNS: []string{"223.5.5.5", "119.29.29.29"}}
	app.mu.Unlock()

	var got []string
	oldControl, oldDNS, oldPublic := controlProbe, dnsProbe, publicProbe
	controlProbe = func(context.Context, string, time.Duration) error { return nil }
	dnsProbe = func(_ context.Context, servers []string) error { got = servers; return nil }
	publicProbe = func(context.Context) error { return nil }
	t.Cleanup(func() { controlProbe, dnsProbe, publicProbe = oldControl, oldDNS, oldPublic })

	if err := app.healthCheck(context.Background()); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
	if len(got) != 2 || got[0] != "223.5.5.5" || got[1] != "119.29.29.29" {
		t.Fatalf("应探测实际配置的 DNS，实际 %v", got)
	}
}

// 配置还没登记（极早的自检）时回退到按连接码算出来的那一份，与接管同源。
func TestHealthCheckFallsBackToNetConfigDNS(t *testing.T) {
	app := healthTestApp(t)
	t.Setenv("ANTAPP_CLIENT_DNS", "")

	var got []string
	oldControl, oldDNS, oldPublic := controlProbe, dnsProbe, publicProbe
	controlProbe = func(context.Context, string, time.Duration) error { return nil }
	dnsProbe = func(_ context.Context, servers []string) error { got = servers; return nil }
	publicProbe = func(context.Context) error { return nil }
	t.Cleanup(func() { controlProbe, dnsProbe, publicProbe = oldControl, oldDNS, oldPublic })

	if err := app.healthCheck(context.Background()); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
	want := BuildNetConfig(app.Invite()).DNS
	if len(got) != len(want) {
		t.Fatalf("应回退到 BuildNetConfig 的 DNS %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应回退到 BuildNetConfig 的 DNS %v，实际 %v", want, got)
		}
	}
}

// 设了 ANTAPP_CLIENT_DNS 覆盖时，回退路径（配置还没登记）也必须拿到覆盖值，
// 不能悄悄退回连接码里的隧道网关。
func TestHealthCheckFallbackHonorsEnvDNS(t *testing.T) {
	app := healthTestApp(t)
	t.Setenv("ANTAPP_CLIENT_DNS", "1.1.1.1,8.8.8.8")

	var got []string
	oldControl, oldDNS, oldPublic := controlProbe, dnsProbe, publicProbe
	controlProbe = func(context.Context, string, time.Duration) error { return nil }
	dnsProbe = func(_ context.Context, servers []string) error { got = servers; return nil }
	publicProbe = func(context.Context) error { return nil }
	t.Cleanup(func() { controlProbe, dnsProbe, publicProbe = oldControl, oldDNS, oldPublic })

	if err := app.healthCheck(context.Background()); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "8.8.8.8" {
		t.Fatalf("应按 ANTAPP_CLIENT_DNS 覆盖值探测，实际 %v", got)
	}
}

// 公网 IP 探测成功不能抵消 DNS 失败：IP 目标不需要解析，拿它顶掉 DNS 检查
// 会让「系统 DNS 坏了」这条故障继续隐身。
func TestHealthCheckDNSFailureNotOffsetByPublicProbe(t *testing.T) {
	app := healthTestApp(t)
	stubHealthProbes(t, nil, errors.New("注入：DNS 失败"), nil)

	err := app.healthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "DNS 解析失败") {
		t.Fatalf("DNS 失败必须报出来，不能因为公网 IP 探测成功就判健康，实际: %v", err)
	}
}
