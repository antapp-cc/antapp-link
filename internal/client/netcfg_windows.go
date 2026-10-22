//go:build windows

package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// CREATE_NO_WINDOW：托盘程序调子进程时不能闪黑框
const createNoWindow = 0x08000000

func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// runCommand 执行外部命令。如今只剩 schtasks（开机自启）这类低频路径。
func runCommand(c Command) error {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.SysProcAttr = hiddenProcAttr()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", c.String(), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// 网络接管的 Windows 实现：地址、MTU、跃点、路由、DNS、NRPT 全部走
// 进程内系统调用（winipcfg / iphlpapi / dnsapi / 注册表），
//
// 历史教训都还适用，只是换了更可靠的执行手段：
//   - /1 路由必须显式绑定隧道网卡 → AddRoute 挂在隧道 LUID 上，结构上不可能挂错
//   - 绕行路由必须先于接管路由，且云服直连时不加 → 见 routePlan
//   - 还原必须先解 DNS 再拆路由，尽力而为不中断 → 见 Restore

// refreshSplitRoutesHook 由平台侧注入：按最新网段表热重铺国内分流路由。
var refreshSplitRoutesHook func(gw string, ifIndex int, table []string)

func init() {
	refreshSplitRoutesHook = refreshSplitRoutesFor
}

// liveSplitOwnership 是本进程当前铺着哪些国内分流路由的实时记录。
//
// 热更新（routeTableRefresher）只拿得到网关/索引/新网段表，拿不到 App 的
// Snapshot，靠它知道上一版装了哪些前缀、该撤哪些。崩溃后这份记录丢失，只剩
// state.json 里 Snapshot.Split 那一份（那是接管当时铺的那一版）。
var liveSplitOwnership atomic.Pointer[splitOwnership]

func setLiveSplitOwnership(o *splitOwnership)    { liveSplitOwnership.Store(o) }
func currentLiveSplitOwnership() *splitOwnership { return liveSplitOwnership.Load() }
func clearLiveSplitOwnership()                   { liveSplitOwnership.Store(nil) }

// refreshSplitRoutesFor 按给定网段表重铺国内直连路由（原生，毫秒级）。
// 由 routeTableRefresher 在拉到新表后调用。
func refreshSplitRoutesFor(gw string, ifIndex int, table []string) {
	gwAddr, err := netip.ParseAddr(gw)
	if err != nil || ifIndex <= 0 {
		return
	}
	a, ok, err := adapterByIndex(uint32(ifIndex))
	if err != nil || !ok {
		return
	}
	// 先撤自己上一版的路由：有 live 记录就只删记录里的；没有（进程刚起、或
	// 接管时没铺成分流）才退回形态扫描。
	if own := currentLiveSplitOwnership(); own != nil {
		if removed, serr := sweepOwnedSplitRoutes(own); serr != nil {
			logf("国内分流热更新：旧路由有 %d 条没删掉（%v），继续铺新表", removed, serr)
		}
	} else if removed, serr := sweepSplitRoutes(gwAddr); serr != nil {
		logf("国内分流热更新：按形态清理旧路由时 %d 条失败: %v", removed, serr)
	}

	installed := make([]string, 0, len(table))
	failed := 0
	for _, p := range table {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			failed++
			continue
		}
		if err := a.LUID.AddRoute(prefix.Masked(), gwAddr, splitRouteMetric); err != nil &&
			!errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
			failed++
			continue
		}
		installed = append(installed, prefix.Masked().String())
	}
	setLiveSplitOwnership(newSplitOwnership(a.key(), gwAddr, installed))
	if failed > 0 {
		logf("国内分流重铺：%d 条失败（这些网段暂走隧道，不影响可用）", failed)
	}
}

// runCommand / hiddenProcAttr 仅供 schtasks（开机自启）这类低频操作使用。

// ---------- 现场抓取 ----------

