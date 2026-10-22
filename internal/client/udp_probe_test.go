package client

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// newLoopbackUDPSession 建一对回环 UDP 端点，客户端通道已经跑起 recvLoop，
// 返回服务端 socket 和它两个方向独立的加解密器（方向字节对齐生产：c2s=1、s2c=2）。
func newLoopbackUDPSession(t *testing.T, ctx context.Context) (*udpChannel, *net.UDPConn, *proto.Cipher, *proto.Cipher) {
	t.Helper()

	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听回环 UDP: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("拨号回环 UDP: %v", err)
	}

	key := bytes.Repeat([]byte{7}, 32)
	c2s, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	s2c, err := proto.NewCipher(key, 2)
	if err != nil {
		t.Fatal(err)
	}
	srvRecv, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	srvSend, err := proto.NewCipher(key, 2)
	if err != nil {
		t.Fatal(err)
	}

	c := &udpChannel{
		conn: conn, recv: s2c, sendSeal: c2s,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		closed: make(chan struct{}),
	}
	c.lastRx.Store(time.Now().UnixNano())
	go c.recvLoop(ctx, nil)
	t.Cleanup(c.close)
	return c, srv, srvRecv, srvSend
}

// 探测必须重发：只发一次时，首包早于服务端处理端口登记、或路上普通丢一次，
// 整条连接就白白失败，而 keepalive 要 15 秒后才轮到下一包，救不了这 5 秒窗口。
func TestUDPProbeRetriesUntilAuthenticatedReply(t *testing.T) {
	saved := udpProbeRetryAt
	udpProbeRetryAt = []time.Duration{20 * time.Millisecond, 60 * time.Millisecond, 120 * time.Millisecond}
	defer func() { udpProbeRetryAt = saved }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, srv, srvRecv, srvSend := newLoopbackUDPSession(t, ctx)

	// 前两个认证探测包故意不回（模拟丢包/登记未生效），第三个才回。
	var got atomic.Int32
	go func() {
		buf := make([]byte, proto.MaxUDPPacket)
		for {
			n, from, err := srv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := srvRecv.Open(buf[:n]); err != nil {
				continue
			}
			if got.Add(1) < 3 {
				continue
			}
			_, _ = srv.WriteToUDP(srvSend.Seal(nil), from)
		}
	}()

	start := time.Now()
	if err := c.probe(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("服务端在第 3 个探测包才回，probe 应当重发并成功，实际 %v", err)
	}
	if n := got.Load(); n < 3 {
		t.Fatalf("服务端只收到 %d 个认证探测包，期望至少 3 个（probe 没有重发）", n)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("重发节奏过慢：耗时 %v", elapsed)
	}
}

// 只有通过认证（GCM 标签校验）的回包才能让探测成功。
// 随便一个 UDP 输入就能刷亮健康状态的话，「端口放行但密钥不对」会被伪装成正常。
func TestUDPProbeIgnoresUnauthenticatedInput(t *testing.T) {
	saved := udpProbeRetryAt
	udpProbeRetryAt = []time.Duration{10 * time.Millisecond}
	defer func() { udpProbeRetryAt = saved }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, srv, _, _ := newLoopbackUDPSession(t, ctx)

	go func() {
		buf := make([]byte, 256)
		for {
			_, from, err := srv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = srv.WriteToUDP([]byte("not an encrypted reply"), from)
		}
	}()

	if err := c.probe(context.Background(), 300*time.Millisecond); err == nil {
		t.Fatal("未认证的 UDP 回包不该让 probe 判定成功")
	}
}
