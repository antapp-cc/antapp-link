//go:build !windows

package client

import (
	"context"
	"errors"
)

var ErrNoNetCfg = errors.New("client: 路由与 DNS 接管只在 Windows 上可用")

func Capture(NetConfig) (Snapshot, error)  { return Snapshot{}, ErrNoNetCfg }
func ConfigureAdapter(NetConfig) error     { return ErrNoNetCfg }
func (s Snapshot) Apply(NetConfig) error   { return ErrNoNetCfg }
func (s Snapshot) Restore(NetConfig) error { return ErrNoNetCfg }

// refreshSplitRoutesHook 只在 Windows 上有实体（本平台不接管网络，热重铺无从谈起）。
// 声明在这里是为了让 app.go（跨平台文件）在非 Windows 上也能编译 —— 它按 nil 跳过。
var refreshSplitRoutesHook func(gw string, ifIndex int, table []string)

// sessionWatcher 只在 Windows 上有实体；其它平台返回 nil，App 按无守护处理。
type sessionWatcher struct{}

func startSessionWatcher(context.Context, Snapshot, NetConfig, func(Snapshot), func()) (*sessionWatcher, error) {
	return nil, nil
}
func (w *sessionWatcher) Stop() {}
