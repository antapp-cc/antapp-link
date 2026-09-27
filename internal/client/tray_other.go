//go:build !windows

package client

import "errors"

func (a *App) RunTray() error {
	return errors.New("client: 托盘界面只在 Windows 上可用")
}