// Capture 抓取接管前的网络现场，全程只读。
//
// 只抓承载默认路由的那张网卡：其它网卡（拔掉的以太网、Hyper-V 虚拟网卡、
// 没连的无线）改不了也不该改 —— netsh 时代在断开的网卡上设 DNS 会直接失败
// 并中止整个接管流程；现在原生调用虽不会失败，但改它们依旧没有意义。
//
// cfg 同时提供云服地址（算绕行路由用）和隧道网关（要一并记进快照，供自愈用）。
func Capture(cfg NetConfig) (Snapshot, error) {
	if err := validateIPv4Only(cfg); err != nil {
		return Snapshot{}, err
	}
	srv, err := resolveIPv4(cfg.ServerIP)
	if err != nil {
		return Snapshot{}, err
	}
	// 排除隧道接口：程序被强杀后适配器可能还带着 /1 路由残留，
	// 不排除的话会把隧道当成默认出口（前缀 0.0.0.0/1 是包含 0.0.0.0 的）。
	var tunLUID winipcfg.LUID
	if tun, ok, _ := adapterByName(AdapterName); ok {
		tunLUID = tun.LUID
	}
	gw, gwIfIndex, err := bestDefaultRoute(tunLUID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("查默认路由: %w", err)
	}
	if gw.IsUnspecified() {
		return Snapshot{}, errors.New("默认网关为空（可能没联网），无法继续")
	}

	// 到云服的下一跳：0.0.0.0 或就是云服本身 → 云服在直连网段，
	// 不加绕行路由（现成直连路由更具体，硬加 /32 反而覆盖它，隧道自噬）。
	hop, _, err := bestRouteTo(srv, tunLUID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("查到 %s 的路由: %w", srv, err)
	}
	serverNextHop := ""
	if !hop.IsUnspecified() && hop.Unmap() != srv {
		serverNextHop = hop.Unmap().String()
	}

	def, ok, err := adapterByIndex(gwIfIndex)
	if err != nil || !ok {
		return Snapshot{}, fmt.Errorf("找不到承载默认路由的网卡（ifIndex %d）", gwIfIndex)
	}

	return newSnapshot(gw.Unmap().String(), int(gwIfIndex), serverNextHop, srv.String(),
		[]IfaceDNS{captureIfaceDNS(def)}, cfg), nil
}

// captureIfaceDNS 抓一张网卡的原 DNS，连同稳定身份和来源一起记下来。
func captureIfaceDNS(a adapterInfo) IfaceDNS {
	rec := IfaceDNS{Name: a.Name, Index: int(a.Index), GUID: a.GUID}
	for _, d := range adapterDNS(a.LUID) {
		rec.DNS = append(rec.DNS, d.String())
	}
	if auto, ok := dnsWasAuto(a.LUID); ok {
		rec.DNSAuto = &auto
	}
	return rec
}

// resolveIPv4 把 host:port 里拆出来的主机部分变成 IPv4 地址。
// 连接码里通常是 IP，但写域名的也要能用。
func resolveIPv4(host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil && a.Is4() {
		return a, nil
	}
	// 必须带超时：net.LookupHost 走的是无超时的默认解析器，DNS 卡住时会把 Capture
	// 整段拖住 —— 而 Capture 在 Connect 的关键路径上，退出流程的 shutdownWait 正是
	// 按这条路径的最坏耗时定的。
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("解析 %s 失败: %w", host, err)
	}
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			return ip, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s 没有可用的 IPv4 地址", host)
}

// ---------- 配置写入 ----------

// tunnelLUID 按固定名找隧道网卡。找不到说明适配器没建起来或已被系统回收。
func tunnelLUID() (winipcfg.LUID, error) {
	a, ok, err := adapterByName(AdapterName)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("找不到虚拟网卡 %q", AdapterName)
	}
	return a.LUID, nil
}

