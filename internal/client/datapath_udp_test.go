package client

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// 数据面告警必须把三个丢弃数都带上：只报空槽的话，队列满导致的「有上行无下行」
// 会显示成「丢 0 批」，把排障引向下行链路。
func TestWatchDataPathWarnsWithAllDropCounts(t *testing.T) {
	old := dataPathSample
	dataPathSample = 10 * time.Millisecond
	t.Cleanup(func() { dataPathSample = old })

	var buf bytes.Buffer
	tun := testTunnel(t)
	tun.Drops.EmptySlot.Store(11)
	tun.Drops.SlotQueueFull.Store(22)
	tun.Drops.PktChFull.Store(33)

	sess := newClientSession(tun, newBlockedConn(), "sid", nil, Ack{Members: 2})
	sess.slots[1] = newRecordConn()
	sess.log = slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.watchDataPath(ctx)
	}()

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sess.stats.TxBytes.Add(100)
			}
		}
	}()

	time.Sleep(120 * time.Millisecond)
	close(stop)
	cancel()
	<-done

	out := buf.String()
	for _, want := range []string{"数据面单向", "槽空11", "队列22", "读33"} {
		if !strings.Contains(out, want) {
			t.Fatalf("告警里缺少 %q：\n%s", want, out)
		}
	}
}

// pumpFromDevice 的 UDP 分支必须按「真正写进 socket 的密文」累加 TxWritten，
// 而不是把尝试发送的明文也算进去。之前只有 send 的单元测试，这条集成路径没覆盖。
func TestPumpFromDeviceUDPCountsWritten(t *testing.T) {
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer srv.Close()

	conn, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	defer conn.Close()

	cipher, err := proto.NewCipher(bytes.Repeat([]byte{9}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	tun := testTunnel(t)
	sess := newClientSession(tun, conn, "sid", nil, Ack{Members: 1})
	sess.udp = &udpChannel{conn: conn, sendSeal: cipher, stats: &tun.Stats, log: tun.log}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sess.pumpFromDevice(ctx)
	}()

	pkt := make([]byte, 100)
	pkt[0] = 0x45
	tun.pktCh <- pkt

	deadline := time.Now().Add(2 * time.Second)
	for tun.Stats.TxWritten.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := tun.Stats.TxBytes.Load(); got != 100 {
		t.Fatalf("TxBytes 应记明文 100，实际 %d", got)
	}
	if got := tun.Stats.TxWritten.Load(); got <= 100 {
		t.Fatalf("TxWritten 应记密文字节（>100），实际 %d", got)
	}
}
