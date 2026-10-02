package server

import "testing"

// 默认并行连接数：产品决策是「默认就用满上限 4」，而不是灰度期的 1。
//
// 光改这里还不够 —— invite 不传 --members 时写入连接码的值也要跟着走上限，
// 否则服务端允许 4 条，签出来的码仍然是单连接。
func TestDefaultMaxMembersIsFour(t *testing.T) {
	cfg := Default()
	if cfg.Tunnel.MaxMembers != 4 {
		t.Errorf("默认 tunnel.max_members = %d，期望 4", cfg.Tunnel.MaxMembers)
	}
}
