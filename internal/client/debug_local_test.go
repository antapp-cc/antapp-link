package client

import "testing"

func TestDebugLocalConfig(t *testing.T) {
	root := `C:\Program Files\AntApp Link`
	inv, err := LoadSavedInvite(root)
	if err != nil {
		t.Log("active 加载失败:", err)
	} else {
		code, _ := inv.Encode()
		t.Log("active:", inv.Name, inv.Server, "code前40:", code[:40])
	}
	cands := ScanInvites(root)
	for _, c := range cands {
		code, _ := c.Inv.Encode()
		t.Log("候选:", c.File, c.Inv.Server, "code前40:", code[:40])
	}
	t.Log("HasAlternate:", HasAlternateInvites(root, inv))
}
