// antapp-sign 生成密钥对、给发布清单签名。
//
// 私钥离线保管，只在发布时手动跑一次，不参与日常构建、不进仓库。公钥填进
// internal/update/pubkey.go 后编译进两端 —— 换密钥对意味着所有已部署节点都要先升级
// 一次（它们内置的是旧公钥），所以别轻易换。
//
//	antapp-sign -genkey -key release.key          # 生成密钥对，把打印的公钥填进 pubkey.go
//	antapp-sign -key release.key -in dist/latest.json -out dist/latest.json.sig
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"

	"github.com/antapp-cc/antapp-link/internal/update"
)

func main() {
	var (
		genKey  = flag.Bool("genkey", false, "生成密钥对（私钥写到 -key，公钥打印到 stdout）")
		verify  = flag.Bool("verify", false, "用编译期内置的公钥校验签名（发布后自检，等同节点端行为）")
		keyPath = flag.String("key", "", "私钥路径")
		in      = flag.String("in", "", "要签名的清单路径")
		out     = flag.String("out", "", "签名输出路径（默认 <in>.sig）")
	)
	flag.Parse()

	if *verify {
		if *in == "" {
			die("-verify 需要 -in")
		}
		if *out == "" {
			*out = *in + ".sig"
		}
		doVerify(*in, *out)
		return
	}
	if *keyPath == "" {
		die("-key 必填")
	}
	if *genKey {
		doGenKey(*keyPath)
		return
	}
	if *in == "" {
		die("-in 必填（或加 -genkey 生成密钥对）")
	}
	if *out == "" {
		*out = *in + ".sig"
	}
	doSign(*keyPath, *in, *out)
}

func doGenKey(path string) {
	// 绝不复用已有私钥文件：误跑一次 -genkey 就换掉了发布密钥，而已部署节点内置的是
	// 旧公钥 —— 新密钥签出来的清单会被全网拒收，表现为「自动更新突然全停」而发布侧
	// 毫无报错。真要换，先手动把旧文件移走。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			die("私钥已存在：%s\n"+
				"  换密钥对意味着所有已部署节点都要先升级一次（它们内置的是旧公钥）。\n"+
				"  确认要换就先把它移走或删掉，再重跑。", path)
		}
		die("创建私钥失败: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		_ = f.Close()
		die("生成密钥失败: %v", err)
	}
	// 0600：这是唯一能签出合法发布清单的东西，泄漏等于把推送权交出去。
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv)); err != nil {
		_ = f.Close()
		die("写私钥失败: %v", err)
	}
	if err := f.Close(); err != nil {
		die("写私钥失败: %v", err)
	}
	fmt.Printf("私钥已写入 %s（0600，离线保管，别进仓库）\n\n", path)
	fmt.Printf("把下面这行填进 internal/update/pubkey.go 的 releasePubKey：\n\n")
	fmt.Printf("\tvar releasePubKey = [32]byte{%s}\n\n", byteList(pub))
	fmt.Printf("（base64 形式，供备查：%s）\n", base64.StdEncoding.EncodeToString(pub))
}

func doSign(keyPath, in, out string) {
	rawKey, err := os.ReadFile(keyPath)
	if err != nil {
		die("读私钥失败: %v", err)
	}
	keyBytes, err := base64.StdEncoding.DecodeString(string(trimSpace(rawKey)))
	if err != nil || len(keyBytes) != ed25519.PrivateKeySize {
		die("私钥格式不对（应是 -genkey 产生的 base64 文件）")
	}
	manifest, err := os.ReadFile(in)
	if err != nil {
		die("读清单失败: %v", err)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(keyBytes), manifest)
	if err := os.WriteFile(out, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		die("写签名失败: %v", err)
	}
	fmt.Printf("已签名：%s（%d 字节清单）\n签名写入：%s\n", in, len(manifest), out)
	fmt.Printf("发布时把 %s 和 %s 一起传到 release 资产里。\n", in, out)
}

// doVerify 用编译期内置的公钥校验签名 —— 就是节点端会做的那件事。
//
// 私钥与内置公钥不配对时（改过 pubkey.go、或用了别的私钥），这里立刻失败，
// 而不是等所有节点拒收更新才发现。
func doVerify(in, sigPath string) {
	manifest, err := os.ReadFile(in)
	if err != nil {
		die("读清单失败: %v", err)
	}
	sigText, err := os.ReadFile(sigPath)
	if err != nil {
		die("读签名失败: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(string(trimSpace(sigText)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		die("签名格式不对（应是 base64 编码的 %d 字节）", ed25519.SignatureSize)
	}
	pub := update.ReleasePublicKey()
	if !ed25519.Verify(ed25519.PublicKey(pub[:]), manifest, sig) {
		die("签名校验失败：私钥与二进制内置的公钥不配对，这个清单会被所有节点拒收。\n" +
			"  确认用的是发布私钥（~/.antapp-release/release.key），且 pubkey.go 没被改过。")
	}
	fmt.Printf("签名校验通过：%s 与内置公钥配对（节点端会接受）\n", in)
}

func byteList(b []byte) string {
	s := ""
	for i, v := range b {
		if i > 0 {
			s += ", "
		}
		if i%8 == 0 && i > 0 {
			s += "\n\t"
		}
		s += fmt.Sprintf("0x%02x", v)
	}
	return s
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == '\n' || b[0] == '\r' || b[0] == ' ') {
		b = b[1:]
	}
	return b
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "antapp-sign: "+format+"\n", args...)
	os.Exit(1)
}
