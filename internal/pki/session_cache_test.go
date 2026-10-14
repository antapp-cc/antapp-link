package pki

import (
	"crypto/tls"
	"fmt"
	"testing"
)

// 票据缓存的键必须同时区分「哪台服务器」和「什么身份」。
//
// 身份这一维是安全项：TLS 1.3 的 PSK 恢复不会重新验证客户端证书，Go 服务端直接沿用
// 票据里存的 peerCertificates，而服务端是按证书 CN 认节点的 —— 只按地址分片的话，
// 换连接码（改名、重签证书）之后旧票据仍会被接受，新码会被认成旧节点。
func TestSessionCacheKeySeparatesServerAndIdentity(t *testing.T) {
	base := Invite{Server: "a.example:62233", CertPEM: "cert-A"}
	otherServer := Invite{Server: "b.example:62233", CertPEM: "cert-A"}
	rotatedCert := Invite{Server: "a.example:62233", CertPEM: "cert-B"}

	if sessionCacheKey(base) == sessionCacheKey(otherServer) {
		t.Error("不同服务端地址不该共用票据缓存")
	}
	if sessionCacheKey(base) == sessionCacheKey(rotatedCert) {
		t.Error("同一地址下不同客户端身份不该共用票据缓存")
	}
	if sessionCacheKey(base) != sessionCacheKey(Invite{Server: "a.example:62233", CertPEM: "cert-A"}) {
		t.Error("同一地址同一身份应算出同一个键")
	}

	if a, b := sessionCacheFor(sessionCacheKey(base)), sessionCacheFor(sessionCacheKey(base)); a != b {
		t.Error("同一个键应返回同一个缓存实例")
	}
	if sessionCacheFor(sessionCacheKey(base)) == sessionCacheFor(sessionCacheKey(otherServer)) {
		t.Error("不同键不该拿到同一个缓存实例")
	}
}

// 分片数必须有界：键里含地址和身份，换码或换机器都会新增条目。
func TestSessionCachesBounded(t *testing.T) {
	sessionCacheMu.Lock()
	saved := sessionCaches
	sessionCaches = map[string]tls.ClientSessionCache{}
	sessionCacheMu.Unlock()
	t.Cleanup(func() {
		sessionCacheMu.Lock()
		sessionCaches = saved
		sessionCacheMu.Unlock()
	})

	for i := 0; i < sessionCacheLimit+10; i++ {
		sessionCacheFor(fmt.Sprintf("srv-%d\x00ident", i))
	}
	sessionCacheMu.Lock()
	n := len(sessionCaches)
	sessionCacheMu.Unlock()
	if n > sessionCacheLimit {
		t.Errorf("分片数 %d 超过上限 %d", n, sessionCacheLimit)
	}
}
