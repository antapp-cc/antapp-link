package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antapp-cc/antapp-link/internal/update"
)

// failedVersionPath 记「上一次自动更新失败的版本号」。
//
// 存在的唯一理由：新版本起不来时必须停止重试。timer 每小时跑一次，没有这条记录
// 就会变成每小时重启一次服务、把所有节点机反复打断，而问题一点没解决。
//
// 放 /run 而不是 /var/lib：/run 是 tmpfs，一定可写（daemon 本来就在那儿写状态文件），
// 而 /var/lib 可能因为磁盘满或只读根写不进去 —— 那恰恰是最需要这条记录的时候。
// 代价是机器重启后记录消失，相当于允许再试一次，这个方向是安全的。
//
// 是变量而不是常量：测试要把它指到临时目录，否则会写到真实路径下。
var failedVersionPath = "/run/antapp-link/update-failed"

// updatedToPath 记「上一次成功装上去的版本号」。
//
// 它守的是另一种静默失效：二进制里没注入版本号（例如有人 go build 手搓再 install，
// 正是 deploy/install.sh 装二进制的手法，只是那份来自官方 Release 所以带注入）。
// 那种情况下 Version 恒为默认值，而"更新成功"会永远成立 —— timer 每小时把同一个
// 新版本下载、替换、重启一遍，Version 却永远不变，节点就每小时断一次。
// 有了这条记录，下次就能发现「刚装过 X，自己却还是旧值」并停下来。
//
// 不能拿 failedVersion 兼这个用途：成功路径会把它清掉。
var updatedToPath = "/run/antapp-link/updated-to"

// errNothingChanged 表示失败发生在任何破坏性动作之前：磁盘上还是原来那个版本，
// 没有回滚动作，也不该把这一版记成「失败」永久跳过。
var errNothingChanged = errors.New("未做任何改动")

