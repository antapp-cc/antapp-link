package pki

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
)

// startALPNProbeServer 起一个只为 ALPN 探测服务的 TLS 监听：握手成功时回一个字节。
func startALPNProbeServer(t *testing.T, dir string) (string, func()) {
	t.Helper()
	srvCfg, err := ServerTLSConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1)
				// 握手（含客户端证书校验）发生在这第一次读里
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x42})
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func dialALPN(t *testing.T, inv Invite, addr string, protos []string) (string, error) {
	t.Helper()
	cfg, err := ClientTLSConfig(inv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NextProtos = protos
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	// TLS 1.3 下客户端发完 Finished 就认为握手成功，服务端的 ALPN 拒绝要等到
	// 之后的读写才暴露 —— 所以这里必须真读一次。
	if _, err := conn.Write([]byte{0x01}); err != nil {
		return "", err
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return "", err
	}
	return conn.ConnectionState().NegotiatedProtocol, nil
}

// 客户端声明了服务端不认识的协议名时必须明确拒绝。
//
// 静默放行会更糟：中间设备看到的是「声明 h2、实跑别的协议」这种自相矛盾的组合，
// 比一个坦荡的自研协议名更可疑。服务端保留两个值是为了兼容，不是为了宽容。
func TestServerRejectsUnknownALPN(t *testing.T) {
	dir := mustInit(t)
	inv := mustIssue(t, dir, "node-alpn-unknown")
	addr, stop := startALPNProbeServer(t, dir)
	defer stop()

	protos := []string{"h3", "not-a-real-proto"}
	got, err := dialALPN(t, inv, addr, protos)
	if err == nil {
		t.Fatalf("声明 %v 却没被拒绝，协商出了 %q", protos, got)
	}
}

// ALPN 是可选扩展：不发它的客户端不该被拒绝。
func TestServerAcceptsClientWithoutALPN(t *testing.T) {
	dir := mustInit(t)
	inv := mustIssue(t, dir, "node-alpn-none")
	addr, stop := startALPNProbeServer(t, dir)
	defer stop()

	got, err := dialALPN(t, inv, addr, nil)
	if err != nil {
		t.Fatalf("不发 ALPN 的客户端被拒绝了: %v", err)
	}
	if got != "" {
		t.Errorf("没声明任何协议，却协商出 %q", got)
	}
}
