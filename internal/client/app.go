package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/update"
)

// App 把「连接 / 断开 / 自愈 / 状态」串起来。托盘只是它的一个界面，
// 所以这里不含任何界面代码，逻辑可以单独测。
type App struct {
	inv       pki.Invite
	invSource string
	rootDir   string
	statePath string
	log       *slog.Logger
	noNetCfg  bool

	mu            sync.Mutex
	snapshot      Snapshot
	cfg           NetConfig
	dev           Device
	tunnel        *Tunnel
	watcher       *sessionWatcher
	cancel        context.CancelFunc
	running       bool
	connecting    bool
	disconnecting bool
	lastError     string

	// connectCtx/connectCancel 是本次连接尝试的上下文，CancelConnect 取消它。
	// 只覆盖到「接管网络之前」：一旦开始改路由就必须走完，否则会留下半接管状态。
	// connectCtx/connectCancel 是本次连接尝试的上下文，CancelConnect 取消它。
	// connectCancelable 记录是否还处在可取消阶段（Apply 之前），供界面决定按钮该写
	// 「取消连接」还是「连接中…」—— 过了那个点还显示可取消、点下去没反应更让人困惑。
	connectCtx        context.Context
	connectCancel     context.CancelFunc
	connectCancelable bool

	checker *update.Checker
	pending *update.Manifest

	// pendingRestore 是「网络还原失败、还欠一次还原」的现场。
	//
	// 没有它的话，Disconnect 在 Restore 之前就把 snapshot/cfg 清空了，失败后第二次
	// 调用命中「没有运行中的连接」直接返回 nil —— 用户以为断干净了，实际上路由和
	// DNS 还指着已经拆掉的隧道，解析全超时。有它之后重复断开会真的重试。
	pendingRestore *pendingRestoreState

	orderTrace *OrderTracer
}

// pendingRestoreState 是一次没做完的网络还原。
type pendingRestoreState struct {
	snap     Snapshot
	cfg      NetConfig
	noNetCfg bool
}

type Option func(*App)

// WithNoNetCfg 让客户端只建隧道、只配好虚拟网卡，但**不动路由和 DNS**。
//
// 用途是联调：端口转发这条链路（服务端 DNAT → 隧道 → Windows 内核 → Pi Node）
// 可以在完全不碰现有网络的前提下验完。
func WithNoNetCfg() Option {
	return func(a *App) { a.noNetCfg = true }
}

// WithOrderTrace 把每个进出隧道的包（方向、槽位、五元组、seq）记到文件，
// 用来断言「同一内层流是否始终走同一个槽」——抓包工具的时间戳做不到这件事。
// 不传这个 Option 时 tracer 为 nil，数据面只多一次 nil 判断。
func WithOrderTrace(path string) Option {
	return func(a *App) {
		tr, err := NewOrderTracer(path)
		if err != nil {
			a.log.Warn("顺序追踪未能启用", "path", path, "err", err)
			return
		}
		a.orderTrace = tr
		a.log.Info("顺序追踪已启用", "file", path)
	}
}

func NewApp(inv pki.Invite, rootDir string, logger *slog.Logger, opts ...Option) *App {
	if logger == nil {
		logger = slog.Default()
	}
	// netcfg 那层不持有 logger，但「分流写了多少条路由」必须能看见，
	// 否则路由没生效就只能靠猜。
	logf = func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }
	app := &App{
		inv:       inv,
		rootDir:   rootDir,
		statePath: StatePath(rootDir),
		log:       logger,
		checker:   update.NewChecker(Version),
	}
	for _, opt := range opts {
		opt(app)
	}
	// 分流网段表：外部文件优先，内置兜底；后台每天自动刷新
	SetRouteTablePath(filepath.Join(RuntimeDir(rootDir), routeTableFileName))
	go app.routeTableRefresher()
	return app
}

// RootDir 是客户端的工作根目录（安装后就是安装目录本身）。
// config/、logs/、data/ 三个子目录都挂在它下面，见 paths.go。
func (a *App) RootDir() string { return a.rootDir }

// Log 给界面层用：界面需要记一些只有它才知道的事（比如检测到连接码被外部改了）。
func (a *App) Log() *slog.Logger { return a.log }

// Configured 表示是否已经导入过连接码。
//
// 没导入时界面照样要起来并引导用户去导入 —— 而不是程序一启动就弹个「没有连接码」
// 然后退出，那样用户装完根本看不到界面。
func (a *App) Configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configuredLocked()
}

func (a *App) configuredLocked() bool { return a.inv.Server != "" }

