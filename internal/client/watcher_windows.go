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

	// update 把迁移后的现场写回 App，让断开还原时用的是最新状态
	update func(Snapshot)
	// kick 唤醒隧道重连
	kick func()

	mu      sync.Mutex
	snap    Snapshot   // 当前生效的接管现场（迁移时更新）
	lastGw  netip.Addr // 上次认定的默认网关（去抖：没变就不动）
	lastIdx uint32     // 上次认定的默认网卡

	events chan struct{} // 回调 → 对账循环的通知（带缓冲，风暴合并）

	stop   chan struct{}
	done   chan struct{}
	unregs []func() error
}

// startSessionWatcher 注册网络变化回调并开始守护。注册回调只挂 /0 前缀过滤，
// 我们自己写的路由（/1、/32、国内分流）不会触发事件风暴。
func startSessionWatcher(ctx context.Context, snap Snapshot, cfg NetConfig,
	update func(Snapshot), kick func(),
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

	logf("出口守护已启动（事件通知 + 每 %s 对账）", pollInterval)
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

	// 出口没变也可能只是出口的 MTU 变了（人为调整、驱动重置、便携设备换网络），
	// 动态 MTU 每次对账都要刷新；已经是目标值时它是空操作。
	if def, ok, _ := adapterByIndex(ifIndex); ok {
		applyDynamicMTU(def.LUID, tunLUID, w.cfg.MTU)
	}

	if watcherDebug {
		logf("对账：出口 %s（if %d），上次 %s（if %d）", gw, ifIndex, w.lastGw, w.lastIdx)
	}
	if ifIndex == w.lastIdx && gw.Unmap() == w.lastGw {
		return // 出口没变
	}

	oldSnap := w.snap
	oldGw, _ := netip.ParseAddr(oldSnap.DefaultGateway)

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
	if oldSnap.ServerNextHop != "" && w.cfg.ServerIP != "" {
		if a, ok, _ := adapterByIndex(uint32(oldSnap.DefaultIfIndex)); ok {
			if hop, err := netip.ParseAddr(oldSnap.ServerNextHop); err == nil {
				if srv, err := netip.ParseAddr(w.cfg.ServerIP); err == nil {
					_ = a.LUID.DeleteRoute(netip.PrefixFrom(srv, 32), hop)
				}
			}
		}
	}

	// 4) 抓新现场
	newDef, ok, err := adapterByIndex(ifIndex)
	if err != nil || !ok {
		return
	}
	newDNS := adapterDNS(newDef.LUID)
	newDNSStrings := make([]string, 0, len(newDNS))
	for _, a := range newDNS {
		newDNSStrings = append(newDNSStrings, a.String())
	}
	hop, _, err := bestRouteTo(w.srv, tunLUID)
	if err != nil {
		return
	}
	serverNextHop := ""
	if !hop.IsUnspecified() && hop.Unmap() != w.srv {
		serverNextHop = hop.Unmap().String()
	}

	// 5) 新网卡接管：DNS、绕行、国内分流、动态 MTU
	if err := setAdapterDNS(newDef.LUID, w.cfg.DNS); err != nil {
		logf("迁移 DNS 到 %s 失败: %v", newDef.Name, err)
	}
	if serverNextHop != "" {
		if nextHop, err := netip.ParseAddr(serverNextHop); err == nil {
			_ = newDef.LUID.AddRoute(netip.PrefixFrom(w.srv, 32), nextHop, 1)
		}
	}
	failed := 0
	for _, p := range CNRoutes() {
		if prefix, err := netip.ParsePrefix(p); err == nil {
			if err := newDef.LUID.AddRoute(prefix.Masked(), gw, splitRouteMetric); err != nil &&
				!errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) {
				failed++
			}
		}
	}
	applyDynamicMTU(newDef.LUID, w.tunLUID(), w.cfg.MTU)

	// 6) 更新现场 + 唤醒隧道立刻重连
	w.snap = newSnapshot(gw.Unmap().String(), int(ifIndex), serverNextHop,
		[]IfaceDNS{{Name: newDef.Name, Index: int(newDef.Index), DNS: newDNSStrings}})
	w.lastGw = gw.Unmap()
	w.lastIdx = ifIndex
	if w.update != nil {
		w.update(w.snap)
	}
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
