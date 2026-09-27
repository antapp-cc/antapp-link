package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// PublicIPv4 探测云服的公网出口地址。
//
// 先问外部服务 —— 那才是 NAT 之后的真实出口；拿不到再退回本机路由表。
// 取不到就报错而绝不瞎猜：生成一个 remote 是内网地址的连接码毫无意义。
func PublicIPv4() (string, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	for _, url := range []string{"https://api.ipify.org", "https://ifconfig.me/ip"} {
		if ip, err := fetchText(client, url); err == nil && IsPublicIPv4(ip) {
			return strings.TrimSpace(ip), nil
		}
	}
	if ip := routeSourceIP(); IsPublicIPv4(ip) {
		return ip, nil
	}
	return "", errors.New("探测不到公网 IPv4，请用 --server 显式指定服务端地址")
}

func fetchText(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func routeSourceIP() string {
	out, err := exec.Command("ip", "-4", "route", "get", "1.1.1.1").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "src" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// reservedIPv4 覆盖私网、回环、链路本地、CGNAT、文档用段、组播与保留段。
// 少一条就可能把内网地址当成公网地址写进连接码。
var reservedIPv4 = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

func IsPublicIPv4(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	for _, cidr := range reservedIPv4 {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if block.Contains(v4) {
			return false
		}
	}
	return true
}
