// Command updatestest 端到端跑一遍更新流程：检查 → 下载 → 校验 → 替换自身。
//
// 有了它，整条链路能在本地验完，不用为了测一次更新真去发一个 GitHub Release。
//
//	go build -ldflags "-X main.Version=0.1.0" -o old.exe ./tools/updatetest
//	./old.exe -sources http://127.0.0.1:8899/latest.json -apply
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/antapp-cc/antapp-link/internal/client"
	"github.com/antapp-cc/antapp-link/internal/pki"
)

// 注意版本号：比较用的是 client.Version，所以构建时要注入它 ——
//
//	go build -ldflags "-X github.com/antapp-cc/antapp-link/internal/client.Version=0.1.0" ...
func main() {
	os.Exit(run())
}

func run() int {
	dir := flag.String("dir", "", "数据目录（下载产物放它的 update/ 下）")
	sources := flag.String("sources", "", "覆盖更新源 URL（分号分隔）")
	apply := flag.Bool("apply", false, "下载完成后真的替换自己并重启")
	flag.Parse()

	if *sources != "" {
		if err := os.Setenv("ANTAPP_UPDATE_SOURCES", *sources); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if *dir == "" {
		d, err := os.MkdirTemp("", "antapp-updatetest")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*dir = d
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	app := client.NewApp(pki.Invite{}, *dir, logger)

	ctx := context.Background()
	m, err := app.CheckUpdate(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "检查更新失败: %v\n", err)
		return 1
	}
	if m == nil {
		fmt.Printf("已是最新（当前 %s）\n", client.Version)
		return 0
	}
	fmt.Printf("发现新版本 %s（当前 %s）\n", m.Version, client.Version)

	path, err := app.DownloadUpdate(ctx, m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
		return 1
	}
	fmt.Printf("下载并校验通过: %s\n", path)

	if !*apply {
		fmt.Println("（加 -apply 才会替换自身）")
		return 0
	}

	fmt.Println("替换自身并启动新版本……")
	if err := app.ApplyUpdate(path); err != nil {
		fmt.Fprintf(os.Stderr, "替换失败: %v\n", err)
		return 1
	}
	// 成功之后必须立刻退出：此刻磁盘上的文件名已经归新版所有
	return 0
}
