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

reset_apt_sources() {
  local src=/etc/apt/sources.list
  local codename
  codename=$(grep -m1 -oP '(?<=^deb ).*? (?=main)' /etc/apt/sources.list 2>/dev/null | awk '{print $2}')
  [[ "$codename" =~ ^[a-z]+$ ]] || codename=${VERSION_CODENAME:-}
  [[ "$codename" =~ ^[a-z]+$ ]] || die "认不出系统代号（codename），换源中止——请检查 /etc/apt/sources.list 或手动换源后重试"

  if [[ "${ID:-}" == "ubuntu" ]]; then
    if grep -q '^deb .*archive.ubuntu.com' "$src" 2>/dev/null; then
      log "软件源已是 Ubuntu 官方源，跳过换源"
      return 0
    fi
    [[ -f $src && ! -f $src.antapp-bak ]] && cp "$src" "$src.antapp-bak"
    cat > "$src" <<SRCEOF
deb https://archive.ubuntu.com/ubuntu/ $codename main restricted universe multiverse
deb https://archive.ubuntu.com/ubuntu/ $codename-updates main restricted universe multiverse
deb https://archive.ubuntu.com/ubuntu/ $codename-backports main restricted universe multiverse
deb https://security.ubuntu.com/ubuntu/ $codename-security main restricted universe multiverse
SRCEOF
  else
    if grep -q '^deb .*deb.debian.org' "$src" 2>/dev/null; then
      log "软件源已是 Debian 官方源，跳过换源"
      return 0
    fi
    [[ -f $src && ! -f $src.antapp-bak ]] && cp "$src" "$src.antapp-bak"
    cat > "$src" <<SRCEOF
deb https://deb.debian.org/debian/ $codename main contrib non-free non-free-firmware
deb https://deb.debian.org/debian/ $codename-updates main contrib non-free non-free-firmware
deb https://deb.debian.org/debian-security/ $codename-security main contrib non-free non-free-firmware
deb https://deb.debian.org/debian/ $codename-backports main contrib non-free non-free-firmware
SRCEOF
  fi
  local f
  for f in /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [[ -f "$f" ]] || continue
    mv "$f" "$f.antapp-bak"
  done
  log "软件源已切换为官方源（$codename）"
}

ver_ge() {
  local lo
  lo=$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)
  [[ "$lo" == "$2" ]]
}
[[ -r /etc/os-release ]] || die "不支持该系统：读不到 /etc/os-release，仅支持 Debian 11+ / Ubuntu 22.04+"
. /etc/os-release
case "${ID:-}" in
  debian)
    ver_ge "${VERSION_ID:-0}" "11" || die "Debian 版本过旧（检测到 ${VERSION_ID:-未知}），需要 Debian 11 及以上"
    ;;
  ubuntu)
    ver_ge "${VERSION_ID:-0}" "22.04" || die "Ubuntu 版本过旧（检测到 ${VERSION_ID:-未知}），需要 Ubuntu 22.04 及以上"
    ;;
  *)
    die "不支持该系统（检测到 ${PRETTY_NAME:-${ID:-未知}}）：仅支持 Debian 11+ / Ubuntu 22.04+"
    ;;
esac
log "系统检查通过：${PRETTY_NAME:-$ID $VERSION_ID}"

if command -v apt-get >/dev/null; then
  reset_apt_sources
fi

if [[ -f $CONF ]]; then
  log "检测到已安装 AntApp Link——本次按升级处理：只更新程序与配置，CA、已签发证书和 /root/pinode.antapp 全部原样保留，老连接码继续有效"
fi
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

modprobe tcp_bbr 2>/dev/null || true
mkdir -p /etc/modules-load.d
grep -q '^tcp_bbr$' /etc/modules-load.d/bbr.conf 2>/dev/null || echo tcp_bbr > /etc/modules-load.d/bbr.conf

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

if [[ -f /etc/sysctl.d/99-antapp-link.conf ]]; then
  sysctl -p /etc/sysctl.d/99-antapp-link.conf >/dev/null 2>&1 || true
fi
if [[ "$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null)" != "bbr" ]]; then
  log "警告: BBR 未生效（当前 $(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null)）——内核可能不支持，隧道功能不受影响，只是弱网下吞吐略低"
fi

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

if [[ -f /root/pinode.antapp ]]; then
  log "已签发过连接码：/root/pinode.antapp 继续有效，本次不重签（确认要重签才执行: $BIN invite pi-node-01 -o /root）"
else
  log "首次安装，自动签发连接码文件到 /root（只签这一个）"
  "$BIN" invite pi-node-01 -o /root || log "警告: 签发失败，可手动执行: $BIN invite pi-node-01 -o /root"
fi

cat <<EOF

[antapp-link] --------------------------------------------------
[antapp-link] 安装完成。连接码文件：/root/pinode.antapp
[antapp-link] 把它发给节点机双击导入即可（内含私钥，等同密码，注意保管）。
[antapp-link] 升级重装不会动这个文件；确认要重签才执行: $BIN invite pi-node-01 -o /root
[antapp-link] --------------------------------------------------
EOF
