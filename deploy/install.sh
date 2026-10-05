#!/usr/bin/env bash

set -euo pipefail

CONF_DIR=/etc/antapp-link
CONF="$CONF_DIR/server.json"
BIN=/usr/local/bin/antapp-linkd
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TUNNEL_PORT=""
FORWARD_RANGE=""
NETWORK=""
ASSUME_YES=0

usage() {
  cat <<'EOF'
用法: bash install.sh [--forward 31400-31409] [--tunnel-port 62233] [--network 10.10.0.0/24] [--yes]
不带参数时全部用默认值。
--yes 已装过时不再询问，直接重装。
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
    --yes|-y)
      ASSUME_YES=1
      shift
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

modprobe tcp_bbr 2>/dev/null || true
mkdir -p /etc/modules-load.d
grep -q '^tcp_bbr$' /etc/modules-load.d/bbr.conf 2>/dev/null || echo tcp_bbr > /etc/modules-load.d/bbr.conf

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
  log "检测到这台机器已经装过 AntApp Link。继续安装会："
  log "  · 把 antapp-linkd 更新到最新版"
  log "  · 重写 dnsmasq / sysctl / systemd 的配置文件"
  log "  · 重新签发 /root/pinode.antapp —— 客户端证书换新，旧连接码立即作废，"
  log "    正在用它的节点机会掉线，必须重新导入新连接码"
  log "  · 不会动 CA，也不会动 $CONF（端口/网段等设置保留）"
  log "什么都不做的话，现有安装保持原样。"
  if [[ $ASSUME_YES -eq 1 ]]; then
    log "已指定 --yes，直接继续重装"
  elif [[ -t 0 ]]; then
    ans=""
    read -r -p "[antapp-link] 继续安装？[y/N] " ans || ans=""
    case "$ans" in
      y|Y|yes|YES) log "继续安装" ;;
      *) log "已取消，什么都没改"; exit 0 ;;
    esac
  else
    log "非交互环境（读不到键盘）——默认继续；想跳过这段提示请加 --yes"
  fi
fi
url="https://github.com/antapp-cc/antapp-link/releases/latest/download/antapp-linkd"
downloaded=0
if ! command -v wget >/dev/null 2>&1 && ! command -v curl >/dev/null 2>&1; then
  log "没有 wget 也没有 curl，先装 wget"
  DEBIAN_FRONTEND=noninteractive apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y wget
fi
if command -v wget >/dev/null 2>&1 || command -v curl >/dev/null 2>&1; then
  log "从 GitHub Release 下载 antapp-linkd（约 7 MB）……"
  tmp_dl="$SRC_DIR/antapp-linkd.download"
  rm -f "$tmp_dl"
  if command -v wget >/dev/null 2>&1; then
    wget -qO "$tmp_dl" "$url" || true
  else
    curl -fsSL --max-time 180 -o "$tmp_dl" "$url" || true
  fi
  if [[ -f "$tmp_dl" ]] && [[ "$(head -c 4 "$tmp_dl")" == $'\x7fELF' ]]; then
    mv -f "$tmp_dl" "$SRC_DIR/antapp-linkd"
    downloaded=1
    log "下载完成（$(du -h "$SRC_DIR/antapp-linkd" | cut -f1)）"
  else
    rm -f "$tmp_dl"
    log "下载失败或拿到的不是有效二进制（网络 / CDN 异常）"
  fi
fi
if [[ "$downloaded" -eq 0 ]]; then
  if [[ -f "$SRC_DIR/antapp-linkd" ]]; then
    log "警告: 改用同目录现成的 antapp-linkd —— 它不一定是最新版，想确保最新请让网络可用后重跑"
  else
    die "下载失败，同目录也没有 antapp-linkd。请检查网络，或手动上传 antapp-linkd 到 $SRC_DIR"
  fi
fi
chmod +x "$SRC_DIR/antapp-linkd"

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

log "启用 BBR 加速"
sysctl -w net.core.default_qdisc=fq >/dev/null
if ! sysctl -w net.ipv4.tcp_congestion_control=bbr >/dev/null 2>&1; then
  modprobe tcp_bbr
  sysctl -w net.ipv4.tcp_congestion_control=bbr >/dev/null
fi
[[ "$(sysctl -n net.ipv4.tcp_congestion_control)" == "bbr" ]] || die "BBR 启用失败——内核不支持且模块加载不了"
log "BBR 已生效（$(sysctl -n net.ipv4.tcp_congestion_control) + $(sysctl -n net.core.default_qdisc)）"

log "调优内核缓冲（多连接数据面）"
apply_sysctl() {
  sysctl -w "$1" >/dev/null 2>&1
}
apply_or_die() {
  apply_sysctl "$1"
  want="${1#*=}"
  got="$(sysctl -n "${1%%=*}")"
  [[ "$got" == "$want" ]] || die "内核参数 $1 应用失败（当前 $got，期望 $want）"
}

apply_list_or_die() {
  apply_sysctl "$1"
  key="${1%%=*}"
  want="$(printf '%s' "${1#*=}" | tr -s ' \t' ' ')"
  got="$(sysctl -n "$key" | tr -s ' \t' ' ')"
  [[ "$got" == "$want" ]] || die "内核参数 $key 应用失败（当前 $got，期望 $want）"
}

mkdir -p "$CONF_DIR"
SYSCTL_BACKUP="$CONF_DIR/sysctl-backup.txt"
: > "$SYSCTL_BACKUP"
for k in net.ipv4.tcp_slow_start_after_idle net.ipv4.tcp_rmem net.ipv4.tcp_wmem \
         net.core.rmem_max net.core.wmem_max net.ipv4.tcp_mtu_probing net.ipv4.tcp_fastopen; do
  printf '%s = %s\n' "$k" "$(sysctl -n "$k" 2>/dev/null || echo '(读取失败)')" >> "$SYSCTL_BACKUP"
done
log "原值已备份到 $SYSCTL_BACKUP"

apply_or_die "net.ipv4.tcp_slow_start_after_idle=0"
apply_or_die "net.core.rmem_max=16777216"
apply_or_die "net.core.wmem_max=16777216"
apply_or_die "net.ipv4.tcp_mtu_probing=1"
apply_list_or_die "net.ipv4.tcp_rmem=4096 87380 16777216"
apply_list_or_die "net.ipv4.tcp_wmem=4096 65536 16777216"
apply_sysctl "net.ipv4.tcp_fastopen=3"
log "内核缓冲调优完成（rmem/wmem 上限 16MB，空闲不重新起步；持久化见 /etc/sysctl.d/99-antapp-link.conf）"

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

log "重新签发连接码到 /root（覆盖旧文件，让里面的参数跟上当前配置）"
"$BIN" invite pi-node-01 -c "$CONF" -o /root || log "警告: 签发失败，可手动执行: $BIN invite pi-node-01 -c $CONF -o /root"

cat <<EOF

[antapp-link] --------------------------------------------------
[antapp-link] 安装完成。连接码文件：/root/pinode.antapp（本次已重新签发）
[antapp-link] 把它发给节点机双击导入即可（内含私钥，等同密码，注意保管）。
[antapp-link] 本次客户端证书已换新，之前发出去的旧连接码作废，节点机必须重新导入。
[antapp-link] 只想重启服务、不换连接码时别跑本脚本，直接: systemctl restart antapp-linkd
[antapp-link] --------------------------------------------------
EOF
