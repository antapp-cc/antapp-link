package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

func TestBackoffSequence(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if got := Backoff(i); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i, got, w)
		}
	}
	if got := Backoff(1000); got != 30*time.Second {
		t.Errorf("退避必须封顶在 30 秒，实际 %v", got)
	}
	if got := Backoff(-5); got != time.Second {
		t.Errorf("负数次数应按首次处理，实际 %v", got)
	}
}

func testInvite(t *testing.T) pki.Invite {
	t.Helper()
	pkiDir := filepath.Join(t.TempDir(), "pki")
	if err := pki.Init(pkiDir); err != nil {
		t.Fatal(err)
	}
	inv, err := pki.Issue(pkiDir, "pinode-01", "103.143.11.34:62233", pki.TunnelParams{
		TunnelIP: "10.10.0.2",
		Gateway:  "10.10.0.1",
		Prefix:   24,
		MTU:      1400,
		DNS:      []string{"8.8.8.8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestLoadInviteFromSingleLineCode(t *testing.T) {
	inv := testInvite(t)
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadInvite(code)
	if err != nil {
		t.Fatalf("LoadInvite(单行码): %v", err)
	}
	if got.Name != "pinode-01" || got.Server != "103.143.11.34:62233" {
		t.Errorf("解析结果不对: %+v", got.Name)
	}
}

func TestLoadInviteFromJSONFile(t *testing.T) {
	inv := testInvite(t)
	raw, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "antapp-node-pinode-01.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadInvite(path)
	if err != nil {
		t.Fatalf("LoadInvite(JSON 文件): %v", err)
	}
	if got.TunnelIP != "10.10.0.2" {
		t.Errorf("TunnelIP = %q", got.TunnelIP)
	}
}

// 用户常把单行码存成 txt 再传过来，这条路径也得走通。
func TestLoadInviteFromTextFileHoldingCode(t *testing.T) {
	inv := testInvite(t)
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "code.txt")
	if err := os.WriteFile(path, []byte(code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInvite(path); err != nil {
		t.Fatalf("LoadInvite(文本文件里的单行码): %v", err)
	}
	// 首尾带空白的粘贴内容也要能用
	if _, err := LoadInvite("  " + code + "  "); err != nil {
		t.Fatalf("LoadInvite(带空白): %v", err)
	}
}

func TestLoadInviteErrors(t *testing.T) {
	if _, err := LoadInvite(""); err == nil {
		t.Error("空输入必须报错")
	}
	if _, err := LoadInvite(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("不存在的文件必须报错")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInvite(bad); err == nil {
		t.Error("垃圾内容必须报错")
	}
}

func TestAckParsesServerHandshake(t *testing.T) {
	raw := []byte(`{"tunnel_ip":"10.10.0.2","gateway":"10.10.0.1","prefix":24,"mtu":1400,"mss":1360,"dns":["8.8.8.8"]}`)
	var ack Ack
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.TunnelIP != "10.10.0.2" || ack.Gateway != "10.10.0.1" || ack.MTU != 1400 || ack.MSS != 1360 {
		t.Errorf("HELLO_ACK 解析结果不对: %+v", ack)
	}
	if len(ack.DNS) != 1 || ack.DNS[0] != "8.8.8.8" {
		t.Errorf("DNS = %v", ack.DNS)
	}
}