// HealIfNeeded 在启动时还原上次残留的网络配置。
//
// state.json 存在就代表上次没干净退出。留着坏路由和坏 DNS 会让用户整机断网，
// 而且光把程序重启也救不回来 —— 所以这一步必须在连接之前无条件执行。
func (a *App) HealIfNeeded() error {
	// 联调模式承诺过不碰网络，连自愈也不做
	if a.noNetCfg {
		return nil
	}

	a.mu.Lock()
	inv := a.inv
	a.mu.Unlock()

	snap, exists, err := LoadSnapshot(a.statePath)
	if !exists {
		return nil
	}

	// inv 在这里多半是零值：自愈跑在界面选中连接码之前（App 以「未配置」状态启动）。
	// 所以还原要用的隧道网关/云服地址一律取自快照，cfg 只是旧快照的回退值；连快照
	// 都坏了的时候，至少还能靠隧道网卡上前缀整批清扫把 /1 路由撤掉。
	cfg := BuildNetConfig(inv)

	if err != nil {
		// 快照本身坏了，DNS 原值已无从得知。至少把接管路由和隧道地址撤掉，
		// 否则用户会一直卡在「所有流量都进了一条没人读的网卡」。
		a.log.Warn("状态文件损坏，做一次保守还原（DNS 可能需要手动确认）", "err", err)
		restoreErr := Snapshot{}.Restore(cfg)
		_ = RemoveSnapshot(a.statePath)
		// 保守还原不保证 DNS 恢复正确，所以仍然要挡住新连接：让用户先重试一次
		// 断开（会走 restoreNetwork），而不是让 Connect 把半接管现场存成新快照。
		a.mu.Lock()
		a.pendingRestore = &pendingRestoreState{snap: Snapshot{}, cfg: cfg, noNetCfg: a.noNetCfg}
		if restoreErr != nil {
			a.lastError = "状态文件损坏，保守还原未完成: " + restoreErr.Error()
		} else {
			a.lastError = "状态文件损坏，DNS 可能没有恢复正确，请断开重试一次"
		}
		a.mu.Unlock()
		return err
	}

	a.log.Info("发现上次残留的网络配置，先还原", "captured_at", snap.CapturedAt)
	if err := snap.Restore(cfg); err != nil {
		// 启动自愈失败同样必须设门禁：不设的话用户随后点连接，Capture 会把当前这个
		// 「半接管」现场当成新快照存下来，把磁盘上正确的恢复信息覆盖掉 —— 之后
		// 再自愈也还原不回去了。运行时的 Disconnect 失败已有同样的门禁，这里补齐。
		a.mu.Lock()
		a.pendingRestore = &pendingRestoreState{snap: snap, cfg: cfg, noNetCfg: a.noNetCfg}
		a.lastError = "启动自愈失败：" + err.Error()
		a.mu.Unlock()
		return fmt.Errorf("还原上次的网络配置失败: %w", err)
	}
	if err := RemoveSnapshot(a.statePath); err != nil {
		a.log.Warn("删除状态文件失败", "err", err)
	}
	return nil
}

// Connect 接管网络并启动隧道。幂等：已经连上时直接返回。
//
// 这里刻意只把「检查状态 / 置标志 / 拷贝出需要的字段」放在锁里，真正的连接过程
// （探测服务端、抓网络现场、改路由）全在锁外做。早先整段都持锁，而界面每秒都会
// 调 Status() 抢同一把锁 —— 结果点几下按钮界面就「未响应」。
func (a *App) Connect() error {
	a.mu.Lock()
	switch {
	case a.disconnecting:
		a.mu.Unlock()
		return errors.New("正在断开，请稍候")
	case a.connecting:
		a.mu.Unlock()
		return errors.New("正在连接，请稍候")
	case a.running:
		a.mu.Unlock()
		return nil
	case a.pendingRestore != nil:
		// 上一次断开没把网络还原干净。这时候去抓网络现场，抓到的是「半接管」状态
		// （DNS 还指着隧道网关、/1 路由还在），存进快照就把磁盘上本来正确的恢复
		// 信息覆盖掉了 —— 之后自愈再也还原不回去。
		a.mu.Unlock()
		return errors.New("上次断开的网络还没有还原干净，请再点一次「断开连接」重试还原")
	case !a.configuredLocked():
		a.mu.Unlock()
		return errors.New("还没有连接码，请先在界面上点「导入连接码」")
	}
	a.connecting = true
	inv, noNetCfg, statePath := a.inv, a.noNetCfg, a.statePath
	// 整段连接尝试共用一个可取消的 ctx：用户点「取消连接」时，probe 以及它之后的
	// 抓网络现场、落盘都还有机会中止（接管网络那一步之后不再中止，理由见 connectSlow）。
	ctx, cancel := context.WithCancel(context.Background())
	a.connectCtx, a.connectCancel, a.connectCancelable = ctx, cancel, true
	a.mu.Unlock()

	err := a.connectSlow(inv, noNetCfg, statePath)

	a.mu.Lock()
	a.connecting = false
	a.connectCtx, a.connectCancel, a.connectCancelable = nil, nil, false
	// 用户主动取消不是故障，别记进 lastError（界面会把它当错误显示出来）
	if err != nil && !errors.Is(err, errConnectCanceled) {
		a.lastError = err.Error()
	}
	a.mu.Unlock()
	cancel()
	return err
}

// errConnectCanceled 表示这次连接尝试被用户主动取消。调用方据此把它与真正的故障区分开。
var errConnectCanceled = errors.New("连接已取消")

