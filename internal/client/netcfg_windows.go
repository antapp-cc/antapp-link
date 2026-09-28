//go:build windows

package client

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
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

// runCommand / hiddenProcAttr 仅供 schtasks（开机自启）这类低频操作使用。

// ---------- 现场抓取 ----------

// Capture 抓取接管前的网络现场，全程只读。
//
// 只抓承载默认路由的那张网卡：其它网卡（拔掉的以太网、Hyper-V 虚拟网卡、
// 没连的无线）改不了也不该改 —— netsh 时代在断开的网卡上设 DNS 会直接失败
// 并中止整个接管流程；现在原生调用虽不会失败，但改它们依旧没有意义。
func Capture(serverIP string) (Snapshot, error) {
	srv, err := resolveIPv4(serverIP)
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

	var dnsStrings []string
	for _, a := range adapterDNS(def.LUID) {
		dnsStrings = append(dnsStrings, a.String())
	}

	return newSnapshot(gw.Unmap().String(), int(gwIfIndex), serverNextHop,
		[]IfaceDNS{{Name: def.Name, Index: int(def.Index), DNS: dnsStrings}}), nil
}

// resolveIPv4 把 host:port 里拆出来的主机部分变成 IPv4 地址。
// 连接码里通常是 IP，但写域名的也要能用。
func resolveIPv4(host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil && a.Is4() {
		return a, nil
	}
	addrs, err := net.LookupHost(host)
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
// 系统刚启动时网络栈可能尚未就绪（表现为 ERROR_NOT_FOUND，WireGuard 同款
// 处理），此时退避重试；其它错误直接报给上层。
func (s Snapshot) Apply(cfg NetConfig) error {
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

func (s Snapshot) applyOnce(cfg NetConfig) error {
	if err := configureAdapterLUID(cfg); err != nil {
		return err
	}
	tunLUID, err := tunnelLUID()
	if err != nil {
		return err
	}

	var def adapterInfo
	haveDef := false
	if s.DefaultIfIndex != 0 {
		def, haveDef, err = adapterByIndex(uint32(s.DefaultIfIndex))
		if err != nil {
			return err
		}
	}

	// 1) 绕行 + 接管路由（顺序由 routePlan 保证）
	for _, op := range routePlan(s, cfg) {
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
		a, ok, err := adapterByName(iface.Name)
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
		for _, p := range CNRoutes() {
			prefix, err := netip.ParsePrefix(p)
			if err != nil {
				continue
			}
			if err := def.LUID.AddRoute(prefix.Masked(), gwAddr, splitRouteMetric); err != nil &&
				!errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				failed++
				continue
			}
			added++
		}
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

// Restore 尽力还原全部配置：某一步失败不该阻断后面的还原。
//
// 此时隧道多半已经拆掉（断开先关适配器），/1 路由和隧道地址会随适配器一起
// 消失；这里只负责还 DNS、撤 NRPT、扫掉国内分流和绕行路由的残留。
func (s Snapshot) Restore(cfg NetConfig) error {
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// NRPT 最先撤：规则指向隧道网关，万一隧道已拆，留着它会让 Pi 域名解析死掉。
	nrptRemove()

	// DNS 还原：原值来自快照。原来就没有静态 DNS 的网卡清空即可回到自动获取
	// —— FlushDNS 是「清空设置」，等价于旧版 netsh source=dhcp，不会塞编造地址。
	for _, iface := range s.Interfaces {
		a, ok, err := adapterByName(iface.Name)
		if err != nil {
			fail(err)
			continue
		}
		if !ok {
			continue // 网卡已不在（如拔掉的 USB 网卡），没什么可还原的
		}
		if err := setAdapterDNS(a.LUID, iface.DNS); err != nil {
			fail(fmt.Errorf("还原网卡 %s 的 DNS: %w", iface.Name, err))
		}
	}

	def, haveDef, err := adapterByIndex(uint32(s.DefaultIfIndex))
	if err != nil {
		fail(err)
	}

	if haveDef {
		// 国内分流残留：按「下一跳 = 原默认网关 且 metric = 5」整批扫除，
		// 网段表更新过也能清干净上一版留下的路由。
		if gw, err := netip.ParseAddr(s.DefaultGateway); err == nil {
			sweepSplitRoutes(gw)
		}

		// 替旧版本擦屁股：早期版本改写过 0.0.0.0/0。只匹配下一跳等于隧道网关的，
		// 不会误伤用户的默认路由。
		if gw, err := netip.ParseAddr(cfg.Gateway); err == nil {
			_ = def.LUID.DeleteRoute(netip.MustParsePrefix("0.0.0.0/0"), gw)
		}

		// 绕行路由：快照里记着下一跳才加过它，按同样的下一跳精确删除。
		if s.ServerNextHop != "" && cfg.ServerIP != "" {
			if srv, err := netip.ParseAddr(cfg.ServerIP); err == nil {
				if hop, err := netip.ParseAddr(s.ServerNextHop); err == nil {
					_ = def.LUID.DeleteRoute(netip.PrefixFrom(srv, 32), hop)
				}
			}
		}

		// 隧道网卡还在（异常路径下没关干净）时兜底撤 /1 路由；正常断开时
		// 适配器已随 dev.Close 消失，这些路由自动没了。
		if tun, ok, _ := adapterByName(AdapterName); ok {
			if gw, err := netip.ParseAddr(cfg.Gateway); err == nil {
				_ = tun.LUID.DeleteRoute(netip.MustParsePrefix("0.0.0.0/1"), gw)
				_ = tun.LUID.DeleteRoute(netip.MustParsePrefix("128.0.0.0/1"), gw)
			}
			// 它身上挂的隧道 DNS 也一并撤掉，别留一个指向死网关的解析器
			_ = tun.LUID.FlushDNS(windows.AF_INET)
		}

		// 旧版「给所有网卡设 DNS」留下的隧道网关地址，逐网卡扫描清除。
		// 只清 IPv4 的设置，v6 侧不受影响。
		if gw, err := netip.ParseAddr(s.DefaultGateway); err == nil {
			aas, err := adapters()
			if err == nil {
				for _, a := range aas {
					if a.Index == uint32(s.DefaultIfIndex) {
						continue
					}
					for _, d := range adapterDNS(a.LUID) {
						if d == gw {
							if err := a.LUID.FlushDNS(windows.AF_INET); err != nil {
								logf("清理网卡 %s 的残留 DNS 失败: %v", a.Name, err)
							}
							break
						}
					}
				}
			}
		}
	}

	flushResolverCache()
	return firstErr
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

// nrptRemove 删掉我们名下的全部 NRPT 规则：固定 GUID 那条 + 任何 Comment
// 等于标记的旧规则（旧版 PowerShell 创建的 GUID 是随机的，只能按标记扫）。
// 键不存在时静默返回，重复执行无害。
func nrptRemove() {
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, nrptKeyPath+`\`+nrptRuleGUID)

	root, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptKeyPath,
		registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		return
	}
	names, err := root.ReadSubKeyNames(-1)
	root.Close()
	if err != nil {
		return
	}
	for _, name := range names {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptKeyPath+`\`+name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		comment, _, _ := k.GetStringValue("Comment")
		k.Close()
		if comment != nrptRuleComment {
			continue
		}
		_ = registry.DeleteKey(registry.LOCAL_MACHINE, nrptKeyPath+`\`+name)
	}
}