// configureAdapterLUID 只配地址 / MTU / 接口跃点，不碰路由和 DNS。
// 联调模式（-no-netcfg）也走这里。
func configureAdapterLUID(cfg NetConfig) error {
	luid, err := tunnelLUID()
	if err != nil {
		return err
	}
	tunnelIP, err := netip.ParseAddr(cfg.TunnelIP)
	if err != nil {
		return fmt.Errorf("隧道地址 %q 不合法: %w", cfg.TunnelIP, err)
	}
	pfx := netip.PrefixFrom(tunnelIP, cfg.Prefix)
	if err := luid.SetIPAddresses([]netip.Prefix{pfx}); err != nil {
		// 上次没干净退场时，10.10.0.2 可能还残留在某张**断开状态**的网卡上
		// （Wi-Fi 掉线时的 WLAN 虚拟面等）。WireGuard 同款处理：先删残留再重设。
		if errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
			cleanupStaleTunnelAddress(pfx)
			err = luid.SetIPAddresses([]netip.Prefix{pfx})
		}
		if err != nil {
			return fmt.Errorf("配置隧道地址 %s/%d: %w", cfg.TunnelIP, cfg.Prefix, err)
		}
	}
	row, err := luid.IPInterface(windows.AF_INET)
	if err != nil {
		return fmt.Errorf("读隧道网卡接口参数: %w", err)
	}
	row.NLMTU = uint32(cfg.MTU)
	// 接口跃点压到最低：Windows 按接口跃点挑 DNS 服务器，不压低的话
	// 解析还可能落到本地网卡上。
	row.Metric = 1
	row.UseAutomaticMetric = false
	// WireGuard 同款三件套：跳过 DAD（地址立即可用，否则连接后头几秒
	// 10.10.0.2 处于探测态，偶发自检失败）；拒绝 RA/DHCPv6 管理隧道网卡。
	row.DadTransmits = 0
	row.RouterDiscoveryBehavior = winipcfg.RouterDiscoveryDisabled
	row.ManagedAddressConfigurationSupported = false
	row.OtherStatefulConfigurationSupported = false
	if err := row.Set(); err != nil {
		return fmt.Errorf("设置隧道网卡 MTU/跃点: %w", err)
	}
	// DNS 设在隧道网卡自己身上（OpenVPN 同款做法，它是把推送的 DNS 挂在
	// 自己的虚拟网卡上）。关键在排序：本网卡接口跃点已是 1，它的解析器
	// 排在系统所有 DNS 之前 —— 物理网卡上删不掉的路由器 IPv6 DNS（RA
	// 下发、Windows 优先用 v6 解析器）会被排到后面，查询不再漏给它。
	// 那台 DNS 对一批国外域名（socialchain.app 等）直接超时挂死，
	// Pi Desktop 的内嵌页面就是被它拖白的；OpenVPN 能开、我们不能，
	// 差异就在这一层。
	if err := setAdapterDNS(luid, cfg.DNS); err != nil {
		return fmt.Errorf("设置隧道网卡 DNS: %w", err)
	}
	// 读回校验：API 返回成功不代表写进去了。实测原生写入有效（探针验证），
	// 这里留一道保险：万一某台机器没写上，重试几次；仍失败就记日志
	// （解析会漏回物理网卡的 DNS，功能降级但不阻断连接）。
	for retry := 0; retry < 3 && len(adapterDNS(luid)) == 0; retry++ {
		time.Sleep(300 * time.Millisecond)
		_ = setAdapterDNS(luid, cfg.DNS)
	}
	if len(adapterDNS(luid)) == 0 {
		logf("警告：隧道网卡的 DNS 未能写入（解析将回落到物理网卡的 DNS）")
	}
	return nil
}

// cleanupStaleTunnelAddress 删除所有**断开状态**网卡上残留的隧道地址。
// 地址残留在断开网卡上时，本网卡设置同名地址会报 OBJECT_ALREADY_EXISTS。
func cleanupStaleTunnelAddress(pfx netip.Prefix) {
	aas, err := adapters()
	if err != nil {
		return
	}
	for _, a := range aas {
		if a.Up {
			continue
		}
		for _, ua := range adapterUnicastIPs(a.LUID) {
			if ua.Addr() == pfx.Addr() {
				logf("清理网卡 %s 上残留的隧道地址 %s", a.Name, ua)
				_ = a.LUID.DeleteIPAddress(ua)
			}
		}
	}
}

// ConfigureAdapter 只把隧道网卡本身配起来。联调模式用：网卡上要有
// 10.10.0.2 内核才认得出隧道里来的包，但路由和 DNS 一概不动。
func ConfigureAdapter(cfg NetConfig) error {
	return configureAdapterLUID(cfg)
}