// cmdUpdate 实现 `antapp-linkd update`：查更新源，有新版本就换掉自己并重启。
//
// 不带 --check 时才会真的替换二进制并重启服务（没有单独的 --apply 开关）。
// 它只替换二进制这一个文件 —— PKI 目录、server.json、已签发的连接码全都不碰，
// 所以 CA 不变、发出去的连接码继续有效。需要动 PKI 或配置格式的版本不能走这条路，
// 得人工跑一遍 deploy/install.sh。
func cmdUpdate(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	checkOnly := fs.Bool("check", false, "只检查有没有新版本，不下载也不重启")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 先确认这台机器确实装了服务端：对着一个不存在的安装做更新只会把文件放错地方
	if _, err := LoadConfig(*cfgPath); err != nil {
		fmt.Fprintf(stderr, "读配置失败（这台机器装了 AntApp Link 服务端吗？）: %v\n", err)
		return 1
	}
	if !*checkOnly && !updateSupported() {
		fmt.Fprintln(stderr, "服务端自动更新只支持 Linux（依赖 systemd 重启与健康检查）")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 手动执行与 timer 可能撞上：拿不到锁就退出，让另一头把这次更新做完。
	// 真撞上很危险 —— 两边会抢同一个备份文件和临时文件，后启动的那个可能把别人
	// 刚换上去的坏版本备份成 .bak，回滚就只能滚回坏的那个。
	if !*checkOnly {
		release, ok, lerr := tryUpdateLock()
		if lerr != nil {
			// 环境坏了（/run 不可写之类）不能当成"别人在跑"，否则更新静默永不执行
			fmt.Fprintf(stderr, "取更新锁失败: %v\n", lerr)
			return 1
		}
		if !ok {
			fmt.Fprintln(stdout, "另一个 update 正在跑，本次跳过")
			return 0
		}
		defer release()
	}

	c := update.NewChecker(Version)
	m, errs := c.Check(ctx)
	for _, e := range errs {
		fmt.Fprintf(stdout, "更新源不可用: %v\n", e)
	}
	if m == nil {
		// 「所有源都没查成」和「确实已是最新」必须分开：混在一起的话，网络长期不通
		// 会表现为一切正常，而实际上更新早就失效了。
		// 判据是"每个源都失败"，不是"有源失败" —— 以后加了备源，主源挂掉而备源
		// 正常回答「已是最新」时不该被算成失败。
		if len(errs) == len(c.Sources) {
			fmt.Fprintf(stderr, "没能检查更新：%d 个更新源都失败\n", len(errs))
			return 1
		}
		fmt.Fprintf(stdout, "已是最新版本（%s）\n", Version)
		return 0
	}
	fmt.Fprintf(stdout, "发现新版本 %s（当前 %s）\n", m.Version, Version)

	if m.Server == nil {
		// 不算失败：这个版本只发了客户端（或清单还是旧版发布脚本生成的）。返回 0，
		// 否则 oneshot unit 每小时被标一次 failed，把 systemctl 状态和 journal 搅浑，
		// 真出问题时反而看不出来。
		fmt.Fprintf(stdout, "清单里没有服务端资产（server 字段）—— 这个版本只发了客户端，跳过\n")
		return 0
	}
	if *checkOnly {
		fmt.Fprintf(stdout, "（--check：到此为止，不下载也不重启）\n")
		return 0
	}
	if failed, _ := readFailedVersion(); failed == m.Version {
		fmt.Fprintf(stdout, "跳过 %s：上一轮更新这个版本失败过，不再反复重启重试\n", m.Version)
		fmt.Fprintf(stdout, "  确认问题已解决后，删掉 %s 就能重新尝试\n", failedVersionPath)
		return 0
	}
	// 上一轮明明把这个版本装上去了，本进程却还是别的版本。两种可能：二进制里的 Version
	// 没被注入（-X 失效，再装也只会白重启），或者运维手动换回了旧版。进程内无从区分，
	// 而把两者都当成错误会让 timer 每小时失败一次、自动更新彻底停摆 —— 所以跳过并提示，
	// 判断留给运维。正常收敛（装完重启后 Version 已是目标）在上面就返回了，走不到这里。
	if applied, _ := readUpdatedTo(); applied == m.Version {
		fmt.Fprintf(stdout, "跳过 %s：上一轮已装过这个版本，但本进程版本仍是 %s\n", m.Version, Version)
		fmt.Fprintf(stdout, "  若是二进制没注入版本号（build.ps1 的 -X），修好后删掉 %s 即可重试\n", updatedToPath)
		return 0
	}

	// 随机名 + 0700：固定路径放 /tmp 会被本机普通用户预先占位，等摘要校验通过之后
	// 再把文件换掉，root 就会装上一个攻击者指定的二进制。
	dir, err := os.MkdirTemp("", "antapp-link-update-")
	if err != nil {
		fmt.Fprintf(stderr, "建下载目录失败: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)
	fmt.Fprintf(stdout, "下载 antapp-linkd %s……\n", m.Version)
	newBin, err := c.DownloadAsset(ctx, *m.Server, dir, "antapp-linkd", nil)
	if err != nil {
		fmt.Fprintf(stderr, "下载失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "已下载并校验 sha256\n")

	if err := applyUpdate(newBin, stdout); err != nil {
		// 只有「已经替换过、可能已经把服务搞坏」才值得锁掉这一版。备份失败、路径不对、
		// 替换没落到目标文件上（update.ErrNotApplied）这些现场都没动，锁掉它只会白挡
		// 一个版本，还让人以为自己在旧版本上。
		if !errors.Is(err, errNothingChanged) && !errors.Is(err, update.ErrNotApplied) {
			if werr := writeFailedVersion(m.Version); werr != nil {
				fmt.Fprintf(stderr, "警告：失败记录没写成（%v），下一轮可能重试同一个版本\n", werr)
			} else {
				fmt.Fprintf(stderr, "已记下失败版本，同期不再重试（删掉 %s 可强制重来）\n", failedVersionPath)
			}
		}
		fmt.Fprintf(stderr, "更新失败: %v\n", err)
		return 1
	}
	clearFailedVersion()
	if werr := writeUpdatedTo(m.Version); werr != nil {
		fmt.Fprintf(stderr, "警告：收敛记录没写成（%v），版本号没注入的话下一轮还会重装一次\n", werr)
	}
	fmt.Fprintf(stdout, "更新完成：%s → %s\n", Version, m.Version)
	return 0
}

func readFailedVersion() (string, error) {
	raw, err := os.ReadFile(failedVersionPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func writeFailedVersion(v string) error {
	if err := os.MkdirAll(filepath.Dir(failedVersionPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(failedVersionPath, []byte(v+"\n"), 0o644)
}

func clearFailedVersion() { _ = os.Remove(failedVersionPath) }

func readUpdatedTo() (string, error) {
	raw, err := os.ReadFile(updatedToPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func writeUpdatedTo(v string) error {
	if err := os.MkdirAll(filepath.Dir(updatedToPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(updatedToPath, []byte(v+"\n"), 0o644)
}
