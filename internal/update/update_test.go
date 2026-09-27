package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"V1.2.3", "1.2.3", 0},
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.1.0", 1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.1", "0.1.0", 0},
		{"0.1.0-beta", "0.1.0", 0},
		{"v1.2.3", "1.3.0", -1},
		{"", "0.0.1", -1},
		{"1.0.0+build7", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func validAsset() Asset {
	return Asset{URL: "https://example.com/antapp-link.exe", SHA256: strings.Repeat("ab", 32)}
}

func TestManifestValidate(t *testing.T) {
	good := Manifest{Version: "1.0.0", Client: validAsset()}
	if err := good.Validate(); err != nil {
		t.Fatalf("合法清单不该被拒: %v", err)
	}

	bad := []struct {
		name string
		m    Manifest
	}{
		{"缺 version", Manifest{Client: validAsset()}},
		{"缺 url", Manifest{Version: "1.0.0", Client: Asset{SHA256: strings.Repeat("ab", 32)}}},
		{"缺 sha256", Manifest{Version: "1.0.0", Client: Asset{URL: "https://x/y.exe"}}},
		{"sha256 太短", Manifest{Version: "1.0.0", Client: Asset{URL: "https://x/y.exe", SHA256: "abcd"}}},
		{"sha256 不是十六进制", Manifest{Version: "1.0.0",
			Client: Asset{URL: "https://x/y.exe", SHA256: strings.Repeat("zz", 32)}}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := c.m.Validate(); err == nil {
				t.Error("应该被拒绝")
			}
		})
	}
}

func writeManifest(t *testing.T, w http.ResponseWriter, version string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(Manifest{Version: version, Client: validAsset()})
	if err != nil {
		t.Error(err)
	}
}

// 多个源按顺序试：坏源跳过、记录错误，最后挑出最新的那一版。
func TestCheckPicksNewestAcrossSources(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/broken.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/old.json", func(w http.ResponseWriter, r *http.Request) {
		writeManifest(t, w, "0.2.0")
	})
	mux.HandleFunc("/new.json", func(w http.ResponseWriter, r *http.Request) {
		writeManifest(t, w, "0.5.0")
	})
	mux.HandleFunc("/junk.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{ not json"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Checker{
		Sources: []Source{
			{Name: "broken", Manifest: srv.URL + "/broken.json"},
			{Name: "junk", Manifest: srv.URL + "/junk.json"},
			{Name: "old", Manifest: srv.URL + "/old.json"},
			{Name: "new", Manifest: srv.URL + "/new.json"},
		},
		Current: "0.1.0",
		HTTP:    srv.Client(),
	}

	m, errs := c.Check(context.Background())
	if m == nil {
		t.Fatalf("应该找到更新（errs=%v）", errs)
	}
	if m.Version != "0.5.0" {
		t.Errorf("应该挑最新的 0.5.0，实际 %s", m.Version)
	}
	if len(errs) != 2 {
		t.Errorf("两个坏源都该被记下来，实际 %d 条: %v", len(errs), errs)
	}
}

func TestCheckReturnsNilWhenUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeManifest(t, w, "0.2.0")
	}))
	defer srv.Close()

	c := &Checker{
		Sources: []Source{{Name: "only", Manifest: srv.URL}},
		Current: "0.2.0",
		HTTP:    srv.Client(),
	}
	m, errs := c.Check(context.Background())
	if m != nil {
		t.Errorf("版本相同时不该报更新，得到 %s", m.Version)
	}
	if len(errs) != 0 {
		t.Errorf("不该有错误: %v", errs)
	}
}

func TestDownloadVerifiesSHA256(t *testing.T) {
	payload := []byte("pretend this is a 9 MB exe")
	sum := sha256.Sum256(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Checker{HTTP: srv.Client()}

	// 摘要不符：必须报错，而且不能留下文件让下次误用
	bad := &Manifest{Version: "1.0.0",
		Client: Asset{URL: srv.URL, SHA256: strings.Repeat("0", 64)}}
	if _, err := c.Download(context.Background(), bad, dir, nil); err == nil {
		t.Fatal("sha256 不符时必须报错")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("校验失败后不该留下文件，实际 %d 个", len(entries))
	}

	// 摘要正确：成功，且内容一致
	good := &Manifest{Version: "1.0.0",
		Client: Asset{URL: srv.URL, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))}}

	var lastDone, lastTotal int64
	got, err := c.Download(context.Background(), good, dir, func(done, total int64) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("下载: %v", err)
	}
	raw, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, payload) {
		t.Error("落地内容与源不一致")
	}
	if lastDone != int64(len(payload)) {
		t.Errorf("进度回调收到的字节数 = %d, want %d", lastDone, len(payload))
	}
	if lastTotal <= 0 {
		t.Error("进度回调应该带上总长度")
	}
}

func TestDownloadRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Checker{HTTP: srv.Client()}
	m := &Manifest{Version: "1.0.0", Client: Asset{URL: srv.URL, SHA256: strings.Repeat("ab", 32)}}
	if _, err := c.Download(context.Background(), m, t.TempDir(), nil); err == nil {
		t.Error("HTTP 404 时必须报错")
	}
}
