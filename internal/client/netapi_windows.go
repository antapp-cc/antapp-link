//go:build windows

package client

import (
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// 本文件是网络接管的底层原语：全部为进程内系统调用（iphlpapi/dnsapi），
// 不依赖任何外部命令。
// 路由与地址读写用 WireGuard 官方封装的 winipcfg 包，久经实战。

// adapterInfo 是一次适配器枚举的快照。
type adapterInfo struct {
	Name  string // FriendlyName，如 "WLAN"；还原 DNS 时按它找回来
	Index uint32
	LUID  winipcfg.LUID
	Up    bool
}

// adapters 枚举全部适配器，含未连接的（旧版残留扫描要能看见它们）。
func adapters() ([]adapterInfo, error) {
	aas, err := winipcfg.GetAdaptersAddresses(windows.AF_UNSPEC, winipcfg.GAAFlagDefault)
	if err != nil {
		return nil, fmt.Errorf("枚举网卡: %w", err)
	}
	out := make([]adapterInfo, 0, len(aas))
	for _, aa := range aas {
		out = append(out, adapterInfo{
			Name:  aa.FriendlyName(),
			Index: aa.IfIndex,
			LUID:  aa.LUID,
			Up:    aa.OperStatus == winipcfg.IfOperStatusUp,
		})
	}
	return out, nil
}

// adapterByName 按友好名找网卡（如隧道网卡的固定名 "AntApp Link"）。
func adapterByName(name string) (adapterInfo, bool, error) {
	aas, err := adapters()
	if err != nil {
		return adapterInfo{}, false, err
	}
	for _, a := range aas {
		if a.Name == name {
			return a, true, nil
		}
	}
	return adapterInfo{}, false, nil
}

// adapterByIndex 按接口索引找网卡。
func adapterByIndex(index uint32) (adapterInfo, bool, error) {
	aas, err := adapters()
	if err != nil {
		return adapterInfo{}, false, err
	}
	for _, a := range aas {
		if a.Index == index {
			return a, true, nil
		}
	}
	return adapterInfo{}, false, nil
}

// bestDefaultRoute 查当前默认出口：只认前缀长度**恰为 0** 且下一跳非 0.0.0.0
// 的路由，并排除 excludeLUID（隧道自己）—— 否则我们写的 0.0.0.0/1 会被当成
// 「到 0.0.0.0 的路由」（Contains 是包含关系，0.0.0.0 落在 0.0.0.0/1 里），
// watcher 就会把隧道当出口。WireGuard 的 findDefaultLUID 同款两条防线。
// 多条默认路由之间按 row.Metric + 接口 Metric 的合成值取最小。
func bestDefaultRoute(excludeLUID winipcfg.LUID) (nextHop netip.Addr, ifIndex uint32, err error) {
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("读路由表: %w", err)
	}
	best := ^uint64(0)
	found := false
	for i := range rows {
		r := &rows[i]
		if r.DestinationPrefix.PrefixLength != 0 || r.InterfaceLUID == excludeLUID {
			continue
		}
		nh := r.NextHop.Addr()
		if !nh.IsValid() || nh.IsUnspecified() {
			continue
		}
		ifrow, err := r.InterfaceLUID.Interface()
		if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp {
			continue
		}
		ipif, err := r.InterfaceLUID.IPInterface(windows.AF_INET)
		if err != nil {
			continue
		}
		combined := uint64(r.Metric) + uint64(ipif.Metric)
		if !found || combined < best {
			best, found = combined, true
			nextHop, ifIndex = nh, r.InterfaceIndex
		}
	}
	if !found {
		return netip.Addr{}, 0, errors.New("找不到可用的默认路由")
	}
	return nextHop, ifIndex, nil
}

// bestRouteTo 查到 destination 的最优下一跳（最长前缀，同长取合成 metric 最小），
// 排除隧道接口。下一跳为 0.0.0.0 表示目的地在直连网段。
func bestRouteTo(dest netip.Addr, excludeLUID winipcfg.LUID) (nextHop netip.Addr, ifIndex uint32, err error) {
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("读路由表: %w", err)
	}
	var best *winipcfg.MibIPforwardRow2
	bestBits, bestCombined := -1, uint64(0)
	for i := range rows {
		r := &rows[i]
		if r.InterfaceLUID == excludeLUID {
			continue
		}
		ip := r.DestinationPrefix.RawPrefix.Addr()
		if !ip.IsValid() {
			continue
		}
		p := netip.PrefixFrom(ip, int(r.DestinationPrefix.PrefixLength))
		if !p.Contains(dest) {
			continue
		}
		ifrow, err := r.InterfaceLUID.Interface()
		if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp {
			continue
		}
		combined := uint64(r.Metric)
		if ipif, err := r.InterfaceLUID.IPInterface(windows.AF_INET); err == nil {
			combined += uint64(ipif.Metric)
		}
		if best == nil || p.Bits() > bestBits || (p.Bits() == bestBits && combined < bestCombined) {
			best, bestBits, bestCombined = r, p.Bits(), combined
		}
	}
	if best == nil {
		return netip.Addr{}, 0, fmt.Errorf("到 %s 没有任何路由", dest)
	}
	return best.NextHop.Addr(), best.InterfaceIndex, nil
}

// ifaceMetricNotNeeded 占位删除标记

// adapterDNS 读一张网卡当前生效的 DNS，只返回 IPv4 的。
func adapterDNS(l winipcfg.LUID) []netip.Addr {
	addrs, err := l.DNS()
	if err != nil {
		return nil
	}
	var v4 []netip.Addr
	for _, a := range addrs {
		if a.Is4() {
			v4 = append(v4, a.Unmap())
		}
	}
	return v4
}

// adapterUnicastIPs 枚举一张网卡上的单播地址（含前缀长度），只返回 IPv4 的。
func adapterUnicastIPs(l winipcfg.LUID) []netip.Prefix {
	rows, err := winipcfg.GetUnicastIPAddressTable(windows.AF_UNSPEC)
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for i := range rows {
		r := &rows[i]
		if r.InterfaceLUID != l {
			continue
		}
		ip := r.Address.Addr().Unmap()
		if !ip.Is4() {
			continue
		}
		out = append(out, netip.PrefixFrom(ip, int(r.OnLinkPrefixLength)))
	}
	return out
}

// flushResolverCache 刷新系统 DNS 缓存，等价于 ipconfig /flushdns。
// 失败不致命（缓存自己会过期），调用方按尽力而为处理。
func flushResolverCache() {
	_, _, _ = windows.NewLazySystemDLL("dnsapi.dll").NewProc("DnsFlushResolverCache").Call()
}

// sweepSplitRoutes 删除「国内直连」残留路由：下一跳等于原默认网关且
// route metric 为 5 的全部条目（默认路由除外）。一次全表扫描 + 逐条原生删除，
// 毫秒级完成；不像 route.exe 那样删不存在的路由会卡死，条目不存在时静默跳过。
func sweepSplitRoutes(gateway netip.Addr) (removed int) {
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return 0
	}
	for i := range rows {
		r := rows[i]
		if r.Metric != splitRouteMetric || r.NextHop.Addr().Unmap() != gateway {
			continue
		}
		dest := r.DestinationPrefix
		ip := dest.RawPrefix.Addr()
		if !ip.IsValid() || (ip.IsUnspecified() && dest.PrefixLength == 0) {
			continue // 绝不动用户的默认路由
		}
		if r.Delete() == nil {
			removed++
		}
	}
	return removed
}
