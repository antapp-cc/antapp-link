// Command antapp-linkd 是 AntApp Link 的服务端。子命令见 `antapp-linkd help`。
package main

import (
	"os"

	"github.com/antapp-cc/antapp-link/internal/server"
)

func main() {
	os.Exit(server.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
