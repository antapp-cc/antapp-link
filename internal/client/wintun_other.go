//go:build !windows

package client

// WintunDevice 在非 Windows 平台只是个占位，让 go test ./... 能在开发机上跑通。
type WintunDevice struct{}

func OpenDevice() (*WintunDevice, error) { return nil, ErrNoWintun }

func (d *WintunDevice) Read([]byte) (int, error)    { return 0, ErrNoWintun }
func (d *WintunDevice) Write(p []byte) (int, error) { return 0, ErrNoWintun }
func (d *WintunDevice) Name() string                { return "" }
func (d *WintunDevice) Close() error                { return nil }
