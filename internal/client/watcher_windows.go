//go:build windows

package client

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

const (
	// debounce：网络切换时系统会连续吐出一串事件，等安静下来再对账。
	debounce = 700 * time.Millisecond
	// pollInterval：周期兜底对账。实测 NotifyRouteChange2 对 /0 默认路由的
	// 增删不投递事件（其它前缀正常），不能只依赖回调；对账是一次路由表
	// 读取，微秒级，常开无负担。
	pollInterval = 5 * time.Second
)

// sessionWatcher 在连接期间监听系统网络变化，把「跟出口相关的配置」迁移到
// 新出口。这是 WireGuard interfacewatcher/monitorMTU 的同思路实现：
//
// 换 Wi-Fi、插拔网线、路由器重启、睡眠唤醒 —— 这些都会让默认路由换到另一张
// 网卡。如果不迁移：绕行路由指着旧网关（隧道断线后连不回去）、DNS 还写在
// 旧网卡（污染回流）、国内分流随旧接口消失（国内流量挤进隧道）。
// watcher 检测到默认出口变化后，原子地把这三样迁到新网卡，并唤醒隧道重连。
type sessionWatcher struct {
	cfg NetConfig
	srv netip.Addr // 云服地址（迁移绕行路由用）

	// update 把迁移后的现场写回 App，让断开还原时用的是最新状态。
	// 返回错误表示落盘失败 —— 那时不能把这次迁移当成已提交。
	update func(Snapshot) error
	// kick 唤醒隧道重连
	kick func()

	mu      sync.Mutex
	snap    Snapshot   // 当前生效的接管现场（迁移时更新）
	lastGw  netip.Addr // 上次认定的默认网关（去抖：没变就不动）
	lastIdx uint32     // 上次认定的默认网卡

	// pending 是「配置已经改到系统上、但快照还没写进磁盘」的现场。
	//
	// 有它才能让重试安全：写盘失败后如果走完整迁移流程重新读网卡 DNS，读到的是
	// 已经被我们改成隧道网关的值，会被当成用户原值存进快照 —— 断开时把物理网卡
	// DNS 恢复到一个已经拆掉的隧道地址，用户解析全超时。所以重试只重做写盘这一步。
	pending *Snapshot

	events chan struct{} // 回调 → 对账循环的通知（带缓冲，风暴合并）

	stop   chan struct{}
	done   chan struct{}
	unregs []func() error
}

