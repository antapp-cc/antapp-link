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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	manifestLimit = 64 << 10  // 清单不该更大
	sigLimit      = 4 << 10   // 签名只有 64 字节（base64/hex 编码后也很小），留足余量
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

	// Sig 是历史遗留字段，不再使用：签名改走清单旁边的独立文件 latest.json.sig
	// （见 verify.go），签的是清单的原始字节 —— 内嵌在清单里做不到，签名要覆盖一个
	// 包含它自己的文档。留着只为兼容已经写成这样的老清单。
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
	// PubKey 覆盖内置的发布公钥，只给测试和本地联调工具（tools/updatetest）用：
	// 它们得拿测试密钥签本地清单，否则只能用生产私钥，那个工具就废了。
	// 生产路径留空，走 releasePubKey。
	PubKey *[32]byte
}

func (c *Checker) publicKey() [32]byte {
	if c.PubKey != nil {
		return *c.PubKey
	}
	return releasePubKey
}

// 这里刻意不留「用环境变量覆盖更新源」的口子（曾经有过 ANTAPP_UPDATE_SOURCES）。
//
// 客户端 manifest 是 requireAdministrator，而 HKCU\Environment 是普通进程就能写的：
// 任何能以该用户身份跑起来的进程都能让客户端以管理员权限去下载它指定的 URL、拿它
// 自己算的 sha256 自校验、替换自身再拉起。环境变量对提权攻击者是零成本的入口，
// 所以更新源只能由构建期定死。内网部署要换源，改 DefaultSources。
func NewChecker(current string) *Checker {
	return &Checker{
		Sources: DefaultSources,
		Current: current,
		// 不用 Client.Timeout：它是整个请求（含读 body）的上限，25 秒对 11 MB 的安装包
		// 太紧 —— 国内经隧道下 GitHub 附件实测下到 28% 就超时。改成分段设限：连接、
		// TLS 握手、响应头各有上限，body 阶段交给调用方的 ctx（服务端那条是 10 分钟）。
		HTTP: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   20 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   20 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       60 * time.Second,
				ExpectContinueTimeout: 5 * time.Second,
			},
		},
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
	// 先验签再解析：解析出来的下载 URL 与 sha256 都要以「清单没被改动」为前提，
	// 顺序反了等于没校验（详见 verifyManifest 的说明）。
	sig, err := c.fetchSignature(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := verifyManifest(raw, sig, c.publicKey()); err != nil {
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

// fetchSignature 取清单旁边的 .sig。
//
// 签名放独立文件而不是清单里的字段：内嵌的话签名要覆盖一个包含它自己的文档，做不到。
// 固定用 "清单URL + .sig"，发布侧只要把两个文件一起传上去。
func (c *Checker) fetchSignature(ctx context.Context, src Source) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.Manifest+".sig", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("取清单签名: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w（%s.sig 不存在）", ErrUnsigned, src.Manifest)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("取清单签名: HTTP %d", resp.StatusCode)
	}
	sig, err := io.ReadAll(io.LimitReader(resp.Body, sigLimit))
	if err != nil {
		return nil, err
	}
	return sig, nil
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
	// Server 段可选（老清单没有），但一旦出现就必须自洽：比摘要失败时日志要截取
	// sha256 前 12 位，字段残缺会在那条错误路径上直接切片越界 panic。
	if m.Server != nil {
		if strings.TrimSpace(m.Server.URL) == "" {
			return errors.New("清单的 server 段缺少 url")
		}
		if len(m.Server.SHA256) != 64 {
			return fmt.Errorf("server.sha256 应该是 64 位十六进制，实际 %d 位", len(m.Server.SHA256))
		}
		if _, err := hex.DecodeString(m.Server.SHA256); err != nil {
			return fmt.Errorf("server.sha256 不是合法十六进制: %w", err)
		}
	}
	return nil
}

// Download 下载客户端更新包、校验 sha256，返回落地路径。
//
// 边下边算摘要：不用把文件读两遍，而且校验不过时直接删掉，
// 不会留下一个「看起来能用」的半成品。
func (c *Checker) Download(ctx context.Context, m *Manifest, dir string,
	progress func(done, total int64)) (string, error) {
	return c.DownloadAsset(ctx, m.Client, dir, "antapp-link.exe.new", progress)
}

// DownloadAsset 下载清单里的任意一个资产并校验 sha256。
//
// 服务端更新用它下 antapp-linkd —— 契约和客户端那条完全一样：边下边算摘要，
// 校验不过就删掉半成品。name 是落地文件名，由调用方决定（两端的产物名不同）。
func (c *Checker) DownloadAsset(ctx context.Context, a Asset, dir, name string,
	progress func(done, total int64)) (string, error) {

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
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

	target := filepath.Join(dir, name)
	// 每次下载写自己的临时文件：自动与手动更新可能并发，固定目标名会互相截断。
	tmp, err := os.CreateTemp(dir, "."+name+".*.part")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()

	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()

	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}

	var done int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			done += int64(n)
			if done > exeLimit {
				return "", fmt.Errorf("更新包超过 %d MB，拒绝继续", exeLimit>>20)
			}
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				return "", werr
			}
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
	// 先落盘再校验：SHA-256 必须对最终写入磁盘的字节算，而不是对 HTTP 流 ——
	// 流校验挡不住写入时的磁盘错误/安全软件改写，也挡不住并发写同一个文件。
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got, err := fileSHA256(tmpName)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(got, a.SHA256) {
		return "", fmt.Errorf("sha256 不匹配（清单 %s，实际 %s…），更新包损坏或被替换过",
			shortHash(a.SHA256), shortHash(got))
	}
	// 并发下载同一资产时，另一个流程可能已经原子落好文件：内容一致就直接复用。
	// Windows 上 rename 覆盖被占用的目标会失败，单靠 rename 会把并发下载变成报错。
	if existing, eerr := fileSHA256(target); eerr == nil && strings.EqualFold(existing, a.SHA256) {
		return target, nil
	}
	// 原子改名：应用阶段看到的永远是完整文件，不会读到半截下载。
	if err := os.Rename(tmpName, target); err != nil {
		if existing, eerr := fileSHA256(target); eerr == nil && strings.EqualFold(existing, a.SHA256) {
			return target, nil
		}
		return "", err
	}
	ok = true
	return target, nil
}

// fileSHA256 对落盘文件重算摘要。下载校验以它为准，而不是边下载边算的流摘要。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// shortHash 截前 12 位给日志用。清单字段可能残缺（Validate 已经挡了一道，这里是最后
// 一道），截断必须自己判长度 —— 否则这条报错路径本身就是个切片越界 panic。
func shortHash(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
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
