//go:build !windows

// Package setup 实现 AntApp Link 的安装与卸载。安装器只做 Windows。
package setup

import "errors"

const (
	AppName       = "AntApp Link"
	AppExeName    = "antapp-link.exe"
	AutostartTask = "AntAppLink"
)

var Version = "0.1.0"

var ErrNotWindows = errors.New("setup: 安装程序只在 Windows 上可用")

type Options struct {
	InstallDir string
	SourceDir  string
	DataDir    string

	StartMenu bool
	Desktop   bool
	Launch    bool

	RemoveData bool
}

func DefaultOptions() Options { return Options{} }

func DefaultInstallDir() string { return "" }
func DefaultDataDir() string    { return "" }

func Installed() (Options, bool) { return Options{}, false }

func Install(Options, func(string)) error   { return ErrNotWindows }
func Uninstall(Options, func(string)) error { return ErrNotWindows }

func RelaunchFromTempIfNeeded(func(string)) (bool, error) { return false, ErrNotWindows }
