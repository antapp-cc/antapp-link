#!/usr/bin/env bash
#
# AntApp Link 云服安装 / 升级脚本。
#
# 用法（在云服上以 root 运行，保持 antapp-linkd 与本脚本同目录）：
#
#   bash install.sh                              # 用默认端口段 31410-31419（并网验证期）
#   bash install.sh --forward 31400-31409        # 验收通过、老服务下线后再切回正式端口段
#
# 幂等：重复执行只刷新二进制、配置与 systemd unit。
# **不会重建 CA** —— 重建会让此前发出的所有连接码一起失效。

set -euo pipefail

CONF_DIR=/etc/antapp-link
CONF="$CONF_DIR/server.json"
BIN=/usr/local/bin/antapp-linkd
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

FORWARD_START=31410
FORWARD_END=31419

log() { echo "[antapp-link] $*"; }
die() { echo "[antapp-link] $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --forward)
      range="${2:-}"
      [[ "$range" =~ ^[0-9]+-[0-9]+$ ]] || die "--forward 需要形如 31400-31409 的参数"
      FORWARD_START="${range%-*}"
      FORWARD_END="${range#*-}"
      shift 2
      ;;
    -h|--help)
      sed -n '2,14p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *)
      die "未知参数: $1"
      ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "请用 root 运行"
[[ -f "$SRC_DIR/antapp-linkd" ]] || die "同目录下没有 antapp-linkd，请把它和本脚本一起上传"

log "安装二进制到 $BIN"
install -m 0755 "$SRC_DIR/antapp-linkd" "$BIN.new"
mv -f "$BIN.new" "$BIN"

log "初始化配置与 PKI（已存在的一律不动）"
"$BIN" init -c "$CONF"

if [[ "$FORWARD_START" != "31410" || "$FORWARD_END" != "31419" ]]; then
  current_start="$(sed -n 's/.*"start": *\([0-9]*\).*/\1/p' "$CONF" | head -n1)"
  current_end="$(sed -n 's/.*"end": *\([0-9]*\).*/\1/p' "$CONF" | head -n1)"
  if [[ "$current_start" != "$FORWARD_START" || "$current_end" != "$FORWARD_END" ]]; then
    log "转发端口段 $current_start-$current_end -> $FORWARD_START-$FORWARD_END"
    # 先撤旧规则再改配置：否则 down 会按新端口段去删、旧规则留在链里
    "$BIN" down -c "$CONF" >/dev/null 2>&1 || true
    sed -i "s/\"start\": *$current_start/\"start\": $FORWARD_START/; s/\"end\": *$current_end/\"end\": $FORWARD_END/" "$CONF"
  fi
fi

log "安装 systemd 服务"
"$BIN" install -c "$CONF"

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
