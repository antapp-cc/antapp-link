package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// Command 是一条要执行的外部命令。如今只剩 schtasks（开机自启）这类
// 低频路径还在用，网络接管已全部原生化。
type Command struct {
	Name string
	Args []string
}

// logf 是包级日志钩子。
//
// 这一层不持有 logger（logger 在 App 里），但「分流到底写进去多少条路由」
// 是排障时第一个要看的东西 —— 没有它，路由没生效就只能靠猜。
// 默认空实现，NewApp 时接上真实 logger；测试环境保持安静。
var logf = func(format string, args ...any) {}

func (c Command) String() string {
	parts := append([]string{c.Name}, c.Args...)
	for i, p := range parts {
		if strings.ContainsAny(p, " \t") {
			parts[i] = `"` + p + `"`
		}
	}
	return strings.Join(parts, " ")
}

// IfaceDNS 记下一张网卡原来的 DNS，断开时要还回去。
//
// GUID 是网卡的稳定身份（系统按它记保存的配置），友好名用户能改、接口索引会被
// 系统复用，所以识别网卡时 GUID 优先。旧快照没有 GUID，只能退回索引/名字。
type IfaceDNS struct {
	Name  string   `json:"name"`
	Index int      `json:"index"`
	GUID  string   `json:"guid,omitempty"`
	DNS   []string `json:"dns"`

	// DNSAuto 记下这些 DNS 是 DHCP 自动下发的还是用户静态配置的。
	// nil 表示旧快照没记来源（或原生探测失败），按自动处理。
	//
	// 还原成同样的静态地址只在同一个网络里成立：换了 Wi-Fi/网线后，旧地址
	// 多半已经不可达。自动来源必须还原成自动（清空静态设置让 DHCP 重新下发）；
	// 来源未知时同样按自动处理 —— 写死旧地址的结果是解析全超时，而清空最多
	// 让用户重配一次。
	DNSAuto *bool `json:"dns_auto,omitempty"`
}

// restoreAsAuto 报告还原这张网卡时是否清空静态 DNS 设置、回到 DHCP 自动获取。
func (i IfaceDNS) restoreAsAuto() bool { return i.DNSAuto == nil || *i.DNSAuto }

// adapterRef / adapterKey 是快照侧与系统侧的网卡标识。
//
// 分开两个类型是为了让「按什么顺序认网卡」成为可单测的纯逻辑：GUID 优先，
// 旧快照没有 GUID 时才退回索引、再退回友好名。
type adapterRef struct {
	GUID  string
	Index int
	Name  string
}

type adapterKey struct {
	GUID  string
	Index uint32
	Name  string
}

func (i IfaceDNS) ref() adapterRef {
	return adapterRef{GUID: i.GUID, Index: i.Index, Name: i.Name}
}

// sameAdapter 判断快照条目与当前网卡是不是同一张。
// watcher 复用 DNS 原值前必须确认，否则会把上一张网卡的配置记到新网卡名下。
func (r adapterRef) sameAdapter(k adapterKey) bool {
	if r.GUID != "" && k.GUID != "" {
		return r.GUID == k.GUID
	}
	return r.Index > 0 && int(k.Index) == r.Index
}

// pickAdapter 在当前网卡里找快照记的那一张。
//
// 有 GUID 却找不到时不退回索引：索引会被系统复用，退回去可能改到另一张网卡的
// DNS。那张网卡真没了就什么都不该做。只有旧快照（无 GUID）才按索引、再按名字找。
func pickAdapter(ref adapterRef, keys []adapterKey) (int, bool) {
	if ref.GUID != "" {
		for i, k := range keys {
			if k.GUID == ref.GUID {
				return i, true
			}
		}
		return 0, false
	}
	if ref.Index > 0 {
		for i, k := range keys {
			if int(k.Index) == ref.Index {
				return i, true
			}
		}
	}
	if ref.Name != "" {
		for i, k := range keys {
			if k.Name == ref.Name {
				return i, true
			}
		}
	}
	return 0, false
}

