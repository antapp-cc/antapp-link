package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// fakeDevice 的 Read 立刻返回预设错误，用来模拟网卡读失败。
//
// closed 用 atomic.Bool：Read 在后台协程里轮询它，而 Close 从测试协程写，
// 用普通 bool 会被 -race 抓出来（第三轮复审 T1 报的就是这个）。
// chan 也行，但 atomic 更直白，也不用管谁来关。
type fakeDevice struct {
	readErr error
	closed  atomic.Bool
}

func (d *fakeDevice) Read(p []byte) (int, error) {
	if d.readErr != nil {
		return 0, d.readErr
	}
	// 没有错误时一直阻塞，直到 Close（模拟无流量）
	for !d.closed.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	return 0, ErrDeviceClosed
}
func (d *fakeDevice) TryRead([]byte) (int, error) { return 0, ErrNoTryRead }
func (d *fakeDevice) Write(p []byte) (int, error) { return len(p), nil }
func (d *fakeDevice) Name() string                { return "fake" }
func (d *fakeDevice) Close() error                { d.closed.Store(true); return nil }

func silentTunnel(dev Device) *Tunnel {
	return NewTunnel(pki.Invite{}, dev, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 网卡读失败必须触发设备重建。
//
// readPump 原来只 log 一句就 return：控制连接照常收发心跳，面板显示在线、延迟也正常，
// 而上行早就停止搬运了 —— 用户看到的是「连上了，但什么都打不开」，且不会自愈。
// 复审第三轮问题一用隔离测试复现过（控制连接在线、未请求重建、Run 仍在跑）。
func TestReadPumpFailureTriggersRebuild(t *testing.T) {
	tun := silentTunnel(&fakeDevice{readErr: ErrAdapterDead})
	rebuilt := make(chan struct{}, 1)
	tun.RebuildDevice = func() { rebuilt <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tun.Run(ctx) }()

	select {
	case <-rebuilt:
	case <-time.After(3 * time.Second):
		t.Fatal("网卡读失败后没有触发设备重建 —— 会留下「显示在线但上行停摆」")
	}
}

// 普通读错误同样要走恢复路径，不能只是静默停掉。
func TestReadPumpGenericFailureAlsoRecovers(t *testing.T) {
	tun := silentTunnel(&fakeDevice{readErr: errors.New("设备读失败")})
	rebuilt := make(chan struct{}, 1)
	tun.RebuildDevice = func() { rebuilt <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tun.Run(ctx) }()

	select {
	case <-rebuilt:
	case <-time.After(3 * time.Second):
		t.Fatal("普通读错误也要走恢复路径，否则上行永久停摆")
	}
}

// 用户主动断开（我们自己 Close 设备）不能触发自动重建，否则一断开就自己连回来。
func TestReadPumpDeviceClosedDoesNotRebuild(t *testing.T) {
	dev := &fakeDevice{}
	tun := silentTunnel(dev)
	rebuilt := make(chan struct{}, 1)
	tun.RebuildDevice = func() { rebuilt <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tun.Run(ctx) }()

	// 模拟用户断开：Close 设备
	time.Sleep(50 * time.Millisecond)
	_ = dev.Close()

	select {
	case <-rebuilt:
		t.Fatal("设备是我们自己关的（用户断开），不该触发自动重建")
	case <-time.After(500 * time.Millisecond):
		// 正确：没有重建
	}
}
