package client

import (
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

func writeInvite(t *testing.T, root, name string, inv pki.Invite) {
	t.Helper()
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ConfigDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ConfigDir(root), name), []byte(code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanInvites(t *testing.T) {
	root := t.TempDir()
	writeInvite(t, root, "pinode (2).antapp", makeInvite(t, "pi-node-01"))
	writeInvite(t, root, "other.antapp", makeInvite(t, "pi-node-02"))
	// 生效配置要被排除
	writeInvite(t, root, InviteFileName, makeInvite(t, "pi-node-01"))
	// 非 .antapp 的文件不算候选
	if err := os.WriteFile(filepath.Join(ConfigDir(root), "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := ScanInvites(root)
	if len(got) != 2 {
		t.Fatalf("应发现 2 个候选（排除生效配置），实际 %d", len(got))
	}
	// 两个候选的节点名相同 → 同名检测
	dups := DuplicateNames(got)
	if len(dups) != 0 {
		t.Fatalf("不同名候选不应报同名: %v", dups)
	}
}

func TestScanInvitesDuplicateNames(t *testing.T) {
	root := t.TempDir()
	writeInvite(t, root, "pinode (3).antapp", makeInvite(t, "pi-node-01"))
	writeInvite(t, root, "pinode (2).antapp", makeInvite(t, "pi-node-01"))

	got := ScanInvites(root)
	if len(got) != 2 {
		t.Fatalf("应发现 2 个候选，实际 %d", len(got))
	}
	dups := DuplicateNames(got)
	if len(dups) != 1 || dups[0] != "pi-node-01" {
		t.Fatalf("应检出同名 pi-node-01，实际 %v", dups)
	}
}

func TestScanInvitesEmpty(t *testing.T) {
	if got := ScanInvites(t.TempDir()); len(got) != 0 {
		t.Fatalf("空目录应返回 0 个候选，实际 %d", len(got))
	}
}

func TestScanInvitesSingleAdoptable(t *testing.T) {
	root := t.TempDir()
	writeInvite(t, root, "pinode (2).antapp", makeInvite(t, "pi-node-02"))

	got := ScanInvites(root)
	if len(got) != 1 || got[0].File != "pinode (2).antapp" || got[0].Inv.Name != "pi-node-02" {
		t.Fatalf("单候选识别错误: %+v", got)
	}
	if dups := DuplicateNames(got); len(dups) != 0 {
		t.Fatalf("单候选不应有同名: %v", dups)
	}
}

func TestMigrateInviteFileName(t *testing.T) {
	root := t.TempDir()
	inv := makeInvite(t, "pi-node-01")
	writeInvite(t, root, "node.antapp", inv)

	MigrateInviteFileName(root)
	if _, err := os.Stat(InviteFilePath(root)); err != nil {
		t.Fatalf("迁移后应有 pinode.antapp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ConfigDir(root), "node.antapp")); !os.IsNotExist(err) {
		t.Fatal("迁移后旧名字应已不存在")
	}

	// 新名字已存在时不迁移（旧文件留作候选）
	writeInvite(t, root, "node.antapp", inv)
	MigrateInviteFileName(root)
	if _, err := os.Stat(filepath.Join(ConfigDir(root), "node.antapp")); err != nil {
		t.Fatal("pinode.antapp 已存在时不应动旧文件")
	}
}