// Apply 分阶段接管网络：先配隧道网卡，再挂绕行路由、接管路由、DNS、NRPT，
// 最后铺国内直连分流（最慢的一步放最后，前面的失败不至于白等）。
// 路由计划由 routePlan（纯函数）产出，绕行先于接管的顺序在那里保证。
//
// 用指针接收者是为了把「实际装了哪些分流路由」写回快照：还原时只清自己装的。
//
// 系统刚启动时网络栈可能尚未就绪（表现为 ERROR_NOT_FOUND，WireGuard 同款
// 处理），此时退避重试；其它错误直接报给上层。
func (s *Snapshot) Apply(cfg NetConfig) error {
	evaluatePitfalls(cfg.ServerIP)
	for attempt := 0; ; attempt++ {
		err := s.applyOnce(cfg)
		if err == nil {
			return nil
		}
		if attempt >= 4 || !errors.Is(err, windows.ERROR_NOT_FOUND) {
			return err
		}
		logf("网络栈尚未就绪（%v），1 秒后重试", err)
		time.Sleep(time.Second)
	}
}

func (s *Snapshot) applyOnce(cfg NetConfig) error {
	if err := validateIPv4Only(cfg); err != nil {
		return err
	}
	if err := configureAdapterLUID(cfg); err != nil {
		return err
	}
	tunLUID, err := tunnelLUID()
	if err != nil {
		return err
	}

	var def adapterInfo
	haveDef := false
	if len(s.Interfaces) > 0 {
		def, haveDef, err = adapterForIface(s.Interfaces[0])
		if err != nil {
			return err
		}
		if !haveDef {
			// 快照里记着 GUID 的网卡不在了：别退回索引硬找一个替身，那可能
			// 把路由和 DNS 写到另一张网卡上。让连接失败，比改错网卡好。
			return fmt.Errorf("找不到快照记录的网卡 %q（可能已被禁用或移除）", s.Interfaces[0].Name)
		}
	} else if s.DefaultIfIndex != 0 {
		def, haveDef, err = adapterByIndex(uint32(s.DefaultIfIndex))
		if err != nil {
			return err
		}
	}

	// 1) 绕行 + 接管路由（顺序由 routePlan 保证）
	for _, op := range routePlan(*s, cfg) {
		luid := def.LUID
		if op.onTunnel {
			luid = tunLUID
		}
		if haveDef || op.onTunnel {
			if err := luid.AddRoute(op.dest, op.nextHop, op.metric); err != nil &&
				!errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				return fmt.Errorf("加路由 %s via %s: %w", op.dest, op.nextHop, err)
			}
		}
	}

	// 2) DNS 改写到隧道地址 + 刷新缓存
	for _, iface := range s.Interfaces {
		a, ok, err := adapterForIface(iface)
		if err != nil || !ok {
			continue
		}
		if err := setAdapterDNS(a.LUID, cfg.DNS); err != nil {
			return fmt.Errorf("改写网卡 %s 的 DNS: %w", iface.Name, err)
		}
	}
	flushResolverCache()

	// 3) NRPT：把被污染的 Pi 域名强制指到隧道网关解析
	if cfg.Gateway != "" {
		if err := nrptApply(cfg.Gateway); err != nil {
			// 失败不阻断：顶多 Pi 域名解析回退到污染状态，其余功能不受影响
			logf("NRPT 规则写入失败（Pi 域名解析可能被污染）: %v", err)
		}
	}

	// 4) 智能分流：国内网段走原网关直连，其余才进隧道。
	//    好处是服务端出口出问题时国内网络照常可用，不会「连上就没有网」。
	gwAddr, gwErr := netip.ParseAddr(s.DefaultGateway)
	if gwErr == nil && haveDef {
		added, failed := 0, 0
		var installed []string
		for _, p := range ActiveCNRoutes() {
			prefix, err := netip.ParsePrefix(p)
			if err != nil {
				continue
			}
			if err := def.LUID.AddRoute(prefix.Masked(), gwAddr, splitRouteMetric); err != nil &&
				!errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				failed++
				continue
			}
			// 已存在的那条也记成自己的：形状（本接口 + 本网关 + metric 5 +
			// 同一前缀）与本程序铺的完全一致，不记的话还原时会留在表里。
			installed = append(installed, prefix.Masked().String())
			added++
		}
		own := newSplitOwnership(def.key(), gwAddr, installed)
		s.Split = own
		setLiveSplitOwnership(own)
		switch {
		case failed == 0:
			logf("智能分流已启用：国内流量直连，其余走隧道")
		case added > 0:
			logf("智能分流部分生效：%d 条直连路由没写进去（这些网段会走隧道，不影响可用）", failed)
		default:
			logf("智能分流未能写入（国内流量将全部走隧道，不影响可用）")
		}
	}
	return nil
}

