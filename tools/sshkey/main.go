// Command sshkey 用密码登录一台新机器，把本机公钥装进 authorized_keys。
//
// 为什么需要它：集群里的机器归 ~/.ssh/config 管，而 config 里的主机都用密钥认证。
// 新机器只有密码，得先有一次密码登录把密钥装上，之后才纳入常规运维。
//
//	go run ./tools/sshkey -host 1.2.3.4 -port 22 -user root -pubkey ~/.ssh/id_ed25519.pub
//
// 密码从环境变量 SSH_PASSWORD 读 —— 不走命令行参数，免得留在 shell 历史和进程列表里。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	os.Exit(run())
}

func run() int {
	host := flag.String("host", "", "目标主机")
	port := flag.Int("port", 22, "SSH 端口")
	user := flag.String("user", "root", "登录用户")
	pubPath := flag.String("pubkey", "", "本机公钥路径（默认 ~/.ssh/id_ed25519.pub）")
	flag.Parse()

	if *host == "" {
		fmt.Fprintln(os.Stderr, "缺少 -host")
		return 2
	}
	password := os.Getenv("SSH_PASSWORD")
	if password == "" {
		fmt.Fprintln(os.Stderr, "请把密码放进环境变量 SSH_PASSWORD")
		return 2
	}

	if *pubPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*pubPath = filepath.Join(home, ".ssh", "antapp_codex_ed25519.pub")
	}
	pubRaw, err := os.ReadFile(*pubPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读公钥 %s 失败: %v\n", *pubPath, err)
		return 1
	}
	pubKey := strings.TrimSpace(string(pubRaw))
	if pubKey == "" {
		fmt.Fprintln(os.Stderr, "公钥文件是空的")
		return 1
	}

	cfg := &ssh.ClientConfig{
		User:            *user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 首次接触，指纹还没得比
		Timeout:         20 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", *host, *port)
	fmt.Printf("连接 %s ...\n", addr)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSH 连接/认证失败: %v\n", err)
		return 1
	}
	defer client.Close()
	fmt.Println("认证成功")

	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer session.Close()

	// 幂等：已经在里面就不重复追加
	script := fmt.Sprintf(`
set -e
mkdir -p ~/.ssh && chmod 700 ~/.ssh
touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
if grep -qF %q ~/.ssh/authorized_keys; then
  echo "公钥已存在，跳过"
else
  echo %q >> ~/.ssh/authorized_keys
  echo "公钥已写入"
fi
echo "--- 当前 authorized_keys ---"
awk '{print $1, substr($2,1,20)"...", $3}' ~/.ssh/authorized_keys
`, pubKey, pubKey)

	var out bytes.Buffer
	session.Stdout = &out
	session.Stderr = os.Stderr
	if err := session.Run(script); err != nil {
		fmt.Fprintf(os.Stderr, "执行失败: %v\n", err)
		return 1
	}
	fmt.Println(out.String())
	return 0
}
