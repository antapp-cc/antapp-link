//go:build windows

package client

import (
	"errors"
	"fmt"
	"sync"
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
			return nil, fmt.Errorf("创建虚拟网卡 %q（需要管理员权限，且 wintun.dll 必须与 exe 同架构）: %w",
				AdapterName, err)
		}
	}
	session, err := adapter.StartSession(ringCapacity)
	if err != nil {
		_ = adapter.Close()
		return nil, fmt.Errorf("启动网卡会话: %w", err)
	}
	return &WintunDevice{adapter: adapter, session: session, closed: make(chan struct{})}, nil
}

// Read 取一个 IP 包。ring 空时等读事件，不能忙等 —— 忙等会吃满一个核。
func (d *WintunDevice) Read(p []byte) (int, error) {
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

// Write 把包交给内核。发送队列满时重试一小会儿，再不行就丢弃 ——
// IP 层本就是尽力而为，丢一个包远好过让整条隧道重连。
func (d *WintunDevice) Write(p []byte) (int, error) {
	select {
	case <-d.closed:
		return 0, ErrDeviceClosed
	default:
	}

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
	return len(p), nil
}

func (d *WintunDevice) Name() string { return AdapterName }

func (d *WintunDevice) Close() error {
	d.once.Do(func() {
		close(d.closed)
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
