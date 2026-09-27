//go:build !windows

package client

import "errors"

var ErrNoAutostart = errors.New("client: 开机自启只在 Windows 上可用")

func EnableAutostart() error  { return ErrNoAutostart }
func DisableAutostart() error { return ErrNoAutostart }
func AutostartEnabled() bool  { return false }
