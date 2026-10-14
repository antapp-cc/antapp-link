package pki

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Scheme 是连接码前缀。
const Scheme = "antapp://"

// clientNamePattern 与云服脚本的白名单一致，挡住把路径拼进文件名的穿越尝试。
var clientNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// TunnelParams 是服务端要下发给客户端的隧道参数。
type TunnelParams struct {
	TunnelIP string
	Gateway  string
	Prefix   int
	MTU      int
	DNS      []string
	Mode     TunnelMode // 空表示 TCP
	Members  int        // 并行连接数（0/1=单连接，最大 4）
}

// TunnelMode 是隧道的数据通道走法。
type TunnelMode string

const (
	// ModeTCP 是默认：TLS over TCP，数据和控制都走它。
	ModeTCP TunnelMode = "tcp"
	// ModeUDP 让数据包走 UDP（AES-GCM，密钥从 TLS 会话导出），控制仍走 TCP。
	// 解决的是「TCP 能连但被限速/干扰」，TCP 完全不通时连握手都做不了。
	ModeUDP TunnelMode = "udp"
)

// ParseMode 把配置里的字符串转成 TunnelMode，空值当 TCP。
func ParseMode(s string) (TunnelMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(ModeTCP):
		return ModeTCP, nil
	case string(ModeUDP):
		return ModeUDP, nil
	default:
		return "", fmt.Errorf("未知的隧道模式 %q（只支持 tcp / udp）", s)
	}
}

// Invite 是一个 Pi 节点的完整连接信息。私钥在内，所以它等同密码：日志和界面都不回显。
type Invite struct {
	Server   string     `json:"server"`
	TunnelIP string     `json:"tunnel_ip"`
	Gateway  string     `json:"gateway"`
	Prefix   int        `json:"prefix"`
	MTU      int        `json:"mtu"`
	DNS      []string   `json:"dns"`
	Mode     TunnelMode `json:"mode,omitempty"`
	Members  int        `json:"members,omitempty"`
	CAPEM    string     `json:"ca_pem"`
	CertPEM  string     `json:"cert_pem"`
	KeyPEM   string     `json:"key_pem"`
	Name     string     `json:"name"`
	Created  time.Time  `json:"created"`
}

// UseUDP 表示这条连接码要求走 UDP 数据通道。
func (i Invite) UseUDP() bool { return i.Mode == ModeUDP }

// Issue 用现有 CA 给 name 签一张客户端证书，并组装出连接码。
func Issue(dir, name, server string, tp TunnelParams) (Invite, error) {
	if !clientNamePattern.MatchString(name) {
		return Invite{}, fmt.Errorf("客户端名 %q 不合法：只允许字母、数字、点、横线、下划线", name)
	}
	if err := validateServerAddr(server); err != nil {
		return Invite{}, err
	}

	caCert, caKey, err := loadCA(dir)
	if err != nil {
		return Invite{}, err
	}
	certDER, keyDER, err := SignLeaf(caCert, caKey, name, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return Invite{}, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return Invite{}, fmt.Errorf("读取 CA 证书: %w", err)
	}

	inv := Invite{
		Server:   server,
		TunnelIP: tp.TunnelIP,
		Gateway:  tp.Gateway,
		Prefix:   tp.Prefix,
		MTU:      tp.MTU,
		DNS:      tp.DNS,
		Mode:     tp.Mode,
		Members:  tp.Members,
		CAPEM:    string(caPEM),
		CertPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		KeyPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		Name:     name,
		Created:  time.Now(),
	}
	if err := inv.Validate(); err != nil {
		return Invite{}, err
	}
	return inv, nil
}

func validateServerAddr(server string) error {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return fmt.Errorf("服务端地址 %q 必须是 host:port 形式: %w", server, err)
	}
	if host == "" || port == "" {
		return fmt.Errorf("服务端地址 %q 缺少主机或端口", server)
	}
	return nil
}

func (inv Invite) Validate() error {
	if err := validateServerAddr(inv.Server); err != nil {
		return err
	}
	if net.ParseIP(inv.TunnelIP) == nil {
		return fmt.Errorf("tunnel_ip %q 不是合法 IP", inv.TunnelIP)
	}
	if net.ParseIP(inv.Gateway) == nil {
		return fmt.Errorf("gateway %q 不是合法 IP", inv.Gateway)
	}
	if inv.Prefix < 1 || inv.Prefix > 32 {
		return fmt.Errorf("prefix %d 越界（1-32）", inv.Prefix)
	}
	// 576 是 IPv4 主机必须能重组的最小值；1500 是常规以太网上限。
	if inv.MTU < 576 || inv.MTU > 1500 {
		return fmt.Errorf("mtu %d 越界（576-1500）", inv.MTU)
	}
	for _, d := range inv.DNS {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("dns %q 不是合法 IP", d)
		}
	}
	fields := []struct {
		name  string
		value string
	}{
		{"ca_pem", inv.CAPEM},
		{"cert_pem", inv.CertPEM},
		{"key_pem", inv.KeyPEM},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("连接码缺少 %s", f.name)
		}
	}
	if _, err := tls.X509KeyPair([]byte(inv.CertPEM), []byte(inv.KeyPEM)); err != nil {
		return fmt.Errorf("证书与私钥不匹配: %w", err)
	}
	return nil
}

