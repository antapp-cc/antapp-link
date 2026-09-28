package client

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "embed"
)

// 智能分流的国内网段表：数据与程序分离。
//
//   exe 内嵌一份兜底表（cn_routes.txt，随版本发布）；
//   data\cn_routes.txt 是云端表（社区每日更新），存在且合法时优先使用。
//   客户端每 24 小时从公共源拉取刷新；拉取/校验失败一律静默保持现状，
//   绝不影响连接。启动时只读本地文件（毫秒级），不发起任何网络请求。

//go:embed cn_routes.txt
var embeddedRoutesRaw string

const (
	routeTableFileName   = "cnr.cache"     // 混淆缓存（二进制，只应由本程序读写）
	legacyRouteTableName = "cn_routes.txt" // 旧版明文表（仅历史机器上可能存在）
	routeTableMaxLines   = 50000           // 网段表行数上限（防喂垃圾）
	routeTableMinLines   = 100             // 合法表的下限（社区 CN 列表有数千条）
	routeTableMaxBytes   = 4 << 20

	routeTableFetchTimeout = 60 * time.Second

	routeTableSourcePrimary   = "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geoip/cn.txt"
	routeTableSourceSecondary = "https://raw.githubusercontent.com/gaoyifan/china-operator-ip/ip-lists/china.txt"
)

var (
	routeTableMu     sync.RWMutex
	routeTablePath   string   // data\cn_routes.txt，NewApp 时注入
	activeRouteTable []string // 当前生效的网段表
)

// SetRouteTablePath 注入外部网段表的落盘路径并加载初始表。
func SetRouteTablePath(path string) {
	routeTableMu.Lock()
	defer routeTableMu.Unlock()
	routeTablePath = path
	initActiveRouteTableLocked()
}

// ActiveCNRoutes 返回当前生效的分流网段表（外部表优先，内置兜底）。
func ActiveCNRoutes() []string {
	routeTableMu.RLock()
	defer routeTableMu.RUnlock()
	if len(activeRouteTable) > 0 {
		return activeRouteTable
	}
	return EmbeddedCNRoutes()
}

// EmbeddedCNRoutes 返回内嵌兜底表。
func EmbeddedCNRoutes() []string {
	embeddedOnce.Do(func() {
		embeddedRoutes = parseRouteTableLines(strings.NewReader(embeddedRoutesRaw))
	})
	return embeddedRoutes
}

var (
	embeddedOnce   sync.Once
	embeddedRoutes []string
)

// initActiveRouteTableLocked 加载外部表（损坏/缺失时回退内置表）。
// 调用方须持有 routeTableMu。
func initActiveRouteTableLocked() {
	activeRouteTable = nil
	if routeTablePath == "" {
		return
	}
	// 现行表：混淆缓存
	if data, err := os.ReadFile(routeTablePath); err == nil {
		if text, err := openSealedCache(data); err == nil {
			if table := parseRouteTableLines(strings.NewReader(string(text))); len(table) >= routeTableMinLines {
				activeRouteTable = table
				return
			}
		}
	}
	// 缓存缺失/损坏：尝试旧版明文表（仅升级机器上可能存在，一次性迁移）
	legacyPath := filepath.Join(filepath.Dir(routeTablePath), legacyRouteTableName)
	if data, err := os.ReadFile(legacyPath); err == nil {
		if table := parseRouteTableLines(strings.NewReader(string(data))); len(table) >= routeTableMinLines {
			activeRouteTable = table
			return
		}
	}
}

// parseRouteTableLines 解析网段表：每行一个 IPv4 CIDR，跳过注释与空行，
// 去重；行数超上限视为非法（防喂垃圾）。
func parseRouteTableLines(r io.Reader) []string {
	var out []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), routeTableMaxBytes)
	lines := 0
	for scanner.Scan() && lines <= routeTableMaxLines {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines++
		prefix, err := netip.ParsePrefix(line)
		if err != nil {
			continue
		}
		ip := prefix.Addr().Unmap()
		if !ip.Is4() {
			continue
		}
		key := netip.PrefixFrom(ip, prefix.Bits()).String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// RouteTableSources 返回网段表的拉取源（公共社区维护、每日自动更新）。
func RouteTableSources() []string {
	return []string{routeTableSourcePrimary, routeTableSourceSecondary}
}

// fetchAndStoreRouteTable 拉取最新网段表并落盘。全部来源失败时返回 false，
// 调用方保持现状稍后重试。
func fetchAndStoreRouteTable() bool {
	routeTableMu.RLock()
	path := routeTablePath
	routeTableMu.RUnlock()
	if path == "" {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), routeTableFetchTimeout)
	defer cancel()
	var lastErr error
	for _, src := range RouteTableSources() {
		table, err := downloadRouteTable(ctx, src)
		if err != nil {
			lastErr = err
			continue
		}
		if err := storeRouteTableFile(path, table); err != nil {
			lastErr = err
			continue
		}
		// 落盘成功后同步换掉内存中的现行表 —— 否则热重铺拿到的还是旧表
		routeTableMu.Lock()
		activeRouteTable = table
		routeTableMu.Unlock()
		logf("分流网段表已更新：%d 条", len(table))
		// 旧版明文表已由混淆缓存取代，顺手清掉（仅升级机器上有）
		_ = os.Remove(filepath.Join(filepath.Dir(path), legacyRouteTableName))
		return true
	}
	if lastErr != nil {
		logf("分流网段表拉取失败，保持现有表：%v", lastErr)
	}
	return false
}

// downloadRouteTable 下载并解析一份网段表。
func downloadRouteTable(ctx context.Context, src string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	table := parseRouteTableLines(io.LimitReader(resp.Body, routeTableMaxBytes))
	if len(table) < routeTableMinLines {
		return nil, fmt.Errorf("内容不合法（仅 %d 条）", len(table))
	}
	return table, nil
}

// storeRouteTableFile 序列化、压缩加扰后原子落盘（tmp + rename）。
func storeRouteTableFile(path string, table []string) error {
	var text bytes.Buffer
	for _, line := range table {
		text.WriteString(line)
		text.WriteByte('\n')
	}
	sealed := sealCache(text.Bytes())
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadSealedCacheFile 读取并解出混淆缓存里的网段表文本。
func loadSealedCacheFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return openSealedCache(data)
}

// cacheObfuscateKey 是缓存混淆密钥：数据本身是公开的 APNIC 分配表，
// 压缩加扰只为不让人随手打开可读，不构成加密。
var cacheObfuscateKey = []byte("AntAppLink")

const cacheFormatVer byte = 0x01

// sealCache 序列化并混淆：格式版本字节 + gzip 压缩 + 逐字节异或。
// 异或只防「随手打开」，不防逆向 —— 数据本身是公开的。
func sealCache(text []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(cacheFormatVer)
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(text)
	_ = gz.Close()
	out := buf.Bytes()
	for i := 1; i < len(out); i++ {
		out[i] ^= cacheObfuscateKey[i%len(cacheObfuscateKey)]
	}
	return out
}

// openSealedCache 还原 sealCache 的输出。
func openSealedCache(sealed []byte) ([]byte, error) {
	if len(sealed) < 2 || sealed[0] != cacheFormatVer {
		return nil, fmt.Errorf("cache: 格式不符")
	}
	body := make([]byte, len(sealed)-1)
	for i := 1; i < len(sealed); i++ {
		body[i-1] = sealed[i] ^ cacheObfuscateKey[(i-1)%len(cacheObfuscateKey)]
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}
