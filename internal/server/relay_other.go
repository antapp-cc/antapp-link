//go:build !linux

package server

import (
	"context"
	"log/slog"
)

// 端口转发器只在 Linux 上有实体（服务端只发布 Linux 版）。
func startPortRelay(context.Context, Config, *slog.Logger) (func(), error) {
	return func() {}, nil
}