// CancelConnect 中断正在进行的连接尝试。
//
// 覆盖到「接管网络之前」为止：probe、抓网络现场、落盘都可以中止。Apply 一旦开始改路由
// 就不再理会取消 —— 半接管状态比让用户多等几秒糟得多。
func (a *App) CancelConnect() {
	a.mu.Lock()
	cancel := a.connectCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CanCancelConnect 报告这次连接尝试是否还处在可取消阶段（Apply 之前）。
// 界面据此决定按钮写「取消连接」还是「连接中…」。
func (a *App) CanCancelConnect() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connectCancel != nil && a.connectCancelable
}

// connectSlow 干连接这件慢活，全程不持有 a.mu。
func (a *App) connectSlow(inv pki.Invite, noNetCfg bool, statePath string) error {
	// 本次尝试的取消信号（见 Connect）。接管网络之前的每一步之间都查一次；Apply 之后
	// 不再查 —— 那时已经动过路由，半途退出会留下半接管状态，比多等一会儿糟得多。
	a.mu.Lock()
	ctx := a.connectCtx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := BuildNetConfig(inv)
	dev, err := OpenDevice()
	if err != nil {
		return err
	}

	if noNetCfg {
		if err := ConfigureAdapter(cfg); err != nil {
			_ = dev.Close()
			return fmt.Errorf("配置隧道网卡失败: %w", err)
		}
		return a.startTunnel(cfg, dev, inv, nil)
	}

	// 先确认服务端真的连得上，再动用户的网络。
	// 顺序反过来的话，服务端不可达时用户要白白经历「网络被接管 → 自检失败 → 再还原」
	// 这十几秒的断网 —— 这种体验不能有第二次。
	// 探测超时与 tunnel.handshakeTimeout 同界：探测本身也要走 dial + TLS 握手，在高丢包
	// 链路上同样会被重传拖过 12 秒，失败就白跑一次「不接管网络」的尝试，用户看到的是
	// 反复「连接失败」。
	//
	// probeServer 内部不得再压更短的硬编码超时，否则这里放宽多少都白费（曾经就是如此：
	// 外面从 12 秒改到 30 秒，里面 dial 写死 8 秒照旧先到）。
	probeCtx, cancelProbe := context.WithTimeout(ctx, probeTimeout)
	probeErr := a.probeServer(inv, probeCtx)
	cancelProbe()
	if probeErr != nil {
		_ = dev.Close()
		if errors.Is(probeErr, context.Canceled) {
			return errConnectCanceled
		}
		// 直接透传原因，不再包一层"连接服务端失败"：调用方打日志时标题已经是
		// 「连接失败」，再包一层就变成「连接失败 错误=连接服务端失败：连接被拒绝」，
		// 同一个意思说两遍，还把行撑过宽度上限。
		return probeErr
	}

	if err := ctx.Err(); err != nil {
		_ = dev.Close()
		return errConnectCanceled
	}
	snap, err := Capture(cfg)
	if err != nil {
		_ = dev.Close()
		return fmt.Errorf("抓取网络现场失败: %w", err)
	}
	// 先落盘再动网络：之后任何一步崩掉，下次启动都还能靠它把网络救回来
	if err := SaveSnapshot(statePath, snap); err != nil {
		_ = dev.Close()
		return fmt.Errorf("写状态文件失败: %w", err)
	}
	// 最后一个检查点：Apply 一旦开始改路由就必须走完，不能再中途退出。
	// 这时状态文件已经落地，取消的话要把它一起清掉，免得下次启动拿它去"自愈"一个
	// 其实从没被接管的网络。
	if err := ctx.Err(); err != nil {
		_ = RemoveSnapshot(statePath)
		_ = dev.Close()
		return errConnectCanceled
	}
	// 过了这个点就不可取消了（Apply 一开始改路由就必须走完），界面也要据此改口
	a.mu.Lock()
	a.connectCancelable = false
	a.mu.Unlock()
	if err := snap.Apply(cfg); err != nil {
		// 网络已经改了一半，立刻还原，别把用户留在半截状态
		restoreErr := snap.Restore(cfg)
		_ = dev.Close()
		if restoreErr != nil {
			// 还原也没成功：留住状态文件让下次启动接着自愈。删掉它等于把唯一的
			// 线索也丢了，用户就只能手动改 DNS —— 与 Disconnect 的处理保持一致。
			return fmt.Errorf("接管网络失败，且还原未完全成功（状态文件已保留，下次启动会重试）: %v / %w",
				restoreErr, err)
		}
		_ = RemoveSnapshot(statePath)
		return fmt.Errorf("接管网络失败（已还原）: %w", err)
	}

	return a.startTunnel(cfg, dev, inv, &snap)
}

