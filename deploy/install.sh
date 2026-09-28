#!/usr/bin/env bash

set -euo pipefail

CONF_DIR=/etc/antapp-link
CONF="$CONF_DIR/server.json"
BIN=/usr/local/bin/antapp-linkd
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TUNNEL_PORT=""
FORWARD_RANGE=""
NETWORK=""

usage() {
  cat <<'EOF'
用法: bash install.sh [--forward 31400-31409] [--tunnel-port 62233] [--network 10.10.0.0/24]
不带参数时全部用默认值。
EOF
}

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
  log "目录下没有 antapp-linkd，从 GitHub Release 自动下载（约 7 MB）……"
  url="https://github.com/antapp-cc/antapp-link/releases/latest/download/antapp-linkd"
  if ! command -v wget >/dev/null 2>&1 && ! command -v curl >/dev/null 2>&1; then
    log "没有 wget 也没有 curl，先装 wget"
    DEBIAN_FRONTEND=noninteractive apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y wget
  fi
  if command -v wget >/dev/null 2>&1; then
    wget -O "$SRC_DIR/antapp-linkd" "$url" || die "下载失败：检查网络后重试，或手动上传 antapp-linkd 到同目录"
  else
    curl -fL -S --progress-bar -o "$SRC_DIR/antapp-linkd" "$url" || die "下载失败：检查网络后重试，或手动上传 antapp-linkd 到同目录"
  fi
  [[ "$(head -c 4 "$SRC_DIR/antapp-linkd")" == $'\x7fELF' ]] || die "下载到的不是有效二进制（网络或 CDN 异常），请手动上传 antapp-linkd 到同目录"
  log "下载完成（$(du -h "$SRC_DIR/antapp-linkd" | cut -f1)）"
  chmod +x "$SRC_DIR/antapp-linkd"
fi

log "检查系统依赖"
missing=()
command -v iptables >/dev/null || missing+=(iptables)
command -v modprobe >/dev/null || missing+=(kmod)
command -v dnsmasq >/dev/null || missing+=(dnsmasq)
if [[ ${#missing[@]} -gt 0 ]]; then
  log "缺少 ${missing[*]}，开始安装——软件源慢时需要一两分钟，请看下面的 apt 进度"
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}"
  elif command -v dnf >/dev/null; then
    dnf install -y "${missing[@]}"
  elif command -v yum >/dev/null; then
    yum install -y "${missing[@]}"
  else
    die "缺少 ${missing[*]}，且没找到可用的包管理器，请手动安装"
  fi
  log "依赖安装完成"
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

"$BIN" down -c "$CONF" >/dev/null 2>&1 || true
"$BIN" init "${init_args[@]}"

log "安装 systemd 服务"
"$BIN" install -c "$CONF"

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
