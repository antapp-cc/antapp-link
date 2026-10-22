//go:build windows

package client

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// 还原必须能重复执行：条目已经不在了不算失败；其它删除错误必须上报 ——
// 没删掉却报成功，用户就带着残留路由继续用，而且下次还原也不会再试。
//
// 真机上 DeleteRoute/DeleteKey 实际返回哪个错误码未在 CI 验证（这里只钉住分类规则）。
func TestMissingErrorsAreNotFailures(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_NOT_FOUND,
		windows.ERROR_FILE_NOT_FOUND,
		windows.ERROR_PATH_NOT_FOUND,
	} {
		if !routeMissing(err) {
			t.Errorf("routeMissing(%v) = false，期望 true", err)
		}
		if !regMissing(err) {
			t.Errorf("regMissing(%v) = false，期望 true", err)
		}
	}
	denied := errors.New("拒绝访问")
	if routeMissing(denied) || regMissing(denied) {
		t.Error("普通错误不能被当成「本来就不在」，否则失败会被吞掉")
	}
	if routeMissing(nil) || regMissing(nil) {
		t.Error("nil 不是 missing")
	}
}

// 没有所有权记录时一条路由都不能删：这时调用方要显式选择退回形态扫描，
// 不能在这里悄悄按网关 + metric 猜，那正是会误删用户路由的老行为。
func TestSweepOwnedSplitRoutesWithoutOwnership(t *testing.T) {
	removed, err := sweepOwnedSplitRoutes(nil)
	if removed != 0 || err != nil {
		t.Errorf("没有所有权记录时应是 (0,nil)，实际 (%d,%v)", removed, err)
	}
}