// startTunnel 登记隧道并拉起循环。只在写状态字段时短暂持锁。
func (a *App) startTunnel(cfg NetConfig, dev Device, inv pki.Invite, snap *Snapshot) error {
	ctx, cancel := context.WithCancel(context.Background())
	tunnel := NewTunnel(inv, dev, a.log)
	tunnel.Trace = a.orderTrace
	// 适配器被外力干掉时，重拨 TCP 救不了，要走一次完整的断开+连接
	// （重开设备、重新接管网络）。它是异步的：先 Disconnect 掉本循环，
	// 再走一遍 connectSlow 的全流程。
	tunnel.RebuildDevice = func() {
		go func() {
			a.Disconnect()
			_ = a.Connect()
		}()
	}

	a.mu.Lock()
	a.cfg = cfg
	a.dev = dev
	a.tunnel = tunnel
	a.cancel = cancel
	a.running = true
	a.lastError = ""
	if snap != nil {
		a.snapshot = *snap
	}
	noNetCfg := a.noNetCfg
	a.mu.Unlock()

	go func() {
		if err := tunnel.Run(ctx); err != nil {
			a.log.Warn("隧道循环退出", connLogAttrs(err, nil)...)
		}
	}()
	// 定期把运行状态写进日志，给「用着用着没网」留现场。
	//
	// 这类故障（20G 上下、不固定阈值）通常是某类资源随流量累积：goroutine 泄漏、
	// 内存增长、上行队列积压。等用户报「没网」时，单看那一刻什么都判断不了 ——
	// 所以每 10 分钟落一条，事后翻日志就能看出是哪一项在单调上涨。
	go a.watchRunStats(ctx, tunnel)
	// 这里不再打「已连接」：此刻隧道循环刚启动，TLS 握手和 HELLO_ACK 都还没完成。
	// 真正的连接结果由 tunnel.session 在握手成功后打一条，避免同一件事报两遍
	// （而且先报一次乐观的「已连接」，握手失败了还要再报一次失败，看着自相矛盾）。

	// 只有真接管了网络才自检：联调模式没动用户网络，出不去也不该由我们背
	if noNetCfg || snap == nil {
		return nil
	}

	// 网络出口守护：换 Wi-Fi/插网线/睡眠唤醒时自动迁移绕行路由、DNS、
	// 国内分流，并唤醒隧道立刻重连。失败不阻断连接，只是降级为旧行为
	// （出口变化后需手动断开重连）。
	w, err := startSessionWatcher(ctx, *snap, cfg, a.updateSnapshot, tunnel.Kick)
	if err != nil {
		a.log.Warn("网络变化监听启动失败", "err", err)
	} else {
		a.mu.Lock()
		a.watcher = w
		a.mu.Unlock()
	}

	go a.watchHealth(ctx)
	return nil
}

// updateSnapshot 供 watcher 在迁移网络出口时更新接管现场，
// 保证断开还原时用的是最新状态。
//
// 必须同时落盘：只改内存的话，程序被强杀/掉电后启动自愈拿到的是旧网卡（迁移前那张）
// 的快照，没法准确恢复新网卡的原 DNS 和路由 —— 用户表现为重启后解析全超时。这正是
// 报告 4.1 复现的那条。
//
// 返回 error 是给 watcher 用的：落盘失败时它不能把这次迁移当成已提交（否则下次出口
// 变化就永远不会重试，磁盘上永远留着旧现场）。
func (a *App) updateSnapshot(s Snapshot) error {
	a.mu.Lock()
	a.snapshot = s
	a.mu.Unlock()

	if err := SaveSnapshot(a.statePath, s); err != nil {
		a.log.Error("迁移后的网络快照写盘失败，崩溃恢复可能用到旧现场", "err", err)
		return err
	}
	return nil
}

// routeTableRefresher 让分流网段表长期保持最新：连接成功 1 分钟后拉一次，
// 之后每 24 小时一次；失败半小时后重试。拉取在后台协程执行，启动与
// 连接路径零影响。拉到新表且处于连接状态时，热重铺国内分流；
// 不在连接状态则落盘等下次连接生效。
func (a *App) routeTableRefresher() {
	a.waitConnected()
	time.Sleep(time.Minute)
	for {
		updated := fetchAndStoreRouteTable()
		if updated {
			a.hotApplyRouteTable()
		}
		if updated {
			time.Sleep(24 * time.Hour)
		} else {
			time.Sleep(30 * time.Minute)
		}
	}
}

// waitConnected 阻塞到隧道进入连接状态（轮询，1 秒粒度）。
func (a *App) waitConnected() {
	for !a.runningFast() {
		time.Sleep(time.Second)
	}
}

func (a *App) runningFast() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// hotApplyRouteTable 连接状态下按最新网段表热重铺国内分流。
func (a *App) hotApplyRouteTable() {
	a.mu.Lock()
	running := a.running
	gw := a.snapshot.DefaultGateway
	ifIndex := a.snapshot.DefaultIfIndex
	table := ActiveCNRoutes()
	a.mu.Unlock()
	if !running || gw == "" || ifIndex == 0 || refreshSplitRoutesHook == nil {
		return
	}
	refreshSplitRoutesHook(gw, ifIndex, table)
}

