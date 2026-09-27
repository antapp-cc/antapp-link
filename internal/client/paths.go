package client

import "path/filepath"

// 客户端在安装目录下按用途分三个子目录，用户翻起来一眼能分清哪是哪。
// 别把它们合成一个 data —— 配置和日志混在一起，想找连接码得先猜文件名。
const (
	ConfigDirName = "config" // 只放配置文件
	LogsDirName   = "logs"   // 只放日志
	DataDirName   = "data"   // 运行时数据：网络快照、界面图标、更新包
)

// ConfigDir 是配置文件目录（连接码 node.conf）。
func ConfigDir(root string) string { return filepath.Join(root, ConfigDirName) }

// LogsDir 是日志目录。
func LogsDir(root string) string { return filepath.Join(root, LogsDirName) }

// RuntimeDir 是运行时数据目录。装的都是程序自己用的东西，用户一般不用管。
func RuntimeDir(root string) string { return filepath.Join(root, DataDirName) }
