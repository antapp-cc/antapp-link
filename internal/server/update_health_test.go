package server

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func freshStatus(stamp string, forwardReady bool) Status {
	return Status{
		UpdatedAt:    stamp,
		ForwardKnown: true,
		ForwardReady: forwardReady,
	}
}

// 两次采样读到同一份状态文件不能算健康：新进程只写一次就僵死（死锁、卡在
// iptables）时，Fresh() 和 is-active 都会被骗过去。
func TestHealthSampleRequiresNewStamp(t *testing.T) {
	stamp1 := time.Now().Format(time.RFC3339)
	stamp2 := time.Now().Add(3 * time.Second).Format(time.RFC3339)

	h, s, done, _ := healthSample(freshStatus(stamp1, true), "", 0, "")
	if h != 1 || s != stamp1 || done {
		t.Fatalf("第一次健康应记下样本、等待第二次，实际 healthy=%d stamp=%q done=%v", h, s, done)
	}

	h, _, done, reason := healthSample(freshStatus(stamp1, true), "", h, s)
	if done || h != 1 || !strings.Contains(reason, "没有刷新") {
		t.Fatalf("同一份文件第二次出现不能算健康，但也不该重置，实际 healthy=%d done=%v reason=%q", h, done, reason)
	}

	h, _, done, _ = healthSample(freshStatus(stamp2, true), "", 1, stamp1)
	if !done {
		t.Fatalf("两次读到不同时间戳应判定健康，实际 healthy=%d", h)
	}
}

// 启动前那份状态文件不能算：新进程还没写过。
func TestHealthSampleRejectsPrestartStamp(t *testing.T) {
	st := freshStatus("startup-before", true)
	st.UpdatedAt = "startup-before"
	_, _, done, reason := healthSample(st, "startup-before", 0, "")
	if done || !strings.Contains(reason, "启动前") {
		t.Fatalf("启动前那份应被拒绝，实际 done=%v reason=%q", done, reason)
	}
}

// 转发器失败要判不健康，且原因要能被上层识别成环境问题。
func TestHealthSampleTreatsForwardFailureAsUnhealthy(t *testing.T) {
	st := freshStatus(time.Now().Format(time.RFC3339), false)
	st.ForwardError = "listen tcp :31400: bind: address already in use"
	h, _, done, reason := healthSample(st, "", 1, "stamp")
	if done || h != 0 || !strings.Contains(reason, "端口转发器启动失败") {
		t.Fatalf("转发器失败应判不健康，实际 healthy=%d done=%v reason=%q", h, done, reason)
	}
}

// forwardEnvError 必须能被 errors.As 识别，cmdUpdate 靠它避免把好版本拉黑。
func TestForwardEnvErrorMatches(t *testing.T) {
	var err error = &forwardEnvError{reason: "端口被占"}
	var fe *forwardEnvError
	if !errors.As(err, &fe) {
		t.Fatal("forwardEnvError 应能被识别")
	}
}

// 只有「端口被占用」这种环境特征才豁免拉黑；转发器因版本 bug 起不来必须照常拉黑，
// 否则坏版本会每小时白重启一次、永不收敛。
func TestIsForwardPortConflict(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"listen tcp :31400: bind: address already in use", true},
		{"listen tcp :31400: bind: Only one usage of each socket address", true},
		{"转发端口监听失败: permission denied", false},
		{"context deadline exceeded", false},
	}
	for _, c := range cases {
		if got := isForwardPortConflict(c.msg); got != c.want {
			t.Errorf("%q: got %v want %v", c.msg, got, c.want)
		}
	}
}
