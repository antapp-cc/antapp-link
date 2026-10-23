package client

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// UDP 发送必须串行化。
//
// Cipher.Seal 会改 sendSeq 和 nonce；而数据面（pumpFromDevice）和 keepalive 是两个
// goroutine 在并发调 send。不加锁会序号重复、nonce 被覆盖，接收端 GCM 认证失败 ——
// 表现为随机丢包。这里用真实回环 socket 并发打，验证序号既不重复也能全部解开。
func TestUDPSendConcurrentNoDuplicateSeq(t *testing.T) {
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer srv.Close()
	// 缓冲开到能装下全部包，避免本机回环上因队列溢出丢包干扰结论
	_ = srv.SetReadBuffer(1 << 20)

	conn, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	defer conn.Close()

	key := bytes.Repeat([]byte{3}, 32)
	sendCipher, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	ch := &udpChannel{conn: conn, sendSeal: sendCipher}

	const workers, perWorker = 8, 200
	total := workers * perWorker
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				_, _ = ch.send([]byte{byte(i), byte(j)})
			}
		}(i)
	}
	wg.Wait()

	recvCipher, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, proto.MaxUDPPacket)
	seen := make(map[uint64]bool, total)
	_ = srv.SetReadDeadline(time.Now().Add(3 * time.Second))
	for len(seen) < total {
		n, _, err := srv.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n < proto.NonceLen {
			continue
		}
		seq := binary.BigEndian.Uint64(buf[:proto.NonceLen])
		if seen[seq] {
			t.Fatalf("序号 %d 重复：send 没有串行化", seq)
		}
		seen[seq] = true
		if _, err := recvCipher.Open(buf[:n]); err != nil {
			t.Fatalf("序号 %d 的包解不开（nonce 或密钥被并发破坏）: %v", seq, err)
		}
	}
	if len(seen) != total {
		t.Fatalf("收到 %d 个包，期望 %d（余额被回环队列吞掉的话也会这样，但重复/解不开会先报）",
			len(seen), total)
	}
}

// send 必须返回真正写进 socket 的密文字节数：UDP 数据面的「真实写出」统计靠它。
// 它一定大于明文长度（GCM 认证开销 + 序号/nonce），拿明文长度记会虚低。
func TestUDPSendReturnsWrittenBytes(t *testing.T) {
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

	key := bytes.Repeat([]byte{5}, 32)
	seal, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	ch := &udpChannel{conn: conn, sendSeal: seal}

	payload := []byte{1, 2, 3, 4}
	n, err := ch.send(payload)
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if n <= len(payload) {
		t.Fatalf("返回的写出字节 %d 不该 <= 明文 %d：说明记的是明文长度，不是真正写出的密文",
			n, len(payload))
	}
}

// 连续发送失败要计数：这是「UDP 路径已死」的早期证据。这里只要求计数和告警，
// 不据此关闭通道 —— 提前判失效需要实测样本，见运行优化报告第 6 项。
func TestUDPSendTracksErrorStreak(t *testing.T) {
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	_ = conn.Close() // 关掉，之后每次 Write 都会失败

	key := bytes.Repeat([]byte{7}, 32)
	seal, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	ch := &udpChannel{conn: conn, sendSeal: seal}

	for i := 0; i < 3; i++ {
		if _, err := ch.send([]byte{1}); err == nil {
			t.Fatal("连接已关闭，发送不该成功")
		}
	}
	if got := ch.sendErrStreak.Load(); got != 3 {
		t.Fatalf("连续失败 3 次，计数应为 3，实际 %d", got)
	}
}
