package client

import (
	"fmt"
	"net"
)

// 智能分流：国内网段走原网关直连，其余才进隧道。
//
// 为什么这么做：全局接管时只要服务端出口出问题，用户整台机器就没网。
// 分流之后国内流量不经过云服，那条路断了也不影响上国内的站。
//
// 网段表只收 /16 及更短（809 条，覆盖国内约 96% 的地址）。全量 5494 条
// 要逐条调 route.exe，实测 34 秒 —— 用户点一次「连接」得干等半分钟，
// 而多出来的 3.8% 地址就算走了隧道也只是慢一点，不会不通。

// splitRouteCommands 生成国内直连路由的添加/删除命令。
//
// 走 route.exe 而不是 iphlpapi 的 CreateIpForwardEntry(2)：
// 后者在真机上试了 Row2 和一号接口、大端和小端四种组合，分别报
// 「返回成功但表里查不到」和 ERROR_INVALID_PARAMETER(87)/
// ERROR_BAD_ARGUMENTS(160)，没有一种能真正写进去。route.exe 慢但确定能work。
func splitRouteCommands(prefixes []string, ifIndex int, nextHop string, add bool) []Command {
	verbs := "delete"
	if add {
		verbs = "add"
	}
	out := make([]Command, 0, len(prefixes))

	for _, p := range prefixes {
		ip, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			continue
		}
		// route.exe 只认点分掩码
		mask := net.IP(ipnet.Mask).String()
		if mask == "<nil>" {
			continue
		}
		args := []string{verbs, ip.Mask(ipnet.Mask).String(), "mask", mask}
		if add {
			args = append(args, nextHop, "metric", "5")
			if ifIndex > 0 {
				args = append(args, "if", fmt.Sprint(ifIndex))
			}
		}
		out = append(out, Command{Name: "route", Args: args})
	}
	return out
}

// splitRoutesSupported 判断这张网卡能不能承载直连路由。
func splitRoutesSupported(s Snapshot) bool {
	return s.DefaultGateway != "" && s.DefaultIfIndex != 0 &&
		net.ParseIP(s.DefaultGateway) != nil
}
