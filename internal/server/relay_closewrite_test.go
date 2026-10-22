package server

import (
	"net"
	"testing"
)

// relayConn 嵌入的是 net.Conn 接口，CloseWrite 不在该接口里。
// 如果 relayConn 没转发这个方法，closeWrite 的类型断言会失败，
// 退回整条 Close —— 那正是原注释里说要避免的「截断应答」。
func TestRelayConnPreservesCloseWrite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无法监听:", err)
	}
	defer ln.Close()

	done := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		done <- c
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	raw := <-done
	defer raw.Close()
	rc := newRelayConn(raw)

	if _, ok := interface{}(rc).(interface{ CloseWrite() error }); !ok {
		t.Fatal("relayConn 丢掉了 CloseWrite —— closeWrite() 会退回整条关闭，" +
			"把服务端还没发完的应答截断，客户端拿到空响应")
	}
}
