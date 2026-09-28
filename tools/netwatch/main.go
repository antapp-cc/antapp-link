//go:build windows

// Command netwatch 验证 winipcfg 的系统回调与 bestDefaultRoute 的判定输入：
// 注册与客户端完全相同的回调，打印收到的每个事件和 /0 路由的合成 metric 视角，
// 30 秒后自动退出。
package main

import (
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

const adapterName = "AntApp Link"

func main() {
	var tunLUID winipcfg.LUID
	if aas, err := winipcfg.GetAdaptersAddresses(windows.AF_UNSPEC, winipcfg.GAAFlagDefault); err == nil {
		for _, aa := range aas {
			if aa.FriendlyName() == adapterName {
				tunLUID = aa.LUID
				fmt.Printf("隧道网卡 LUID=%d（被排除）\n", tunLUID)
			}
		}
	}
	if tunLUID == 0 {
		fmt.Println("（隧道网卡不存在，排除列表为空）")
	}

	dump := func(tag string) {
		rows, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
		if err != nil {
			fmt.Println("读路由表失败:", err)
			return
		}
		best := ^uint64(0)
		found := false
		var bnh netip.Addr
		var bidx uint32
		for i := range rows {
			r := &rows[i]
			if r.DestinationPrefix.PrefixLength != 0 || r.InterfaceLUID == tunLUID {
				continue
			}
			nh := r.NextHop.Addr()
			if !nh.IsValid() || nh.IsUnspecified() {
				continue
			}
			ifrow, err := r.InterfaceLUID.Interface()
			if err != nil || ifrow.OperStatus != winipcfg.IfOperStatusUp {
				fmt.Printf("  /0 via %s if=%d row=%d —— 接口不可用(%v)\n", nh, r.InterfaceIndex, r.Metric, err)
				continue
			}
			ipif, err := r.InterfaceLUID.IPInterface(windows.AF_INET)
			ifm := uint32(0)
			if err == nil {
				ifm = ipif.Metric
			}
			combined := uint64(r.Metric) + uint64(ifm)
			fmt.Printf("  /0 via %s if=%d row=%d ifmetric=%d combined=%d alias=%s\n",
				nh, r.InterfaceIndex, r.Metric, ifm, combined, ifrow.Alias())
			if !found || combined < best {
				best, found, bnh, bidx = combined, true, nh, r.InterfaceIndex
			}
		}
		if found {
			fmt.Printf("  ==> 判定出口: %s if=%d\n", bnh, bidx)
		} else {
			fmt.Println("  ==> 没有可用默认路由")
		}
	}

	cbr, err := winipcfg.RegisterRouteChangeCallback(func(mt winipcfg.MibNotificationType, route *winipcfg.MibIPforwardRow2) {
		if route != nil {
			fmt.Printf("%s ROUTE-EVENT %v prefixlen=%d nexthop=%s if=%d\n",
				time.Now().Format("15:04:05.000"), mt,
				route.DestinationPrefix.PrefixLength, route.NextHop.Addr(), route.InterfaceIndex)
		} else {
			fmt.Printf("%s ROUTE-EVENT %v (nil row)\n", time.Now().Format("15:04:05.000"), mt)
		}
	})
	if err != nil {
		fmt.Println("注册路由回调失败:", err)
		return
	}
	defer cbr.Unregister()

	fmt.Println("初始视角：")
	dump("init")
	fmt.Println("监听中，40 秒后自动退出……")
	time.Sleep(40 * time.Second)
	fmt.Println("结束")
}
