//go:build linux

package update

import (
	"fmt"
	"os"
)

// Apply 用下载好的新二进制替换当前可执行文件。
//
// Linux 允许 rename 覆盖正在运行的程序（旧 inode 留到进程退出），所以不需要
// Windows 那套「先改名再写回」的花招，也不会出现「手上既没旧版也没新版」的窗口。
//
// 与 Windows 版一致的最后一步契约：Apply 返回后磁盘上的文件名已经是新版，而内存里
// 跑的还是旧代码 —— 调用方必须尽快重启自己（服务端由 systemd 拉起）。
// 替换前备份旧二进制由调用方负责（服务端自己留 $BIN.bak 以便回滚）。
func Apply(newBinary string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取当前程序路径: %w", err)
	}
	if err := installBinary(self, newBinary); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", self, err)
	}
	return nil
}

// CleanupOld / CleanupOldAfterHealthy 只服务于 Windows 的 .old 回滚副本机制
// （Windows 不能覆盖运行中的 exe，只能改名腾位置）。Linux 这边不产生 .old，
// 但这两个符号要在所有平台存在，调用点才不用做平台分支。
func CleanupOld() {}

func CleanupOldAfterHealthy() {}
