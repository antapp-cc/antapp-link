package client

import "errors"

// AdapterName 是虚拟网卡的显示名。固定不变：重启时要能靠它找回上次留下的网卡，
// 而不是在系统里堆出一串同名适配器。
const AdapterName = "AntApp Link"

var (
	ErrDeviceClosed = errors.New("client: 虚拟网卡已关闭")
	ErrNoWintun     = errors.New("client: 虚拟网卡只在 Windows 上可用")
)
