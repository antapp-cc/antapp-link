//go:build windows

package client

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

const (
	tunnelType = "AntApp"

	// ring 容量 4 MiB，够扛住 Pi 节点这种量级，又不至于白占内存。
	ringCapacity = 0x400000
)

// WintunDevice 用 Wintun 做虚拟网卡。wintun.dll 随程序分发，
// 用户不需要装驱动包、不需要单独跑安装程序。
type WintunDevice struct {
	adapter *wintun.Adapter
	session wintun.Session
	closed  chan struct{}
	once    sync.Once
	recvMu  sync.Mutex
	sendMu  sync.Mutex

	// ReceivePacket/AllocateSendPacket 返回的是 ring 缓冲里的指针，EndSession 会把
	// ring 整个释放掉，所以还有在飞调用时 End 就是 use-after-free。用 ioMu 把
	// 「已开始关闭」和「在飞计数」放进同一个临界区判断，Close 才不会漏掉刚好挤进来的那次调用。
	ioMu     sync.Mutex
	closing  bool
	inflight int
	drained  chan struct{}

	// writeDrops 是「发送 ring 满、下行包被丢弃」的次数。
	// 它长期为 0 才正常；涨起来说明下行消费跟不上（对端丢 ACK、链路拥堵）。
	writeDrops atomic.Int64
}

// OpenDevice 打开虚拟网卡。
//
// 先 OpenAdapter 再 CreateAdapter：程序被强杀、或者上次没清理干净时，
// 适配器还在系统里 —— 直接 CreateAdapter 会失败，而复用它才是用户期望的行为。
func OpenDevice() (*WintunDevice, error) {
	// wintun.dll 只从 exe 同目录加载，先把内嵌的那份释放出去
	if _, err := EnsureWintunDLL(); err != nil {
		return nil, err
	}

	adapter, err := wintun.OpenAdapter(AdapterName)
	if err != nil {
		adapter, err = wintun.CreateAdapter(AdapterName, tunnelType, nil)
		if err != nil {
			return nil, fmt.Errorf("创建虚拟网卡 %q 失败：%s（需要管理员权限，且 wintun.dll 必须与 exe 同架构）",
				AdapterName, wintunReason(err))
		}
	}
	session, err := adapter.StartSession(ringCapacity)
	if err != nil {
		_ = adapter.Close()
		return nil, fmt.Errorf("启动网卡会话失败：%s", wintunReason(err))
	}
	return &WintunDevice{
		adapter: adapter, session: session,
		closed: make(chan struct{}), drained: make(chan struct{}),
	}, nil
}

// wintunReason 把 Wintun/Windows 的英文错误翻成一句中文。
//
// 这些错误平时见不到，但一旦出现用户看到的就是一整句英文，既看不懂也不知道该做什么。
// 翻不出来时给「原因不明」并保留原文进日志文件，别编一个听起来对但会误导的理由。
func wintunReason(err error) string {
	if err == nil {
		return "原因不明"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "initialization has already been completed"),
		strings.Contains(msg, "already initialized"):
		return "网卡会话已初始化过（上一个进程可能还没退干净，退出后重启程序即可）"
	case strings.Contains(msg, "cannot find"), strings.Contains(msg, "not found"):
		return "找不到虚拟网卡驱动（wintun.dll 可能缺失或架构不符）"
	case strings.Contains(msg, "access is denied"), strings.Contains(msg, "拒绝访问"):
		return "没有权限操作虚拟网卡（需要以管理员身份运行）"
	case strings.Contains(msg, "already exists"):
		return "同名虚拟网卡已存在（可能被别的进程占用）"
	case strings.Contains(msg, "handle is invalid"), strings.Contains(msg, "invalid handle"):
		return "网卡句柄已失效（驱动可能被系统重置）"
	}
	return "原因不明（详细错误见日志文件）"
}

// Read 取一个 IP 包。ring 空时等读事件，不能忙等 —— 忙等会吃满一个核。
func (d *WintunDevice) Read(p []byte) (int, error) {
	if !d.enter() {
		return 0, ErrDeviceClosed
	}
	defer d.leave()

	for {
		select {
		case <-d.closed:
			return 0, ErrDeviceClosed
		default:
		}

		d.recvMu.Lock()
		packet, err := d.session.ReceivePacket()
		if err == nil {
			n := copy(p, packet)
			d.session.ReleaseReceivePacket(packet)
			d.recvMu.Unlock()
			return n, nil
		}
		d.recvMu.Unlock()

		if !errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			// ERROR_HANDLE_EOF（Reached the end of the file）= 适配器/会话在系统层面
			// 已死（被停用、移除、驱动重置），不是我们自己 Close 的 —— 打上标记，
			// 让重连循环重建设备，而不是拿死句柄无限重试。
			if errors.Is(err, windows.ERROR_HANDLE_EOF) {
				return 0, fmt.Errorf("读网卡: %w: %w", ErrAdapterDead, err)
			}
			return 0, fmt.Errorf("读网卡: %w", err)
		}
		if err := waitReadable(d.session.ReadWaitEvent(), d.closed); err != nil {
			return 0, err
		}
	}
}

