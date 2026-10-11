package update

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNotApplied 表示替换还没落到目标文件上就失败了：磁盘上仍是原来那个程序。
// 服务端 update 据此判断「不需要回滚，也不该把这一版记成失败永久跳过」。
//
// installBinary 的每条失败路径都在 rename 之前，所以它天然满足这个语义。
var ErrNotApplied = errors.New("update: 未改动目标文件")

// installBinary 把 newBinary 装到 self 的位置：先写同目录的临时文件，再 rename 覆盖。
//
// 同目录是硬要求 —— 跨文件系统的 rename 会失败，而 /tmp 常常是另一个挂载点。
// Linux 上这条路径正好能覆盖正在运行的程序（旧 inode 留到进程退出），所以服务端
// 更新不需要先停服务。
//
// 临时文件名随机：手动 update 与 timer 撞上时，固定的临时名会被后启动的那个截断，
// 而前一个已经 rename 出去的内容可能写到一半就没了。
func installBinary(self, newBinary string) error {
	raw, err := os.ReadFile(newBinary)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(self), "."+filepath.Base(self)+"-*.new")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// rename 成功后 tmpName 置空，defer 就不会误删刚装好的程序
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	// 先落盘再改名：掉电时宁可留下一个完整的临时文件，也不要一个长度 0 的"新版本"
	// 占住正式名字 —— 那会让 Restart=always 直接进崩溃循环，只能人工救。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 显式 chmod：CreateTemp 建出来是 0600，systemd 拿到没有执行位的文件会 EACCES
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, self); err != nil {
		return err
	}
	tmpName = ""
	syncDir(filepath.Dir(self))
	return nil
}

// syncDir 把目录项也刷到盘上，保证 rename 本身掉电后不丢。目录 fsync 失败无关紧要
// （部分文件系统不支持），只尽力而为。
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
