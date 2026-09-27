//go:build linux

package server

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TUN 是 Linux 上的一张 tun 网卡。直接开 /dev/net/tun 做 TUNSETIFF，
// 不引第三方网络库；地址配置交给 iproute2（每台现代 Linux 都有）。
type TUN struct {
	file *os.File
	name string
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

func (t *TUN) Read(p []byte) (int, error)  { return t.file.Read(p) }
func (t *TUN) Write(p []byte) (int, error) { return t.file.Write(p) }
func (t *TUN) Close() error                { return t.file.Close() }
