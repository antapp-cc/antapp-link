package server

import (
	"context"
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
// 是变量而不是常量：测试要把它指到临时目录，否则会写到真实的 /var/lib 下。
var failedVersionPath = "/var/lib/antapp-link/update-failed"

// cmdUpdate 实现 `antapp-linkd update`：查更新源，有新版本就换掉自己并重启。
//
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	c := update.NewChecker(Version)
	m, errs := c.Check(ctx)
	for _, e := range errs {
		fmt.Fprintf(stdout, "更新源不可用: %v\n", e)
	}
	if m == nil {
		fmt.Fprintf(stdout, "已是最新版本（%s）\n", Version)
		return 0
	}
	fmt.Fprintf(stdout, "发现新版本 %s（当前 %s）\n", m.Version, Version)

	if m.Server == nil {
		fmt.Fprintf(stderr, "清单里没有服务端资产（server 字段）—— 这个版本只发了客户端\n")
		return 1
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

	dir := filepath.Join(os.TempDir(), "antapp-link-update")
	defer os.RemoveAll(dir)
	fmt.Fprintf(stdout, "下载 antapp-linkd %s……\n", m.Version)
	newBin, err := c.DownloadAsset(ctx, *m.Server, dir, "antapp-linkd", nil)
	if err != nil {
		fmt.Fprintf(stderr, "下载失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "已下载并校验 sha256\n")

	if err := applyUpdate(newBin, stdout); err != nil {
		_ = writeFailedVersion(m.Version)
		fmt.Fprintf(stderr, "更新失败，已回滚: %v\n", err)
		fmt.Fprintf(stderr, "已记下失败版本，同期不再重试（删掉 %s 可强制重来）\n", failedVersionPath)
		return 1
	}
	clearFailedVersion()
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
