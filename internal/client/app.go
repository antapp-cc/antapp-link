package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/update"
)

// App 把「连接 / 断开 / 自愈 / 状态」串起来。托盘只是它的一个界面，
// 所以这里不含任何界面代码，逻辑可以单独测。
type App struct {
	inv       pki.Invite
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

	checker *update.Checker
	pending *update.Manifest
}

type Option func(*App)

// WithNoNetCfg 让客户端只建隧道、只配好虚拟网卡，但**不动路由和 DNS**。
//
// 用途是联调：端口转发这条链路（服务端 DNAT → 隧道 → Windows 内核 → Pi Node）
// 可以在完全不碰现有网络的前提下验完。
func WithNoNetCfg() Option {
	return func(a *App) { a.noNetCfg = true }
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

	if err != nil {
		// 快照本身坏了，DNS 原值已无从得知。至少把接管路由和隧道地址撤掉，
		// 否则用户会一直卡在「所有流量都进了一条没人读的网卡」。
		a.log.Warn("状态文件损坏，做一次保守还原（DNS 可能需要手动确认）", "err", err)
		_ = Snapshot{}.Restore(BuildNetConfig(inv))
		_ = RemoveSnapshot(a.statePath)
		return err
	}

	a.log.Info("发现上次残留的网络配置，先还原", "captured_at", snap.CapturedAt)
	if err := snap.Restore(BuildNetConfig(inv)); err != nil {
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
	case !a.configuredLocked():
		a.mu.Unlock()
		return errors.New("还没有连接码，请先在界面上点「导入连接码」")
	}
	a.connecting = true
	inv, noNetCfg, statePath := a.inv, a.noNetCfg, a.statePath
	a.mu.Unlock()

	err := a.connectSlow(inv, noNetCfg, statePath)

	a.mu.Lock()
	a.connecting = false
	if err != nil {
		a.lastError = err.Error()
	}
	a.mu.Unlock()
	return err
}

// connectSlow 干连接这件慢活，全程不持有 a.mu。
func (a *App) connectSlow(inv pki.Invite, noNetCfg bool, statePath string) error {
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
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 12*time.Second)
	probeErr := a.probeServer(inv, probeCtx)
	cancelProbe()
	if probeErr != nil {
		_ = dev.Close()
		return fmt.Errorf("连接服务端失败，未改动网络: %w", probeErr)
	}

	snap, err := Capture(cfg.ServerIP)
	if err != nil {
		_ = dev.Close()
		return fmt.Errorf("抓取网络现场失败: %w", err)
	}
	// 先落盘再动网络：之后任何一步崩掉，下次启动都还能靠它把网络救回来
	if err := SaveSnapshot(statePath, snap); err != nil {
		_ = dev.Close()
		return fmt.Errorf("写状态文件失败: %w", err)
	}
	if err := snap.Apply(cfg); err != nil {
		// 网络已经改了一半，立刻还原，别把用户留在半截状态
		_ = snap.Restore(cfg)
		_ = dev.Close()
		_ = RemoveSnapshot(statePath)
		return fmt.Errorf("接管网络失败（已还原）: %w", err)
	}

	return a.startTunnel(cfg, dev, inv, &snap)
}

