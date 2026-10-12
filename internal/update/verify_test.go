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

// 用临时密钥对替换内置公钥，避免测试依赖真实发布密钥。
func withTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	orig := releasePubKey
	releasePubKey = [32]byte(pub)
	t.Cleanup(func() { releasePubKey = orig })
	return priv
}

func TestVerifyManifestAcceptsGoodSignature(t *testing.T) {
	priv := withTestKey(t)
	raw := []byte(`{"version":"1.2.3","client":{"url":"https://x/a.exe","sha256":"` +
		strings.Repeat("a", 64) + `"}}`)

	for _, enc := range []string{
		base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)),
		hex.EncodeToString(ed25519.Sign(priv, raw)),
		base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)) + "\n", // shell 重定向常见的尾换行
	} {
		if err := verifyManifest(raw, []byte(enc)); err != nil {
			t.Errorf("合法签名被拒: %v", err)
		}
	}
}

// 篡改清单必须被拒 —— 这正是这条防线存在的理由：能改 Release 的人可以把
// sha256 与下载 URL 一起换掉，只校 sha256 挡不住。
func TestVerifyManifestRejectsTamperedManifest(t *testing.T) {
	priv := withTestKey(t)
	raw := []byte(`{"version":"1.2.3"}`)
	sig := ed25519.Sign(priv, raw)

	tampered := []byte(`{"version":"9.9.9"}`)
	if err := verifyManifest(tampered, []byte(base64.StdEncoding.EncodeToString(sig))); err == nil {
		t.Fatal("篡改后的清单竟然通过了验签")
	}
}

func TestVerifyManifestRejectsForeignKey(t *testing.T) {
	withTestKey(t)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":"1.2.3"}`)
	sig := ed25519.Sign(otherPriv, raw)
	if err := verifyManifest(raw, []byte(base64.StdEncoding.EncodeToString(sig))); err == nil {
		t.Fatal("别的私钥签出来的清单竟然通过了验签")
	}
}

// 无签名绝不能放行：允许它等于整条验签链路不存在。
func TestVerifyManifestRejectsEmptySignature(t *testing.T) {
	withTestKey(t)
	for _, s := range []string{"", "   ", "\n"} {
		err := verifyManifest([]byte(`{"version":"1.2.3"}`), []byte(s))
		if !errors.Is(err, ErrUnsigned) {
			t.Errorf("空签名（%q）应返回 ErrUnsigned，实际 %v", s, err)
		}
	}
}

func TestVerifyManifestRejectsMalformedSignature(t *testing.T) {
	withTestKey(t)
	raw := []byte(`{"version":"1.2.3"}`)
	for _, s := range []string{"!!!not-base64!!!", "abcd", strings.Repeat("z", 200)} {
		if err := verifyManifest(raw, []byte(s)); err == nil {
			t.Errorf("畸形签名（%q）应被拒", s)
		}
	}
}

// 内置公钥被清空（比如忘了填）时必须拒绝更新，而不是退化成不校验。
func TestVerifyManifestRejectsUnsetPublicKey(t *testing.T) {
	orig := releasePubKey
	releasePubKey = [32]byte{}
	t.Cleanup(func() { releasePubKey = orig })

	if err := verifyManifest([]byte(`{"version":"1.2.3"}`), []byte("anything")); err == nil {
		t.Fatal("公钥未内置时必须拒绝更新")
	}
}
