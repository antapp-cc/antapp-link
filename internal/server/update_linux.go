//go:build linux

package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antapp-cc/antapp-link/internal/update"
)

func updateSupported() bool { return true }

// updateLockPath 是互斥锁文件。放 /run：tmpfs 一定可写，机器重启后也会自动清掉，
// 不会留下一个锁死后续更新的僵尸文件。
const updateLockPath = "/run/antapp-link/update.lock"

// tryUpdateLock 拿非阻塞排他锁。
//
// 返回的 err 非空表示"锁根本没拿上手"（/run 不可写、只读根之类）—— 调用方要如实报错
// 退出，不能当成"别人在跑"，否则环境坏掉时更新会静默永不执行。
// ok=false 且 err=nil 才是真的被另一个 update 占着。
func tryUpdateLock() (release func(), ok bool, err error) {
	if err := os.MkdirAll(filepath.Dir(updateLockPath), 0o755); err != nil {
		return func() {}, false, fmt.Errorf("建锁目录失败: %w", err)
	}
	f, err := os.OpenFile(updateLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, false, fmt.Errorf("打开锁文件失败: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return func() {}, false, nil
		}
		return func() {}, false, fmt.Errorf("锁 %s 失败: %w", updateLockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
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

	// 健康判据要两条互相独立的信号：状态文件被"新进程"重写过（before），以及
	// systemd 记的重启次数没涨。只看前者会被崩溃循环骗过去 —— 每次重启后
	// reportStatus 都会写出一份新的时间戳。
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
	// 重启之后再取计数基线，不能拿重启前那个比。
	//
	// systemd 在非自动重启的启动流程里会把 NRestarts 清零（v252 的 service.c 如此），
	// 于是「旧实例 7、新实例从 0 涨到 1」的对比会判成「没涨」而漏掉崩溃。现在基线
	// 取的是新实例自己的起点：清零了就是 0，没清零就是它保留的值，两种都对。
	baseline, berr := serviceRestarts()
	if berr != nil {
		// 读不到基线就直接判失败：没有它，崩溃检查等于不存在
		return rollback(self, backup, fmt.Errorf("无法读取重启计数基线: %w", berr))
	}
	if err := waitHealthy(60*time.Second, before, baseline); err != nil {
		return rollback(self, backup, err)
	}
	return nil
}

// rollback 把旧二进制放回去并重启，然后同样做一次健康检查。它自己失败时要讲清旧版本
// 在哪，别让现场既没有能跑的、也没有能回滚的。
func rollback(self, backup string, cause error) error {
	// 回滚后的检查也要有"相对起点"：坏版本刚写过状态文件，拿它当基准等于没判
	before := ""
	if st, err := ReadStatus(); err == nil {
		before = st.UpdatedAt
	}

	if err := copyBinary(backup, self); err != nil {
		return fmt.Errorf("%v；回滚也失败（旧版本仍在 %s，可用 install -m755 手动放回）: %w", cause, backup, err)
	}
	if err := runSystemctl("restart", "antapp-linkd"); err != nil {
		return fmt.Errorf("%v；文件已回滚但重启失败: %w", cause, err)
	}
	// 同 applyUpdate：基线取回滚之后新实例自己的起点；读不到就按失败处理
	baseline, berr := serviceRestarts()
	if berr != nil {
		return fmt.Errorf("%v；文件已回滚但无法确认新实例是否稳定: %w", cause, berr)
	}
	if err := waitHealthy(30*time.Second, before, baseline); err != nil {
		return fmt.Errorf("%v；文件已回滚但回滚后的服务也没就绪: %w", cause, err)
	}
	return fmt.Errorf("%w（已回滚到备份版本）", cause)
}

// waitHealthy 要求三件事同时成立，并且连续两次采样都健康。
//
// 三个误判必须都堵上：
//   - 只看 is-active：Restart=always 下启动即崩的进程在 activating 与 auto-restart
//     之间循环，systemd 照样报 active；
//   - 只看状态文件新鲜：旧进程每 5 秒写一次、重启只要几秒，读到的还是旧的那份；
//   - 只看时间戳相对 before 变化：崩溃循环里每次重启都会写出新时间戳，照样骗过去。
//
// 最后一条正是靠 NRestarts 堵的 —— 它是"起来又崩"最直接的证据。
func waitHealthy(timeout time.Duration, before string, restartsBefore int) error {
	deadline := time.Now().Add(timeout)
	last := "（还没有一次检查完成）"
	healthy := 0
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		n, rerr := serviceRestarts()
		if rerr != nil {
			// 读不到重启计数就不能判健康：拿不到"起来又崩"的证据，等于放行。
			// 安全方向必须 fail-closed（旧版这里返回 0，崩溃会被完全掩盖）。
			last, healthy = "无法确认服务是否稳定: "+rerr.Error(), 0
			continue
		}
		if n > restartsBefore {
			// 重启次数涨了就没什么可等的：它已经证明自己稳不住
			return fmt.Errorf("新版本启动后崩溃重启了 %d 次（systemd NRestarts %d → %d）",
				n-restartsBefore, restartsBefore, n)
		}
		if !serviceActive() {
			last, healthy = "服务未处于 active", 0
			continue
		}
		st, err := ReadStatus()
		if err != nil {
			last, healthy = fmt.Sprintf("读状态文件失败: %v", err), 0
			continue
		}
		if st.UpdatedAt == before {
			last, healthy = "状态文件还是启动前那一份（新进程尚未写入）", 0
			continue
		}
		if !st.Fresh() {
			last, healthy = "状态文件没有刷新", 0
			continue
		}
		// 转发器是节点机访问服务的入口。它启动失败时进程照样健康、状态文件照样刷新，
		// 但节点机的端口转发全不通 —— 这种「活着但没用」不能算健康，否则坏版本会被
		// 记成更新成功、跳过回滚。
		//
		// 只认「已确认失败」：状态文件来自旧版 daemon 时 ForwardKnown 是 false，
		// 那种未知状态不能当成失败（否则老版本一升级就被误回滚）。
		if st.ForwardKnown && !st.ForwardReady {
			last, healthy = "端口转发器启动失败: "+st.ForwardError, 0
			continue
		}
		// 连两次都健康才算：单次采样可能正好落在两次崩溃之间
		healthy++
		if healthy >= 2 {
			return nil
		}
		last = "第一次健康，等第二次确认"
	}
	return fmt.Errorf("新版本启动后 %s 内没就绪：%s", timeout, last)
}

// serviceRestarts 读 systemd 记的服务重启次数。
//
// 它是"新版本没稳住"最直接的证据：崩溃重启会让它 +1，而状态文件时间戳做不到这一点
// （每次重启后都会写出新的时间戳）。
//
// **读不到就报错，不能返回 0**：调用方拿 0 当基线的话，"0 > 0 = false" 会让崩溃
// 完全不可见 —— 服务恰好 active、状态文件恰好新鲜，坏版本就被判成健康、跳过回滚。
// 这是安全方向的判断，必须 fail-closed。
func serviceRestarts() (int, error) {
	out, err := exec.Command("systemctl", "show", "-p", "NRestarts", "--value", "antapp-linkd").Output()
	if err != nil {
		return 0, fmt.Errorf("查 systemd 重启计数失败: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("解析 systemd 重启计数失败（输出 %q）: %w", raw, err)
	}
	return n, nil
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
//
// 与 internal/update 的 installBinary 是同一套手法（那边的目标是自己、这边是显式路径），
// 改任意一处时记得同步另一处。
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
