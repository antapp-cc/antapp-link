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

// splitRouteCommands 生成国内直连路由的**添加**命令。
//
// 删除不走这条路：route.exe 删一条不存在的路由会卡住，809 条逐条删能把整个
// 还原流程堵死（实测 route.exe 挂在那一动不动，连 /1 接管路由都没来得及撤，
// 客户端卡在「发现上次残留的网络配置，先还原」这一步）。
// 删除统一交给 splitRouteDeleteCommand 的一条 PowerShell 批量做。
//
// 添加走 route.exe 而不是 iphlpapi 的 CreateIpForwardEntry(2)：
// 后者在真机上试了 Row2 和一号接口、大端和小端四种组合，分别报
// 「返回成功但表里查不到」和 ERROR_INVALID_PARAMETER(87)/
// ERROR_BAD_ARGUMENTS(160)，没有一种能真正写进去。route.exe 慢但确定能work。
func splitRouteCommands(prefixes []string, ifIndex int, nextHop string) []Command {
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
		args := []string{"add", ip.Mask(ipnet.Mask).String(), "mask", mask,
			nextHop, "metric", "5"}
		if ifIndex > 0 {
			args = append(args, "if", fmt.Sprint(ifIndex))
		}
		out = append(out, Command{Name: "route", Args: args})
	}
	return out
}

// splitRouteDeleteCommand 一条命令删掉全部国内直连路由。
//
// 按「下一跳 == 原默认网关 且 metric == 5」筛，而不是照着网段表逐条删：
// 这样即使网段表更新过，也能把上一版留下的路由清干净。
func splitRouteDeleteCommand(s Snapshot) Command {
	script := fmt.Sprintf(
		"try { Get-NetRoute -AddressFamily IPv4 -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.NextHop -eq '%s' -and $_.RouteMetric -eq 5 -and $_.DestinationPrefix -ne '0.0.0.0/0' } | "+
			"Remove-NetRoute -Confirm:$false -ErrorAction SilentlyContinue } catch { }; exit 0",
		s.DefaultGateway)
	return Command{"powershell", []string{
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}}
}

// splitRoutesSupported 判断这张网卡能不能承载直连路由。
func splitRoutesSupported(s Snapshot) bool {
	return s.DefaultGateway != "" && s.DefaultIfIndex != 0 &&
		net.ParseIP(s.DefaultGateway) != nil
}
