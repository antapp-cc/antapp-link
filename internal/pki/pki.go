// Package pki 生成并管理自签 CA、服务端证书与客户端证书，取代 easy-rsa。
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ServerName 是服务端证书的固定 SAN。用固定名而不是云服 IP，这样换 IP 只需要改
// 连接码里的地址，不必重签任何证书。
const ServerName = "antapp-link"

// ALPN 是客户端在 TLS 握手里声明的协议名。取最常见的两个值、顺序也和浏览器一致：
// 自研名字等于在明文握手里挂一块牌子，中间设备看一眼就知道这不是普通 HTTPS。
// 真实浏览器都带 http/1.1 作为降级，只声明 h2 反而不太常见。
const (
	ALPN         = "h2"
	ALPNFallback = "http/1.1"
)

// ALPNLegacy 是上线时用过的名字。服务端继续接受它，否则已经发出去的连接码会
// 一次性全部连不上 —— 连接码里没有协商 ALPN 的余地。
const ALPNLegacy = "antapp-link/1"

// 两个列表刻意分开：客户端只声明伪装值，服务端要多留一个旧名字给没升级的客户端。
var (
	clientALPN = []string{ALPN, ALPNFallback}
	serverALPN = []string{ALPN, ALPNFallback, ALPNLegacy}
)

const (
	caCertFile  = "ca.crt"
	caKeyFile   = "ca.key"
	srvCertFile = "server.crt"
	srvKeyFile  = "server.key"

	caDays   = 3650
	leafDays = 1095
)

// Init 幂等地准备好 PKI。已存在的 CA 绝不覆盖 —— 重建 CA 会让此前发出的所有连接码失效。
func Init(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建 PKI 目录: %w", err)
	}

	caCert := filepath.Join(dir, caCertFile)
	caKey := filepath.Join(dir, caKeyFile)
	haveCert, haveKey := fileExists(caCert), fileExists(caKey)
	switch {
	case haveCert && haveKey:
		// 复用现有 CA
	case !haveCert && !haveKey:
		if err := generateCA(caCert, caKey); err != nil {
			return err
		}
	default:
		return fmt.Errorf("CA 不完整：%s 与 %s 必须同时存在。"+
			"这里拒绝自动重建，因为重建会让已发出的连接码全部失效；确认要重建请先手动删掉两者", caCert, caKey)
	}

	// 服务端证书可以安全重签：客户端只校验它由 CA 签发，不认指纹。
	if !fileExists(filepath.Join(dir, srvCertFile)) || !fileExists(filepath.Join(dir, srvKeyFile)) {
		if err := generateServerCert(dir); err != nil {
			return err
		}
	}
	return nil
}

func generateCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 CA 私钥: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "antapp-ca", Organization: []string{"AntApp"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, caDays),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("自签 CA: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("序列化 CA 私钥: %w", err)
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	return writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600)
}

func generateServerCert(dir string) error {
	caCert, caKey, err := loadCA(dir)
	if err != nil {
		return err
	}
	return issueLeaf(dir, caCert, caKey, ServerName, srvCertFile, srvKeyFile,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{ServerName})
}

// issueLeaf 用 CA 签一张叶子证书。客户端证书返回 PEM 而不是落盘：连接码本身就是要发走的载体，
// 服务端不需要留副本，随时可以用 CA 重签。
func issueLeaf(dir string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey,
	commonName, certName, keyName string, usages []x509.ExtKeyUsage, dnsNames []string,
) error {
	certPEM, keyPEM, err := SignLeaf(caCert, caKey, commonName, usages, dnsNames)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, certName), "CERTIFICATE", certPEM, 0o644); err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, keyName), "EC PRIVATE KEY", keyPEM, 0o600)
}

// SignLeaf 签一张叶子证书，返回 DER 编码的证书与 PKCS#8 私钥。
func SignLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey,
	commonName string, usages []x509.ExtKeyUsage, dnsNames []string,
) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成私钥: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"AntApp"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, leafDays),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usages,
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("签发证书 %s: %w", commonName, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化私钥: %w", err)
	}
	return der, keyDER, nil
}

// ServerTLSConfig 要求客户端出示由本 CA 签发的证书；没有有效证书的连接在握手阶段就被拒。
func ServerTLSConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, srvCertFile), filepath.Join(dir, srvKeyFile))
	if err != nil {
		return nil, fmt.Errorf("加载服务端证书: %w", err)
	}
	pool, err := caPool(dir)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   serverALPN,
	}, nil
}

func caPool(dir string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA 证书无法解析")
	}
	return pool, nil
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, nil, fmt.Errorf("读取 CA 证书: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, errors.New("CA 证书不是合法的 PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 CA 证书: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, nil, fmt.Errorf("读取 CA 私钥: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("CA 私钥不是合法的 PEM")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 CA 私钥: %w", err)
	}
	return caCert, caKey, nil
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	// 先写临时文件再改名：中途失败不会留下半个证书，也不会破坏已存在的文件。
	tmp := path + ".tmp"
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(tmp, buf, perm); err != nil {
		return fmt.Errorf("写 %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("落盘 %s: %w", path, err)
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成序列号: %w", err)
	}
	return serial, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
