package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// testKey 生成一对临时密钥供本文件使用，不改动包级 releasePubKey。
func testKey(t *testing.T) (ed25519.PrivateKey, [32]byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var arr [32]byte
	copy(arr[:], pub)
	return priv, arr
}

func TestVerifyManifestAcceptsGoodSignature(t *testing.T) {
	priv, pub := testKey(t)
	raw := []byte(`{"version":"1.2.3","client":{"url":"https://x/a.exe","sha256":"` +
		strings.Repeat("a", 64) + `"}}`)

	for _, enc := range []string{
		base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)),
		hex.EncodeToString(ed25519.Sign(priv, raw)),
		base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)) + "\n", // shell 重定向常见的尾换行
	} {
		if err := verifyManifest(raw, []byte(enc), pub); err != nil {
			t.Errorf("合法签名被拒: %v", err)
		}
	}
}

// 篡改清单必须被拒 —— 这正是这条防线存在的理由：能改 Release 的人可以把
// sha256 与下载 URL 一起换掉，只校 sha256 挡不住。
func TestVerifyManifestRejectsTamperedManifest(t *testing.T) {
	priv, pub := testKey(t)
	raw := []byte(`{"version":"1.2.3"}`)
	sig := ed25519.Sign(priv, raw)

	tampered := []byte(`{"version":"9.9.9"}`)
	if err := verifyManifest(tampered, []byte(base64.StdEncoding.EncodeToString(sig)), pub); err == nil {
		t.Fatal("篡改后的清单竟然通过了验签")
	}
}

func TestVerifyManifestRejectsForeignKey(t *testing.T) {
	_, pub := testKey(t)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":"1.2.3"}`)
	sig := ed25519.Sign(otherPriv, raw)
	if err := verifyManifest(raw, []byte(base64.StdEncoding.EncodeToString(sig)), pub); err == nil {
		t.Fatal("别的私钥签出来的清单竟然通过了验签")
	}
}

// 无签名绝不能放行：允许它等于整条验签链路不存在。
func TestVerifyManifestRejectsEmptySignature(t *testing.T) {
	_, pub := testKey(t)
	for _, s := range []string{"", "   ", "\n"} {
		err := verifyManifest([]byte(`{"version":"1.2.3"}`), []byte(s), pub)
		if !errors.Is(err, ErrUnsigned) {
			t.Errorf("空签名（%q）应返回 ErrUnsigned，实际 %v", s, err)
		}
	}
}

func TestVerifyManifestRejectsMalformedSignature(t *testing.T) {
	_, pub := testKey(t)
	raw := []byte(`{"version":"1.2.3"}`)
	for _, s := range []string{"!!!not-base64!!!", "abcd", strings.Repeat("z", 200)} {
		if err := verifyManifest(raw, []byte(s), pub); err == nil {
			t.Errorf("畸形签名（%q）应被拒", s)
		}
	}
}

// 公钥为全零（比如 pubkey.go 忘了填、或 Checker.PubKey 传了个零值）时必须拒绝更新，
// 而不是退化成不校验。签名要给长度合法的，否则会被 decodeSignature 的格式检查先拦下，
// 用例看着通过、其实没走到公钥判断。
func TestVerifyManifestRejectsUnsetPublicKey(t *testing.T) {
	sig := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	err := verifyManifest([]byte(`{"version":"1.2.3"}`), []byte(sig), [32]byte{})
	if err == nil {
		t.Fatal("公钥为零值时必须拒绝更新")
	}
	if !strings.Contains(err.Error(), "公钥") {
		t.Errorf("错误信息应指出是公钥问题（说明真的走到了那道判断），实际: %v", err)
	}
}

// Checker.PubKey 是留给测试与本地联调工具的钩子：给了就用它，没给才用内置的。
func TestCheckerPubKeyOverride(t *testing.T) {
	_, pub := testKey(t)
	if got := (&Checker{}).publicKey(); got != releasePubKey {
		t.Error("PubKey 为 nil 时应该用内置公钥")
	}
	if got := (&Checker{PubKey: &pub}).publicKey(); got != pub {
		t.Error("PubKey 非 nil 时应该用它")
	}
}