// watchHealth 在接管网络后确认「真的能出去」，失败就自动回退。
//
// 为什么必须有这一步 —— 实测踩过一次：隧道建好了、/1 路由也正确挂上了，但出口的
// DNS 查不通（那次拿 WSL 当服务端，出口等于本机宽带，8.8.8.8 压根连不上）。
// 结果用户面对的是一台「显示已连接、却什么都打不开、还没有任何提示」的机器。
// 宁可明确报「连不上」并把网络还原，也不要留下这种状态。
func (a *App) watchHealth(ctx context.Context) {
	// 等隧道真正建立，而不是固定睡 3 秒：TLS 握手 + HELLO_ACK 在丢包链路上可能超过
	// 3 秒（实测晚高峰能到 7 秒），固定睡眠会把一条好隧道误判成连接失败。
	a.mu.Lock()
	tl := a.tunnel
	a.mu.Unlock()
	if tl == nil {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for !tl.Stats.Connected.Load() {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			// 等不到握手就是连接失败。此时网络已经被接管（路由指向隧道），必须还原 ——
			// 直接返回会把用户留在一个「隧道没通、路由还指着隧道」的断网状态里。
			a.log.Error("等待隧道建立超时，判定连接失败并还原网络", "waited", 30*time.Second)
			a.mu.Lock()
			a.lastError = "隧道 30 秒内未建立"
			a.mu.Unlock()
			if derr := a.Disconnect(); derr != nil {
				a.log.Error("还原网络失败，状态文件已保留，下次启动会重试", "err", derr)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}

	// 握手成功后再给路由与 DNS 一点落定时间
	select {
	case <-ctx.Done():
		return
	case <-time.After(1500 * time.Millisecond):
	}

	// 自检失败要重试：链路抖一下不该直接拆掉一条本来能用的隧道。
	// 但整个自检要有总预算 —— 等握手 30s + 3 轮 × (2 DNS × 2 域名 × 12s) 最坏能拖到
	// 两分多钟，用户点完连接等两分钟才看到失败是不可接受的。
	healthDeadline := time.Now().Add(60 * time.Second)
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if time.Now().After(healthDeadline) {
			err = errors.New("自检总预算（60 秒）耗尽")
			break
		}
		err = a.healthCheck(ctx)
		if err == nil {
			a.log.Info("出网自检通过")
			return
		}
		if ctx.Err() != nil {
			return
		}
		if attempt < 3 {
			a.log.Warn("出网自检失败，稍后重试", "attempt", attempt, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}

	a.log.Error("出网自检失败，自动断开并还原网络", connLogAttrs(err, nil)...)
	a.mu.Lock()
	a.lastError = "出网自检失败：" + err.Error()
	a.mu.Unlock()

	if derr := a.Disconnect(); derr != nil {
		a.log.Error("自检失败后还原网络也失败，状态文件已保留，下次启动会重试", "err", derr)
	}
}

// probeServer 只做一次 TLS 握手，用来确认「服务端可达且证书可信」。
//
// 它存在的意义是保住顺序：连不上服务端时，一点都不要碰用户的网络。
// 邀请码由调用方传进来 —— 这个方法在锁外跑，不能去读 a.inv。
func (a *App) probeServer(inv pki.Invite, ctx context.Context) error {
	tlsCfg, err := pki.ClientTLSConfig(inv)
	if err != nil {
		return err
	}
	// 超时完全交给调用方的 ctx，这里不再压一层更短的硬编码。
	//
	// 原来 dial 写死 8 秒、握手写死 10 秒，结果外面把探测预算从 12 秒放宽到 30 秒毫无
	// 作用：先到的仍是那 8 秒，报错也仍是 dial i/o timeout（Dialer.Timeout 报 i/o timeout，
	// context 超时报 context deadline exceeded —— 看错误文案就能分辨是谁掐的）。
	d := net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", inv.Server)
	if err != nil {
		// 只返回原因，不拼地址：调用方会加上「连接服务端失败」，状态栏也已经显示
		// 服务端地址了。带上地址这条日志就超出一行，界面里折成两截，还得靠缩进
		// 区分续行 —— 一行能说完的事别拆成两行。
		return fmt.Errorf("%s", humanReason(err))
	}
	defer raw.Close()

	conn := tls.Client(raw, tlsCfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		if reason := connBrokenReason(err); reason != "" {
			return fmt.Errorf("TLS 握手失败：%s", reason)
		}
		return fmt.Errorf("TLS 握手失败（证书或网络问题）: %w", err)
	}
	return nil
}

// healthCheck 探两件事：隧道内通不通，以及下发的 DNS 能不能真的解析。
//
// 调用方须保证此刻隧道仍在运行；读 a.inv 是安全的（不持锁读单个字段，
// 而 inv 只会被 UpdateInvite 换掉，换的时候会先断开）。
func (a *App) healthCheck(ctx context.Context) error {
	a.mu.Lock()
	inv := a.inv
	a.mu.Unlock()

	// 第一级：连服务端在隧道里的监听地址 —— 这条一定走隧道，不受出口影响
	// 这里的超时按跨境链路的实际成本给：隧道内 RTT 就是链路 RTT（晚高峰 250ms 且会重传），
	// 原来给的 5 秒只够 SYN 重传两次，一旦赶上就要误判成「出网自检失败」—— 而那条路的代价
	// 是把好好的隧道断掉、再还原用户网络。宁宽勿紧。
	tunnelAddr := net.JoinHostPort(inv.Gateway, tunnelPortOf(inv))
	if err := probeTCP(ctx, tunnelAddr, 15*time.Second); err != nil {
		return fmt.Errorf("隧道内不通（%s）：%s", tunnelAddr, humanReason(err))
	}

	// 第二级：用下发的 DNS 真解析一次。这正是整套方案存在的理由 ——
	// DNS 查不通，用户看到的就是「连上了但网页打不开」。
	//
	// 多 DNS × 多域名：只试第一个 DNS + 固定 www.baidu.com 时，单个 DNS 短暂不可用
	// 或那个域名恰好被过滤，就会误判成"出网失败"并拆掉一条本来能用的隧道 —— 而拆隧道
	// 的代价是断网 + 还原，远比多试几次贵。
	dnsServers := inv.DNS
	if len(dnsServers) == 0 {
		dnsServers = []string{"8.8.8.8"}
	}
	probeDomains := []string{"www.baidu.com", "www.qq.com"}
	var lastErr error
	for _, srv := range dnsServers {
		for _, domain := range probeDomains {
			resolver := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 10 * time.Second}
					return d.DialContext(ctx, network, net.JoinHostPort(srv, "53"))
				},
			}
			lookupCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			_, err := resolver.LookupHost(lookupCtx, domain)
			cancel()
			if err == nil {
				return nil
			}
			lastErr = err
		}
	}
	return fmt.Errorf("DNS 解析失败（试过 %d 个服务器 × %d 个域名）：%s",
		len(dnsServers), len(probeDomains), humanReason(lastErr))
}

