//go:build !windows

package client

import (
	"context"
	"errors"
)

var ErrNoNetCfg = errors.New("client: 路由与 DNS 接管只在 Windows 上可用")

func Capture(string) (Snapshot, error)     { return Snapshot{}, ErrNoNetCfg }
func ConfigureAdapter(NetConfig) error     { return ErrNoNetCfg }
func (s Snapshot) Apply(NetConfig) error   { return ErrNoNetCfg }
func (s Snapshot) Restore(NetConfig) error { return ErrNoNetCfg }

// cleanupLegacyFirewallRules 的 Windows 实现要借道 PowerShell，仅升级扫尾用。
func cleanupLegacyFirewallRules() {}

// sessionWatcher 只在 Windows 上有实体；其它平台返回 nil，App 按无守护处理。
type sessionWatcher struct{}

func startSessionWatcher(context.Context, Snapshot, NetConfig, func(Snapshot), func()) (*sessionWatcher, error) {
	return nil, nil
}
func (w *sessionWatcher) Stop() {}
