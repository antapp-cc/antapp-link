//go:build windows

package client

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 用临时名字创建互斥体，返回「关掉它的函数」。测试结束后务必关，别让句柄泄漏
// 影响后面的用例。
func holdMutex(t *testing.T, name string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("名字转 UTF16: %v", err)
	}
	h, cerr := windows.CreateMutex(nil, false, p)
	if cerr != nil && cerr != windows.ERROR_ALREADY_EXISTS {
		t.Fatalf("创建互斥体: %v", cerr)
	}
	return func() { _ = windows.CloseHandle(h) }
}

func testMutexName(t *testing.T) string {
	return `Global\AntAppLink-Test-` + t.Name()
}

// 旧实例已经退出：新实例直接拿到所有权。
func TestAcquireInstanceWhenPreviousAlreadyGone(t *testing.T) {
	name := testMutexName(t)
	holdMutex(t, name)() // 立刻关掉，模拟旧实例已退出

	release, ok, err := acquireInstance(name)
	if err != nil || !ok {
		t.Fatalf("旧实例已退出时应立即取得所有权: ok=%v err=%v", ok, err)
	}
	release()
}

// 旧实例在新实例第一次检查时仍持有、随后释放：新实例必须在重试中拿到所有权。
//
// 这是自动更新后「客户端消失」的根因场景。原代码拿到 ERROR_ALREADY_EXISTS 的
// 有效句柄却没关，旧实例放手后对象仍被它自己撑着，4 次重试全失败 → 新进程退出。
func TestAcquireInstanceAfterPreviousReleasesDuringRetry(t *testing.T) {
	name := testMutexName(t)
	releaseOld := holdMutex(t, name)

	go func() {
		time.Sleep(450 * time.Millisecond) // 落在第 2 次重试之前
		releaseOld()
	}()

	release, ok, err := acquireInstance(name)
	if err != nil {
		t.Fatalf("acquireInstance: %v", err)
	}
	if !ok {
		t.Fatal("旧实例退出后新实例必须能取得所有权 —— 否则更新完一个实例都不剩")
	}
	release()
}

// 旧实例拖到第 3 次重试才放手，新实例仍然要拿到。
func TestAcquireInstanceLateRelease(t *testing.T) {
	name := testMutexName(t)
	releaseOld := holdMutex(t, name)

	go func() {
		time.Sleep(700 * time.Millisecond) // 第 3 次重试附近
		releaseOld()
	}()

	release, ok, err := acquireInstance(name)
	if err != nil {
		t.Fatalf("acquireInstance: %v", err)
	}
	if !ok {
		t.Fatal("旧实例延迟退出后新实例仍须取得所有权")
	}
	release()
}

// 真正的双实例：第一个一直持有，第二个必须失败（不能因为修句柄就丢掉单实例保护）。
func TestAcquireInstanceRejectsRealSecondInstance(t *testing.T) {
	name := testMutexName(t)
	releaseOld := holdMutex(t, name)
	defer releaseOld()

	release, ok, err := acquireInstance(name)
	if err != nil {
		t.Fatalf("acquireInstance: %v", err)
	}
	if ok {
		release()
		t.Fatal("已有实例在跑时必须返回 ok=false，否则单实例保护就失效了")
	}
}

// 成功实例释放后，后来的实例能接手。
func TestAcquireInstanceReusableAfterRelease(t *testing.T) {
	name := testMutexName(t)

	release, ok, err := acquireInstance(name)
	if err != nil || !ok {
		t.Fatalf("首个实例应成功: ok=%v err=%v", ok, err)
	}
	release()

	// 释放后应能重新取得。给它一点时间让内核对象真正销毁。
	time.Sleep(50 * time.Millisecond)
	release2, ok2, err2 := acquireInstance(name)
	if err2 != nil || !ok2 {
		t.Fatalf("释放后应能重新取得: ok=%v err=%v", ok2, err2)
	}
	release2()
}