// humanReason 把底层错误翻成一个短中文短语。
//
// 往界面上写的错误必须是中文：`connectex: No connection could be made because the
// target machine actively refused it.` 这种原文对用户毫无意义，只会让人以为程序出错。
//
// 兜底也刻意用短词而不是「原因不明（详细错误见调试日志）」：日志区一行只有约 54 个
// 字符宽（中文算两格），长句子会被折成两行，反而更乱。原始错误在日志文件里能查到。
func humanReason(err error) string {
	if err == nil {
		return "连接失败"
	}
	if r := connBrokenReason(err); r != "" {
		return r
	}
	return "连接失败"
}

func probeTCP(ctx context.Context, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func tunnelPortOf(inv pki.Invite) string {
	_, port, err := net.SplitHostPort(inv.Server)
	if err != nil {
		return "62233"
	}
	return port
}

// Disconnect 停隧道并把网络还原回去。幂等：没连上时直接返回。
//
// 跟 Connect 互斥：连接进行中点断开会被拒绝 —— 旧版这里存在竞态，断开的还原
// 和新连接并发跑，批量删路由可能把刚加的新路由删掉。
//
// 先把内部状态清干净（界面立刻就能显示「未连接」），再在锁外慢慢还原。
// 整段持锁的话，还原路由那几秒界面是死的。
func (a *App) Disconnect() error {
	a.mu.Lock()
	switch {
	case a.connecting:
		a.mu.Unlock()
		return errors.New("正在连接，请稍候")
	case a.disconnecting:
		a.mu.Unlock()
		// 不能返回 nil：调用方（尤其 ApplyUpdate）会据此认为「网络已经还原干净」，
		// 于是在还原还没跑完时就替换程序 —— 旧进程带着半接管的网络消失、新进程还没
		// 起来，用户直接断网。这与 Connect 在 connecting 时的处理保持一致。
		return errors.New("正在断开，请稍候")
	case !a.running && a.dev == nil:
		// 没有运行中的连接。但上次还原可能失败过（路由/DNS 没撤干净）——
		// 那种时候必须重试，不能因为「看起来没连」就假装成功，否则用户点几次
		// 断开都拿不到真正的结果，网络也永远留着隧道残留。
		pending := a.pendingRestore
		if pending == nil {
			a.mu.Unlock()
			return nil
		}
		a.disconnecting = true
		a.mu.Unlock()

		err := a.restoreNetwork(pending.snap, pending.cfg, pending.noNetCfg)
		a.mu.Lock()
		a.disconnecting = false
		if err == nil {
			a.pendingRestore = nil
			a.lastError = ""
			a.log.Info("待还原的网络配置已补还原完成")
		} else {
			a.lastError = err.Error()
		}
		a.mu.Unlock()
		return err
	}
	a.disconnecting = true
	cancel, dev, watcher := a.cancel, a.dev, a.watcher
	a.cancel, a.dev, a.tunnel, a.watcher = nil, nil, nil, nil
	a.running = false
	a.mu.Unlock()

	// 先停 watcher：它可能正在把快照迁移到新出口，停完再取才是终态。
	if watcher != nil {
		watcher.Stop()
		// Stop 已经等 reconcile 退出，这里没有并发。
		//
		// 停完还要看一眼有没有未完成的迁移：那笔 pending 指向的可能是另一张网卡
		// （A→B 失败后用户切到 C 又立刻断开），它的隧道 DNS 和云服绕行路由不在
		// 最终快照里 —— 不在这里清就会一直留在那张网卡上。
		watcher.revertPending()
	}
	a.mu.Lock()
	snap, cfg, noNetCfg := a.snapshot, a.cfg, a.noNetCfg
	a.snapshot, a.cfg = Snapshot{}, NetConfig{}
	a.mu.Unlock()

	// 停掉搬运协程
	if cancel != nil {
		cancel()
	}
	if dev != nil {
		// 给搬运 goroutine 一点时间退出，免得一边还原网络一边往里写包
		time.Sleep(150 * time.Millisecond)
		_ = dev.Close()
	}

	restoreErr := a.restoreNetwork(snap, cfg, noNetCfg)

	a.mu.Lock()
	a.disconnecting = false
	if restoreErr != nil {
		// 保留现场：下一次 Disconnect 会拿它重试，而不是当成「已经断开」
		a.pendingRestore = &pendingRestoreState{snap: snap, cfg: cfg, noNetCfg: noNetCfg}
		a.lastError = restoreErr.Error()
	} else {
		a.pendingRestore = nil
	}
	a.mu.Unlock()
	return restoreErr
}

// restoreNetwork 把接管过的网络还回去。失败时调用方负责保留现场以便重试。
func (a *App) restoreNetwork(snap Snapshot, cfg NetConfig, noNetCfg bool) error {
	// 联调模式本来就没动过路由和 DNS，没什么可还原的
	if noNetCfg {
		a.log.Info("已断开")
		return nil
	}
	if err := snap.Restore(cfg); err != nil {
		// 留着 state.json：下次启动自愈还会再试一次
		return fmt.Errorf("还原网络配置失败: %w", err)
	}
	if err := RemoveSnapshot(a.statePath); err != nil {
		a.log.Warn("删除状态文件失败", "err", err)
	}
	a.log.Info("已断开")
	return nil
}

// probeTimeout 是探测服务端的预算。probeServer 内部不再压更短的硬编码超时，所以它就是
// 探测的实际上限（见那里的注释：曾经内层 dial 写死 8 秒，把外层的放宽完全架空）。
const probeTimeout = 30 * time.Second

// resolveTimeout 是连接码里写域名时那次解析的上限。放在这里而不是平台文件里，是为了让
// 超时预算的总账在任何平台都算得出来（Windows 专用文件里的常量别处看不见）。
const resolveTimeout = 5 * time.Second

// shutdownWait 是退出时等待「正在进行的连接/断开」收尾的上限。
//
// 要盖住连接流程里最长的三段：probeTimeout、Capture 里那次域名解析（连接码写域名时才会
// 走到），外加接管网络那几秒。留 45 秒而不是贴着 35 秒算，是因为超时的代价不对称：等久了
// 只是关窗慢一点，等漏了则会让退出时网络停在半接管状态 —— 那正是这个常量要防的。
const shutdownWait = 45 * time.Second

// Shutdown 是「退出程序」专用的收尾：等正在进行的连接/断开结束，再走一次还原。
//
// 不能直接复用 Disconnect —— 它在 connecting 时直接返回错误、在 disconnecting 时
// 直接返回 nil，而调用方（托盘退出）拿到返回就会关窗口退进程，于是网络停在半接管
// 状态，用户整机断网直到下次启动。退出这条路上，网络优先于速度。
func (a *App) Shutdown() error {
	deadline := time.Now().Add(shutdownWait)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		busy := a.connecting || a.disconnecting
		a.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a.Disconnect()
}

type AppStatus struct {
	Running bool
	Online  bool
	// DataPathBroken 表示「控制连接在、但 UDP 数据面已经不通」。
	//
	// 单独一个字段而不是悄悄把 Online 置 false：那会让人以为掉线了，其实只是数据面
	// 坏了。UI 要显示成「数据通道异常」，用户才知道该刷新连接码还是查防火墙。
	DataPathBroken bool
	Connecting     bool
	Disconnecting  bool
	TunnelIP       string
	Server         string
	RTT            time.Duration
	RxBytes        uint64
	TxBytes        uint64
	LastError      string
}

func (a *App) Status() AppStatus {
	a.mu.Lock()
	defer a.mu.Unlock()

	tunnel := a.tunnel
	connected := tunnel != nil && tunnel.Stats.Connected.Load()
	// UDP 模式下控制走 TCP，光看 Connected 会一直显示在线而业务包早就发不出去了。
	// 数据面不健康时明确报出来，别让它假装正常。
	dataPathBroken := false
	if connected && tunnel != nil {
		if enabled, healthy := tunnel.UDPState(); enabled && !healthy {
			dataPathBroken = true
		}
	}

	st := AppStatus{
		Running:        a.running,
		Online:         connected && !dataPathBroken,
		DataPathBroken: dataPathBroken,
		Connecting:     a.connecting,
		Disconnecting:  a.disconnecting,
		Server:         a.inv.Server,
		TunnelIP:       a.cfg.TunnelIP,
		LastError:      a.lastError,
	}
	if a.tunnel != nil {
		st.RTT = a.tunnel.Stats.RTT()
		st.RxBytes = a.tunnel.Stats.RxBytes.Load()
		st.TxBytes = a.tunnel.Stats.TxBytes.Load()
	}
	return st
}

func (a *App) Invite() pki.Invite {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inv
}

// UpdateInvite 换一个连接码。source 是给人看的来源描述（文件名或「手动导入」），
// 会跟着「检测到新的连接码」日志一起记录。必须先断开：旧连接码对应的隧道地址和
// 路由要还原干净，否则新旧两套配置会叠在一起。
func (a *App) UpdateInvite(inv pki.Invite, source string) error {
	if err := a.Disconnect(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inv = inv
	a.invSource = source
	// 日志随配置文件切会话：pinode.antapp → logs\pinode.log，新会话清空重来
	SwitchLogSession(source)
	return nil
}

// watchRunStats 每 10 分钟记录一次运行状态，供「用着用着没网」定位用。
//
// 只记可能单调上涨的量：goroutine 数、堆内存、上行队列丢弃次数、累计流量。正常情况
// 下它们应该长期持平（流量除外）；哪一项一直涨，问题就在那一块。
func (a *App) watchRunStats(ctx context.Context, tun *Tunnel) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			a.log.Info("运行状态",
				"goroutines", runtime.NumGoroutine(),
				"heap_mb", m.HeapAlloc/1024/1024,
				"rx_mb", tun.Stats.RxBytes.Load()/1024/1024,
				"tx_mb", tun.Stats.TxBytes.Load()/1024/1024,
				"pkt_drops", tun.pktDrops.Load())
		}
	}
}

