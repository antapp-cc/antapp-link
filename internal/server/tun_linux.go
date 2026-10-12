//go:build linux

package server

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TUN 是 Linux 上的一张 tun 网卡。直接开 /dev/net/tun 做 TUNSETIFF，
// 不引第三方网络库；地址配置交给 iproute2（每台现代 Linux 都有）。
type TUN struct {
	file   *os.File
	name   string
	closed atomic.Bool
}

type ifreq struct {
	Name  [unix.IFNAMSIZ]byte
	Flags uint16
	_     [22]byte
}

func OpenTUN(name string) (*TUN, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("打开 /dev/net/tun（需要 root，且内核要有 tun 模块）: %w", err)
	}

	var req ifreq
	copy(req.Name[:], name)
	// ifreq 的 flags 是主机字节序的 short，不能按大端写字节
	req.Flags = uint16(unix.IFF_TUN | unix.IFF_NO_PI)

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&req))); errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %s: %w", name, errno)
	}
	return &TUN{file: os.NewFile(uintptr(fd), "/dev/net/tun"), name: name}, nil
}

func (t *TUN) Name() string { return t.name }

func (t *TUN) Configure(ip string, prefix, mtu int) error {
	if err := runIP("addr", "add", fmt.Sprintf("%s/%d", ip, prefix), "dev", t.name); err != nil {
		return err
	}
	if err := runIP("link", "set", "dev", t.name, "mtu", strconv.Itoa(mtu)); err != nil {
		return err
	}
	return runIP("link", "set", "dev", t.name, "up")
}

func runIP(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %v: %w: %s", args, err, out)
	}
	return nil
}

// Read 先 poll 等包就绪再读。
//
// 不能直接在阻塞 fd 上 read：/dev/net/tun 是阻塞模式，而 os.NewFile 不注册 runtime
// poller，Close 唤不醒已经阻塞的 read —— 更糟的是 fd 关闭后会被内核释放、可能被新
// 连接复用，那个 read 就会读到别人的数据。这里用带超时的 poll 轮询，Close 置标志后
// 循环自己退出（Run 返回时不再留下一个永久阻塞的协程）。
func (t *TUN) Read(p []byte) (int, error) {
	for {
		if t.closed.Load() {
			return 0, os.ErrClosed
		}
		fds := []unix.PollFd{{Fd: int32(t.file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 200)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if t.closed.Load() {
				return 0, os.ErrClosed
			}
			return 0, err
		}
		if n == 0 {
			continue // 超时：回去看是否已被关闭
		}
		// 错误位不能当成"有数据"：这个 fd 是阻塞模式（没注册 netpoller），带着
		// POLLERR/POLLHUP 直接 read 会一直挂住，pumpTun 就此卡死、还不会投 fatal，
		// 表现为下行静默中断 —— 正是这个函数要防的故障形态。
		if rev := fds[0].Revents; rev&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return 0, fmt.Errorf("网卡不可用（poll revents=0x%x）", rev)
		}
		return t.file.Read(p)
	}
}

// TryRead 非阻塞收割一个已就绪的包（写合并的收割端）：无包立即 (0, nil)。
//
// /dev/net/tun 的 fd 是阻塞模式，空队列上直接 read 会挂住 —— pumpTun 的收割循环
// 一旦卡在这里，服务端就再也不往下发 IP 包，而连接看着还全在（控制帧走另一条
// 循环）。所以先用零超时的 poll 探测，确认有包才去读。
func (t *TUN) TryRead(p []byte) (int, error) {
	fds := []unix.PollFd{{Fd: int32(t.file.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		if err == unix.EINTR {
			return 0, nil
		}
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	// 同 Read：错误位不能当成有数据，否则会在阻塞 fd 上挂死
	if rev := fds[0].Revents; rev&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return 0, fmt.Errorf("网卡不可用（poll revents=0x%x）", rev)
	}
	return t.file.Read(p)
}
func (t *TUN) Write(p []byte) (int, error) { return t.file.Write(p) }

func (t *TUN) Close() error {
	t.closed.Store(true)
	return t.file.Close()
}