// startSessionWatcher 注册网络变化回调并开始守护。注册回调只挂 /0 前缀过滤，
// 我们自己写的路由（/1、/32、国内分流）不会触发事件风暴。
func startSessionWatcher(ctx context.Context, snap Snapshot, cfg NetConfig,
	update func(Snapshot) error, kick func(),
) (*sessionWatcher, error) {
	srv, err := resolveIPv4(cfg.ServerIP)
	if err != nil {
		return nil, err
	}
	w := &sessionWatcher{
		cfg:    cfg,
		srv:    srv,
		update: update,
		kick:   kick,
		snap:   snap,
		events: make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if gw, err := netip.ParseAddr(snap.DefaultGateway); err == nil {
		w.lastGw = gw
		w.lastIdx = uint32(snap.DefaultIfIndex)
	}

	cbr, err := winipcfg.RegisterRouteChangeCallback(func(_ winipcfg.MibNotificationType, route *winipcfg.MibIPforwardRow2) {
		if route != nil && route.DestinationPrefix.PrefixLength == 0 {
			w.notify()
		}
	})
	if err != nil {
		return nil, fmt.Errorf("注册路由变化回调: %w", err)
	}
	w.unregs = append(w.unregs, cbr.Unregister)

	cbi, err := winipcfg.RegisterInterfaceChangeCallback(func(_ winipcfg.MibNotificationType, row *winipcfg.MibIPInterfaceRow) {
		if row != nil && row.Family == windows.AF_INET {
			w.notify()
		}
	})
	if err != nil {
		for _, u := range w.unregs {
			_ = u()
		}
		return nil, fmt.Errorf("注册接口变化回调: %w", err)
	}
	w.unregs = append(w.unregs, cbi.Unregister)

	logf("出口守护已启动")
	go w.loop(ctx)
	return w, nil
}

// watcherDebug 决定是否打印每次对账的明细。正常关闭（5 秒一次会刷屏），
// 排障时设 ANTAPP_WATCHER_DEBUG=1 打开。
var watcherDebug = func() bool { return os.Getenv("ANTAPP_WATCHER_DEBUG") != "" }()

// notify 请求一次对账。回调线程里非阻塞发送，事件风暴由 loop 里的防抖合并。
func (w *sessionWatcher) notify() {
	select {
	case w.events <- struct{}{}:
	default:
	}
}

// loop 消费事件并防抖对账。防抖间隔 700ms：网络切换时系统会连续吐出
// 一串路由/接口事件，等它安静下来再动手，避免迁到半截状态上。
//
// 另有 5 秒周期兜底对账：实测本机 NotifyRouteChange2 对 /0 默认路由的
// 增删**不投递事件**（其它前缀正常），不能只依赖回调。对账本身是
// 一次路由表读取，微秒级，常开无负担。
func (w *sessionWatcher) loop(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-w.events:
		case <-ticker.C:
		}
		// 防抖：事件后的安静期内再有新事件就顺延
		timer := time.NewTimer(debounce)
	drain:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-w.stop:
				timer.Stop()
				return
			case <-timer.C:
				break drain
			case <-w.events:
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(debounce)
			}
		}
		w.reconcile()
	}
}

