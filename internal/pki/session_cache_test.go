package pki

import (
	"crypto/tls"
	"fmt"
	"testing"
)

// 注意：本文件的用例会触碰包级 sessionCaches，所以不能加 t.Parallel() ——
// 并行跑起来会互相清空对方的分片。

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

// 只测 key 和 map 是不够的：③ 真正的回归点是 ClientTLSConfig 有没有把这个缓存用上。
// 把 invite.go 里那行改回 tls.NewLRUClientSessionCache(8)，上面那些用例照样会过。
func TestClientTLSConfigSharesSessionCache(t *testing.T) {
	inv := mustIssue(t, mustInit(t), "cache-wired")
	cfg1, err := ClientTLSConfig(inv)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	cfg2, err := ClientTLSConfig(inv)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if cfg1.ClientSessionCache == nil {
		t.Fatal("ClientTLSConfig 没配会话缓存：重连永远命中不了票据，每次都要完整握手")
	}
	if cfg1.ClientSessionCache != cfg2.ClientSessionCache {
		t.Error("同一个连接码两次建配置应共享同一个会话缓存实例")
	}

	other := mustIssue(t, mustInit(t), "cache-wired-other")
	cfg3, err := ClientTLSConfig(other)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if cfg3.ClientSessionCache == cfg1.ClientSessionCache {
		t.Error("不同身份不该共用会话缓存")
	}
}
