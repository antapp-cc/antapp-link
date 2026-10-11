//go:build !linux

package server

import (
	"errors"
	"io"
)

// 服务端自动更新依赖 systemd 重启和 /run 下的状态文件做健康检查，目前只有 Linux 实现，
// 但 cmdUpdate 要在所有平台能编译。
func applyUpdate(string, io.Writer) error {
	return errors.New("服务端自动更新目前只支持 Linux")
}
