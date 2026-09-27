//go:build !windows

package client

import "errors"

var ErrNoNetCfg = errors.New("client: 路由与 DNS 接管只在 Windows 上可用")

func Capture(string) (Snapshot, error)     { return Snapshot{}, ErrNoNetCfg }
func (s Snapshot) Apply(NetConfig) error   { return ErrNoNetCfg }
func (s Snapshot) Restore(NetConfig) error { return ErrNoNetCfg }