// Snapshot 是接管网络之前的现场。它必须能完整还原 —— 还原不了就是把用户搞断网。
type Snapshot struct {
	DefaultGateway string     `json:"default_gateway"`
	DefaultIfIndex int        `json:"default_if_index"`
	Interfaces     []IfaceDNS `json:"interfaces"`
	CapturedAt     string     `json:"captured_at"`

	// ServerNextHop 是到达云服所用的下一跳。
	//
	// 空字符串表示云服就在直连网段里（本地拿 WSL 当服务端验证时就是这种情况）。
	// 这时**不加**绕行路由：现成的直连路由已经比默认路由更具体，抢不走；
	// 硬加一条指向默认网关的 /32 反而会覆盖它，把承载隧道的连接自己掐死。
	ServerNextHop string `json:"server_next_hop,omitempty"`

	// TunnelGateway / ServerIP 是本次接管用的隧道网关与云服地址。
	//
	// 存进快照是为了让自愈不依赖连接码：HealIfNeeded 跑在连接码加载之前，那时
	// App 手里是零值 Invite，而撤掉 /1 接管路由与绕行路由偏偏要用这两个值 ——
	// 只从 cfg 取就会在自愈时全部落空，把用户留在「所有流量灌进一张没人读的
	// 网卡」的状态里。旧快照没有这两个字段，还原时回退到 cfg。
	TunnelGateway string `json:"tunnel_gateway,omitempty"`
	ServerIP      string `json:"server_ip,omitempty"`

	// Split 记录本次实际铺上的国内分流路由。旧快照没有它（nil），还原时只能
	// 退回「下一跳 + metric」的形态扫描（会碰到同形态的用户路由）。
	Split *splitOwnership `json:"split_routes,omitempty"`
}

// NetConfig 是接管网络需要的参数。
type NetConfig struct {
	AdapterName string
	ServerIP    string
	TunnelIP    string
	Gateway     string
	Prefix      int
	MTU         int
	DNS         []string
}

// ServerIPOf 从连接码里的 host:port 取出主机部分。
func ServerIPOf(inv pki.Invite) string {
	host, _, err := net.SplitHostPort(inv.Server)
	if err != nil {
		return inv.Server
	}
	return host
}

// BuildNetConfig 从连接码拼出网络配置。
//
// 参数直接取自连接码而不等服务端 HELLO_ACK：连接码里已经有同样的值，
// 这样「抓快照 → 落盘 → 改网络」可以一气呵成，不必先把隧道拉起来再改网络。
func BuildNetConfig(inv pki.Invite) NetConfig {
	return NetConfig{
		AdapterName: AdapterName,
		ServerIP:    ServerIPOf(inv),
		TunnelIP:    inv.TunnelIP,
		Gateway:     inv.Gateway,
		Prefix:      inv.Prefix,
		MTU:         inv.MTU,
		DNS:         resolveClientDNS(inv),
	}
}

// defaultClientDNS 是接管期间给系统网卡配的 DNS。
//
// 连接码里带的是 10.10.0.1 —— 隧道内的 dnsmasq。用它意味着每个查询都要多走一趟
// 跨境隧道，实测解析平均 578ms（223.5.5.5 只要 93ms），网页打开速度差 40%。
//
// Pi 自己的域名不依赖这个：.minepi.com 由 NRPT 强制指到隧道网关解析（那个作用域
// 在国内所有递归上都会被污染，见 nrptNamespace 的注释）。所以系统 DNS 交给公共的
// 更快，Pi 解析不受影响。
var defaultClientDNS = []string{"223.5.5.5", "119.29.29.29"}

