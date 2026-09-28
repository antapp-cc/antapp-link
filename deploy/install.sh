#!/usr/bin/env bash
#
# AntApp Link 云服安装 / 升级脚本。
#
# 用法（在云服上以 root 运行，保持 antapp-linkd 与本脚本同目录）：
#
#   bash install.sh                                   # 全部用默认值
#   bash install.sh --forward 31400-31409             # 指定转发端口段
#   bash install.sh --tunnel-port 62233               # 指定隧道监听端口
#   bash install.sh --network 10.10.0.0/24            # 指定隧道网段
#
# 默认值与"什么都不传"等价，脚本不依赖任何现网状态：网卡、公网 IP 都是运行时探测的。
# 幂等：重复执行只刷新二进制、配置与 systemd unit。
# **不会重建 CA** —— 重建会让此前发出的所有连接码一起失效。

set -euo pipefail

CONF_DIR=/etc/antapp-link
CONF="$CONF_DIR/server.json"
BIN=/usr/local/bin/antapp-linkd
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 留空表示"不改这一项"，由 antapp-linkd 用它自己的默认值
TUNNEL_PORT=""
FORWARD_RANGE=""
NETWORK=""

usage() { sed -n '2,15p' "${BASH_SOURCE[0]}"; }

log() { echo "[antapp-link] $*"; }
die() { echo "[antapp-link] $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --forward)
      range="${2:-}"
      [[ "$range" =~ ^[0-9]+-[0-9]+$ ]] || die "--forward 需要形如 31400-31409 的参数"
      FORWARD_RANGE="$range"
      shift 2
      ;;
    --tunnel-port)
      port="${2:-}"
      [[ "$port" =~ ^[0-9]+$ ]] || die "--tunnel-port 需要端口号"
      TUNNEL_PORT="$port"
      shift 2
      ;;
    --network)
      net="${2:-}"
      [[ "$net" =~ ^[0-9.]+/[0-9]+$ ]] || die "--network 需要形如 10.10.0.0/24 的参数"
      NETWORK="$net"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "未知参数: $1"
      ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "请用 root 运行"
if [[ ! -f "$SRC_DIR/antapp-linkd" ]]; then
	log "目录下没有 antapp-linkd，从 GitHub Release 自动下载……"
	# 极简系统可能没有 curl，wget 优先（Debian 基础镜像自带）
	wget -q "https://github.com/antapp-cc/antapp-link/releases/latest/download/antapp-linkd" -O "$SRC_DIR/antapp-linkd" 		|| curl -fsSL "https://github.com/antapp-cc/antapp-link/releases/latest/download/antapp-linkd" -o "$SRC_DIR/antapp-linkd" 		|| die "下载失败：检查网络后重试，或手动上传 antapp-linkd 到同目录"
	chmod +x "$SRC_DIR/antapp-linkd"
fi

# 全新 VPS 上真实踩到的两件事：
# 1) Debian 12 默认不带 iptables（它转向 nftables 了），而 DNAT 规则要用它；
# 2) tun 模块在精简系统上不会自动加载，/dev/net/tun 也就不会出现。
# 3) dnsmasq 提供隧道 DNS 中继（filter-AAAA：v4-only 隧道不能把 AAAA 发给客户端）。
log "检查系统依赖"
missing=()
command -v iptables >/dev/null || missing+=(iptables)
command -v modprobe >/dev/null || missing+=(kmod)
command -v dnsmasq >/dev/null || missing+=(dnsmasq)
if [[ ${#missing[@]} -gt 0 ]]; then
  log "缺少 ${missing[*]}，尝试安装"
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}"
  elif command -v dnf >/dev/null; then
    dnf install -y -q "${missing[@]}"
  elif command -v yum >/dev/null; then
    yum install -y -q "${missing[@]}"
  else
    die "缺少 ${missing[*]}，且没找到可用的包管理器，请手动安装"
  fi
fi
command -v iptables >/dev/null || die "iptables 仍不可用"

modprobe tun 2>/dev/null || true
[[ -c /dev/net/tun ]] || die "/dev/net/tun 不可用 —— 这台机器不支持 TUN 设备（老式 OpenVZ 容器常见），换一台"

log "安装二进制到 $BIN"
install -m 0755 "$SRC_DIR/antapp-linkd" "$BIN.new"
mv -f "$BIN.new" "$BIN"

log "初始化配置与 PKI（已存在的一律保留，只同步下面这几项）"
init_args=(-c "$CONF")
[[ -n "$FORWARD_RANGE" ]] && init_args+=(--forward "$FORWARD_RANGE")
[[ -n "$TUNNEL_PORT" ]]   && init_args+=(--listen "0.0.0.0:$TUNNEL_PORT")
[[ -n "$NETWORK" ]]       && init_args+=(--network "$NETWORK")

# 换端口段/网段之前先 down：否则 down 会按新值去删规则，旧规则留在链里
"$BIN" down -c "$CONF" >/dev/null 2>&1 || true
"$BIN" init "${init_args[@]}"

log "安装 systemd 服务"
"$BIN" install -c "$CONF"

# 隧道 DNS 中继（配置由 `antapp-linkd install` 写入 /etc/dnsmasq.d/antapp.conf）。
# 必须 restart 而非 start：apt 装 dnsmasq 时 Debian 会立刻自启它，那时 antapp.conf
# 还没写入，之后的 enable --now 是空操作——全新安装会一直跑着没有 filter-AAAA 的裸配置。
systemctl enable dnsmasq >/dev/null 2>&1 || true
systemctl restart dnsmasq >/dev/null 2>&1 || log "警告: dnsmasq 未启动，隧道 DNS 中继不可用"

log "配置 netfilter（幂等）"
"$BIN" up -c "$CONF"

log "启动隧道"
systemctl restart antapp-linkd
sleep 2

log "当前状态"
"$BIN" status -c "$CONF" || true

cat <<EOF

[antapp-link] --------------------------------------------------
[antapp-link] 安装完成。给节点机签发连接码：
[antapp-link]   $BIN invite pi-node-01 -o /root
[antapp-link] 输出的单行 antapp:// 连接码发给节点机导入即可。
[antapp-link] --------------------------------------------------
EOF