// startTunnel 登记隧道并拉起循环。只在写状态字段时短暂持锁。
func (a *App) startTunnel(cfg NetConfig, dev Device, inv pki.Invite, snap *Snapshot) error {
	ctx, cancel := context.WithCancel(context.Background())
	tunnel := NewTunnel(inv, dev, a.log)
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
			a.log.Warn("隧道循环退出", "err", err)
		}
	}()
	a.log.Info("已连接", "server", inv.Server, "tunnel_ip", cfg.TunnelIP)

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
func (a *App) updateSnapshot(s Snapshot) {
	a.mu.Lock()
	a.snapshot = s
	a.mu.Unlock()
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
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}

	err := a.healthCheck(ctx)
	if err == nil {
		a.log.Info("出网自检通过")
		return
	}
	if ctx.Err() != nil {
		// 用户自己断开或者换了连接码，不是故障
		return
	}

	a.log.Error("出网自检失败，自动断开并还原网络", "err", err)
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
	d := net.Dialer{Timeout: 8 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", inv.Server)
	if err != nil {
		return fmt.Errorf("连接 %s: %w", inv.Server, err)
	}
	defer raw.Close()

	conn := tls.Client(raw, tlsCfg)
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
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
	tunnelAddr := net.JoinHostPort(inv.Gateway, tunnelPortOf(inv))
	if err := probeTCP(ctx, tunnelAddr, 5*time.Second); err != nil {
		return fmt.Errorf("隧道内不通（%s）: %w", tunnelAddr, err)
	}

	// 第二级：用下发的 DNS 真解析一次。这正是整套方案存在的理由 ——
	// DNS 查不通，用户看到的就是「连上了但网页打不开」。
	dnsServer := "8.8.8.8"
	if len(inv.DNS) > 0 {
		dnsServer = inv.DNS[0]
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(dnsServer, "53"))
		},
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := resolver.LookupHost(lookupCtx, "www.baidu.com"); err != nil {
		return fmt.Errorf("DNS %s 解析不了域名: %w", dnsServer, err)
	}
	return nil
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
		return nil // 已经在断开了，别重复跑
	case !a.running && a.dev == nil:
		a.mu.Unlock()
		return nil
	}
	a.disconnecting = true
	cancel, dev, watcher := a.cancel, a.dev, a.watcher
	a.cancel, a.dev, a.tunnel, a.watcher = nil, nil, nil, nil
	a.running = false
	a.mu.Unlock()

	// 先停 watcher：它可能正在把快照迁移到新出口，停完再取才是终态。
	if watcher != nil {
		watcher.Stop()
	}
	a.mu.Lock()
	snap, cfg, noNetCfg := a.snapshot, a.cfg, a.noNetCfg
	a.snapshot, a.cfg = Snapshot{}, NetConfig{}
	a.mu.Unlock()

	restoreErr := func() error {
		if cancel != nil {
			cancel()
		}
		if dev != nil {
			// 给搬运 goroutine 一点时间退出，免得一边还原网络一边往里写包
			time.Sleep(150 * time.Millisecond)
			_ = dev.Close()
		}

		// 联调模式本来就没动过路由和 DNS，没什么可还原的
		if noNetCfg {
			a.log.Info("已断开")
			return nil
		}

		if err := snap.Restore(cfg); err != nil {
			// 还原失败就留着 state.json，让下次启动继续尝试自愈
			return fmt.Errorf("还原网络配置失败: %w", err)
		}
		if err := RemoveSnapshot(a.statePath); err != nil {
			a.log.Warn("删除状态文件失败", "err", err)
		}
		a.log.Info("已断开")
		return nil
	}()

	a.mu.Lock()
	a.disconnecting = false
	if restoreErr != nil {
		a.lastError = restoreErr.Error()
	}
	a.mu.Unlock()
	return restoreErr
}

type AppStatus struct {
	Running       bool
	Online        bool
	Connecting    bool
	Disconnecting bool
	TunnelIP      string
	Server        string
	RTT           time.Duration
	RxBytes       uint64
	TxBytes       uint64
	LastError     string
}

func (a *App) Status() AppStatus {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := AppStatus{
		Running:       a.running,
		Online:        a.tunnel != nil && a.tunnel.Stats.Connected.Load(),
		Connecting:    a.connecting,
		Disconnecting: a.disconnecting,
		Server:        a.inv.Server,
		TunnelIP:      a.cfg.TunnelIP,
		LastError:     a.lastError,
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

// UpdateInvite 换一个连接码。必须先断开：旧连接码对应的隧道地址和路由要还原干净，
// 否则新旧两套配置会叠在一起。
func (a *App) UpdateInvite(inv pki.Invite) error {
	if err := a.Disconnect(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inv = inv
	return nil
}

// ---------- 在线更新 ----------

// CheckUpdate 询问更新源。返回 nil 表示已经是最新。
//
// 部分源失败不算错（有备源就是干这个的）；只有全部源都失败才报错。
func (a *App) CheckUpdate(ctx context.Context) (*update.Manifest, error) {
	m, errs := a.checker.Check(ctx)
	if m == nil {
		if len(errs) > 0 && len(errs) == len(a.checker.Sources) {
			for _, e := range errs {
				a.log.Warn("更新源不可用", "err", e)
			}
			return nil, fmt.Errorf("所有更新源都不可用：%v", errs[0])
		}
		a.log.Info("已是最新版本", "version", Version)
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
		a.log.Warn("更新前还原网络失败，仍然继续替换", "err", err)
	}
	a.log.Info("替换程序并重启", "new", newExe)
	return update.Apply(newExe)
}
