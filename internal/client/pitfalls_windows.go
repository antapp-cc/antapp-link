//go:build windows

package client

import (
	"net/netip"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// evaluatePitfalls 在连接前做一次环境体检，把已知的地雷写进日志。
// 借鉴 WireGuard 的 tunnel/pitfalls.go：这些环境问题用户自己几乎不可能
// 排查出来，日志里有结论，远程支持时能少走一小时弯路。
func evaluatePitfalls(serverIP string) {
	go func() {
		pitfallDNSCacheDisabled()
		pitfallWeakHostLoop(serverIP)
	}()
}

// pitfallDNSCacheDisabled：dnscache 服务被禁用后，系统解析整体失灵
// （NRPT 也依赖它），这是「连上了但打不开任何网页」的常见根因。
func pitfallDNSCacheDisabled() {
	scm, err := mgr.Connect()
	if err != nil {
		return
	}
	defer scm.Disconnect()
	svc, err := scm.OpenService("dnscache")
	if err != nil {
		return
	}
	defer svc.Close()
	cfg, err := svc.Config()
	if err != nil {
		return
	}
	if cfg.StartType == mgr.StartDisabled {
		logf("体检警告：系统 DNS 缓存服务（dnscache）被禁用了，域名解析会整体失灵，请在服务管理器里重新启用它")
	}
}

// pitfallWeakHostLoop：到云服的最优路由所在网卡如果开着转发/弱主机发送，
// 隧道流量会被再转一圈形成路由环路（WireGuard 实测踩过的坑）。
// 只检测并写日志，不替用户改系统配置。
func pitfallWeakHostLoop(serverIP string) {
	srv, err := netip.ParseAddr(strings.TrimSpace(serverIP))
	if err != nil || !srv.Is4() {
		return
	}
	rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		return
	}
	checked := map[winipcfg.LUID]bool{}
	warned := map[string]bool{}
	for i := range rows {
		r := &rows[i]
		p := netip.PrefixFrom(r.DestinationPrefix.RawPrefix.Addr(), int(r.DestinationPrefix.PrefixLength))
		if !p.Contains(srv) {
			continue
		}
		if checked[r.InterfaceLUID] {
			continue
		}
		checked[r.InterfaceLUID] = true
		ifrow, err := r.InterfaceLUID.Interface()
		if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp {
			continue
		}
		ipif, err := r.InterfaceLUID.IPInterface(windows.AF_INET)
		if err != nil {
			continue
		}
		if (ipif.ForwardingEnabled || ipif.WeakHostSend) && !warned[ifrow.Alias()] {
			warned[ifrow.Alias()] = true
			logf("体检警告：网卡 %q 开了转发/弱主机发送，可能导致隧道流量成环，建议关闭该网卡的这两项属性", ifrow.Alias())
		}
	}
}
