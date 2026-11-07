//go:build windows

package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseReview_RestoreFailurePreservesRecoveryFiles(t *testing.T) {
	t.Setenv("ProgramData", t.TempDir())
	dir := t.TempDir()
	state := filepath.Join(dir, "data", "state.json")
	if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{InstallDir: dir}
	if err := restoreNetwork(opts, func(string) {}); err == nil {
		t.Fatal("missing recovery executable accepted")
	}
	exe := filepath.Join(dir, AppExeName)
	if err := os.WriteFile(exe, []byte("not an executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := restoreNetwork(opts, func(string) {}); err == nil {
		t.Fatal("failed recovery process ignored")
	}
	for _, file := range []string{state, exe} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("recovery file removed: %v", err)
		}
	}
}
