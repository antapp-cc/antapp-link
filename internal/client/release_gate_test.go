package client

import (
	"context"
	"github.com/antapp-cc/antapp-link/internal/pki"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestReleaseGateCanceledTunnelDoesNotRebuild(t *testing.T) {
	dev := &fakeDevice{}
	defer dev.Close()
	tun := silentTunnel(dev)
	rebuilt := make(chan struct{}, 1)
	tun.RebuildDevice = func() { rebuilt <- struct{}{} }
	tun.readErr <- ErrAdapterDead
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tun.Run(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rebuilt:
		t.Fatal("已取消的隧道仍派发设备重建回调")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReleaseGateOldRebuildCannotCloseNewDevice(t *testing.T) {
	a := &App{noNetCfg: true, log: slog.New(slog.NewTextHandler(io.Discard, nil)), rootDir: t.TempDir()}
	a.statePath = filepath.Join(a.rootDir, "state.json")
	oldDev := &fakeDevice{}
	if err := a.startTunnel(NetConfig{}, oldDev, pki.Invite{}, nil); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	oldTun, oldCancel := a.tunnel, a.cancel
	a.mu.Unlock()
	oldCancel()
	oldDev.Close()
	nextDev := &fakeDevice{}
	defer nextDev.Close()
	// 调度器可把已经派发的旧回调延迟到新隧道登记之后；设置无连接码避免重建触碰真实网卡。
	a.mu.Lock()
	a.dev = nextDev
	a.tunnel = NewTunnel(pki.Invite{}, nextDev, a.log)
	a.cancel = nil
	a.running = true
	a.mu.Unlock()
	oldTun.RebuildDevice()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		done := !a.running && !a.disconnecting
		a.mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if nextDev.closed.Load() {
		t.Fatal("旧隧道的重建回调关闭了后来登记的新设备")
	}
}

func TestReleaseGateManualDisconnectRevokesRebuild(t *testing.T) {
	a := &App{noNetCfg: true, log: slog.New(slog.NewTextHandler(io.Discard, nil)), statePath: filepath.Join(t.TempDir(), "state.json")}
	dev := &fakeDevice{}
	source := silentTunnel(dev)
	a.dev, a.tunnel, a.running = dev, source, true
	if err := a.disconnect(source); err != nil {
		t.Fatal(err)
	}
	if !dev.closed.Load() || a.rebuilding != source {
		t.Fatal("当前设备没有被正确收尾或重建授权丢失")
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	// 手动断开发生在自动断开完成、自动重连领取之前。
	if err := a.connect(source); err != nil {
		t.Fatalf("迟到的自动重连仍进入连接流程: %v", err)
	}
	if a.running || a.connecting || a.rebuilding != nil {
		t.Fatal("用户断开后不应恢复自动连接")
	}
}

func TestReleaseGateDuplicateRebuildKeepsOriginalRequest(t *testing.T) {
	a := &App{noNetCfg: true, log: slog.New(slog.NewTextHandler(io.Discard, nil)), statePath: filepath.Join(t.TempDir(), "state.json")}
	dev := &fakeDevice{}
	source := silentTunnel(dev)
	a.dev, a.tunnel, a.running = dev, source, true
	if err := a.disconnect(source); err != nil {
		t.Fatal(err)
	}
	a.rebuildTunnel(context.Background(), source)
	if a.rebuilding != source {
		t.Fatal("重复的过期请求撤销了已经领取的重建")
	}
	if err := a.connect(source); err == nil {
		t.Fatal("合法重建应进入现有连接流程，并报告此测试未配置连接码")
	}
	if a.rebuilding != nil {
		t.Fatal("重建授权应只消费一次")
	}
	if err := a.connect(source); err != nil {
		t.Fatalf("重复重连应忽略: %v", err)
	}
}

func TestReleaseGateStartupFaultWaitsForConnectionCleanup(t *testing.T) {
	a := &App{connecting: true, running: true, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dev := &fakeDevice{}
	source := silentTunnel(dev)
	a.dev, a.tunnel = dev, source
	a.rebuildTunnel(context.Background(), source)
	if a.rebuilding != source || dev.closed.Load() {
		t.Fatal("连接收尾前不应并发拆设备，也不能丢弃当前故障")
	}
	a.connecting = false
	a.running = false
	a.rebuilding = nil
	dev.Close()
}

func TestReleaseGateShutdownRevokesAutomaticReconnect(t *testing.T) {
	a := &App{noNetCfg: true, log: slog.New(slog.NewTextHandler(io.Discard, nil)), statePath: filepath.Join(t.TempDir(), "state.json")}
	dev := &fakeDevice{}
	source := silentTunnel(dev)
	a.dev, a.tunnel, a.running = dev, source, true
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	a.rebuildTunnel(context.Background(), source)
	if err := a.Connect(); err == nil {
		t.Fatal("退出过程不应允许重新接管网络")
	}
	if a.rebuilding != nil || a.running {
		t.Fatal("退出后仍保留重建请求")
	}
}
