package client

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// App 把「连接 / 断开 / 自愈 / 状态」串起来。托盘只是它的一个界面，
// 所以这里不含任何界面代码，逻辑可以单独测。
type App struct {
	inv       pki.Invite
	dataDir   string
	statePath string
	log       *slog.Logger

	mu        sync.Mutex
	snapshot  Snapshot
	cfg       NetConfig
	dev       Device
	tunnel    *Tunnel
	cancel    context.CancelFunc
	running   bool
	lastError string
}

func NewApp(inv pki.Invite, dataDir string, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	return &App{
		inv:       inv,
		dataDir:   dataDir,
		statePath: StatePath(dataDir),
		log:       logger,
	}
}

func (a *App) DataDir() string { return a.dataDir }

// HealIfNeeded 在启动时还原上次残留的网络配置。
//
// state.json 存在就代表上次没干净退出。留着坏路由和坏 DNS 会让用户整机断网，
// 而且光把程序重启也救不回来 —— 所以这一步必须在连接之前无条件执行。
func (a *App) HealIfNeeded() error {
	snap, exists, err := LoadSnapshot(a.statePath)
	if !exists {
		return nil
	}

	if err != nil {
		// 快照本身坏了，DNS 原值已无从得知。至少把默认路由和隧道地址撤掉，
		// 否则用户会一直卡在「所有流量都进了一条没人读的网卡」。
		a.log.Warn("状态文件损坏，做一次保守还原（DNS 可能需要手动确认）", "err", err)
		_ = Snapshot{}.Restore(BuildNetConfig(a.inv, ""))
		_ = RemoveSnapshot(a.statePath)
		return err
	}

	a.log.Info("发现上次残留的网络配置，先还原", "captured_at", snap.CapturedAt)
	if err := snap.Restore(BuildNetConfig(a.inv, snap.DefaultGateway)); err != nil {
		return fmt.Errorf("还原上次的网络配置失败: %w", err)
	}
	if err := RemoveSnapshot(a.statePath); err != nil {
		a.log.Warn("删除状态文件失败", "err", err)
	}
	return nil
}

// Connect 接管网络并启动隧道。幂等：已经连上时直接返回。
func (a *App) Connect() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return nil
	}

	snap, err := Capture()
	if err != nil {
		return fmt.Errorf("抓取网络现场失败: %w", err)
	}
	// 先落盘再动网络：之后任何一步崩掉，下次启动都还能靠它把网络救回来
	if err := SaveSnapshot(a.statePath, snap); err != nil {
		return fmt.Errorf("写状态文件失败: %w", err)
	}
	cfg := BuildNetConfig(a.inv, snap.DefaultGateway)

	dev, err := OpenDevice()
	if err != nil {
		_ = RemoveSnapshot(a.statePath)
		a.lastError = err.Error()
		return err
	}

	if err := snap.Apply(cfg); err != nil {
		// 网络已经改了一半，立刻还原，别把用户留在半截状态
		_ = snap.Restore(cfg)
		_ = dev.Close()
		_ = RemoveSnapshot(a.statePath)
		a.lastError = err.Error()
		return fmt.Errorf("接管网络失败（已还原）: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	tunnel := NewTunnel(a.inv, dev, a.log)

	a.snapshot = snap
	a.cfg = cfg
	a.dev = dev
	a.tunnel = tunnel
	a.cancel = cancel
	a.running = true
	a.lastError = ""

	go func() {
		if err := tunnel.Run(ctx); err != nil {
			a.log.Warn("隧道循环退出", "err", err)
		}
	}()
	a.log.Info("已连接", "server", a.inv.Server, "tunnel_ip", cfg.TunnelIP)
	return nil
}

// Disconnect 停隧道并把网络还原回去。幂等：没连上时直接返回。
func (a *App) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running && a.dev == nil {
		return nil
	}

	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if a.dev != nil {
		// 给搬运 goroutine 一点时间退出，免得一边还原网络一边往里写包
		time.Sleep(150 * time.Millisecond)
		_ = a.dev.Close()
		a.dev = nil
	}
	a.running = false
	a.tunnel = nil

	if err := a.snapshot.Restore(a.cfg); err != nil {
		// 还原失败就留着 state.json，让下次启动继续尝试自愈
		a.lastError = err.Error()
		return fmt.Errorf("还原网络配置失败: %w", err)
	}
	a.snapshot = Snapshot{}
	a.cfg = NetConfig{}
	if err := RemoveSnapshot(a.statePath); err != nil {
		a.log.Warn("删除状态文件失败", "err", err)
	}
	a.log.Info("已断开")
	return nil
}

type AppStatus struct {
	Running   bool
	Online    bool
	TunnelIP  string
	Server    string
	RTT       time.Duration
	RxBytes   uint64
	TxBytes   uint64
	LastError string
}

func (a *App) Status() AppStatus {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := AppStatus{
		Running:   a.running,
		Server:    a.inv.Server,
		TunnelIP:  a.cfg.TunnelIP,
		LastError: a.lastError,
	}
	if a.tunnel != nil {
		st.Online = a.tunnel.Stats.Connected.Load()
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
