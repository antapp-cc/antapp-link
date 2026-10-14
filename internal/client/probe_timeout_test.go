package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// probeServer 必须只服从调用方的 ctx。
//
// 曾经它内部把 dial 写死 8 秒、TLS 握手写死 10 秒，外面把探测预算从 12 秒放宽到 30 秒
// 也毫无作用：先到的仍是那 8 秒，报错是 dial i/o timeout（Dialer.Timeout 的特征）而非
// context deadline exceeded（ctx 的特征）。这个测试把「谁说了算」钉住。
//
// 局限：ctx 取得比任何可能的内部硬编码都短，所以它只能证明「ctx 更短时 ctx 说了算」；
// 要证明「内部没有更长的硬编码」（比如又写死 10 秒）需要跑 10 秒以上的测试，不划算。
func TestProbeServerHonoursCallerContext(t *testing.T) {
	// 只监听、永不 accept：三次握手由内核完成（dial 立刻成功），但没人回 TLS 握手，
	// 于是这一段只能由调用方的 ctx 来结束。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	inv := testInvite(t)
	inv.Server = ln.Addr().String()

	a := &App{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = a.probeServer(inv, ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("对不回应的服务端应当报错")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误应来自调用方的 ctx（context.DeadlineExceeded），实际 %v "+
			"—— 若报 i/o timeout，说明 probeServer 内部又压了更短的硬编码超时", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("probeServer 没有服从调用方 ctx：耗时 %v", elapsed)
	}
}