// setAdapterDNS 把一张网卡的 IPv4 DNS 指到给定的服务器（原生 dnsapi 调用）。
func setAdapterDNS(l winipcfg.LUID, servers []string) error {
	addrs := make([]netip.Addr, 0, len(servers))
	for _, s := range servers {
		if a, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil && a.Is4() {
			addrs = append(addrs, a)
		}
	}
	return l.SetDNS(windows.AF_INET, addrs, nil)
}

// applyIfaceDNS 按快照记录的方式还原一张网卡的 DNS。
// 自动来源或来源未知 → 清空静态设置让 DHCP 重新下发；只有明确是静态配置时
// 才写回原地址（见 IfaceDNS.DNSAuto）。
func applyIfaceDNS(l winipcfg.LUID, iface IfaceDNS) error {
	if iface.restoreAsAuto() {
		return l.FlushDNS(windows.AF_INET)
	}
	return setAdapterDNS(l, iface.DNS)
}

// restoreIfacesDNS 按快照逐张网卡还原 DNS，返回每张网卡的错误。
// 网卡已不在（拔掉的 USB 网卡等）时跳过：没有可还原的对象，也不该报失败。
func restoreIfacesDNS(ifaces []IfaceDNS) []error {
	var errs []error
	for _, iface := range ifaces {
		a, ok, err := adapterForIface(iface)
		if err != nil {
			errs = append(errs, fmt.Errorf("查快照里的网卡 %s: %w", iface.Name, err))
			continue
		}
		if !ok {
			continue
		}
		if err := applyIfaceDNS(a.LUID, iface); err != nil {
			errs = append(errs, fmt.Errorf("还原网卡 %s 的 DNS: %w", iface.Name, err))
		}
	}
	return errs
}

// defaultAdapter 找快照里承载默认路由的那张网卡（GUID 优先，旧快照退回索引）。
func defaultAdapter(s Snapshot) (adapterInfo, bool) {
	if len(s.Interfaces) > 0 {
		if a, ok, err := adapterForIface(s.Interfaces[0]); err == nil && ok {
			return a, true
		}
		return adapterInfo{}, false
	}
	if s.DefaultIfIndex != 0 {
		if a, ok, err := adapterByIndex(uint32(s.DefaultIfIndex)); err == nil && ok {
			return a, true
		}
	}
	return adapterInfo{}, false
}

