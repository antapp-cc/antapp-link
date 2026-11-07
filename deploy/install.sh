#!/usr/bin/env bash

set -euo pipefail

CONF_DIR=/etc/antapp-link
CONF="$CONF_DIR/server.json"
BIN=/usr/local/bin/antapp-linkd
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TUNNEL_PORT=""
FORWARD_RANGE=""
NETWORK=""
MODE=""
RELEASE_TAG=""
ASSUME_YES=0

usage() {
  cat <<'EOF'
用法: bash install.sh [--mode tcp|udp] [--version v0.4.8] [--forward 31400-31409] [--tunnel-port 62233] [--network 10.10.0.0/24] [--yes]
不带参数时全部用默认值。
--mode     数据通道模式：tcp（默认，N 条 TCP 并联）或 udp（单条控制 + 加密 UDP 数据面）。
           不传则保留配置里已有的值（新装即 tcp）。
--version  指定 antapp-linkd 的 release 标签（如 v0.4.8）。不传则取 latest。
--yes      已装过时不再询问，直接重装。
EOF
}

log() { echo "[antapp-link] $*"; }
die() { echo "[antapp-link] $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)
      m="${2:-}"
      [[ "$m" == "tcp" || "$m" == "udp" ]] || die "--mode 只接受 tcp 或 udp"
      MODE="$m"
      shift 2
      ;;
    --version)
      v="${2:-}"
      [[ "$v" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || die "--version 需要形如 v0.4.8 的完整标签"
      RELEASE_TAG="v${v#v}"
      shift 2
      ;;
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

if [[ -f $CONF ]]; then
  log "检测到这台机器已经装过 AntApp Link。继续安装会："
  log "  · 把 antapp-linkd 更新到最新版"
  log "  · 重写 dnsmasq / sysctl / systemd 的配置文件"
  log "  · 重新签发 /root/pinode.antapp —— 换一张新的客户端证书"
  log "    注意：不重建 CA，所以之前发出去的旧连接码依然有效；"
  log "    真要作废泄漏出去的旧码只能重建 CA，那会让所有节点掉线并重新导入"
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

modprobe tcp_bbr 2>/dev/null || true
mkdir -p /etc/modules-load.d
grep -q '^tcp_bbr$' /etc/modules-load.d/bbr.conf 2>/dev/null || echo tcp_bbr > /etc/modules-load.d/bbr.conf

reset_apt_sources() {
  local src=/etc/apt/sources.list
  local codename
  codename=$(grep -m1 -oP '(?<=^deb ).*? (?=main)' /etc/apt/sources.list 2>/dev/null | awk '{print $2}' || true)
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
SRCEOF
  fi
  local f
  for f in /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [[ -f "$f" ]] || continue
    mv "$f" "$f.antapp-bak"
  done
  log "软件源已切换为官方源（$codename）"
}

strip_backports() {
  local f
  for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [[ -f "$f" ]] || continue
    grep -q 'backports' "$f" 2>/dev/null || continue
    sed -i '/backports/d' "$f" || true
    log "已移除 $f 里的 backports 源（该仓库对老版本可能已废弃，会让 apt 更新失败）"
  done
}

ver_ge() {
  local lo
  lo=$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)
  [[ "$lo" == "$2" ]]
}
[[ -r /etc/os-release ]] || die "不支持该系统：读不到 /etc/os-release，仅支持 Debian 12+ / Ubuntu 22.04+"
. /etc/os-release
case "${ID:-}" in
  debian)
    ver_ge "${VERSION_ID:-0}" "12" || die "Debian 版本过旧（检测到 ${VERSION_ID:-未知}），需要 Debian 12 及以上。Debian 11 已于 2026-08-31 结束支持，它的 backports 和 security 仓库都已下架，装了也没法正常更新——建议先升级系统。"
    ;;
  ubuntu)
    ver_ge "${VERSION_ID:-0}" "22.04" || die "Ubuntu 版本过旧（检测到 ${VERSION_ID:-未知}），需要 Ubuntu 22.04 及以上"
    ;;
  *)
    die "不支持该系统（检测到 ${PRETTY_NAME:-${ID:-未知}}）：仅支持 Debian 12+ / Ubuntu 22.04+"
    ;;
esac
log "系统检查通过：${PRETTY_NAME:-$ID $VERSION_ID}"

