//go:build !linux

package server

import (
	"fmt"
	"io"
)

func Install(stdout, stderr io.Writer, args []string) int {
	fmt.Fprintln(stderr, "install 只在 Linux（systemd）上可用；服务端请部署到云服或 WSL")
	return 1
}
