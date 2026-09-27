//go:build !windows

package client

import "errors"

// RunUI 在非 Windows 平台只是个占位，让 go test ./... 能跑通。
func RunUI(*App, *LogBuffer, string) error {
	return errors.New("client: 图形界面只在 Windows 上可用")
}
