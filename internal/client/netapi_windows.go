//go:build windows

package client

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// 本文件是网络接管的底层原语：全部为进程内系统调用（iphlpapi/dnsapi），
// 不依赖任何外部命令。
// 路由与地址读写用 WireGuard 官方封装的 winipcfg 包，久经实战。

// adapterInfo 是一次适配器枚举的快照。
type adapterInfo struct {
	Name  string // FriendlyName，如 "WLAN"；Tunnel 按固定名查找仍用它
	Index uint32
	GUID  string // 稳定身份（IF GUID），注册表与跨重启都不变
	LUID  winipcfg.LUID
	Up    bool
}

func (a adapterInfo) key() adapterKey {
	return adapterKey{GUID: a.GUID, Index: a.Index, Name: a.Name}
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
			GUID:  luidGUID(aa.LUID),
			LUID:  aa.LUID,
			Up:    aa.OperStatus == winipcfg.IfOperStatusUp,
		})
	}
	return out, nil
}

// luidGUID 把接口 LUID 转成稳定 GUID。转换失败返回空串，调用方退回索引。
func luidGUID(l winipcfg.LUID) string {
	g, err := l.GUID()
	if err != nil {
		return ""
	}
	return g.String()
}

// adapterForIface 按快照里的身份找网卡（GUID → 索引 → 友好名，见 pickAdapter）。
func adapterForIface(iface IfaceDNS) (adapterInfo, bool, error) {
	aas, err := adapters()
	if err != nil {
		return adapterInfo{}, false, err
	}
	keys := make([]adapterKey, len(aas))
	for i := range aas {
		keys[i] = aas[i].key()
	}
	if i, ok := pickAdapter(iface.ref(), keys); ok {
		return aas[i], true, nil
	}
	return adapterInfo{}, false, nil
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

// tcpipIfaceKey 是 Windows 按网卡保存 TCP/IP 配置的注册表位置（键名是网卡 GUID）。
const tcpipIfaceKey = `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters\Interfaces\`

// dnsWasAuto 判断一张网卡当前的 IPv4 DNS 是 DHCP 下发还是静态配置。
//
// 静态 DNS 写在 Tcpip\Parameters\Interfaces\{GUID}\NameServer，DHCP 下发的
// 放在 DhcpNameServer —— 这是唯一可靠的原生来源。dnsapi 的
// GetInterfaceDnsSettings 只给生效值，不区分来源；解析 netsh 输出又要面对
// 本地化关键字（中文系统上 "DHCP" 显示成别的词），都不该再走。
//
// ok=false 表示读不到注册表（键缺失/权限），调用方按「来源未知」处理。
func dnsWasAuto(l winipcfg.LUID) (auto bool, ok bool) {
	guid := luidGUID(l)
	if guid == "" {
		return false, false
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, tcpipIfaceKey+guid, registry.QUERY_VALUE)
	if err != nil {
		return false, false
	}
	defer k.Close()
	// ProfileNameServer 是「按网络位置」配置的静态 DNS，Windows 10/11 的
	// 设置界面会写它，同样属于静态来源。
	for _, name := range []string{"NameServer", "ProfileNameServer"} {
		if v, _, err := k.GetStringValue(name); err == nil && strings.TrimSpace(v) != "" {
			return false, true
		}
	}
	if v, _, err := k.GetStringValue("DhcpNameServer"); err == nil && strings.TrimSpace(v) != "" {
		return true, true
	}
	// 两个值都空：没有静态设置，生效值只能来自 DHCP（或根本没有 IPv4 DNS）。
	return true, true
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

// routeMissing 报告「路由本来就不在」。复原可以重复执行，或条目在扫描与删除
// 之间消失，都不该算失败；除此之外的删除错误必须上报 —— 没删掉却报成功，
// 用户就带着残留路由继续用，而且下次还原也不会再试。
func routeMissing(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

// deleteRouteIgnoringMissing 删一条路由，「本来就不在」视为成功。
func deleteRouteIgnoringMissing(l winipcfg.LUID, dest netip.Prefix, nextHop netip.Addr) error {
	if err := l.DeleteRoute(dest, nextHop); err != nil && !routeMissing(err) {
		return fmt.Errorf("删除路由 %s via %s: %w", dest, nextHop, err)
	}
	return nil
}

// guidByLUID 缓存 LUID → GUID，避免每行路由都做一次系统调用。
func guidByLUID(cache map[winipcfg.LUID]string, l winipcfg.LUID) string {
	if g, ok := cache[l]; ok {
		return g
	}
	g := luidGUID(l)
	cache[l] = g
	return g
}

func prefixOfRow(r *winipcfg.MibIPforwardRow2) (netip.Prefix, bool) {
	ip := r.DestinationPrefix.RawPrefix.Addr()
	if !ip.IsValid() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip.Unmap(), int(r.DestinationPrefix.PrefixLength)).Masked(), true
}

// sweepTunnelHalfRoutes 删掉隧道网卡上的 /1 接管路由，不按下一跳匹配。
//
// 给「快照损坏」或「快照来自还没记录隧道网关的旧版本」这种拿不到网关的保守
// 还原用：0.0.0.0/1 与 128.0.0.0/1 只可能是本程序铺的（Windows 自己不会这么
// 分半默认路由），所以无条件撤掉是安全的。
func sweepTunnelHalfRoutes(luid winipcfg.LUID) (removed int, err error) {
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return 0, fmt.Errorf("读路由表: %w", err)
	}
	var errs []error
	for i := range rows {
		r := &rows[i]
		if r.InterfaceLUID != luid || r.DestinationPrefix.PrefixLength != 1 {
			continue
		}
		dest, ok := prefixOfRow(r)
		if !ok {
			continue
		}
		if derr := r.Delete(); derr != nil {
			if !routeMissing(derr) {
				errs = append(errs, fmt.Errorf("删除接管路由 %s: %w", dest, derr))
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// sweepSplitRoutes 是旧的形态扫描：下一跳等于原默认网关、metric = 5 且不是
// 默认路由的条目一律删。只在快照没有所有权记录（旧版本写的 state.json）时
// 使用 —— 它可能删到同形态的用户路由，新快照一律走 sweepOwnedSplitRoutes。
func sweepSplitRoutes(gateway netip.Addr) (removed int, err error) {
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return 0, fmt.Errorf("读路由表: %w", err)
	}
	var errs []error
	for i := range rows {
		r := &rows[i]
		if r.Metric != splitRouteMetric || r.NextHop.Addr().Unmap() != gateway {
			continue
		}
		dest, ok := prefixOfRow(r)
		if !ok || (dest.Addr().IsUnspecified() && dest.Bits() == 0) {
			continue // 绝不动用户的默认路由
		}
		if derr := r.Delete(); derr != nil {
			if !routeMissing(derr) {
				errs = append(errs, fmt.Errorf("删除国内分流 %s: %w", dest, derr))
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// sweepOwnedSplitRoutes 只删所有权记录里的国内分流路由：接口、网关、metric、
// 前缀四层全对上才动手。没有记录时什么都不做 —— 这时宁可留残留，也不能按
// 形态猜着删用户的静态路由。
func sweepOwnedSplitRoutes(own *splitOwnership) (removed int, err error) {
	if own == nil {
		return 0, nil
	}
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return 0, fmt.Errorf("读路由表: %w", err)
	}
	guids := map[winipcfg.LUID]string{}
	var errs []error
	for i := range rows {
		r := &rows[i]
		dest, ok := prefixOfRow(r)
		if !ok {
			continue
		}
		guid := guidByLUID(guids, r.InterfaceLUID)
		if !own.ownsRoute(guid, r.InterfaceIndex, r.NextHop.Addr(), r.Metric, dest) {
			continue
		}
		if derr := r.Delete(); derr != nil {
			if !routeMissing(derr) {
				errs = append(errs, fmt.Errorf("删除国内分流 %s: %w", dest, derr))
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
