//go:build windows

// Command netwatch 验证 winipcfg 回调与 MTU 写入链路（客户端 applyDynamicMTU 同款调用序列）。
package main

import (
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func main() {
	const adapterName = "AntApp Link"
	var tunLUID winipcfg.LUID
	aas, err := winipcfg.GetAdaptersAddresses(windows.AF_UNSPEC, winipcfg.GAAFlagDefault)
	if err != nil {
		fmt.Println("枚举网卡失败:", err)
		return
	}
	var wlanLUID winipcfg.LUID
	for _, aa := range aas {
		switch aa.FriendlyName() {
		case adapterName:
			tunLUID = aa.LUID
		case "WLAN":
			wlanLUID = aa.LUID
		}
	}
	fmt.Printf("隧道 LUID=%d  WLAN LUID=%d\n", tunLUID, wlanLUID)

	ifrow, err := wlanLUID.Interface()
	fmt.Printf("WLAN  MibIfRow2.MTU=%d err=%v\n", ifrowMTU(ifrow, err), err)
	row, err := tunLUID.IPInterface(windows.AF_INET)
	fmt.Printf("隧道  IPInterface.NLMTU=%d err=%v\n", rowNLMTU(row, err), err)

	if err == nil {
		const target = 1344
		row.NLMTU = target
		fmt.Println("尝试 row.Set() → 1344 …")
		if serr := row.Set(); serr != nil {
			fmt.Println("  Set 失败:", serr)
		} else {
			row2, _ := tunLUID.IPInterface(windows.AF_INET)
			fmt.Printf("  Set 成功，读回 NLMTU=%d\n", rowNLMTU(row2, nil))
			row.NLMTU = 1400
			_ = row.Set()
			row3, _ := tunLUID.IPInterface(windows.AF_INET)
			fmt.Printf("  已恢复 1400，读回 NLMTU=%d\n", rowNLMTU(row3, nil))
		}
	}

	// DNS 写入链路验证：清空 → LUID.SetDNS 写 10.10.0.1 → 读回。
	if tunLUID != 0 {
		fmt.Println("DNS 写入链路验证：")
		if err := tunLUID.FlushDNS(windows.AF_INET); err != nil {
			fmt.Println("  FlushDNS err:", err)
		}
		time.Sleep(300 * time.Millisecond)
		fmt.Printf("  清空后 DNS=%v\n", dnsV4(tunLUID))
		gw := netip.MustParseAddr("10.10.0.1")
		if err := tunLUID.SetDNS(windows.AF_INET, []netip.Addr{gw}, nil); err != nil {
			fmt.Println("  SetDNS err:", err)
		}
		time.Sleep(300 * time.Millisecond)
		fmt.Printf("  写入后 DNS=%v\n", dnsV4(tunLUID))
	}

	cbr, err := winipcfg.RegisterRouteChangeCallback(func(mt winipcfg.MibNotificationType, route *winipcfg.MibIPforwardRow2) {
		plen := -1
		nh := "nil"
		if route != nil {
			plen = int(route.DestinationPrefix.PrefixLength)
			nh = route.NextHop.Addr().String()
		}
		fmt.Printf("%s ROUTE-EVENT %v prefixlen=%d nexthop=%s\n", time.Now().Format("15:04:05.000"), mt, plen, nh)
	})
	if err != nil {
		fmt.Println("注册路由回调失败:", err)
		return
	}
	defer cbr.Unregister()
	cbi, err := winipcfg.RegisterInterfaceChangeCallback(func(mt winipcfg.MibNotificationType, row *winipcfg.MibIPInterfaceRow) {
		fam := int32(-1)
		idx := uint32(0)
		if row != nil {
			fam, idx = int32(row.Family), row.InterfaceIndex
		}
		fmt.Printf("%s IFACE-EVENT %v family=%d if=%d\n", time.Now().Format("15:04:05.000"), mt, fam, idx)
	})
	if err != nil {
		fmt.Println("注册接口回调失败:", err)
		return
	}
	defer cbi.Unregister()

	fmt.Println("监听事件 30 秒……（期间去改 WLAN 的 MTU）")
	time.Sleep(30 * time.Second)
	fmt.Println("结束")
}

func ifrowMTU(r *winipcfg.MibIfRow2, err error) uint32 {
	if err != nil || r == nil {
		return 0
	}
	return r.MTU
}

func rowNLMTU(r *winipcfg.MibIPInterfaceRow, err error) uint32 {
	if err != nil || r == nil {
		return 0
	}
	return r.NLMTU
}

func dnsV4(l winipcfg.LUID) []netip.Addr {
	addrs, err := l.DNS()
	if err != nil {
		return nil
	}
	var v4 []netip.Addr
	for _, a := range addrs {
		if a.Is4() {
			v4 = append(v4, a.Unmap())
		}
	}
	return v4
}

var _ = netip.Addr{}