// resolveClientDNS 决定接管时给系统配哪些 DNS。
//
// 默认用公共 DNS。想回到「全部走隧道」或者指定别的解析器，用环境变量覆盖：
//
//	ANTAPP_CLIENT_DNS=tunnel          用连接码里的地址（10.10.0.1）
//	ANTAPP_CLIENT_DNS=1.1.1.1,8.8.8.8 指定一个列表
func resolveClientDNS(inv pki.Invite) []string {
	v := strings.TrimSpace(os.Getenv("ANTAPP_CLIENT_DNS"))
	if v == "tunnel" {
		return inv.DNS
	}
	if v != "" {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if len(inv.DNS) > 0 && !isTunnelDNS(inv.DNS) {
		// 连接码明确指定了别的解析器，尊重它
		return inv.DNS
	}
	return defaultClientDNS
}

// isTunnelDNS 判断这组 DNS 是不是「只有隧道网关」。
//
// 只有这种情况才替换成公共 DNS：连接码如果已经写了具体的公共解析器，那是用户
// 有意选的，不该被默认值覆盖。
func isTunnelDNS(servers []string) bool {
	if len(servers) != 1 {
		return false
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(servers[0]))
	if err != nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback()
}

// validateIPv4Only 在接管或还原前确认这次配置全在 IPv4 范围内。
//
// 当前版本只接管 IPv4：`/1` 路由、服务端的 MSS clamp、dnsmasq 中继都只处理 IPv4，
// filter-AAAA 也不等于接管了 IPv6。传进来 IPv6 的隧道地址或网关时如果不明确拒绝，
// 后面的写入会一半成功一半失败，还原时还可能把宿主的 IPv6 配置留在半改状态 ——
// 那比直接拒绝难查得多。宁可在这里报清楚「当前版本只接管 IPv4」。
//
// 空值跳过：Gateway 在「云服就在直连网段」时是空的，那是合法情况。
// 域名之类解析不了的值也跳过，交给 resolveIPv4 报它自己的错。
func validateIPv4Only(cfg NetConfig) error {
	for _, f := range []struct{ name, val string }{
		{"服务端地址", cfg.ServerIP},
		{"隧道地址", cfg.TunnelIP},
		{"默认网关", cfg.Gateway},
	} {
		host := strings.TrimSpace(f.val)
		if host == "" {
			continue
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		a, err := netip.ParseAddr(host)
		if err != nil {
			continue
		}
		if a.Is6() {
			return fmt.Errorf("%s 是 IPv6（%s），当前版本只接管 IPv4", f.name, f.val)
		}
	}
	return nil
}

// splitRouteMetric 是国内直连路由的 route metric。还原时按
// 「下一跳 = 原默认网关 且 metric = 此值」整批扫除，网段表更新过也能清干净。
const splitRouteMetric = 5

// netTxnMu 串行化所有网络变更：接管（Apply）、还原（Restore）、网卡迁移
// （watcher.reconcile）、分流热重铺。
//
// 这几个操作改的都是同一批路由和 DNS。以前它们各自读快照、各自写，插入执行会留下
// 半套配置：比如还原刚撤完走行路由，热重铺又把它们铺回来 —— 用户点了断开，流量却
// 还在往隧道里走。
//
// 锁序：调用方先放掉自己的 a.mu / w.mu，再进这里。netTxnMu 永远是最内层，
// 不在持它的时候去拿那两把锁。
var netTxnMu sync.Mutex

// netEpoch 是网络代际，每次还原自增。
//
// 光有锁不够：热重铺可能已经拿到锁、正在算要铺哪些前缀，此时还原插不进来，
// 等热重铺写完才轮到还原 —— 那是安全的。但反过来，热重铺排队等着还原先跑完，
// 醒来后必须知道「网络已经被还愿过了」，否则会把刚撤掉的路由重新铺上。
// 它在真正写路由前比一次代际，变了就放弃这一轮。
var netEpoch atomic.Uint64

// splitOwnership 是本程序实际铺上的国内直连路由。
//
// 只按「下一跳 = 原网关 且 metric = 5」筛选会误删用户自己建的形态相同的路由
// （手工路由、其它 VPN 客户端都可能长这样）。记下装在哪张网卡、走哪个网关、
// 具体哪些前缀，还原时只清这一组。
type splitOwnership struct {
	IfGUID   string   `json:"if_guid,omitempty"`
	IfIndex  int      `json:"if_index"`
	Gateway  string   `json:"gateway"`
	Prefixes []string `json:"prefixes,omitempty"`
}

// ownsRoute 判断一条路由是不是这次接管装上的。
//
// GUID 存在时只认 GUID：索引会被系统复用，退回索引可能删到别的网卡上的同类
// 路由。网关和前缀集合再各挡一层，同形态的用户路由要同时满足三样才会被误判。
func (o *splitOwnership) ownsRoute(guid string, ifIndex uint32, nextHop netip.Addr, metric uint32, dest netip.Prefix) bool {
	if o == nil {
		return false
	}
	if o.IfGUID != "" {
		if guid != o.IfGUID {
			return false
		}
	} else if int(ifIndex) != o.IfIndex {
		return false
	}
	if metric != splitRouteMetric {
		return false
	}
	gw, err := netip.ParseAddr(o.Gateway)
	if err != nil || nextHop.Unmap() != gw.Unmap() {
		return false
	}
	want := dest.Masked().String()
	for _, p := range o.Prefixes {
		if p == want {
			return true
		}
	}
	return false
}

// newSplitOwnership 汇总本次装上的分流前缀。prefixes 只收实际写进去的，
// 没铺成功的条目不该出现在所有权里。
func newSplitOwnership(ref adapterKey, gateway netip.Addr, prefixes []string) *splitOwnership {
	return &splitOwnership{
		IfGUID:   ref.GUID,
		IfIndex:  int(ref.Index),
		Gateway:  gateway.String(),
		Prefixes: prefixes,
	}
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

// splitLedgersForRestore 汇总一次还原要清理的两份账本：快照里那份与进程内那份。
func splitLedgersForRestore(snapshot, live *splitOwnership) []*splitOwnership {
	owns := make([]*splitOwnership, 0, 2)
	if snapshot != nil {
		owns = append(owns, snapshot)
	}
	if live != nil && live != snapshot {
		owns = append(owns, live)
	}
	return owns
}

// sweepSplitLedgers 按账本逐份清理分流路由，返回删掉的条数、这次没清干净的那份
// 账本，以及汇总的删除错误。
//
// 失败时要把 live 账本原样交回去，是因为它只存在于进程内：快照里那份（A）还躺在
// state.json 里等下一次还原重试，热更新后的那份（B）一旦从内存里清掉，就再没有
// 第二处知道它铺过什么，B 的残留路由会从此无人认领。
func sweepSplitLedgers(owns []*splitOwnership, live *splitOwnership,
	sweep func(*splitOwnership) (int, error)) (removed int, remain *splitOwnership, err error) {
	var errs []error
	for _, own := range owns {
		n, serr := sweep(own)
		removed += n
		if serr == nil {
			continue
		}
		errs = append(errs, serr)
		if own == live {
			remain = own
		}
	}
	return removed, remain, errors.Join(errs...)
}

// restoreSplitRoutes 按快照账本与进程内账本清理分流路由，并据此维护进程内账本：
// 全部清干净才清空，没清干净就留到下一次还原重试。
func restoreSplitRoutes(snapshot *splitOwnership, sweep func(*splitOwnership) (int, error)) (removed int, err error) {
	live := currentLiveSplitOwnership()
	owns := splitLedgersForRestore(snapshot, live)
	if len(owns) == 0 {
		return 0, nil
	}
	removed, remain, serr := sweepSplitLedgers(owns, live, sweep)
	if remain != nil {
		setLiveSplitOwnership(remain)
	} else {
		clearLiveSplitOwnership()
	}
	return removed, serr
}

// mergeSplitOwnership 把 extra 的前缀并进 base，给「上一版没删干净」的残留用。
// 两者不属于同一张网卡或同一个网关时返回 nil：一条账本记录装不下两套路由。
//
// 已删掉的前缀也会留在合并结果里；下次重试对「本来就不在」的路由不计失败，
// 所以这份账本只是需要重试的超集，不会因此误删。
func mergeSplitOwnership(base, extra *splitOwnership) *splitOwnership {
	if base == nil {
		return extra
	}
	if extra == nil {
		return base
	}
	if !sameSplitScope(base, extra) {
		return nil
	}
	seen := make(map[string]struct{}, len(base.Prefixes)+len(extra.Prefixes))
	out := &splitOwnership{IfGUID: base.IfGUID, IfIndex: base.IfIndex, Gateway: base.Gateway}
	for _, list := range [][]string{base.Prefixes, extra.Prefixes} {
		for _, p := range list {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out.Prefixes = append(out.Prefixes, p)
		}
	}
	return out
}

// sameSplitScope 判断两份账本记的是不是同一张网卡加同一个网关（GUID 优先）。
func sameSplitScope(a, b *splitOwnership) bool {
	if a.IfGUID != "" || b.IfGUID != "" {
		if a.IfGUID == "" || a.IfGUID != b.IfGUID {
			return false
		}
	} else if a.IfIndex != b.IfIndex {
		return false
	}
	return a.Gateway == b.Gateway
}

// splitLedgerAfterRefresh 算热更新铺完之后该留哪份账本。
//
// 上一版没删干净时，那些路由还在表里，而新网段表未必包含它们。不并回账本就等于
// 把它们从账本上抹掉，下一轮热更新和还原都不会再去删。kept 报告是否真的并了进来
// （两份账本分属不同网卡/网关时一条记录装不下，只能保留新账本并报数）。
func splitLedgerAfterRefresh(fresh, previous *splitOwnership, oldSweepFailed bool) (own *splitOwnership, kept bool) {
	if !oldSweepFailed || previous == nil {
		return fresh, false
	}
	if merged := mergeSplitOwnership(fresh, previous); merged != nil {
		return merged, true
	}
	return fresh, false
}

// claimedBySplitLedger 报告一条「本来就在」的路由是否有账本证明是本程序装的。
//
// 形状（本接口 + 原网关 + metric 5）不能当证据：用户手工建的路由、其它 VPN
// 客户端建的路由都是这个形状。没有账本还认领，断开时就会删掉用户的配置。
func claimedBySplitLedger(ledgers []*splitOwnership, ref adapterKey, gw netip.Addr, dest netip.Prefix) bool {
	for _, own := range ledgers {
		if own.ownsRoute(ref.GUID, ref.Index, gw, splitRouteMetric, dest) {
			return true
		}
	}
	return false
}

// splitRouteOps 是热重铺用到的系统操作。抽成参数是为了让「先写盘、再动路由」
// 这个崩溃安全顺序能被单测钉住，不必真去改路由表。
type splitRouteOps struct {
	existing  func() (map[string]struct{}, error)
	sweep     func(*splitOwnership) (int, error)
	sweepForm func() (int, error)
	install   func([]*splitOwnership) ([]string, int)
	persist   func(*splitOwnership) error
}

// hotSplitLedgerSink 是热重铺把「即将铺上」的账本落盘的口子，由 App 在热重铺前
// 注入（生产写 state.json）。默认空实现：没有 App 侧注入时（非 Windows、单测）
// 没有热重铺，也就没有要落的盘。
var hotSplitLedgerSink = func(*splitOwnership) error { return nil }

// splitLedgerToPersist 算热重铺动路由之前应该先落盘的账本：上一版账本，加上新表里
// 「当前路由表里还没有」的前缀 —— 那些是本轮一定要新建的。
//
// 只提前记这些，是因为「本来就在」的路由没有账本证据时可能是用户的（见
// installSplitRoutes）：提前认领会把用户的配置写进账本，断开时一并删掉。
func splitLedgerToPersist(ref adapterKey, gw netip.Addr, previous *splitOwnership,
	table []string, existing map[string]struct{}) *splitOwnership {
	prefixes := make([]string, 0, len(table))
	seen := make(map[string]struct{}, len(table))
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		prefixes = append(prefixes, p)
	}
	if previous != nil {
		for _, p := range previous.Prefixes {
			add(p)
		}
	}
	for _, raw := range table {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		dest := prefix.Masked().String()
		if _, ok := existing[dest]; ok {
			continue
		}
		add(dest)
	}
	return newSplitOwnership(ref, gw, prefixes)
}

// refreshSplitRoutes 执行一次热重铺，返回最终账本。
//
// 顺序是崩溃安全的关键：
//  1. 先落盘「旧账本 + 预计新建」的账本，此时路由一条都还没动；写盘失败就放弃
//     本轮，系统保持上一版可用状态。
//  2. 撤旧账本、铺新表。任一点崩溃，磁盘上那份账本都已经覆盖了已铺的前缀。
//  3. 落盘最终账本。失败也不回滚：预写的是超集，只多记不漏记，自愈仍清得掉。
func refreshSplitRoutes(ref adapterKey, gw netip.Addr, previous *splitOwnership,
	table []string, ops splitRouteOps) *splitOwnership {
	cur := newSplitOwnership(ref, gw, nil)
	if previous != nil && !sameSplitScope(previous, cur) {
		// 旧账本属于别的网卡/网关：那是出口迁移该处理的状态，混着铺会把账本
		// 钉到错误的接口上。等迁移完成后的下一轮。
		logf("国内分流热更新：旧账本属于另一张网卡或网关，等出口迁移处理，本轮放弃")
		return previous
	}

	existing, err := ops.existing()
	if err != nil {
		logf("国内分流热更新：读路由表失败（%v），本轮放弃", err)
		return previous
	}
	if err := ops.persist(splitLedgerToPersist(ref, gw, previous, table, existing)); err != nil {
		logf("国内分流热更新：账本预写盘失败（%v），本轮放弃（旧路由未动）", err)
		return previous
	}

	oldSweepFailed := false
	if previous != nil {
		if removed, serr := ops.sweep(previous); serr != nil {
			logf("国内分流热更新：旧路由有 %d 条没删掉（%v），继续铺新表", removed, serr)
			oldSweepFailed = true
		}
	} else if removed, serr := ops.sweepForm(); serr != nil {
		logf("国内分流热更新：按形态清理旧路由时 %d 条失败: %v", removed, serr)
	}

	installed, failed := ops.install([]*splitOwnership{previous})
	own := newSplitOwnership(ref, gw, installed)
	own, kept := splitLedgerAfterRefresh(own, previous, oldSweepFailed)
	if oldSweepFailed && !kept {
		logf("国内分流热更新：旧账本属于另一张网卡或网关，%d 条残留无法并入新账本",
			len(previous.Prefixes))
	}
	if serr := ops.persist(own); serr != nil {
		logf("国内分流热更新：新账本写盘失败（%v），磁盘上留的是不会漏清的预写账本", serr)
	}
	if failed > 0 {
		logf("国内分流重铺：%d 条失败（这些网段暂走隧道，不影响可用）", failed)
	}
	setLiveSplitOwnership(own)
	return own
}

// nrptRuleComment 是 NRPT 规则的识别标记，增删都按它筛，不碰别人的规则。
const nrptRuleComment = "AntApp Link DNS"

// nrptNamespace 是被污染、必须走隧道解析的域名作用域。
//
// api.minepi.com 在国内所有递归上都是被污染的假答案（实测路由器 DNS、AliDNS、
// DNSPod 给的全是 Twitter/Facebook 地址段的 IP），唯一干净的来源是隧道出口的
// 解析。而路由器 RA 下发的 IPv6 DNS 应答快、抢得赢隧道，它还是系统托管的
// 删不掉，所以用 NRPT（名称解析策略表）按域名作用域直接规定解析去向 ——
// 它的优先级高于一切网卡 DNS 配置，且不加防火墙规则。
//
// 只圈 Pi 自己的域名，刻意不扩大干预面：其它域照常走系统默认（快），规则万一
// 成为孤儿（崩溃后又卸载）也只影响 Pi 域名，不影响整机上网。
const nrptNamespace = ".minepi.com"

// nrptKeyPath / nrptRuleGUID 是 NRPT 规则在注册表的落脚点。
// GUID 固定不变：重写即覆盖（幂等），删除按标记扫时也认得出自己。
const nrptKeyPath = `SYSTEM\CurrentControlSet\Services\Dnscache\Parameters\DnsPolicyConfig`
const nrptRuleGUID = "{8F2A7B41-9C5D-4E3A-B6D8-21F0A9C7E4D2}"

// routeOp 是接管时的一条路由。onTunnel 区分挂隧道网卡还是原默认网卡：
// Windows 按接口选路，挂错网卡流量就进不了隧道（旧版 netsh 时代的硬伤，
// 现在在数据结构上杜绝）。
type routeOp struct {
	dest     netip.Prefix
	nextHop  netip.Addr
	metric   uint32
	onTunnel bool
}

// routePlan 按生效顺序产出全部接管路由。顺序有两条硬约束：
//
//  1. 绕行路由必须在 /1 接管路由之前。反了的话承载隧道的 TCP 连接会被
//     自己送进隧道，表现是「连不上，但没有任何报错」。
//  2. 云服在直连网段内时不加绕行路由：现成的直连路由更具体，加一条指向
//     默认网关的 /32 会覆盖它，隧道自己掐死自己。
func routePlan(snap Snapshot, cfg NetConfig) []routeOp {
	gw, err := netip.ParseAddr(cfg.Gateway)
	if err != nil {
		// 没有隧道网关就谈不上接管，宁可一条不加也不产半个计划
		return nil
	}
	var ops []routeOp
	// 用快照里那个**已解析的 IP**，不是 cfg.ServerIP：连接码可能写域名，
	// ParseAddr 会失败，绕行路由就不生成 —— 隧道自己的流量会被 /1 吸回隧道。
	if snap.ServerNextHop != "" && snap.ServerIP != "" {
		if hop, err := netip.ParseAddr(snap.ServerNextHop); err == nil {
			if srv, err := netip.ParseAddr(snap.ServerIP); err == nil {
				ops = append(ops, routeOp{dest: netip.PrefixFrom(srv, 32), nextHop: hop, metric: 1})
			}
		}
	}
	for _, dest := range [2]netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
	} {
		ops = append(ops, routeOp{dest: dest, nextHop: gw, metric: 1, onTunnel: true})
	}
	return ops
}

// ---------- 快照落盘 ----------

const StateFileName = "state.json"

func StatePath(root string) string { return filepath.Join(RuntimeDir(root), StateFileName) }

// SaveSnapshot 落盘。文件存在本身就代表「上次接管过网络且没干净退出」，
// 所以它必须在改写网络之前写入、在还原成功之后才删除。
func SaveSnapshot(path string, snap Snapshot) error {
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot 的第二个返回值表示「文件是否存在」。文件存在但内容坏了时同样返回 true ——
// 调用方据此知道网络可能还被接管着，不能当没事发生。
func LoadSnapshot(path string) (Snapshot, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, true, fmt.Errorf("状态文件 %s 损坏: %w", path, err)
	}
	return snap, true, nil
}

func RemoveSnapshot(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// newSnapshot 组装快照。
//
// serverIP 必须是**已经解析好的 IP**，不是连接码里那个 host —— 连接码可能写域名，
// 而 routePlan 和 watcher 都用 netip.ParseAddr 解析它：域名会直接失败，结果是绕行
// 路由不生成，隧道流量被 /1 吸回隧道自己。
func newSnapshot(gateway string, ifIndex int, serverNextHop, serverIP string,
	ifaces []IfaceDNS, cfg NetConfig) Snapshot {
	return Snapshot{
		DefaultGateway: gateway,
		DefaultIfIndex: ifIndex,
		ServerNextHop:  serverNextHop,
		Interfaces:     ifaces,
		CapturedAt:     time.Now().Format(time.RFC3339),
		TunnelGateway:  cfg.Gateway,
		ServerIP:       serverIP,
	}
}
