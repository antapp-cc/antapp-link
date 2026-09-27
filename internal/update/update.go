// Package update 负责检查、下载并应用客户端更新。
//
// 更新源是「一串 URL，按顺序试」：主源走 GitHub Releases，将来加自建源只需要往
// DefaultSources 里塞一条，代码不用动。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	manifestLimit = 64 << 10  // 清单不该更大
	exeLimit      = 200 << 20 // 更新包上限，防止被喂一个超大文件把磁盘写满
	userAgent     = "AntAppLink-Updater"
)

// DefaultSources 是内置的更新源，按顺序尝试。
//
// GitHub 的 releases/latest/download/<文件名> 是「最新一版的固定链接」，
// 不用调 API —— 未认证的 GitHub API 每 IP 每小时只有 60 次，而且 assets 结构
// 解析起来脆。等有了自己的服务器，在下面加一条即可，主源不通时自动回落。
var DefaultSources = []Source{
	{
		Name:     "github",
		Manifest: "https://github.com/antapp-cc/antapp-link/releases/latest/download/latest.json",
	},
	// 自建备源（还没有服务器时留空，不影响）：
	// {Name: "self", Manifest: "https://pi.antapp.cc/antapp-link/latest.json"},
}

// Asset 是清单里的一个可下载文件。
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`

	// Sig 预留给将来的签名校验。
	//
	// 现在只校 sha256，挡得住传输损坏，但挡不住更新源本身被替换 —— 谁能改
	// latest.json 就能往所有节点机推任意程序。这个字段先占好位置，
	// 以后加 Ed25519 签名不用改清单结构，老客户端也不会因为多了字段而解析失败。
	Sig string `json:"sig,omitempty"`
}

// Manifest 是更新清单。
type Manifest struct {
	Version     string `json:"version"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"published_at"`
	Client      Asset  `json:"client"`
	Server      *Asset `json:"server,omitempty"`
}

// Source 是一个更新源。
type Source struct {
	Name     string
	Manifest string
}

// Checker 负责查与下载。
type Checker struct {
	Sources []Source
	Current string
	HTTP    *http.Client
}

// updateSourcesEnv 可以覆盖内置更新源（分号分隔的 URL）。
//
// 留这个口子是为了内网部署和本地联调：不然想验一次更新流程，就得先真发一个
// GitHub Release 出来。
const updateSourcesEnv = "ANTAPP_UPDATE_SOURCES"

func NewChecker(current string) *Checker {
	sources := DefaultSources
	if env := strings.TrimSpace(os.Getenv(updateSourcesEnv)); env != "" {
		var custom []Source
		for i, u := range strings.Split(env, ";") {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			custom = append(custom, Source{Name: fmt.Sprintf("env%d", i+1), Manifest: u})
		}
		if len(custom) > 0 {
			sources = custom
		}
	}
	return &Checker{
		Sources: sources,
		Current: current,
		HTTP:    &http.Client{Timeout: 25 * time.Second},
	}
}

// Check 依次询问各个源，返回比当前版本新的清单里最新的那个。
// 已是最新时返回 (nil, nil)；所有源都失败时返回 (nil, errs)。
func (c *Checker) Check(ctx context.Context) (*Manifest, []error) {
	var errs []error
	var newest *Manifest

	for _, src := range c.Sources {
		m, err := c.fetchManifest(ctx, src)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Name, err))
			continue
		}
		if CompareVersions(m.Version, c.Current) <= 0 {
			continue
		}
		if newest == nil || CompareVersions(m.Version, newest.Version) > 0 {
			newest = m
		}
	}
	return newest, errs
}

func (c *Checker) fetchManifest(ctx context.Context, src Source) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.Manifest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, manifestLimit))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析清单: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("清单缺少 version")
	}
	if strings.TrimSpace(m.Client.URL) == "" {
		return errors.New("清单缺少 client.url")
	}
	if len(m.Client.SHA256) != 64 {
		return fmt.Errorf("client.sha256 应该是 64 位十六进制，实际 %d 位", len(m.Client.SHA256))
	}
	if _, err := hex.DecodeString(m.Client.SHA256); err != nil {
		return fmt.Errorf("client.sha256 不是合法十六进制: %w", err)
	}
	return nil
}

// Download 下载更新包、校验 sha256，返回落地路径。
//
// 边下边算摘要：不用把文件读两遍，而且校验不过时直接删掉，
// 不会留下一个「看起来能用」的半成品。
func (c *Checker) Download(ctx context.Context, m *Manifest, dir string,
	progress func(done, total int64)) (string, error) {

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Client.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载更新包: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新包: HTTP %d", resp.StatusCode)
	}

	target := filepath.Join(dir, "antapp-link.exe.new")
	f, err := os.Create(target)
	if err != nil {
		return "", err
	}

	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(target)
		}
	}()

	total := resp.ContentLength
	if total <= 0 {
		total = m.Client.Size
	}

	digest := sha256.New()
	var done int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			done += int64(n)
			if done > exeLimit {
				return "", fmt.Errorf("更新包超过 %d MB，拒绝继续", exeLimit>>20)
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", werr
			}
			digest.Write(buf[:n])
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	got := hex.EncodeToString(digest.Sum(nil))
	if !strings.EqualFold(got, m.Client.SHA256) {
		return "", fmt.Errorf("sha256 不匹配（清单 %s…，实际 %s…），更新包损坏或被替换过",
			m.Client.SHA256[:12], got[:12])
	}
	ok = true
	return target, nil
}

// CompareVersions 比较两个版本号，返回 -1 / 0 / 1。
//
// 只比前导的 主.次.修订 三段数字：开头的 v 可有可无，后面的后缀
// （-beta、+build 之类）一律忽略 —— 客户端只需要知道「是不是变新了」。
func CompareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			n = 0
		}
		out[i] = n
	}
	return out
}
