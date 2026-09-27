package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testParams() TunnelParams {
	return TunnelParams{
		TunnelIP: "10.10.0.2",
		Gateway:  "10.10.0.1",
		Prefix:   24,
		MTU:      1400,
		DNS:      []string{"8.8.8.8", "149.112.112.112"},
	}
}

func mustInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return dir
}

func mustIssue(t *testing.T, dir, name string) Invite {
	t.Helper()
	inv, err := Issue(dir, name, "127.0.0.1:62233", testParams())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return inv
}

func TestInitIsIdempotentAndKeepsCA(t *testing.T) {
	dir := mustInit(t)
	caPath := filepath.Join(dir, caCertFile)
	before, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(dir); err != nil {
		t.Fatalf("第二次 Init: %v", err)
	}
	after, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("重复 Init 改动了 CA —— 那会让已发出的连接码全部失效")
	}
}

func TestInitRefusesHalfCA(t *testing.T) {
	dir := mustInit(t)
	before, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir); err == nil {
		t.Fatal("CA 私钥缺失时必须报错，而不是悄悄重建 CA")
	}
	after, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("报错的同时还是覆盖了 CA")
	}
}

func TestIssueProducesVerifiableClientCert(t *testing.T) {
	dir := mustInit(t)
	inv := mustIssue(t, dir, "pinode-01")

	block, _ := pem.Decode([]byte(inv.CertPEM))
	if block == nil {
		t.Fatal("CertPEM 不是合法 PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(inv.CAPEM)) {
		t.Fatal("CAPEM 无法解析")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("客户端证书无法用连接码里的 CA 验证: %v", err)
	}
	if cert.Subject.CommonName != "pinode-01" {
		t.Errorf("CN = %q, want pinode-01", cert.Subject.CommonName)
	}
}

func TestIssueRejectsTraversalName(t *testing.T) {
	dir := mustInit(t)
	for _, name := range []string{"../evil", "a/b", "a b", ""} {
		if _, err := Issue(dir, name, "127.0.0.1:62233", testParams()); err == nil {
			t.Errorf("客户端名 %q 必须被拒绝", name)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	inv := mustIssue(t, mustInit(t), "pinode-01")
	code, err := inv.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(code, Scheme) {
		t.Errorf("连接码应以 %s 开头", Scheme)
	}
	if strings.ContainsAny(code, "\n\r") {
		t.Error("连接码必须是单行，方便粘贴转发")
	}

	got, err := Decode(code)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Server != inv.Server || got.TunnelIP != inv.TunnelIP || got.CertPEM != inv.CertPEM {
		t.Error("往返后内容不一致")
	}

	if _, err := Decode(strings.TrimPrefix(code, Scheme)); err != nil {
		t.Errorf("不带 scheme 也应该能解析: %v", err)
	}
	if _, err := Decode("  " + code + "\n"); err != nil {
		t.Errorf("应容忍首尾空白: %v", err)
	}
}

func TestDecodeRejectsTampered(t *testing.T) {
	inv := mustIssue(t, mustInit(t), "pinode-01")
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(code, Scheme)

	t.Run("改一个字符", func(t *testing.T) {
		b := []byte(body)
		if b[10] == 'A' {
			b[10] = 'B'
		} else {
			b[10] = 'A'
		}
		if _, err := Decode(Scheme + string(b)); err == nil {
			t.Error("被改动的连接码必须解析失败")
		}
	})
	t.Run("截断", func(t *testing.T) {
		if _, err := Decode(Scheme + body[:len(body)-8]); err == nil {
			t.Error("被截断的连接码必须解析失败")
		}
	})
	t.Run("非 base64", func(t *testing.T) {
		if _, err := Decode(Scheme + "!!! not base64 !!!"); err == nil {
			t.Error("非 base64 必须解析失败")
		}
	})
}

func TestValidateRejectsBadParams(t *testing.T) {
	base := mustIssue(t, mustInit(t), "pinode-01")
	other := mustIssue(t, mustInit(t), "node-x")

	cases := []struct {
		name   string
		mutate func(*Invite)
	}{
		{"服务端地址没有端口", func(i *Invite) { i.Server = "103.143.11.34" }},
		{"服务端地址为空", func(i *Invite) { i.Server = "" }},
		{"tunnel_ip 非法", func(i *Invite) { i.TunnelIP = "not-an-ip" }},
		{"gateway 非法", func(i *Invite) { i.Gateway = "" }},
		{"prefix 越界", func(i *Invite) { i.Prefix = 33 }},
		{"mtu 越界", func(i *Invite) { i.MTU = 9000 }},
		{"dns 里混进非 IP", func(i *Invite) { i.DNS = []string{"8.8.8.8", "dns.google"} }},
		{"私钥为空", func(i *Invite) { i.KeyPEM = "" }},
		{"证书与私钥不匹配", func(i *Invite) { i.KeyPEM = other.KeyPEM }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inv := base
			c.mutate(&inv)
			if err := inv.Validate(); err == nil {
				t.Error("应该报错")
			}
			if _, err := inv.Encode(); err == nil {
				t.Error("坏的连接码不该能编码出去")
			}
		})
	}
}

// 这条是本包最重要的一条：服务端必须拒绝别家 CA 签的客户端证书，
// 否则任何人自己签一张证书就能连进隧道。
func TestServerRejectsClientFromForeignCA(t *testing.T) {
	dirA := mustInit(t)
	dirB := mustInit(t)
	invA := mustIssue(t, dirA, "node-a")
	invB := mustIssue(t, dirB, "node-b")

	srvCfg, err := ServerTLSConfig(dirA)
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
				buf := make([]byte, 1)
				// 握手（含客户端证书校验）发生在这第一次读里
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x42})
			}(conn)
		}
	}()

	// 客户端在 TLS 1.3 下发完 Finished 就认为握手成功，所以必须真读一次
	// 才能区分「被服务端拒绝」和「连上了」。
	dial := func(inv Invite) error {
		cfg, err := ClientTLSConfig(inv)
		if err != nil {
			return err
		}
		conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{0x01}); err != nil {
			return err
		}
		reply := make([]byte, 1)
		_, err = io.ReadFull(conn, reply)
		return err
	}

	if err := dial(invA); err != nil {
		t.Errorf("自家 CA 签的客户端应该能完成握手: %v", err)
	}
	if err := dial(invB); err == nil {
		t.Error("别家 CA 签的客户端证书必须被拒绝")
	}
}

func TestServerTLSConfigNeedsCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := ServerTLSConfig(dir); err == nil {
		t.Error("PKI 目录为空时应该报错，而不是退化成不校验客户端")
	}
}
