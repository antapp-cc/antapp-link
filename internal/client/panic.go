package client

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WritePanicLog 把 panic 现场写到 logs\panic-<时间>.log。
//
// GUI 程序没有控制台，panic 只打到 stderr 就丢了：进程静默消失、事件日志和
// WER 都不留痕。之前「好多电脑自己关了」就是靠这条日志才能看出原因。
func WritePanicLog(rootDir string, v any, stack []byte) error {
	dir := LogsDir(rootDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("panic-%s.log", time.Now().Format("20060102-150405"))
	body := fmt.Sprintf("time: %s\nversion: %s\npanic: %v\n\n%s\n",
		time.Now().Format(time.RFC3339), Version, v, stack)
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
}
