package server

import (
	"net"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// 成员连接上的心跳应答要写回**同一条**连接：handleMember 的 Ping 分支就是调
// slotConn.write 干的活。它持写锁，所以不会和 pumpTun 的数据批交错。
func TestSlotConnWritePong(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	sc := &slotConn{conn: server}

	errCh := make(chan error, 1)
	go func() { errCh <- sc.write(proto.TypePong, []byte("12345678")) }()

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, payload, err := proto.ReadFrame(peer)
	if err != nil {
		t.Fatalf("读 PONG: %v", err)
	}
	if typ != proto.TypePong {
		t.Errorf("期望 PONG，收到 %s", typ)
	}
	if string(payload) != "12345678" {
		t.Errorf("PONG 载荷 = %q", payload)
	}
	if err := <-errCh; err != nil {
		t.Errorf("write: %v", err)
	}
	if got := sc.txBytes.Load(); got != uint64(len(payload)+proto.HeaderSize) {
		t.Errorf("发送统计 = %d，期望 %d", got, len(payload)+proto.HeaderSize)
	}
}
