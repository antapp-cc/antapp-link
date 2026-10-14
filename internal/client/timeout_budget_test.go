package client

import (
	"testing"
	"time"
)

// 钉住这组超时的相对关系，防止「外层预算被内层更短的硬编码架空」那类回归再次出现 ——
// 曾经 probeCtx 从 12 秒放宽到 30 秒，而 probeServer 里 dial 写死 8 秒，改动一点没生效，
// 报错照旧是 dial i/o timeout。
func TestHandshakeBudgets(t *testing.T) {
	if handshakeTimeout <= 0 || probeTimeout <= 0 || memberHandshakeTimeout <= 0 || shutdownWait <= 0 {
		t.Fatal("超时预算必须为正")
	}

	// 成员连接是「补齐」（断了自己会退避重拨），总预算不该超过会话级首次建立 ——
	// 否则一次补拨最坏会被拖到几十秒，违背「成员断开后 30 秒内补齐」的设计目标。
	if memberHandshakeTimeout > handshakeTimeout {
		t.Errorf("成员建连预算 %v 超过会话级 %v，补齐会变得很慢",
			memberHandshakeTimeout, handshakeTimeout)
	}

	// 退出时等待必须盖得住一次完整的连接尝试：探测最坏 probeTimeout，加上 Capture 里
	// 那次域名解析（连接码写域名时才会走到，但预算要按最坏算），再加接管网络那几秒。
	if shutdownWait < probeTimeout+resolveTimeout {
		t.Errorf("shutdownWait %v 盖不住 probeTimeout %v + resolveTimeout %v",
			shutdownWait, probeTimeout, resolveTimeout)
	}
	if shutdownWait-probeTimeout-resolveTimeout < 5*time.Second {
		t.Errorf("shutdownWait %v 留给接管网络的余量不足 5 秒", shutdownWait)
	}

	// 隧道建立后靠心跳判死，判死线必须显著大于一次写超时，否则慢批会把好会话判死。
	if deadAfter <= slotWriteTimeout {
		t.Errorf("deadAfter %v 没有明显大于 slotWriteTimeout %v", deadAfter, slotWriteTimeout)
	}
}
