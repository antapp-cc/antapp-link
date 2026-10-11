package server

import (
	"errors"
	"io"
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

	for _, tc := range []struct {
		name string
		call func(*slotConn) error
	}{
		{"write", func(sc *slotConn) error { return sc.write(proto.TypeIP, make([]byte, 64)) }},
		{"writeRaw", func(sc *slotConn) error { return sc.writeRaw(make([]byte, 64)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 每条子用例各一条独立管道：write 超时会把连接 Close，共用同一条 conn 时
			// writeRaw 子用例会在 SetWriteDeadline 上直接失败，三个断言全是白送的
			server, peer := net.Pipe()
			defer peer.Close()   // 对端保持打开但从不读：Write 会一直挂着
			defer server.Close() // 已关闭时重复 Close 是空操作

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
			// 只认被主动关闭：管道上的写超时同样会让 Write 报错，光看非 nil 分不清
			if _, werr := server.Write([]byte{1}); !errors.Is(werr, io.ErrClosedPipe) {
				t.Errorf("超时后应主动关闭这条连接，让对端察觉并补拨，实际 %v", werr)
			}
		})
	}
}
