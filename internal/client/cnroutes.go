package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
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
	routeTableFileName     = "cn_routes.txt"
	routeTableMaxLines     = 50000 // 网段表行数上限（防喂垃圾）
	routeTableMinLines     = 100   // 合法表的下限（社区 CN 列表有数千条）
	routeTableMaxBytes     = 4 << 20
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
	f, err := os.Open(routeTablePath)
	if err != nil {
		return // 文件不存在：用内置表
	}
	defer f.Close()
	table := parseRouteTableLines(f)
	if len(table) < routeTableMinLines {
		return // 内容不合法：用内置表
	}
	activeRouteTable = table
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

// storeRouteTableFile 原子落盘（tmp + rename），格式与内置表一致。
func storeRouteTableFile(path string, table []string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, line := range table {
		if _, err := f.WriteString(line + "\n"); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
