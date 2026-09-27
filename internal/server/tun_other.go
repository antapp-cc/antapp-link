//go:build !linux

package server

import "errors"

// ErrNoTUN 让「在 Windows 开发机上跑服务端」这件事给出明确提示，而不是一个看不懂的报错。
var ErrNoTUN = errors.New("server: 只有 Linux 能开 tun 网卡，服务端请在云服或 WSL 里跑")

type TUN struct{}

func OpenTUN(string) (*TUN, error)              { return nil, ErrNoTUN }
func (t *TUN) Name() string                     { return "" }
func (t *TUN) Configure(string, int, int) error { return ErrNoTUN }
func (t *TUN) Read([]byte) (int, error)         { return 0, ErrNoTUN }
func (t *TUN) Write([]byte) (int, error)        { return 0, ErrNoTUN }
func (t *TUN) Close() error                     { return nil }