// CurrentInvite 返回当前生效的连接码（未导入时为零值）。给界面比对候选用。
func (a *App) CurrentInvite() pki.Invite {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inv
}

// CurrentSource 返回当前连接码的来源描述（文件名或「剪贴板粘贴」），日志和列表标注用。
func (a *App) CurrentSource() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.invSource
}

// ---------- 在线更新 ----------

// CheckUpdate 询问更新源。返回 nil 表示已经是最新。
//
// 部分源失败不算错（有备源就是干这个的）；只有全部源都失败才报错。
func (a *App) CheckUpdate(ctx context.Context, quiet bool) (*update.Manifest, error) {
	m, errs := a.checker.Check(ctx)
	if m == nil {
		if len(errs) > 0 && len(errs) == len(a.checker.Sources) {
			// 只给一句结论，不贴原始错误：GitHub 的下载链接是 Azure Blob 带 SAS
			// 签名的 URL，一长串 `?sp=r&sig=...` 对用户毫无意义，还把日志区撑爆。
			// 看 sources 数量就能判断是本地出网还是某个源单独挂了。
			a.log.Warn("更新检查失败（更新源不可达），本次跳过", "sources", len(errs))
			return nil, fmt.Errorf("所有更新源都不可用：%v", errs[0])
		}
		if !quiet {
			// 周期性自动检测成功且无新版本是常态，不写日志——否则几小时就把
			// 有用的连接/错误日志顶出缓冲区。手动检查仍有弹窗反馈。
			a.log.Info("已是最新版本", "version", Version)
		}
		return nil, nil
	}

	a.log.Info("发现新版本", "current", Version, "latest", m.Version)
	a.mu.Lock()
	a.pending = m
	a.mu.Unlock()
	return m, nil
}

