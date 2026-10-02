package server

import (
	"net"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// 写不动的槽连接必须超时报错并自我了断。
//
// pumpTun 是单 goroutine：它同步往被选中的槽写。某条连接的接收端不消费
// （跨洋链路拥塞、对端卡住）时，裸 Write 会一直挂着，于是**其余几条槽的
// 流量也跟着停**。加超时 + 主动关闭，才能把坏的那条摘出去。
func TestSlotWriteTimesOutAndCloses(t *testing.T) {
	old := slotWriteTimeout
	slotWriteTimeout = 150 * time.Millisecond
	defer func() { slotWriteTimeout = old }()

	server, peer := net.Pipe()
	defer peer.Close() // 对端保持打开但从不读：Write 会一直挂着

	for _, tc := range []struct {
		name string
		call func(*slotConn) error
	}{
		{"write", func(sc *slotConn) error { return sc.write(proto.TypeIP, make([]byte, 64)) }},
		{"writeRaw", func(sc *slotConn) error { return sc.writeRaw(make([]byte, 64)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &slotConn{conn: server}
			start := time.Now()
			err := tc.call(sc)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("写不动的连接必须超时报错")
			}
			if elapsed > time.Second {
				t.Fatalf("应在 %v 量级超时，实际 %v", slotWriteTimeout, elapsed)
			}
			if _, werr := server.Write([]byte{1}); werr == nil {
				t.Error("超时后应主动关闭这条连接，让对端察觉并补拨")
			}
		})
	}
}