// Encode 产出可粘贴的单行连接码。尾部带 CRC32：连接码常经微信/邮件转发，
// 复制截断比想象中常见 —— 宁可在导入时明确报错，也不要带着坏配置连不上。
func (inv Invite) Encode() (string, error) {
	if err := inv.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(inv)
	if err != nil {
		return "", fmt.Errorf("序列化连接码: %w", err)
	}
	raw := make([]byte, 0, len(body)+4)
	raw = append(raw, body...)
	raw = binary.BigEndian.AppendUint32(raw, crc32.ChecksumIEEE(body))
	return Scheme + base64.StdEncoding.EncodeToString(raw), nil
}

// maxCodeLen 是连接码的长度上限。连接码本体只有几 KB（三段 PEM + 隧道参数），
// 1 MiB 已有三个数量级余量；上游可能把整个文件或一大段剪贴板喂进来，
// 而去空白那一步会先把输入整份复制，所以必须在它之前卡住。
const maxCodeLen = 1 << 20

// Decode 解析连接码。带不带 antapp:// 前缀都接受，并容忍从聊天窗口粘来的空白。
func Decode(code string) (Invite, error) {
	s := strings.TrimSpace(code)
	if len(s) > maxCodeLen {
		return Invite{}, fmt.Errorf("连接码过长（%d 字节，上限 %d）：多半是没复制对，或贴进来的是整个文件", len(s), maxCodeLen)
	}
	s = strings.TrimPrefix(s, Scheme)
	s = strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(s)

	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Invite{}, fmt.Errorf("连接码不是合法 base64（可能没复制全）: %w", err)
	}
	if len(raw) < 5 {
		return Invite{}, errors.New("连接码太短")
	}
	body := raw[:len(raw)-4]
	want := binary.BigEndian.Uint32(raw[len(raw)-4:])
	if got := crc32.ChecksumIEEE(body); got != want {
		return Invite{}, errors.New("连接码校验失败：内容在传输中被改动或截断，请让服务端重新生成")
	}

	var inv Invite
	if err := json.Unmarshal(body, &inv); err != nil {
		return Invite{}, fmt.Errorf("连接码内容无法解析: %w", err)
	}
	if err := inv.Validate(); err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// 会话票据缓存必须跨 ClientTLSConfig 调用共享才有意义。
//
// 原来每次调用都 tls.NewLRUClientSessionCache 新建一个，看着配了 resumption，实际只有
// 同一会话内的成员连接能命中（它们共用同一个 tls.Config）—— 一旦重连就是完整握手，
// 在 250ms 的跨境链路上白搭掉一个 RTT。这里按服务端地址分片：换连接码去连另一台服务器
// 时，不会拿旧服务器的票据去试（那必然失败并回退，等于白费）。
// 缓存键必须同时含「连哪台服务器」和「以什么身份连」。
//
// 地址：Go 客户端缓存的 key 就是 ServerName，而本项目用的是固定常量（见 ServerName），
// 不按地址分片就会跨服务器串用票据。
//
// 身份：TLS 1.3 的 PSK 恢复不会重新验证客户端证书 —— Go 服务端直接沿用票据里存的
// peerCertificates（crypto/tls/handshake_server_tls13.go 在 usingPSK 时不发
// CertificateRequest），而服务端是按证书 CN 认节点的。只按地址分片的话，换了连接码
// （改名、重签证书）之后旧票据仍会被接受，服务端会把新码认成旧节点。
var (
	sessionCacheMu sync.Mutex
	sessionCaches  = map[string]tls.ClientSessionCache{}
)

func sessionCacheKey(inv Invite) string {
	sum := sha256.Sum256([]byte(inv.CertPEM))
	return inv.Server + "\x00" + hex.EncodeToString(sum[:8])
}

// sessionCacheLimit 是票据缓存的分片上限。键里含服务端地址和客户端身份，换码或换机器
// 都会新增条目，所以要有界：正常用不了几条（一台机器一个身份），超过说明在频繁换码，
// 整体清掉重来即可 —— 代价只是这些会话重新做一次完整握手。
const sessionCacheLimit = 32

func sessionCacheFor(key string) tls.ClientSessionCache {
	sessionCacheMu.Lock()
	defer sessionCacheMu.Unlock()
	if c, ok := sessionCaches[key]; ok {
		return c
	}
	if len(sessionCaches) >= sessionCacheLimit {
		sessionCaches = map[string]tls.ClientSessionCache{}
	}
	// 容量给足：一次会话最多 4 条连接，外加重连后遗留的几个旧票据
	c := tls.NewLRUClientSessionCache(16)
	sessionCaches[key] = c
	return c
}

// ClientTLSConfig 用连接码里的材料建 TLS 配置。ServerName 用固定名而不是 IP，
// 这样服务端换 IP 不用重签证书。
func ClientTLSConfig(inv Invite) (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(inv.CertPEM), []byte(inv.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(inv.CAPEM)) {
		return nil, errors.New("连接码里的 CA 无法解析")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   ServerName,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{ALPN},
		// 成员连接、以及断线重连都复用 TLS 会话（老熟人免重验），省一个 RTT。
		// 不开 EarlyData：Go 无客户端 0-RTT 支持且有重放风险。
		ClientSessionCache: sessionCacheFor(sessionCacheKey(inv)),
	}, nil
}
