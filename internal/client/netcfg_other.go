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

// refreshSplitRoutesHook 由平台侧注入（见 netcfg_windows.go）：按最新网段表热重铺
// 国内分流路由。非 Windows 没有分流路由这回事，留个 nil 占位让 app.go 的跨平台代码
// 能编译 —— 那边本来就判了 nil。
var refreshSplitRoutesHook func(gw string, ifIndex int, table []string)

// sessionWatcher 只在 Windows 上有实体；其它平台返回 nil，App 按无守护处理。
type sessionWatcher struct{}

func startSessionWatcher(context.Context, Snapshot, NetConfig, func(Snapshot), func()) (*sessionWatcher, error) {
	return nil, nil
}
func (w *sessionWatcher) Stop() {}
