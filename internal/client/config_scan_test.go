package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// makeInvite 用真实 PKI 签一个可用的连接码（Encode/Validate 需要完整字段）。
func makeInvite(t *testing.T, name string) pki.Invite {
	t.Helper()
	dir := t.TempDir()
	if err := pki.Init(dir); err != nil {
		t.Fatal(err)
	}
	inv, err := pki.Issue(dir, name, "1.2.3.4:62233", pki.TunnelParams{
		TunnelIP: "10.10.0.2", Gateway: "10.10.0.1", Prefix: 24, MTU: 1400, DNS: []string{"10.10.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func writeInviteFile(t *testing.T, root, name string, inv pki.Invite) {
	t.Helper()
	// 与服务端 invite 的产物一致：JSON 格式
	code, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ConfigDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ConfigDir(root), name), code, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListInviteFiles(t *testing.T) {
	root := t.TempDir()
	writeInviteFile(t, root, "pinode (3).antapp", makeInvite(t, "pi-node-01"))
	writeInviteFile(t, root, "pinode (2).antapp", makeInvite(t, "pi-node-01"))
	writeInviteFile(t, root, "other.antapp", makeInvite(t, "pi-node-02"))
	if err := os.WriteFile(filepath.Join(ConfigDir(root), "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := ListInviteFiles(root)
	if len(got) != 3 {
		t.Fatalf("应列出 3 个 .antapp 文件，实际 %v", got)
	}
}

func TestListInviteFilesEmpty(t *testing.T) {
	if got := ListInviteFiles(t.TempDir()); len(got) != 0 {
		t.Fatalf("空目录应返回 0 个，实际 %v", got)
	}
}

func TestDuplicateFileNames(t *testing.T) {
	// pinode (2)/(3) 去掉 Windows 复制序号后同名 → 检出一组
	names := []string{"pinode (2).antapp", "pinode (3).antapp"}
	dups := DuplicateFileNames(names)
	if len(dups) != 1 || dups[0].Name != "pinode.antapp" || len(dups[0].Files) != 2 {
		t.Fatalf("应检出同名组 pinode.antapp×2，实际 %+v", dups)
	}

	// 不同文件名不报同名
	if dups := DuplicateFileNames([]string{"a.antapp", "b.antapp"}); len(dups) != 0 {
		t.Fatalf("不同文件名不应报同名: %+v", dups)
	}
}

func TestLoadInviteFile(t *testing.T) {
	root := t.TempDir()
	writeInviteFile(t, root, "pinode.antapp", makeInvite(t, "pi-node-02"))

	inv, err := LoadInviteFile(root, "pinode.antapp")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Name != "pi-node-02" {
		t.Fatalf("加载到的节点名错误: %s", inv.Name)
	}
}