// PendingUpdate 返回最近一次检查到的新版本，没有则为 nil。
func (a *App) PendingUpdate() *update.Manifest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending
}

// DownloadUpdate 下载并校验更新包，返回落地路径。
func (a *App) DownloadUpdate(ctx context.Context, m *update.Manifest) (string, error) {
	dir := filepath.Join(RuntimeDir(a.rootDir), "update")
	var lastLog time.Time

	path, err := a.checker.Download(ctx, m, dir, func(done, total int64) {
		// 进度只偶尔记一条，否则几 MB 下来能把日志刷满
		if time.Since(lastLog) < 3*time.Second {
			return
		}
		lastLog = time.Now()
		if total > 0 {
			a.log.Info("下载更新包", "进度", fmt.Sprintf("%.0f%%", float64(done)/float64(total)*100))
		} else {
			a.log.Info("下载更新包", "已下载", done)
		}
	})
	if err != nil {
		a.log.Error("更新包下载失败", "err", err)
		return "", err
	}
	a.log.Info("更新包已下载并通过校验", "file", path)
	return path, nil
}

// ApplyUpdate 断开隧道、还原网络，然后把新版本替换上去。
//
// 顺序不能反：先还原网络再替换文件。旧进程要是带着「接管中」的网络直接消失，
// 用户就卡在断网状态，而新进程还没起来。
//
// 成功返回后调用方必须立刻退出自己 —— 磁盘上的文件名已经被新版占用了。
func (a *App) ApplyUpdate(newExe string) error {
	if err := a.Disconnect(); err != nil {
		// 还原失败还继续替换的话：旧进程带着半接管的网络消失、新进程还没起来，
		// 用户就直接断网了。宁可这次不更新，让用户手动断开后重试。
		return fmt.Errorf("更新前还原网络失败，已取消更新: %w", err)
	}
	a.log.Info("替换程序并重启", "new", newExe)
	return update.Apply(newExe)
}
