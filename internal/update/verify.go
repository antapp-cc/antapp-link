package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrUnsigned 表示清单没有可用签名。调用方必须把它当成拒绝理由，不能降级放行 ——
// 允许无签名清单就等于整条验签链路不存在。
var ErrUnsigned = errors.New("update: 清单缺少签名")

// verifyManifest 校验清单原始字节的 Ed25519 签名。
//
// 必须在校验 sha256、解析 JSON 之前做：清单里的 sha256 与下载 URL 出自同一份文件，
// 先信 sha256 等于没校验 —— 能改 Release 的人可以把摘要和 URL 一起换掉。而服务端的
// 自动更新以 root 运行，一次投毒就是所有节点上的任意代码执行。
//
// pub 由调用方给（Checker.PubKey，默认内置的那把），这样 tools/updatetest 这类
// 本地联调工具能注入测试密钥，不必拿生产私钥签本地清单。
func verifyManifest(raw, sigText []byte, pub [32]byte) error {
	// 比的是内容而不是 len()：pub 是 [32]byte，len() 是编译期常量 32，拿它跟
	// PublicKeySize 比是恒假的死条件（这道防线曾经等于不存在）。
	if pub == [32]byte{} {
		return errors.New("update: 公钥无效（未内置或全零），拒绝自动更新")
	}
	sig, err := decodeSignature(sigText)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub[:]), raw, sig) {
		return errors.New("update: 清单签名校验失败（清单被改动，或签名与清单不是一对）")
	}
	return nil
}

// decodeSignature 接受 base64 或 hex，容忍首尾空白 —— 签名文件常由 shell 重定向产生，
// 尾部多半带一个换行。
func decodeSignature(text []byte) ([]byte, error) {
	s := strings.TrimSpace(string(text))
	if s == "" {
		return nil, ErrUnsigned
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == ed25519.SignatureSize {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == ed25519.SignatureSize {
		return b, nil
	}
	return nil, fmt.Errorf("update: 签名格式不对（需要 %d 字节的 base64 或 hex）", ed25519.SignatureSize)
}
