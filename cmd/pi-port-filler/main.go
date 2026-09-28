// Command pi-port-filler 让 Pi 节点要求的 31400-31409 端口段始终对外可达。
//
// Pi 的容器只绑定自己用到的端口，其余端口没有服务监听，外部检查就会显示
// 「不通」。本工具补位监听空闲端口（接受 TCP 连接即保持，直到对端关闭），
// 让整段端口全部可达；每 2 秒重扫一次，容器新绑定的端口自动让位。
//
// 与云服端口转发的配合：外部 → 云服 31400-31409 → 隧道 → 节点机本工具。
//
// 让位规则：本工具只补位「空闲」端口；若以后 Pi 容器要绑定被补位的端口，
// 重启本工具（或重启电脑后先启动 Docker）即可让它让出全部端口。
package main

import (
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	portLo = 31400
	portHi = 31409
)

var (
	mu    sync.Mutex
	holds = map[int]net.Listener{}
)

func main() {
	fmt.Println("pi-port-filler: 监听", portLo, "-", portHi, "中所有空闲端口（每 2s 重扫）")
	for {
		scan()
		time.Sleep(2 * time.Second)
	}
}

// scan 补齐空闲端口、释放已不该由我们持有的端口。
func scan() {
	mu.Lock()
	defer mu.Unlock()

	for port := portLo; port <= portHi; port++ {
		if _, ok := holds[port]; ok {
			continue // 已在监听
		}
		addr := net.TCPAddr{IP: net.IPv4zero, Port: port}
		ln, err := net.ListenTCP("tcp4", &addr)
		if err != nil {
			continue // 别人已占用（Pi 容器等），不碰
		}
		holds[port] = ln
		fmt.Println("补位监听 :", port)
		go serve(port, ln)
	}
}

// serve 接受连接并保持到对端关闭。端口检查类工具只测 TCP 能否建立，
// 这里不需要回应任何应用层内容。
func serve(port int, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			mu.Lock()
			delete(holds, port)
			mu.Unlock()
			return
		}
		go func(c net.Conn) {
			buf := make([]byte, 4096)
			for {
				if _, err := c.Read(buf); err != nil {
					c.Close()
					return
				}
			}
		}(conn)
	}
}