// Restore 还原全部配置：某一步失败不阻断后面的还原，但每一步的结果都要检查。
//
// 以前这里对路由/NRPT 删除一律 `_ =`，然后调用方直接报「已断开」：其实路由
// 可能还在、NRPT 规则可能还指着已拆掉的隧道网关，用户下次断网时无从判断。
// 现在把所有失败汇总返回，调用方据此保留 state.json 重试。
//
// 此时隧道多半已经拆掉（断开先关适配器），/1 路由和隧道地址会随适配器一起
// 消失；这里只负责还 DNS、撤 NRPT、扫掉国内分流和绕行路由的残留。
func (s Snapshot) Restore(cfg NetConfig) error {
	var errs []error
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// NRPT 最先撤：规则指向隧道网关，万一隧道已拆，留着它会让 Pi 域名解析死掉。
	fail(nrptRemove())

	// DNS 还原：原值来自快照。自动来源/来源未知的清空即可回到 DHCP 自动获取
	// —— FlushDNS 是「清空静态设置」，不会塞编造地址。
	errs = append(errs, restoreIfacesDNS(s.Interfaces)...)

	// 隧道网关与云服地址优先取快照：自愈路径上 cfg 是零值（那时连接码还没加载），
	// 只有快照里记着本次接管用的是什么。
	tunnelGw := s.TunnelGateway
	if tunnelGw == "" {
		tunnelGw = cfg.Gateway
	}
	serverIP := s.ServerIP
	if serverIP == "" {
		serverIP = cfg.ServerIP
	}

	// 隧道网卡上的接管痕迹（/1 路由、隧道 DNS）不依赖快照，所以放在 haveDef 之外：
	// 快照损坏时拿不到默认网卡索引，而这两样恰恰是「全部流量灌进一张没人读的
	// 网卡」的元凶，必须无条件撤掉。
	if tun, ok, _ := adapterByName(AdapterName); ok {
		if gw, err := netip.ParseAddr(tunnelGw); err == nil {
			fail(deleteRouteIgnoringMissing(tun.LUID, netip.MustParsePrefix("0.0.0.0/1"), gw))
			fail(deleteRouteIgnoringMissing(tun.LUID, netip.MustParsePrefix("128.0.0.0/1"), gw))
		} else {
			// 连网关都拿不到（快照损坏，或来自还没记录它的旧版本）：按前缀整批扫，
			// 隧道网卡上的 /1 只可能是我们铺的。
			if _, err := sweepTunnelHalfRoutes(tun.LUID); err != nil {
				fail(fmt.Errorf("撤隧道接管路由: %w", err))
			}
		}
		// 它身上挂的隧道 DNS 也一并撤掉，别留一个指向死网关的解析器
		if err := tun.LUID.FlushDNS(windows.AF_INET); err != nil && !routeMissing(err) {
			fail(fmt.Errorf("清理隧道网卡 DNS: %w", err))
		}
	}

	var def adapterInfo
	haveDef := false
	var err error
	if len(s.Interfaces) > 0 {
		def, haveDef, err = adapterForIface(s.Interfaces[0])
	} else if s.DefaultIfIndex != 0 {
		def, haveDef, err = adapterByIndex(uint32(s.DefaultIfIndex))
	}
	if err != nil {
		fail(err)
	}

	if haveDef {
		// 国内分流残留：有所有权记录时只清自己装的那一组；旧快照没记，只能退回
		// 「下一跳 + metric」的形态扫描（会碰到同形态的用户路由，见 sweepSplitRoutes）。
		owns := make([]*splitOwnership, 0, 2)
		if s.Split != nil {
			owns = append(owns, s.Split)
		}
		if live := currentLiveSplitOwnership(); live != nil && live != s.Split {
			owns = append(owns, live)
		}
		if len(owns) > 0 {
			for _, own := range owns {
				removed, serr := sweepOwnedSplitRoutes(own)
				if serr != nil {
					fail(fmt.Errorf("清理国内分流: %w", serr))
				} else if removed > 0 {
					logf("已清理国内分流残留 %d 条", removed)
				}
			}
		} else if gw, gerr := netip.ParseAddr(s.DefaultGateway); gerr == nil {
			removed, serr := sweepSplitRoutes(gw)
			if serr != nil {
				fail(fmt.Errorf("清理国内分流（旧快照无所有权记录）: %w", serr))
			} else if removed > 0 {
				logf("已清理国内分流残留 %d 条（按形态扫描）", removed)
			}
		}

		// 替旧版本擦屁股：早期版本改写过 0.0.0.0/0。只匹配下一跳等于隧道网关的，
		// 不会误伤用户的默认路由。
		if gw, gerr := netip.ParseAddr(tunnelGw); gerr == nil {
			fail(deleteRouteIgnoringMissing(def.LUID, netip.MustParsePrefix("0.0.0.0/0"), gw))
		}

		// 绕行路由：快照里记着下一跳才加过它，按同样的下一跳精确删除。
		if s.ServerNextHop != "" && serverIP != "" {
			if srv, gerr := netip.ParseAddr(serverIP); gerr == nil {
				if hop, herr := netip.ParseAddr(s.ServerNextHop); herr == nil {
					fail(deleteRouteIgnoringMissing(def.LUID, netip.PrefixFrom(srv, 32), hop))
				}
			}
		}

		// 旧版「给所有网卡设 DNS」留下的隧道网关地址，逐网卡扫描清除。
		// 只清 IPv4 的设置，v6 侧不受影响。
		//
		// 判据必须是**隧道网关**：用物理默认网关当判据会误清用户手工配置的 DNS
		// （手工 DNS 常常正好填的是路由器地址，也就是原默认网关）。
		if gw, gerr := netip.ParseAddr(tunnelGw); gerr == nil {
			aas, aerr := adapters()
			if aerr != nil {
				fail(fmt.Errorf("枚举网卡清理残留 DNS: %w", aerr))
			}
			for _, a := range aas {
				isDefault := a.Index == uint32(s.DefaultIfIndex)
				if len(s.Interfaces) > 0 {
					isDefault = s.Interfaces[0].ref().sameAdapter(a.key())
				}
				if isDefault {
					continue
				}
				for _, d := range adapterDNS(a.LUID) {
					if d == gw {
						if ferr := a.LUID.FlushDNS(windows.AF_INET); ferr != nil && !routeMissing(ferr) {
							fail(fmt.Errorf("清理网卡 %s 的残留 DNS: %w", a.Name, ferr))
						}
						break
					}
				}
			}
		}
	}

	// 进程内的所有权记录随这次还原一起失效：不管快照是否完好，接下来要么重新
	// 接管（Apply 会重设），要么就在没接管的干净状态。
	clearLiveSplitOwnership()

	flushResolverCache()
	if len(errs) > 0 {
		joined := errors.Join(errs...)
		// 报数不报「成功」：调用方看到 error 会保留 state.json，下次断开/启动重试。
		logf("网络还原有 %d 处未完成（状态文件保留，稍后会重试）: %v", len(errs), joined)
		return joined
	}
	return nil
}

