//go:build !windows

package client

import "errors"

func ReadClipboard() (string, error) {
	return "", errors.New("client: 读剪贴板只在 Windows 上可用")
}
