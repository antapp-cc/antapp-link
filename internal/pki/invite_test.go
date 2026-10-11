package pki

import (
	"bytes"
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

// 用户照 Init 的提示删掉 CA 两者重建时，旧 server.crt 必须跟着重签：
// 只查文件在不在会把它当成有效证书继续用，服务端出示旧 CA 签的证书，
// 而新连接码里带的是新 CA —— 全部节点在握手阶段掉线，Init 却报成功。
func TestInitResignsServerCertAfterCAReplaced(t *testing.T) {
	dir := mustInit(t)
	oldCert := mustReadFile(t, filepath.Join(dir, srvCertFile))

	for _, name := range []string{caCertFile, caKeyFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Init(dir); err != nil {
		t.Fatalf("重建 CA 后 Init: %v", err)
	}

	newCert := mustReadFile(t, filepath.Join(dir, srvCertFile))
	if bytes.Equal(oldCert, newCert) {
		t.Fatal("CA 换了但 server.crt 没重签")
	}
	caCert, srvCert := parseCertPEM(mustReadFile(t, filepath.Join(dir, caCertFile))), parseCertPEM(newCert)
	if caCert == nil || srvCert == nil {
		t.Fatal("证书无法解析")
	}
	if err := srvCert.CheckSignatureFrom(caCert); err != nil {
		t.Errorf("重签后的服务端证书不是新 CA 签的: %v", err)
	}
	// 私钥也要跟着换，否则这里加载不出证书对
	if _, err := ServerTLSConfig(dir); err != nil {
		t.Errorf("ServerTLSConfig: %v", err)
	}
}

// 私钥坏了（文件在，但读不出或与证书不配对）同样必须重签：
// 服务端证书能不能用不能只看文件在不在。
func TestInitResignsServerCertWhenKeyBroken(t *testing.T) {
	dir := mustInit(t)
	oldCert := mustReadFile(t, filepath.Join(dir, srvCertFile))
	other := mustInit(t)
	if err := os.WriteFile(filepath.Join(dir, srvKeyFile), mustReadFile(t, filepath.Join(other, srvKeyFile)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Init(dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if bytes.Equal(oldCert, mustReadFile(t, filepath.Join(dir, srvCertFile))) {
		t.Error("私钥与证书不配对时必须重签")
	}
	if _, err := ServerTLSConfig(dir); err != nil {
		t.Errorf("ServerTLSConfig: %v", err)
	}
}

// CA 没变时不该重签服务端证书：每次重装都换一张是无谓的抖动。
func TestInitKeepsServerCertWhenCAUnchanged(t *testing.T) {
	dir := mustInit(t)
	before := mustReadFile(t, filepath.Join(dir, srvCertFile))
	if err := Init(dir); err != nil {
		t.Fatalf("重复 Init: %v", err)
	}
	if after := mustReadFile(t, filepath.Join(dir, srvCertFile)); !bytes.Equal(before, after) {
		t.Error("CA 未变却重签了服务端证书")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
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

// 超长输入必须在解码/反序列化之前就被拒：否则几百 MB 的粘贴会在解析阶段
// 把内存放大数倍（base64 + JSON 副本），客户端直接 OOM。
func TestDecodeRejectsOversizedInput(t *testing.T) {
	_, err := Decode(Scheme + strings.Repeat("A", maxCodeLen+1))
	if err == nil {
		t.Fatal("超长输入必须被拒绝")
	}
	if !strings.Contains(err.Error(), "过长") {
		t.Errorf("应在长度检查处就拒绝，实际错误: %v", err)
	}
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
