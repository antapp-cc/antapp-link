package client

import (
	_ "embed"
	"strings"
	"sync"
)

// cnRoutesRaw 是中国大陆 IPv4 网段列表（APNIC 分配记录聚合后）。
//
// 为什么内置而不是运行时下载：分流要覆盖「服务端出口出问题时国内仍可用」这个场景，
// 而那种时候网络恰恰是不可靠的。列表随 APNIC 分配变化很慢，内置一份够用很久。
//
//go:embed cn_routes.txt
var cnRoutesRaw string

var (
	cnRoutesOnce sync.Once
	cnRoutes     []string
)

// CNRoutes 返回国内网段。首次调用时解析并缓存。
func CNRoutes() []string {
	cnRoutesOnce.Do(func() {
		for _, line := range strings.Split(cnRoutesRaw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			cnRoutes = append(cnRoutes, line)
		}
	})
	return cnRoutes
}
