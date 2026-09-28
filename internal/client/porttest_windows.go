package client

// Node端口测试：借 Pi 官方的 pi-node-port-checker 容器做应答器，从本机连云服公网的
// 转发端口，验证「云服转发器 → 隧道 → 节点容器」整条链路。
//
// checker 占用全部 31400-31409，与节点容器发布的端口互斥，Pi Desktop 官方测端口
// 也是先停节点再测——这里照搬这个时序，测完把环境恢复原样。Pi Network 程序在
// 后台守护节点容器，不停它的话 testnet2 会被立刻拉起来，所以也要先退出。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	nodeContainer    = "testnet2"
	checkerContainer = "pi-port-checker"
	piAppName        = "Pi Network.exe"
	portTestFirst    = 31400
	portTestLast     = 31409
)

// PortTestResult 是单个端口的探测结果。
type PortTestResult struct {
	Port int
	OK   bool
	Note string
}

// RunPortTest 完整跑一遍端口测试，并在结束后恢复节点环境。
// serverIP 是云服公网 IP（从连接码的服务端地址里取主机部分）。
func RunPortTest(serverIP string, log *slog.Logger) ([]PortTestResult, error) {
	docker, err := dockerCmd()
	if err != nil {
		return nil, err
	}
	run := func(timeout time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, docker, args...).CombinedOutput()
		return string(out), err
	}

	log.Info("检查 Docker 环境……")
	if out, err := run(10*time.Second, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return nil, fmt.Errorf("Docker 未运行（%s）", strings.TrimSpace(out))
	}

	_, piExe, piRunning := findProcess(piAppName)
	if piRunning {
		log.Info("临时退出 Pi 界面程序（测完自动拉起）……")
		closeApp(log)
	} else {
		log.Info("Pi 界面程序未在运行")
	}

	log.Info("停止节点容器 testnet2……")
	_, _ = run(60*time.Second, "stop", nodeContainer)

	log.Info("启动端口应答容器……")
	if out, err := run(60*time.Second, "start", checkerContainer); err != nil {
		log.Warn("端口应答容器启动失败，恢复节点", "输出", strings.TrimSpace(out))
		_, _ = run(60*time.Second, "start", nodeContainer)
		if piRunning {
			relaunchApp(piExe)
		}
		return nil, fmt.Errorf("启动 %s 失败（先用 Pi Desktop 跑一次端口检查来创建它）", checkerContainer)
	}

	// node 冷启动到 10 个端口全部监听要几秒，等容器 health 变 healthy 再开始探测
	log.Info("等待端口应答容器就绪……")
	for i := 0; i < 12; i++ {
		out, err := run(5*time.Second, "inspect", "--format", "{{.State.Health.Status}}", checkerContainer)
		if err == nil && strings.TrimSpace(out) == "healthy" {
			break
		}
		time.Sleep(2 * time.Second)
	}

	log.Info("开始逐端口探测（本机 → 云服转发 → 隧道 → 节点应答）……")
	results := make([]PortTestResult, 0, portTestLast-portTestFirst+1)
	for port := portTestFirst; port <= portTestLast; port++ {
		res := probePort(serverIP, port)
		results = append(results, res)
		if res.OK {
			log.Info(fmt.Sprintf("端口 %d ✓ 应答正常", port))
		} else {
			log.Warn(fmt.Sprintf("端口 %d ✗ %s", port, res.Note))
		}
	}

	log.Info("恢复节点环境……")
	_, _ = run(60*time.Second, "stop", checkerContainer)
	_, _ = run(60*time.Second, "start", nodeContainer)
	if piRunning {
		relaunchApp(piExe)
	}
	return results, nil
}

// probePort 连一个转发端口，发 HTTP 请求，收到 checker 的特征应答才算通。
func probePort(serverIP string, port int) PortTestResult {
	res := PortTestResult{Port: port, Note: "无应答"}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(serverIP, strconv.Itoa(port)), 4*time.Second)
	if err != nil {
		res.Note = "连接失败（隧道通吗？）"
		return res
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// checker 是 HTTP 服务：不发请求它就一直等，裸 TCP 干等只会双端超时
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.0\r\n\r\n"); err != nil {
		res.Note = "发送失败"
		return res
	}
	data, _ := io.ReadAll(conn)
	if strings.Contains(string(data), fmt.Sprintf("OK FROM PORT %d", port)) {
		res.OK = true
		res.Note = ""
		return res
	}
	if note := strings.TrimSpace(string(data)); note != "" {
		res.Note = "应答异常: " + note
	}
	return res
}

func dockerCmd() (string, error) {
	if p, err := exec.LookPath("docker"); err == nil {
		return p, nil
	}
	const fallback = `C:\Program Files\Docker\Docker\resources\bin\docker.exe`
	if _, err := os.Stat(fallback); err == nil {
		return fallback, nil
	}
	return "", errors.New("找不到 docker 命令——Docker Desktop 未安装或未运行")
}

// findProcess 按进程名找进程，返回 PID 和 exe 完整路径（退出界面后拉起要用）。
func findProcess(name string) (pid uint32, exe string, running bool) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, "", false
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			continue
		}
		var buf [1024]uint16
		n := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
			exe = windows.UTF16ToString(buf[:n])
		}
		windows.CloseHandle(h)
		return pe.ProcessID, exe, true
	}
	return 0, "", false
}

// closeApp 退出 Pi Network 的全部进程。它是多进程 Electron 应用，且点 × 常常只是
// 缩托盘不退出——必须按映像名杀干净并确认，留一个守护进程在就会把节点拉回去。
func closeApp(log *slog.Logger) {
	_ = exec.Command("taskkill", "/IM", piAppName).Run()
	if waitGone(8 * time.Second) {
		return
	}
	_ = exec.Command("taskkill", "/F", "/IM", piAppName).Run()
	if waitGone(5 * time.Second) {
		return
	}
	log.Warn("Pi 界面程序未能完全退出，测试结果可能不准")
}

func waitGone(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, _, running := findProcess(piAppName); !running {
			return true
		}
		time.Sleep(time.Second)
	}
	_, _, running := findProcess(piAppName)
	return !running
}

func relaunchApp(exe string) {
	if exe == "" {
		return
	}
	_ = exec.Command(exe).Start()
}
