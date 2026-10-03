#!/usr/bin/env bash
set -euo pipefail

TARGET=/etc/dnsmasq.d/antapp-cn.conf
PRIMARY="https://cdn.jsdelivr.net/gh/felixonmars/dnsmasq-china-list@master/accelerated-domains.china.conf"
SECONDARY="https://raw.githubusercontent.com/felixonmars/dnsmasq-china-list/master/accelerated-domains.china.conf"
UPSTREAM_FROM="114.114.114.114"
UPSTREAM_TO="223.5.5.5"

if command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" --timeout=60 "$1"; }
elif command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --max-time 60 "$1" -o "$2"; }
else
  echo "需要 wget 或 curl" >&2
  exit 1
fi

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

ok=0
for url in "$PRIMARY" "$SECONDARY"; do
  echo "下载 $url"
  if fetch "$url" "$TMP" 2>/dev/null; then
    lines=$(wc -l < "$TMP")
    if [[ "$lines" -gt 1000 ]]; then
      echo "  拿到 $lines 行"
      ok=1
      break
    fi
    echo "  只有 $lines 行，换下一个源"
  else
    echo "  失败，换下一个源"
  fi
done

if [[ "$ok" -ne 1 ]]; then
  : > "$TARGET"
  echo "两个源都不可用。已把 $TARGET 置空，DNS 分流未启用（隧道本身不受影响）。" >&2
  exit 1
fi

sed "s|/${UPSTREAM_FROM}/|/${UPSTREAM_TO}/|" "$TMP" > "$TARGET"
echo "已写入 $TARGET（$(wc -l < "$TARGET") 条，上游 $UPSTREAM_TO）"

if command -v dig >/dev/null; then
  echo -n "验证 @$UPSTREAM_TO 解析 www.baidu.com: "
  if dig "@$UPSTREAM_TO" www.baidu.com +short +time=3 +tries=1 2>/dev/null | grep -qE '^[0-9]+\.'; then
    dig "@$UPSTREAM_TO" www.baidu.com +short +time=3 +tries=1 2>/dev/null | head -3 | tr '\n' ' '
    echo
  else
    echo "失败（从本机查不到国内 DNS，分流可能无效）" >&2
  fi
fi

systemctl restart dnsmasq >/dev/null 2>&1 || echo "警告: dnsmasq 重启失败，请检查 systemctl status dnsmasq" >&2
echo "完成。"
