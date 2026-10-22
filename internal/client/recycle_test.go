package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// 把周期压到毫秒级，否则一个用例要跑 8 小时。
func withFastRecycle(t *testing.T, every, probe, retry time.Duration, busy uint64) {
	t.Helper()
	oldEvery, oldProbe, oldRetry, oldBusy := recycleEvery, recycleBusyProbe, recycleRetry, recycleBusyBytes
	recycleEvery, recycleBusyProbe, recycleRetry, recycleBusyBytes = every, probe, retry, busy
	t.Cleanup(func() {
		recycleEvery, recycleBusyProbe, recycleRetry, recycleBusyBytes = oldEvery, oldProbe, oldRetry, oldBusy
	})
}

func newRecycleSession() *clientSession {
	return &clientSession{
		stats: &Stats{},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// 空闲到点必须返回哨兵错误，让 Run 重建会话 —— 这是「8 小时自动重连」的全部逻辑。
func TestRecycleAtFiresWhenIdle(t *testing.T) {
	withFastRecycle(t, 30*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 1<<20)
	s := newRecycleSession()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	err := s.recycleAt(ctx)
	if !errors.Is(err, errScheduledRecycle) {
		t.Fatalf("空闲到点应返回 errScheduledRecycle，实际: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("不该拖这么久才返回，耗时 %v", d)
	}
}

// 正在传大量数据时必须推迟：重建会断 8 秒，比劣化本身更难受。
func TestRecycleAtDefersWhenBusy(t *testing.T) {
	// 门槛设得很低，只要有一点流量就算忙
	withFastRecycle(t, 30*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 100)
	s := newRecycleSession()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	// 持续制造流量
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			default:
				s.stats.TxBytes.Add(10_000)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	err := s.recycleAt(ctx)
	// 一直忙 → 永远推迟 → ctx 到期返回 nil，绝不能返回哨兵错误
	if errors.Is(err, errScheduledRecycle) {
		t.Fatal("正在传数据时不该重建连接")
	}
}

// 关闭开关（周期 <= 0）时必须立刻返回，不能阻塞。
func TestRecycleAtDisabled(t *testing.T) {
	withFastRecycle(t, 0, 10*time.Millisecond, 10*time.Millisecond, 1<<20)
	s := newRecycleSession()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	if err := s.recycleAt(ctx); err != nil {
		t.Fatalf("关闭时不该返回错误: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("关闭时应立刻返回，实际耗时 %v", d)
	}
}

// ctx 取消（用户手动断开）时不能返回哨兵错误，否则会被当成一次「自动重建」。
func TestRecycleAtStopsOnContextCancel(t *testing.T) {
	withFastRecycle(t, time.Hour, 10*time.Millisecond, 10*time.Millisecond, 1<<20)
	s := newRecycleSession()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	err := s.recycleAt(ctx)
	if errors.Is(err, errScheduledRecycle) {
		t.Fatal("ctx 取消不是到点重建")
	}
}

// 定期重建必须被翻译成「不是故障」的一句话，否则每 8 小时亮一个警告，
// 用户会来问「为什么老是断」。
func TestSessionEndInfoScheduledRecycle(t *testing.T) {
	lvl, reason := sessionEndInfo(errScheduledRecycle)
	if lvl != slog.LevelInfo {
		t.Fatalf("定期重建应该是 Info 级别，实际 %v", lvl)
	}
	if reason == "" {
		t.Fatal("必须给出中文原因")
	}
	if !contains(reason, "不是故障") {
		t.Fatalf("原因里要明确说明不是故障，实际: %q", reason)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
