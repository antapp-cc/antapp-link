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
)

func main() {
	var (
		genKey  = flag.Bool("genkey", false, "生成密钥对（私钥写到 -key，公钥打印到 stdout）")
		keyPath = flag.String("key", "", "私钥路径")
		in      = flag.String("in", "", "要签名的清单路径")
		out     = flag.String("out", "", "签名输出路径（默认 <in>.sig）")
	)
	flag.Parse()

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
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		die("生成密钥失败: %v", err)
	}
	// 0600：这是唯一能签出合法发布清单的东西，泄漏等于把推送权交出去。
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
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
