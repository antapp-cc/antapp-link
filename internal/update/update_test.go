package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 更新源不能再被环境变量改掉：客户端是提权运行的，HKCU\Environment 普通进程就能写。
func TestNewCheckerIgnoresEnvOverride(t *testing.T) {
	t.Setenv("ANTAPP_UPDATE_SOURCES", "http://evil.example/latest.json")

	c := NewChecker("0.1.0")
	if len(c.Sources) != len(DefaultSources) {
		t.Fatalf("更新源数量 = %d, want %d", len(c.Sources), len(DefaultSources))
	}
	for i, src := range c.Sources {
		if src != DefaultSources[i] {
			t.Errorf("源 %d = %+v, want %+v", i, src, DefaultSources[i])
		}
	}
	if c.Current != "0.1.0" {
		t.Errorf("Current = %q, want \"0.1.0\"", c.Current)
	}
}

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

// testSignPriv 是包级测试密钥。这些用例只关心「清单有没有被正确签名并被接受」，
// 不关心密钥本身，所以进程内生成一次、整包共用；releasePubKey 也换成对应的公钥，
// 免得测试依赖真实发布私钥。
var testSignPriv ed25519.PrivateKey

func init() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	releasePubKey = [32]byte(pub)
	testSignPriv = priv
}

// writeManifest 按请求路径写清单或它的签名。
//
// 发布流程就是成对上传 latest.json 与 latest.json.sig，测试也照这个形状来 ——
// 否则严格验签会把每个用例都直接拒掉。
func writeManifest(t *testing.T, w http.ResponseWriter, r *http.Request, version string) {
	t.Helper()
	body, err := json.Marshal(Manifest{Version: version, Client: validAsset()})
	if err != nil {
		t.Error(err)
		return
	}
	if strings.HasSuffix(r.URL.Path, ".sig") {
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(testSignPriv, body))))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// 多个源按顺序试：坏源跳过、记录错误，最后挑出最新的那一版。
func TestCheckPicksNewestAcrossSources(t *testing.T) {
	// 用单个 handler 分发而不是给每个路径注册：ServeMux 是精确匹配，
	// 注册了 /old.json 并不会让 /old.json.sig 也走进来（签名请求会 404）。
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimSuffix(r.URL.Path, ".sig") {
		case "/broken.json":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/old.json":
			writeManifest(t, w, r, "0.2.0")
		case "/new.json":
			writeManifest(t, w, r, "0.5.0")
		case "/junk.json":
			_, _ = w.Write([]byte("{ not json"))
		default:
			http.NotFound(w, r)
		}
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
		writeManifest(t, w, r, "0.2.0")
	}))
	defer srv.Close()

	c := &Checker{
		// 清单 URL 必须带路径：签名取的是 "<清单URL>.sig"，裸的 srv.URL 会把 .sig
		// 拼到端口后面，变成一个非法 URL。
		Sources: []Source{{Name: "only", Manifest: srv.URL + "/latest.json"}},
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

// 自动与手动更新可能同时下载同一个目标名：每次下载写独立临时文件、落盘重算、
// 原子改名，两条路径都不该留下坏文件或互相截断。
func TestDownloadConcurrentSameTarget(t *testing.T) {
	payload := []byte("concurrent update payload")
	sum := sha256.Sum256(payload)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 两个请求都到齐后再一起返回，放大交错窗口
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Checker{HTTP: srv.Client()}
	m := &Manifest{Version: "1.0.0", Client: Asset{
		URL: srv.URL, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)),
	}}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Download(context.Background(), m, dir, nil)
			errs <- err
		}()
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发下载失败: %v", err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "antapp-link.exe.new"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, payload) {
		t.Error("最终文件内容不完整")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}

// 清单版本号必须符合 主.次.修订 规则：格式错了签名发布照样能过，但所有节点会
// 把它比较成「没有更新」，所以 Validate 必须拦下。
func TestManifestValidateVersionFormat(t *testing.T) {
	base := func(version string) *Manifest {
		return &Manifest{
			Version: version,
			Client:  Asset{URL: "https://example.com/a", SHA256: strings.Repeat("a", 64)},
		}
	}
	for _, v := range []string{"0.4.12", "1.2.3", "v1.2.3", "1.2.3-rc.1", "1.2.3+build.5"} {
		if err := base(v).Validate(); err != nil {
			t.Errorf("版本 %q 应通过校验: %v", v, err)
		}
	}
	for _, v := range []string{"", "oops", "1.2", "1.2.3.4"} {
		if err := base(v).Validate(); err == nil {
			t.Errorf("版本 %q 应被拒绝", v)
		}
	}
}
