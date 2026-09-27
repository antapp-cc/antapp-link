package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
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

// Decode 解析连接码。带不带 antapp:// 前缀都接受，并容忍从聊天窗口粘来的空白。
func Decode(code string) (Invite, error) {
	s := strings.TrimSpace(code)
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
	}, nil
}