if command -v apt-get >/dev/null; then
  reset_apt_sources
  strip_backports
fi

release_base="https://github.com/antapp-cc/antapp-link/releases/latest/download"
if [[ -n "$RELEASE_TAG" ]]; then
  release_base="https://github.com/antapp-cc/antapp-link/releases/download/$RELEASE_TAG"
fi
[[ "$(uname -m)" == "x86_64" ]] || die "当前发布包只支持 Linux amd64"
missing_verify=()
command -v openssl >/dev/null || missing_verify+=(openssl)
command -v python3 >/dev/null || missing_verify+=(python3)
if ! command -v wget >/dev/null && ! command -v curl >/dev/null; then missing_verify+=(wget); fi
if [[ ${#missing_verify[@]} -gt 0 ]]; then
  DEBIAN_FRONTEND=noninteractive apt-get update || die "无法安装可信验签工具"
  DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing_verify[@]}" || die "无法安装验签工具（openssl/python3/wget）"
fi

fetch_release_file() {
  local target="$1" source="$2"
  rm -f -- "$target"
  if command -v wget >/dev/null; then
    if timeout 180 wget --timeout=30 --tries=2 -qO "$target" "$source"; then return 0; fi
  else
    if curl -fsSL --connect-timeout 30 --max-time 180 -o "$target" "$source"; then return 0; fi
  fi
  rm -f -- "$target"
  return 1
}

verify_release() {
  python3 - "$1" "$2" "$RELEASE_TAG" <<'PY'
import base64, hashlib, json, pathlib, re, struct, subprocess, sys
folder, phase, requested = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
try:
    raw = (folder / "latest.json").read_bytes()
    text = (folder / "latest.json.sig").read_text().strip()
    sig = bytes.fromhex(text) if re.fullmatch(r"[0-9a-fA-F]{128}", text) else base64.b64decode(text, validate=True)
    if len(sig) != 64:
        raise ValueError("签名长度错误")
    (folder / "release.pub.der").write_bytes(base64.b64decode("MCowBQYDK2VwAyEALbDBj1bBCEUnEKLbNXgM4vxEDr3t8pEc2ot11tUz2lc="))
    (folder / "release.sig").write_bytes(sig)
    subprocess.run(["openssl", "pkeyutl", "-verify", "-pubin", "-keyform", "DER", "-inkey", str(folder / "release.pub.der"), "-rawin", "-in", str(folder / "latest.json"), "-sigfile", str(folder / "release.sig")], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    manifest = json.loads(raw)
    version = manifest["version"]
    if not isinstance(version, str) or not re.fullmatch(r"[0-9]+[.][0-9]+[.][0-9]+(?:[-+][0-9A-Za-z.-]+)?", version):
        raise ValueError("版本号不合法")
    if requested and requested != "v" + version:
        raise ValueError("清单版本与指定版本不符")
    asset = manifest["server"]
    url = "https://github.com/antapp-cc/antapp-link/releases/download/v" + version + "/antapp-linkd"
    if asset["url"] != url or not re.fullmatch(r"[0-9a-fA-F]{64}", asset["sha256"]):
        raise ValueError("清单下载地址或摘要无效")
    if type(asset["size"]) is not int or not 64 <= asset["size"] <= 128 * 1024 * 1024:
        raise ValueError("清单大小无效")
    if phase == "metadata":
        print(url)
    else:
        binary = (folder / "antapp-linkd").read_bytes()
        if len(binary) != asset["size"] or hashlib.sha256(binary).hexdigest() != asset["sha256"].lower():
            raise ValueError("程序大小或 SHA-256 不符")
        if binary[:7] != bytes.fromhex("7f454c46020101") or struct.unpack_from("<HHI", binary, 16) not in ((2, 62, 1), (3, 62, 1)):
            raise ValueError("程序不是 Linux amd64 ELF")
        print(version)
except Exception as error:
    print("发布包验证失败: " + str(error), file=sys.stderr)
    sys.exit(1)
PY
}

stage="$(mktemp -d /tmp/antapp-install.XXXXXX)"
trap 'rm -f -- "$BIN.probe.$$"; rm -rf -- "$stage"' EXIT
online=0
if fetch_release_file "$stage/latest.json" "$release_base/latest.json" &&
   fetch_release_file "$stage/latest.json.sig" "$release_base/latest.json.sig"; then
  if asset_url="$(verify_release "$stage" metadata)"; then
    if fetch_release_file "$stage/antapp-linkd" "$asset_url"; then
      online=1
    else
      log "在线二进制下载失败，检查同目录离线签名包"
      rm -f -- "$stage/antapp-linkd"
    fi
  else
    log "在线清单验证失败，检查同目录离线签名包"
    rm -f -- "$stage/latest.json" "$stage/latest.json.sig"
  fi
fi
if [[ "$online" -eq 0 ]]; then
  for name in latest.json latest.json.sig antapp-linkd; do
    [[ -f "$SRC_DIR/$name" ]] || die "缺少离线签名包 $name；请把同版本 antapp-linkd、latest.json、latest.json.sig 放到脚本目录，或恢复联网；保留旧程序"
    cp -- "$SRC_DIR/$name" "$stage/$name"
  done
fi
verified_version="$(verify_release "$stage" binary)" || die "程序校验失败，保留旧程序"
chmod 0755 "$stage/antapp-linkd"
probe="$BIN.probe.$$"
if ! install -m 0755 "$stage/antapp-linkd" "$probe"; then
  die "无法写入 $probe，检查 $BIN 所在目录权限"
fi
if [[ "$(timeout 15 "$probe" version)" != "antapp-linkd $verified_version" ]]; then
  rm -f -- "$probe"
  die "程序版本或可执行性检查失败，保留旧程序"
fi
rm -f -- "$probe"
log "签名、大小、SHA-256、平台和版本校验通过：$verified_version"

log "检查系统依赖"
missing=()
command -v iptables >/dev/null || missing+=(iptables)
command -v modprobe >/dev/null || missing+=(kmod)
command -v dnsmasq >/dev/null || missing+=(dnsmasq)
if [[ ${#missing[@]} -gt 0 ]]; then
  log "缺少 ${missing[*]}，开始安装——软件源慢时需要一两分钟，请看下面的 apt 进度"
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get update || die "apt 更新失败：请检查 /etc/apt/sources.list 里的软件源是否可用"
    DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" || die "系统依赖安装失败：${missing[*]}"
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

command -v flock >/dev/null 2>&1 || die "缺少系统工具 flock，不能安全地与自动更新互斥"
mkdir -p /run/antapp-link
exec {install_lock}>/run/antapp-link/update.lock
flock -n "$install_lock" || die "正在安装或自动更新，请稍后重试"
log "安装二进制到 $BIN"
had_old=0
was_active=0
service_restart_attempted=0
install_state_modified=0
netfilter_attempted=0
mkdir -p "$stage/rollback"
state_files=("$CONF" "$CONF_DIR/sysctl-backup.txt"
  /etc/sysctl.d/99-antapp-link.conf /etc/dnsmasq.d/antapp.conf
  /etc/systemd/system/antapp-linkd.service /etc/systemd/system/antapp-link-up.service
  /etc/systemd/system/antapp-link-update.service /etc/systemd/system/antapp-link-update.timer
  /run/antapp-link/forward-range /run/antapp-link/forward-range.rules.json)
for i in "${!state_files[@]}"; do
  file="${state_files[$i]}"
  if [[ -e "$file" || -L "$file" ]]; then cp -a -- "$file" "$stage/rollback/file-$i"; fi
done
sysctl_keys=(net.core.default_qdisc net.ipv4.tcp_congestion_control net.ipv4.ip_forward
  net.ipv4.tcp_slow_start_after_idle net.ipv4.tcp_rmem net.ipv4.tcp_wmem
  net.core.rmem_max net.core.wmem_max net.ipv4.tcp_mtu_probing net.ipv4.tcp_fastopen)
for key in "${sysctl_keys[@]}"; do
  setting="$(sysctl -n "$key" 2>/dev/null || echo '(读取失败)')"
  printf '%s=%s\n' "$key" "$setting" >> "$stage/rollback/sysctl"
done
services=(antapp-linkd antapp-link-up.service antapp-link-update.timer dnsmasq)
for i in "${!services[@]}"; do
  if systemctl is-active --quiet "${services[$i]}"; then touch "$stage/rollback/active-$i"; fi
  systemctl is-enabled "${services[$i]}" > "$stage/rollback/enabled-$i" 2>/dev/null || true
done
[[ ! -f "$stage/rollback/active-0" ]] || was_active=1
if [[ -f "$BIN" ]]; then cp -p -- "$BIN" "$stage/rollback/binary"; had_old=1; fi
rollback_install() {
  local result=$? restored=0 rollback_ok=1 core_ok=1 file i setting enabled
  trap - EXIT
  if [[ "$result" -ne 0 ]]; then
    if [[ "$service_restart_attempted" -eq 1 ]]; then
      systemctl stop antapp-linkd || true
    fi
    if [[ "$install_state_modified" -eq 1 ]]; then
      systemctl stop antapp-link-update.timer || true
      for i in "${!services[@]}"; do
        enabled="$(cat "$stage/rollback/enabled-$i" 2>/dev/null || true)"
        case "$enabled" in
          enabled|masked|masked-runtime) ;;
          *) systemctl disable "${services[$i]}" >/dev/null 2>&1 || true ;;
        esac
      done
    fi
    if [[ "$netfilter_attempted" -eq 1 ]]; then
      "$BIN" down -c "$CONF" || rollback_ok=0
    fi
    for i in "${!state_files[@]}"; do
      file="${state_files[$i]}"
      if [[ -e "$stage/rollback/file-$i" || -L "$stage/rollback/file-$i" ]]; then
        if ! cp -a -- "$stage/rollback/file-$i" "$file.rollback" || ! mv -f -- "$file.rollback" "$file"; then
          log "错误: 无法恢复 $file"; rollback_ok=0
          [[ "$i" -eq 0 ]] && core_ok=0
        fi
      else
        rm -f -- "$file" || rollback_ok=0
      fi
    done
    if [[ "$had_old" -eq 1 ]]; then
      if cp -p -- "$stage/rollback/binary" "$BIN.rollback" && mv -f -- "$BIN.rollback" "$BIN"; then
        restored=1
      else
        log "错误: 程序文件回退失败"; rollback_ok=0; core_ok=0
      fi
    else
      rm -f -- "$BIN" || { rollback_ok=0; core_ok=0; }
    fi
    while IFS= read -r setting; do
      [[ "$setting" == *"(读取失败)"* ]] && continue
      sysctl -w "$setting" >/dev/null 2>&1 || rollback_ok=0
    done < "$stage/rollback/sysctl"
    if [[ "$install_state_modified" -eq 1 ]]; then
      systemctl daemon-reload || true
      for i in "${!services[@]}"; do
        enabled="$(cat "$stage/rollback/enabled-$i" 2>/dev/null || true)"
        case "$enabled" in
          enabled) systemctl enable "${services[$i]}" >/dev/null 2>&1 || true ;;
          enabled-runtime) systemctl enable --runtime "${services[$i]}" >/dev/null 2>&1 || true ;;
        esac
      done
    fi
    if [[ "$netfilter_attempted" -eq 1 && "$restored" -eq 1 && -f "$stage/rollback/file-0" ]]; then
      "$BIN" up -c "$CONF" || log "警告: 重新应用 netfilter 失败，转发端口可能不可用"
    fi
    if [[ "$install_state_modified" -eq 1 ]]; then
      for i in 1 2 3; do
        if [[ -f "$stage/rollback/active-$i" ]]; then
          systemctl restart "${services[$i]}" || true
        else
          systemctl stop "${services[$i]}" || true
        fi
      done
    fi
    if [[ "$service_restart_attempted" -eq 1 && "$was_active" -eq 1 && "$restored" -eq 1 && "$core_ok" -eq 1 ]]; then
      if ! systemctl restart antapp-linkd || ! wait_install_service; then
        log "警告: 旧服务未能稳定启动，请检查 journalctl -u antapp-linkd"
        rollback_ok=0
      fi
    fi
    rm -f -- "$BIN.new" "$BIN.rollback"
    if [[ "$rollback_ok" -ne 1 ]]; then
      log "错误: 回退未全部成功，恢复资料保留在 $stage/rollback；请修复后恢复配置、程序和服务"
      exit "$result"
    fi
    log "安装未完成，原程序、配置和服务状态已恢复"
  fi
  rm -f -- "$BIN.probe.$$"
  rm -rf -- "$stage"
  exit "$result"
}
trap rollback_install EXIT
install -m 0755 "$stage/antapp-linkd" "$BIN.new"
mv -f -- "$BIN.new" "$BIN"

log "初始化配置与 PKI（已存在的一律保留，只同步下面这几项）"
init_args=(-c "$CONF")
[[ -n "$FORWARD_RANGE" ]] && init_args+=(--forward "$FORWARD_RANGE")
[[ -n "$TUNNEL_PORT" ]]   && init_args+=(--listen "0.0.0.0:$TUNNEL_PORT")
[[ -n "$NETWORK" ]]       && init_args+=(--network "$NETWORK")
[[ -n "$MODE" ]]          && init_args+=(--mode "$MODE")

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
install_state_modified=1
"$BIN" install -c "$CONF"

systemctl enable dnsmasq >/dev/null 2>&1 || true
systemctl restart dnsmasq >/dev/null 2>&1 || log "警告: dnsmasq 未启动，隧道 DNS 中继不可用"

log "配置 netfilter（幂等）"
netfilter_attempted=1
"$BIN" down -c "$CONF" >/dev/null 2>&1 || true
"$BIN" up -c "$CONF"

wait_install_service() {
  local pid="" current restarts base="" check
  for check in 1 2 3; do
    sleep 2
    systemctl is-active --quiet antapp-linkd || return 1
    current="$(systemctl show -p MainPID --value antapp-linkd)" || return 1
    restarts="$(systemctl show -p NRestarts --value antapp-linkd)" || return 1
    [[ "$current" =~ ^[1-9][0-9]*$ ]] || return 1
    [[ "$restarts" =~ ^[0-9]+$ ]] || return 1
    [[ -n "$base" ]] || base="$restarts"
    [[ "$restarts" == "$base" ]] || return 1
    [[ -z "$pid" || "$current" == "$pid" ]] || return 1
    pid="$current"
  done
}

log "启动隧道"
service_restart_attempted=1
systemctl restart antapp-linkd
wait_install_service || die "隧道服务未稳定启动，执行回退"

log "当前状态"
"$BIN" status -c "$CONF" || true

trap 'rm -f -- "$BIN.probe.$$"; rm -rf -- "$stage"' EXIT

wan_iface="$(ip -4 route show default 2>/dev/null | awk 'NR==1{print $5}')" || true
if [[ -n "$wan_iface" ]]; then
  modprobe sch_fq 2>/dev/null || true
  if tc qdisc replace dev "$wan_iface" root fq 2>/dev/null &&
     tc qdisc show dev "$wan_iface" 2>/dev/null | head -1 | grep -q 'qdisc fq '; then
    log "出口 qdisc 已切换：$wan_iface -> fq"
  else
    log "警告: $wan_iface 的 qdisc 切不到 fq（继续用现有 qdisc，不影响功能）"
  fi
else
  log "警告: 找不到默认出口网卡，跳过 qdisc 切换"
fi


log "重新签发连接码到 /root（覆盖旧文件，让里面的参数跟上当前配置）"
"$BIN" invite pi-node-01 -c "$CONF" -o /root || log "警告: 签发失败，可手动执行: $BIN invite pi-node-01 -c $CONF -o /root"

EFFECTIVE_MODE="$MODE"
if [[ -z "$EFFECTIVE_MODE" ]]; then
  EFFECTIVE_MODE="$(sed -n 's/.*"mode"[[:space:]]*:[[:space:]]*"\([a-zA-Z]*\)".*/\1/p' "$CONF" 2>/dev/null | head -1)"
  [[ -n "$EFFECTIVE_MODE" ]] || EFFECTIVE_MODE="tcp"
fi

cat <<EOF

[antapp-link] --------------------------------------------------
[antapp-link] 安装完成。连接码文件：/root/pinode.antapp（本次已重新签发）
[antapp-link] 数据通道模式：$EFFECTIVE_MODE
[antapp-link] 把它发给节点机双击导入即可（内含私钥，等同密码，注意保管）。
[antapp-link] 本次只换新了客户端证书；CA 未变，之前发出去的旧连接码依然有效。
[antapp-link] 旧码泄漏过要真正作废，只能重建 CA —— 那会让所有节点掉线并重新导入。
[antapp-link] 只想重启服务、不换连接码时别跑本脚本，直接: systemctl restart antapp-linkd
[antapp-link] --------------------------------------------------
EOF
