//go:build !linux

package server

import (
	"errors"
	"log/slog"
)

// ErrNoNetfilter 让「在 Windows 开发机上跑 up/down」给出明确提示。
var ErrNoNetfilter = errors.New("server: netfilter 只在 Linux 上可用，请在云服或 WSL 里执行")

func Up(Config, *slog.Logger, bool) error { return ErrNoNetfilter }
func Down(Config, *slog.Logger) error     { return ErrNoNetfilter }
func CheckForwardPortsFree(Config) error  { return ErrNoNetfilter }
