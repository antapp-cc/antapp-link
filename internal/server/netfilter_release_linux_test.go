//go:build linux

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReleaseReview_NetfilterDeletesActualOldWANRule(t *testing.T) {
	if os.Getenv("ANTAPP_TEST_NETFILTER") != "isolated" {
		t.Skip("requires isolated network namespace")
	}
	old := forwardRangePath
	forwardRangePath = filepath.Join(t.TempDir(), "range")
	defer func() { forwardRangePath = old }()
	cfg := Default()
	rules := Rules(cfg, "old0")
	marked := Rules(cfg, "old0")
	for i := range marked {
		marked[i].Args = append(marked[i].Args, "-m", "comment", "--comment", "AntAppLink")
	}
	rules = append(rules, marked...)
	for _, r := range rules {
		if err := ensureRule(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveNetfilterLedger(rules); err != nil {
		t.Fatal(err)
	}
	if err := writeForwardRange("31400:31409"); err != nil {
		t.Fatal(err)
	}
	unrelated := Rule{Table: "nat", Chain: "POSTROUTING", Args: []string{"-s", cfg.Tunnel.Network, "-o", "old0", "-m", "comment", "--comment", "OtherVPN", "-j", "MASQUERADE"}}
	if err := ensureRule(unrelated); err != nil {
		t.Fatal(err)
	}
	// 注入一次删除失败，账本必须保留，再用真实 iptables 重试。
	real, err := exec.LookPath("iptables")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := []byte("#!/bin/sh\ncase \"$*\" in *' -D '*) exit 1;; esac\nexec " + real + " \"$@\"\n")
	if err := os.WriteFile(filepath.Join(bin, "iptables"), wrapper, 0755); err != nil {
		t.Fatal(err)
	}
	originalPATH := os.Getenv("PATH")
	t.Setenv("PATH", bin+":"+originalPATH)
	if err := Down(cfg, discardLogger()); err == nil {
		t.Fatal("deletion failure ignored")
	}
	if _, err := os.Stat(forwardRangePath + ".rules.json"); err != nil {
		t.Fatal("ledger lost after failure")
	}
	t.Setenv("PATH", originalPATH)
	if err := Down(cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if err := Down(cfg, discardLogger()); err != nil {
		t.Fatal("repeated Down:", err)
	}
	actual, err := listNetfilterRules("nat", "POSTROUTING")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range actual {
		if sameRule(r, unrelated, false) {
			found = true
		}
		if sameRule(r, rules[0], true) {
			t.Fatal("old WAN MASQUERADE remains")
		}
	}
	if !found {
		t.Fatal("unrelated rule removed")
	}
	for _, tc := range [][2]string{{"filter", "FORWARD"}, {"filter", "INPUT"}, {"mangle", "FORWARD"}} {
		left, err := listNetfilterRules(tc[0], tc[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Fatalf("owned rules remain in %v: %+v", tc, left)
		}
	}
}
