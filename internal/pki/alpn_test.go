package pki

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
)

// 服务端必须同时接受新的伪装值和上线时用过的旧名字。
//
// 连接码里没有协商 ALPN 的余地：一旦服务端只认新值，所有已经发出去的连接码
// 会一次性全部连不上，而且没有回滚的办法（客户端连不上就收不到新版本）。
func TestServerAcceptsBothNewAndLegacyALPN(t *testing.T) {
	if ALPN == ALPNLegacy {
		t.Fatal("ALPN 与 ALPNLegacy 相同，这条测试失去意义")
	}

	dir := mustInit(t)
	inv := mustIssue(t, dir, "node-alpn")

	srvCfg, err := ServerTLSConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc := c.(*tls.Conn)
				buf := make([]byte, 1)
				// 握手（含客户端证书校验）发生在这第一次读里
				if _, err := io.ReadFull(tc, buf); err != nil {
					return
				}
				_, _ = tc.Write([]byte{0x42})
			}(conn)
		}
	}()

	// 客户端在 TLS 1.3 下发完 Finished 就认为握手成功，所以必须真读一次。
	dial := func(protos []string) (string, error) {
		cfg, err := ClientTLSConfig(inv)
		if err != nil {
			return "", err
		}
		cfg.NextProtos = protos
		conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{0x01}); err != nil {
			return "", err
		}
		reply := make([]byte, 1)
		if _, err := io.ReadFull(conn, reply); err != nil {
			return "", err
		}
		return conn.ConnectionState().NegotiatedProtocol, nil
	}

	got, err := dial([]string{ALPN})
	if err != nil {
		t.Fatalf("按默认配置握手失败: %v", err)
	}
	if got != ALPN {
		t.Errorf("协商出 %q，期望 %q", got, ALPN)
	}

	got, err = dial([]string{ALPNLegacy})
	if err != nil {
		t.Fatalf("只声明旧名字的客户端握手失败，兼容性被破坏: %v", err)
	}
	if got != ALPNLegacy {
		t.Errorf("旧客户端协商出 %q，期望 %q", got, ALPNLegacy)
	}
}

// 客户端默认就该声明伪装值，否则这个改动等于没做。
func TestClientTLSConfigDeclaresDisguisedALPN(t *testing.T) {
	dir := mustInit(t)
	inv := mustIssue(t, dir, "node-alpn-default")

	cfg, err := ClientTLSConfig(inv)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != ALPN {
		t.Errorf("客户端 NextProtos = %v，期望 [%s]", cfg.NextProtos, ALPN)
	}
}
