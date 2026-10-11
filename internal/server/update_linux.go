//go:build linux

package server

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/antapp-cc/antapp-link/internal/update"
)

// applyUpdate 备份 → 替换 → 重启 → 健康检查；任何一步失败都回滚。
//
// 备份不是可选项：新版启动即崩时，磁盘上那份跑不起来的就是唯一在场的版本，没有
// .bak 就只能人工上传旧二进制救场。
func applyUpdate(newBinary string, out io.Writer) error {
	self, err := selfPath()
	if err != nil {
		return err
	}
	backup := self + ".bak"
	if err := copyBinary(self, backup); err != nil {
		return fmt.Errorf("备份当前版本到 %s 失败（未做任何改动）: %w", backup, err)
	}
	fmt.Fprintf(out, "已备份当前版本到 %s\n", backup)

	if err := update.Apply(newBinary); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", self, err)
	}
	fmt.Fprintf(out, "已替换 %s，重启服务……\n", self)

	if err := runSystemctl("restart", "antapp-linkd"); err != nil {
		return rollback(self, backup, fmt.Errorf("重启失败: %w", err))
	}
	if err := waitHealthy(60 * time.Second); err != nil {
		return rollback(self, backup, err)
	}
	return nil
}

// rollback 把旧二进制放回去并重启。它自己也失败时要讲清旧版本在哪，别让现场既没有
// 能跑的、也没有能回滚的。
func rollback(self, backup string, cause error) error {
	if err := copyBinary(backup, self); err != nil {
		return fmt.Errorf("%v；回滚也失败（旧版本仍在 %s，可用 install -m755 手动放回）: %w", cause, backup, err)
	}
	if err := runSystemctl("restart", "antapp-linkd"); err != nil {
		return fmt.Errorf("%v；文件已回滚但重启失败: %w", cause, err)
	}
	return fmt.Errorf("%w（已回滚到备份版本）", cause)
}

// waitHealthy 等到「systemd 认为它 active」且「状态文件正在被刷新」。
//
// 光看 is-active 会误判：Restart=always 下启动即崩的进程在 activating 和
// auto-restart 之间循环，systemd 照样能报 active。状态文件每 5 秒刷一次，
// 它新鲜才说明真有个进程在跑隧道。
func waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "（还没有一次检查完成）"
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if !serviceActive() {
			last = "服务未处于 active"
			continue
		}
		st, err := ReadStatus()
		if err != nil {
			last = fmt.Sprintf("读状态文件失败: %v", err)
			continue
		}
		if statusFresh(st) {
			return nil
		}
		last = "状态文件没有刷新"
	}
	return fmt.Errorf("新版本启动后 %s 内没就绪：%s", timeout, last)
}

// statusStaleAfter 是「状态文件还在被刷新」的判定窗口：run 进程每 5 秒写一次，
// 超过这个时间没更新就说明写它的进程已经不在了。
//
// 这里自己算而不复用 Status.Fresh —— 那个方法在 fix/full-audit-fixes 分支上，
// 自动更新这条线要从 main 单独发布，不能依赖尚未合并的改动。两条线合并时把这段
// 删掉、改用 Fresh 即可。
const statusStaleAfter = 20 * time.Second

func statusFresh(st Status) bool {
	t, err := time.Parse(time.RFC3339, st.UpdatedAt)
	return err == nil && time.Since(t) <= statusStaleAfter
}

func serviceActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "antapp-linkd").Run() == nil
}

// selfPath 取当前可执行文件的真实路径（解开符号链接），备份与替换都以它为基准。
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("取当前程序路径: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// copyBinary 先写同目录临时文件再改名。目标可能是正在运行的程序：直接截断写会拿到
// ETXTBSY，而 rename 覆盖正在运行的 ELF 是允许的（旧 inode 留到进程退出）。
func copyBinary(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp")
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
