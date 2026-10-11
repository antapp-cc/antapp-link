//go:build linux

package server

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/antapp-cc/antapp-link/internal/update"
)

func updateSupported() bool { return true }

// updateLockPath 是互斥锁文件。放 /run：tmpfs 一定可写，机器重启后也会自动清掉，
// 不会留下一个锁死后续更新的僵尸文件。
const updateLockPath = "/run/antapp-link/update.lock"

// tryUpdateLock 拿非阻塞排他锁。拿不到说明另一个 update 正在跑，调用方应直接退出：
// 两边同时干会抢同一个备份文件与临时文件，最坏的结果是把别人刚换上去的坏版本备份
// 成 .bak —— 那就连回滚都回不到好版本了。
func tryUpdateLock() (func(), bool) {
	if err := os.MkdirAll(filepath.Dir(updateLockPath), 0o755); err != nil {
		return func() {}, false
	}
	f, err := os.OpenFile(updateLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return func() {}, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

// applyUpdate 备份 → 替换 → 重启 → 健康检查；任何一步失败都回滚。
//
// 备份不是可选项：新版启动即崩时，磁盘上那份跑不起来的就是唯一在场的版本，没有
// .bak 就只能人工上传旧二进制救场。
func applyUpdate(newBinary string, out io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w：要替换二进制并重启 systemd 服务，需要 root", errNothingChanged)
	}
	self, err := selfPath()
	if err != nil {
		return fmt.Errorf("%w：%v", errNothingChanged, err)
	}
	// 只认安装位置那一份。从 /root 下的拷贝跑 update 时替换的是这份拷贝，而
	// systemctl restart 重启的还是没变的旧文件 —— 健康检查照样通过，于是打印
	// 「更新完成」却什么都没更新。
	if self != binaryPath {
		return fmt.Errorf("%w：当前跑的是 %s，不是安装位置 %s；请在安装位置执行",
			errNothingChanged, self, binaryPath)
	}

	// 记下更新前那一份状态文件的时间戳：健康检查要靠它变化来确认新进程真的在写。
	// 读不到（首次启动、文件被清）就留空，任何时间戳都算变化。
	before := ""
	if st, err := ReadStatus(); err == nil {
		before = st.UpdatedAt
	}

	backup := self + ".bak"
	if err := copyBinary(self, backup); err != nil {
		return fmt.Errorf("%w：备份当前版本到 %s 失败: %v", errNothingChanged, backup, err)
	}
	fmt.Fprintf(out, "已备份当前版本到 %s\n", backup)

	if err := update.Apply(newBinary); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", self, err)
	}
	fmt.Fprintf(out, "已替换 %s，重启服务……\n", self)

	if err := runSystemctl("restart", "antapp-linkd"); err != nil {
		return rollback(self, backup, fmt.Errorf("重启失败: %w", err))
	}
	if err := waitHealthy(60*time.Second, before); err != nil {
		return rollback(self, backup, err)
	}
	return nil
}

// rollback 把旧二进制放回去并重启，然后同样做一次健康检查。它自己失败时要讲清旧版本
// 在哪，别让现场既没有能跑的、也没有能回滚的。
func rollback(self, backup string, cause error) error {
	if err := copyBinary(backup, self); err != nil {
		return fmt.Errorf("%v；回滚也失败（旧版本仍在 %s，可用 install -m755 手动放回）: %w", cause, backup, err)
	}
	if err := runSystemctl("restart", "antapp-linkd"); err != nil {
		return fmt.Errorf("%v；文件已回滚但重启失败: %w", cause, err)
	}
	// restart 返回 0 只代表 systemd 接下了请求，不代表服务真起来了
	if err := waitHealthy(30*time.Second, ""); err != nil {
		return fmt.Errorf("%v；文件已回滚但回滚后的服务也没就绪: %w", cause, err)
	}
	return fmt.Errorf("%w（已回滚到备份版本）", cause)
}

// waitHealthy 等到「systemd 认为它 active」且「状态文件被写过一份新的」。
//
// 光看 is-active 会误判：Restart=always 下启动即崩的进程会在 activating 和
// auto-restart 之间循环，systemd 照样能报 active。光看状态文件「新鲜」也会误判：
// 旧进程每 5 秒写一次、重启只要几秒，20 秒窗口内读到的那份必然还是旧的。
// 所以必须等时间戳相对 before 真的变了。
func waitHealthy(timeout time.Duration, before string) error {
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
		if st.UpdatedAt == before {
			last = "状态文件还是启动前那一份（新进程尚未写入）"
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

// selfPath 取当前可执行文件的真实路径（解开符号链接）。
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

// copyBinary 先写同目录的随机临时文件再改名。目标可能是正在运行的程序：直接截断写
// 会拿到 ETXTBSY，而 rename 覆盖正在运行的 ELF 是允许的（旧 inode 留到进程退出）。
func copyBinary(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	// 先落盘再改名，理由同 install_binary
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	tmpName = ""
	syncDir(filepath.Dir(dst))
	return nil
}

// syncDir 把目录项刷到盘上，保证 rename 掉电后不丢。目录 fsync 失败无关紧要
// （部分文件系统不支持），只尽力而为。
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
