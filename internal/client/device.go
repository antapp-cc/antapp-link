package client

import "errors"

// AdapterName 是虚拟网卡的显示名。固定不变：重启时要能靠它找回上次留下的网卡，
// 而不是在系统里堆出一串同名适配器。
const AdapterName = "AntApp Link"

var (
	ErrDeviceClosed = errors.New("client: 虚拟网卡已关闭")
	ErrNoWintun     = errors.New("client: 虚拟网卡只在 Windows 上可用")

	// ErrAdapterDead 表示适配器在系统层面失效（被停用/移除/驱动重置）。
	// 和 ErrDeviceClosed 的区别：没人主动 Close，是外力干的 —— 重连循环见到它
	// 必须重建设备，拿着死句柄重试只会永远 EOF。
	ErrAdapterDead = errors.New("client: 虚拟网卡已失效")
)