// TryRead 非阻塞收割一个已就绪的包：Wintun 的 ReceivePacket 本就是"无包立即
// 返回 ERROR_NO_MORE_ITEMS"的轮询语义，正好当非阻塞读用（写合并的收割端）。
func (d *WintunDevice) TryRead(p []byte) (int, error) {
	if !d.enter() {
		return 0, ErrDeviceClosed
	}
	defer d.leave()
	d.recvMu.Lock()
	defer d.recvMu.Unlock()
	packet, err := d.session.ReceivePacket()
	if err == nil {
		n := copy(p, packet)
		d.session.ReleaseReceivePacket(packet)
		return n, nil
	}
	if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
		return 0, nil // 无包，不等待
	}
	if errors.Is(err, windows.ERROR_HANDLE_EOF) {
		return 0, fmt.Errorf("%w: %w", ErrAdapterDead, err)
	}
	return 0, fmt.Errorf("读网卡: %w", err)
}

// Write 把包交给内核。发送队列满时重试一小会儿，再不行就丢弃 ——
// IP 层本就是尽力而为，丢一个包远好过让整条隧道重连。
func (d *WintunDevice) Write(p []byte) (int, error) {
	if !d.enter() {
		return 0, ErrDeviceClosed
	}
	defer d.leave()

	for attempt := 0; attempt < 64; attempt++ {
		d.sendMu.Lock()
		packet, err := d.session.AllocateSendPacket(len(p))
		if err == nil {
			copy(packet, p)
			d.session.SendPacket(packet)
			d.sendMu.Unlock()
			return len(p), nil
		}
		d.sendMu.Unlock()

		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			if errors.Is(err, windows.ERROR_HANDLE_EOF) {
				return 0, fmt.Errorf("写网卡: %w: %w", ErrAdapterDead, err)
			}
			return 0, fmt.Errorf("写网卡: %w", err)
		}
		time.Sleep(time.Millisecond)
	}
	// 64 次重试（约 64ms）都拿不到发送 slot：ring 被打满了。
	//
	// 这里仍然返回成功，让内层 TCP 重传 —— 返回错误会把整条隧道断掉，代价比丢包大。
	// 但必须计数并告警：这是「下行持续堵塞」的第一现场，静默丢掉的话用户只会看到
	// 「用着用着没网」，而日志里一点线索都没有。
	if n := d.writeDrops.Add(1); n == 1 || n%500 == 0 {
		logf("网卡发送缓冲已满，丢弃下行包（内层 TCP 会重传）：累计 %d 次", n)
	}
	return len(p), nil
}

func (d *WintunDevice) Name() string { return AdapterName }

// enter 登记一次在飞收发。设备已开始关闭时返回 false，调用方必须立刻退出，
// 绝不能再碰 session。
func (d *WintunDevice) enter() bool {
	d.ioMu.Lock()
	defer d.ioMu.Unlock()
	if d.closing {
		return false
	}
	d.inflight++
	return true
}

func (d *WintunDevice) leave() {
	d.ioMu.Lock()
	defer d.ioMu.Unlock()
	d.inflight--
	if d.closing && d.inflight == 0 {
		close(d.drained)
	}
}

func (d *WintunDevice) Close() error {
	d.once.Do(func() {
		d.ioMu.Lock()
		d.closing = true
		// 唤醒轮询中的 waitReadable，让阻塞在 dev.Read 上的调用尽快返回
		close(d.closed)
		if d.inflight == 0 {
			close(d.drained)
		}
		d.ioMu.Unlock()

		// 顺序不能动：等在飞的收发包全部退出之后才能 End
		<-d.drained
		d.session.End()
		_ = d.adapter.Close()
	})
	return nil
}

// waitReadable 必须同时等「有包可读」和「被要求关闭」。Wintun 的读事件没有配套的
// 取消机制，所以用有限等待轮询，保证关闭信号最多 200ms 就能被看到。
func waitReadable(handle windows.Handle, closed <-chan struct{}) error {
	for {
		select {
		case <-closed:
			return ErrDeviceClosed
		default:
		}
		status, err := windows.WaitForSingleObject(handle, 200)
		if err != nil {
			return fmt.Errorf("等待网卡可读: %w", err)
		}
		if status == uint32(windows.WAIT_OBJECT_0) {
			return nil
		}
	}
}