// ---------- NRPT（注册表直写，Dnscache 动态生效）----------

// nrptApply 写入 Pi 域名的解析规则。规则本体是 DnsPolicyConfig 下一个固定
// GUID 的注册表键（实测 Dnscache 无需重启即时生效），重写即覆盖，天然幂等。
// 旧版用 PowerShell WMI 创建的规则 GUID 随机，由 nrptRemove 按标记扫除。
func nrptApply(gateway string) error {
	keyPath := nrptKeyPath + `\` + nrptRuleGUID
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, keyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 NRPT 注册表键: %w", err)
	}
	defer k.Close()
	if err := k.SetDWordValue("Version", 2); err != nil {
		return err
	}
	if err := k.SetStringsValue("Name", []string{nrptNamespace}); err != nil {
		return err
	}
	if err := k.SetStringValue("GenericDNSServers", gateway); err != nil {
		return err
	}
	if err := k.SetDWordValue("ConfigOptions", 8); err != nil {
		return err
	}
	if err := k.SetStringValue("DisplayName", nrptRuleComment); err != nil {
		return err
	}
	return k.SetStringValue("Comment", nrptRuleComment)
}

// regMissing 报告注册表对象本来就不在（重复删除必须幂等）。
func regMissing(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_NOT_FOUND)
}

// nrptRemove 删掉我们名下的全部 NRPT 规则：固定 GUID 那条 + 任何 Comment
// 等于标记的旧规则（旧版 PowerShell 创建的 GUID 是随机的，只能按标记扫）。
// 键不存在时不算失败，重复执行无害；其它删除失败会汇总返回，不能吞掉。
func nrptRemove() error {
	var errs []error
	delKey := func(path string) {
		if err := registry.DeleteKey(registry.LOCAL_MACHINE, path); err != nil && !regMissing(err) {
			errs = append(errs, fmt.Errorf("删除 NRPT 规则 %s: %w", path, err))
		}
	}
	delKey(nrptKeyPath + `\` + nrptRuleGUID)

	root, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptKeyPath,
		registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		if !regMissing(err) {
			errs = append(errs, fmt.Errorf("打开 NRPT 根键: %w", err))
		}
		return errors.Join(errs...)
	}
	names, err := root.ReadSubKeyNames(-1)
	root.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("枚举 NRPT 规则: %w", err))
		return errors.Join(errs...)
	}
	for _, name := range names {
		path := nrptKeyPath + `\` + name
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			if !regMissing(err) {
				errs = append(errs, fmt.Errorf("打开 NRPT 规则 %s: %w", path, err))
			}
			continue
		}
		comment, _, _ := k.GetStringValue("Comment")
		k.Close()
		if comment != nrptRuleComment {
			continue
		}
		delKey(path)
	}
	return errors.Join(errs...)
}
