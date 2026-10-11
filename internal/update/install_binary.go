package update

import (
	"os"
	"path/filepath"
)

// installBinary 把 newBinary 装到 self 的位置：先写同目录的临时文件，再 rename 覆盖。
//
// 同目录是硬要求 —— 跨文件系统的 rename 会失败，而 /tmp 常常是另一个挂载点。
// Linux 上这条路径正好能覆盖正在运行的程序（旧 inode 留到进程退出），所以服务端
// 更新不需要先停服务。
func installBinary(self, newBinary string) error {
	raw, err := os.ReadFile(newBinary)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(self), "."+filepath.Base(self)+".new")
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		return err
	}
	// 显式再 chmod 一次：umask 或"文件已存在"都可能让上面的 perm 不生效，而 systemd
	// 重启时拿到一个没有执行位的文件会直接 EACCES。
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, self); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
