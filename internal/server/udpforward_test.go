package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// UDP 转发端到端：外部包经转发到达「节点机」（本机 echo 代替），回包原路返回。
func TestUDPForwardEcho(t *testing.T) {
	// 节点机侧：一个 UDP echo，监听 127.0.0.1:19871
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 19871})
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n > 0 {
				_, _ = echo.WriteToUDP(buf[:n], from)
			}
		}
	}()

	// 转发器：监听 127.0.0.1:19870，目标 127.0.0.1:19871
	f, err := newUDPForward(19870, 19871, net.ParseIP("127.0.0.1"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), func() bool { return true })
	if err != nil {
		t.Fatalf("newUDPForward: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.run(ctx)
	defer f.close()
	time.Sleep(50 * time.Millisecond)

	// 外部客户端：发一个包，等回包
	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 19870})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer client.Close()
	payload := []byte("udp-forward-hello")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("client read（转发没回来）: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("回包不一致: %q", buf[:n])
	}
}
