package pki

import "testing"

// 两端声明的 ALPN 必须有交集。
//
// 曾经把客户端改成 "h2" 伪装、服务端同时接受新旧两个值，以为向后兼容就够了 ——
// 但客户端是自动更新的、服务端不是：客户端先跑到新版，服务端回
// no application protocol，节点直接离线。这个测试就是不让两端再出现「列表不交集」。
func TestClientAndServerALPNIntersect(t *testing.T) {
	dir := mustInit(t)
	inv := mustIssue(t, dir, "node-alpn-agree")

	cliCfg, err := ClientTLSConfig(inv)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, err := ServerTLSConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cliCfg.NextProtos {
		for _, s := range srvCfg.NextProtos {
			if c == s {
				return
			}
		}
	}
	t.Fatalf("客户端声明 %v，服务端接受 %v，交集为空 —— 握手必定失败",
		cliCfg.NextProtos, srvCfg.NextProtos)
}

// 默认配置下要能真的握手成功，而不只是两个列表有交集。
func TestDefaultALPNHandshakes(t *testing.T) {
	dir := mustInit(t)
	inv := mustIssue(t, dir, "node-alpn-default")
	addr, stop := startALPNProbeServer(t, dir)
	defer stop()

	cfg, err := ClientTLSConfig(inv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dialALPN(t, inv, addr, cfg.NextProtos)
	if err != nil {
		t.Fatalf("按默认配置握手失败: %v", err)
	}
	if got != ALPN {
		t.Errorf("协商出 %q，期望 %q", got, ALPN)
	}
}
