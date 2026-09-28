package client

import (
	"fmt"
	"strings"
	"testing"
)

// ---------- 路由计划（routePlan）：接管顺序的硬约束都在这里 ----------

// 绕行路由必须排在 /1 接管路由之前。反了的话承载隧道的 TCP 连接
// 会被自己送进隧道形成自噬，表现是「连不上，但没有任何报错」。
func TestRoutePlanPutsBypassBeforeTunnelRoutes(t *testing.T) {
	ops := routePlan(testSnapshot(), testNetConfig())
	bypass, tunnel := -1, -1
	for i, op := range ops {
		switch {
		case op.dest.Bits() == 32 && !op.onTunnel:
			bypass = i
		case op.onTunnel:
			if tunnel < 0 {
				tunnel = i
			}
		}
	}
	if bypass < 0 || tunnel < 0 {
		t.Fatalf("路由计划里缺绕行或接管路由: %s", dumpOps(ops))
	}
	if bypass > tunnel {
		t.Errorf("绕行在第 %d 条、接管在第 %d 条 —— 绕行必须在前，否则隧道自噬",
			bypass+1, tunnel+1)
	}
}

// 云服在直连网段内时不加绕行路由：现成直连路由更具体，
// 加一条指向默认网关的 /32 会覆盖它，隧道自己掐死自己。
func TestRoutePlanSkipsBypassWhenServerOnLink(t *testing.T) {
	snap := testSnapshot()
	snap.ServerNextHop = ""

	for _, op := range routePlan(snap, testNetConfig()) {
		if !op.onTunnel {
			t.Errorf("云服直连时不该有绕行路由: %s", dumpOps(routePlan(snap, testNetConfig())))
			return
		}
	}
	if got := len(routePlan(snap, testNetConfig())); got != 2 {
		t.Errorf("应该只剩两条 /1 接管路由，实际 %d 条", got)
	}
}

// /1 接管路由必须挂隧道网卡（onTunnel），且下一跳是隧道网关。
// 挂错网卡流量就进不了隧道 —— 旧版 netsh 时代踩过的坑，现在数据结构上杜绝。
func TestRoutePlanBindsTunnelRoutesToTunnelAdapter(t *testing.T) {
	ops := routePlan(testSnapshot(), testNetConfig())
	var halves []routeOp
	for _, op := range ops {
		if op.onTunnel {
			halves = append(halves, op)
		}
	}
	if len(halves) != 2 {
		t.Fatalf("应该正好两条 /1 接管路由，实际 %d 条", len(halves))
	}
	want := map[string]bool{"0.0.0.0/1": false, "128.0.0.0/1": false}
	for _, op := range halves {
		if _, ok := want[op.dest.String()]; !ok {
			t.Errorf("unexpected 路由 %s", op.dest)
			continue
		}
		want[op.dest.String()] = true
		if op.nextHop.String() != "10.10.0.1" {
			t.Errorf("%s 的下一跳应是隧道网关 10.10.0.1，实际 %s", op.dest, op.nextHop)
		}
		if op.metric != 1 {
			t.Errorf("%s 的 metric 应为 1，实际 %d", op.dest, op.metric)
		}
	}
	for dest, seen := range want {
		if !seen {
			t.Errorf("缺少接管路由 %s", dest)
		}
	}
}

// 没有网关（快照异常）时至少不能带下一跳为空的接管路由。
func TestRoutePlanWithoutGateway(t *testing.T) {
	cfg := testNetConfig()
	cfg.Gateway = ""
	if ops := routePlan(testSnapshot(), cfg); len(ops) != 0 {
		t.Errorf("没有网关时不应产出路由计划: %s", dumpOps(ops))
	}
}

// ---------- NRPT ----------

// Pi 域名在境内所有递归上都被污染（实测路由器 DNS、AliDNS、DNSPod 全是假 IP），
// 唯一干净来源是隧道出口。规则的作用域、标记和 GUID 必须准确：
// 作用域只圈 Pi 域名（孤儿规则最坏只影响 Pi），GUID 固定才能幂等覆盖。
func TestNRPTConstants(t *testing.T) {
	if nrptNamespace != ".minepi.com" {
		t.Errorf("nrptNamespace = %q", nrptNamespace)
	}
	if nrptRuleComment == "" || !strings.HasPrefix(nrptRuleComment, "AntApp") {
		t.Errorf("nrptRuleComment = %q，应能标识本软件", nrptRuleComment)
	}
	if !strings.HasPrefix(nrptRuleGUID, "{") || !strings.HasSuffix(nrptRuleGUID, "}") {
		t.Errorf("nrptRuleGUID = %q，应是带花括号的 GUID", nrptRuleGUID)
	}
}

func dumpOps(ops []routeOp) string {
	var b strings.Builder
	for i, op := range ops {
		fmt.Fprintf(&b, "%2d. %s via %s metric %d onTunnel=%v\n",
			i+1, op.dest, op.nextHop, op.metric, op.onTunnel)
	}
	return b.String()
}