// reconcile 对账：默认出口没变就什么都不做；变了就原子迁移。
// 出口判定与迁移全程排除隧道接口自己 —— 我们写的 0.0.0.0/1 路由
// 在前缀包含意义上也「通向 0.0.0.0」，不排除就会把隧道当出口。
func (w *sessionWatcher) reconcile() {
	w.mu.Lock()
	defer w.mu.Unlock()

	tunLUID := w.tunLUID()
	gw, ifIndex, err := bestDefaultRoute(tunLUID)
	if err != nil || gw.IsUnspecified() {
		return // 眼下没有出口（断网中），等下次事件
	}

	if watcherDebug {
		logf("对账：出口 %s（if %d），上次 %s（if %d）", gw, ifIndex, w.lastGw, w.lastIdx)
	}
	if ifIndex == w.lastIdx && gw.Unmap() == w.lastGw {
		return // 出口没变
	}

	// 上一次迁移已经把配置改到系统上，只是快照没写进磁盘：这次只补写盘，不重跑迁移。
	//
	// 必须在这里短路：走完整流程会重新读新网卡的 DNS，而它此刻已经是隧道网关，
	// 于是把「我们自己的值」当成用户原值保存下来 —— 断开时物理网卡 DNS 被恢复成
	// 一个不可用的隧道地址，用户表现为解析全超时。
	if w.pending != nil && w.pending.DefaultIfIndex == int(ifIndex) &&
		w.pending.DefaultGateway == gw.Unmap().String() {
		pending := *w.pending
		if w.update != nil {
			if err := w.update(pending); err != nil {
				logf("迁移现场仍写不进磁盘（%v），保留待提交状态，下次事件再试", err)
				return
			}
		}
		w.snap = pending
		w.pending = nil
		w.lastGw = gw.Unmap()
		w.lastIdx = ifIndex
		logf("上次迁移的现场已补写入磁盘")
		return
	}

	oldSnap := w.snap
	oldGw, _ := netip.ParseAddr(oldSnap.DefaultGateway)

	// 先把新现场完整抓下来，再动旧配置。
	//
	// 顺序反了的话（先拆旧的、再抓新的），中途任何一步失败都会留下「旧配置已删、
	// 新配置没建」的半迁移状态，内存快照和真实系统也对不上。现在抓不到就直接返回，
	// 用户那条本来能用的隧道一点没被动过。
	newDef, ok, err := adapterByIndex(ifIndex)
	if err != nil || !ok {
		// 抓不到新网卡也要把旧网卡的 DNS 还原：否则它会一直指着隧道网关，
		// 用户切回那张网卡时解析全超时（这是迁移失败时唯一还有副作用的残留）。
		for _, iface := range oldSnap.Interfaces {
			if a, ok2, _ := adapterByName(iface.Name); ok2 {
				_ = setAdapterDNS(a.LUID, iface.DNS)
			}
		}
		return
	}
	// DNS 原值只认第一次接管时记下的那份。
	//
	// 同一张网卡换出口（笔记本从家里 Wi-Fi 换到公司 Wi-Fi、路由器换了网段）时，
	// 它身上的 DNS 已经是我们写进去的隧道网关 —— 直接读当前值会把「我们自己的值」
	// 当成用户原值存进快照，断开时就把这张网卡的 DNS 永久指到一个已经拆掉的网关，
	// 用户表现为解析全超时。换了网卡才读新网卡的当前值。
	var newDNSStrings []string
	reusedDNS := false
	for _, iface := range oldSnap.Interfaces {
		if uint32(iface.Index) == ifIndex {
			newDNSStrings = iface.DNS
			reusedDNS = true
			break
		}
	}
	if !reusedDNS {
		newDNS := adapterDNS(newDef.LUID)
		newDNSStrings = make([]string, 0, len(newDNS))
		for _, a := range newDNS {
			newDNSStrings = append(newDNSStrings, a.String())
		}
	}

	// 新现场齐了，才开始拆旧的。
	// 1) 还原旧网卡的 DNS（用旧快照里记录的原值；网卡已消失则跳过）
	for _, iface := range oldSnap.Interfaces {
		if uint32(iface.Index) == ifIndex {
			continue
		}
		if a, ok, _ := adapterByName(iface.Name); ok {
			_ = setAdapterDNS(a.LUID, iface.DNS) // 原值为空 = 清空回到自动获取
		}
	}
	// 2) 扫掉旧网卡上的国内分流（按旧网关 + metric 筛）
	if oldGw.IsValid() {
		sweepSplitRoutes(oldGw)
	}
	// 3) 撤旧绕行路由
	if oldSnap.ServerNextHop != "" && w.srv.IsValid() {
		if a, ok, _ := adapterByIndex(uint32(oldSnap.DefaultIfIndex)); ok {
			if hop, err := netip.ParseAddr(oldSnap.ServerNextHop); err == nil {
				_ = a.LUID.DeleteRoute(netip.PrefixFrom(w.srv, 32), hop)
			}
		}
	}

	// 旧绕行撤掉之后，再算新出口到云服该怎么绕。
	//
	// 必须排在这之后：连接期那条云服 /32 绕行还在表里时，最长前缀会命中它，
	// 算出来的下一跳是旧网关（换出口后通常已不可达）。那样装到新网卡上的 /32
	// 指向死路，隧道重拨的流量会被 /1 吸回隧道自己，永久连不上。
	hop, _, err := bestRouteTo(w.srv, tunLUID)
	if err != nil {
		// 算不出来就把旧绕行原样装回去，别停在「旧绕行已删、新绕行没建」的状态
		if oldSnap.ServerNextHop != "" && w.srv.IsValid() {
			if a, ok, _ := adapterByIndex(uint32(oldSnap.DefaultIfIndex)); ok {
				if oh, e := netip.ParseAddr(oldSnap.ServerNextHop); e == nil {
					_ = a.LUID.AddRoute(netip.PrefixFrom(w.srv, 32), oh, 1)
				}
			}
		}
		logf("迁移中止：算不出到云服 %s 的下一跳: %v（旧绕行已恢复）", w.srv, err)
		return
	}
	serverNextHop := ""
	if !hop.IsUnspecified() && hop.Unmap() != w.srv {
		serverNextHop = hop.Unmap().String()
	}

	// 4) 新网卡接管：DNS、绕行、国内分流、动态 MTU
	//
	// DNS 和云服绕行是「隧道能不能用」的前提，失败必须算迁移失败 —— 只靠后面的
	// 快照写盘成功就把出口标记提交掉的话，对账发现接口与网关没变就直接返回，
	// 这两个配置永远不会被补修，隧道一直连不上。
	dnsApplied := true
	if err := setAdapterDNS(newDef.LUID, w.cfg.DNS); err != nil {
		dnsApplied = false
		logf("迁移 DNS 到 %s 失败（待补修）: %v", newDef.Name, err)
	}
	routeApplied := true
	if serverNextHop != "" {
		if nextHop, err := netip.ParseAddr(serverNextHop); err == nil {
			if rerr := newDef.LUID.AddRoute(netip.PrefixFrom(w.srv, 32), nextHop, 1); rerr != nil &&
				!errors.Is(rerr, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				routeApplied = false
				logf("给新出口加云服绕行路由失败（隧道可能连不上，待补修）: %v", rerr)
			}
		}
	}
	failed := 0
	for _, p := range ActiveCNRoutes() {
		if prefix, err := netip.ParsePrefix(p); err == nil {
			if err := newDef.LUID.AddRoute(prefix.Masked(), gw, splitRouteMetric); err != nil &&
				!errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				failed++
			}
		}
	}

	// 6) 提交现场 + 唤醒隧道立刻重连
	//
	// 只有「关键配置都写上 + 快照落盘成功」才算提交。任一条件不满足就保留待提交
	// 状态（pending），下一轮事件会重试同一份现场 —— 重试只补写盘/补配置，不会重新
	// 读网卡 DNS（那已经被我们改过了）。
	snap := newSnapshot(gw.Unmap().String(), int(ifIndex), serverNextHop,
		w.srv.String(), []IfaceDNS{{Name: newDef.Name, Index: int(newDef.Index), DNS: newDNSStrings}}, w.cfg)
	if !dnsApplied || !routeApplied {
		w.pending = &snap
		logf("网络出口已切换，但关键配置没写全：保留待提交状态，下次事件会补修")
		if w.kick != nil {
			w.kick()
		}
		return
	}
	if w.update != nil {
		if err := w.update(snap); err != nil {
			w.pending = &snap
			logf("网络出口迁移已生效，但现场写盘失败（%v）：保留待提交状态，下次事件会重试", err)
			if w.kick != nil {
				w.kick()
			}
			return
		}
	}
	w.pending = nil
	w.snap = snap
	w.lastGw = gw.Unmap()
	w.lastIdx = ifIndex
	if w.kick != nil {
		w.kick()
	}
	logf("网络出口变化，已迁移到 %s（网关 %s）%s",
		newDef.Name, gw, migrationNote(failed))
}

func migrationNote(failed int) string {
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf("，%d 条国内分流未迁移（这些网段暂走隧道）", failed)
}

// tunLUID 隧道网卡可能已被系统重建（睡眠唤醒），每次现查；查不到返回 0，
// 动态 MTU 拿着 0 也无害（IPInterface 会失败并静默返回）。
func (w *sessionWatcher) tunLUID() winipcfg.LUID {
	if a, ok, _ := adapterByName(AdapterName); ok {
		return a.LUID
	}
	return 0
}

// Stop 停止守护。先撤回调（不再有新事件），再等在跑的对账结束 ——
// 调用方（Disconnect）之后做的整体还原不会被并发迁移踩到。
func (w *sessionWatcher) Stop() {
	for _, u := range w.unregs {
		_ = u()
	}
	close(w.stop)
	<-w.done
}
