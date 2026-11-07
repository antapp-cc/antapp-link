//go:build linux

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStrictReview_NetfilterRemovesMarkedOldRange(t *testing.T) {
	if os.Getenv("ANTAPP_TEST_NETFILTER") != "isolated" {
		t.Skip("requires isolated network namespace")
	}
	old := Rule{Chain: "INPUT", Args: []string{"-p", "tcp", "--dport", "31400:31409", "-j", "ACCEPT"}}
	marked := Rule{Chain: old.Chain, Args: append(append([]string(nil), old.Args...), "-m", "comment", "--comment", "AntAppLink")}
	foreign := Rule{Chain: old.Chain, Args: append(append([]string(nil), old.Args...), "-m", "comment", "--comment", "OtherVPN")}
	for _, r := range []Rule{old, marked, foreign} {
		if err := ensureRule(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeInputRange("31400:31409"); err != nil {
		t.Fatal(err)
	}
	rules, err := listNetfilterRules("filter", "INPUT")
	if err != nil {
		t.Fatal(err)
	}
	keptForeign := false
	for _, r := range rules {
		if sameRule(r, old, false) || sameRule(r, marked, false) {
			t.Fatalf("old INPUT range still open: %+v", r)
		}
		keptForeign = keptForeign || sameRule(r, foreign, false)
	}
	if !keptForeign {
		t.Fatal("unrelated INPUT rule removed")
	}
}

func TestStrictReview_NetfilterRangeDeletionFailureIsRetryable(t *testing.T) {
	if os.Getenv("ANTAPP_TEST_NETFILTER") != "isolated" {
		t.Skip("requires isolated network namespace")
	}
	rule := Rule{Chain: "INPUT", Args: []string{"-p", "tcp", "--dport", "31500:31509", "-m", "comment", "--comment", "AntAppLink", "-j", "ACCEPT"}}
	if err := ensureRule(rule); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("iptables")
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *' -D '*) exit 1;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(folder, "iptables"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	originalPATH := os.Getenv("PATH")
	t.Setenv("PATH", folder+":"+originalPATH)
	if err := removeInputRange("31500:31509"); err == nil {
		t.Fatal("deletion failure ignored")
	}
	t.Setenv("PATH", originalPATH)
	if err := removeInputRange("31500:31509"); err != nil {
		t.Fatal(err)
	}
	if err := removeInputRange("31500:31509"); err != nil {
		t.Fatal("repeat cleanup:", err)
	}
}
