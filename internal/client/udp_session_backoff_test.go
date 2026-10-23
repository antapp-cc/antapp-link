package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

// UDP 通道建不起来时不能报「连上过」：否则 Run 每次都把退避清零，UDP 端口被封时
// 变成每十几秒一次「握手 → 探测 → 失败」的死循环，服务端日志也被「新连接取代旧连接」刷满。
func TestSessionUDPFailureDoesNotCountAsConnected(t *testing.T) {
	dir := t.TempDir()
	if err := pki.Init(dir); err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	srvCfg, err := pki.ServerTLSConfig(dir)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// 假服务端：回 HELLO_ACK，之后只读不回。同端口没有 UDP 监听，探测必然失败。
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, _, err := proto.ReadFrame(c); err != nil {
			return
		}
		ack, _ := json.Marshal(Ack{TunnelIP: "10.10.0.2", Gateway: "10.10.0.1", Prefix: 24, MTU: 1400, Members: 1})
		if err := proto.WriteFrame(c, proto.TypeHelloAck, ack); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c)
	}()

	inv, err := pki.Issue(dir, "test-client", ln.Addr().String(), pki.TunnelParams{
		TunnelIP: "10.10.0.2", Gateway: "10.10.0.1", Prefix: 24, MTU: 1400, Members: 1, Mode: pki.ModeUDP,
	})
	if err != nil {
		t.Fatalf("pki.Issue: %v", err)
	}
	tun := NewTunnel(inv, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connected, err := tun.session(ctx)
	if err == nil || !strings.Contains(err.Error(), "UDP") {
		t.Fatalf("期望 UDP 通道失败的错误，实际 %v", err)
	}
	if connected {
		t.Fatal("UDP 通道没建起来却报告 connected=true，Run 会因此清零退避")
	}
}
