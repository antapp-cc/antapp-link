#!/usr/bin/env bash
#
# 刷新服务端的「国内域名走国内 DNS」规则（/etc/dnsmasq.d/antapp-cn.conf）。
#
# 为什么需要：DNS 返回哪个 CDN 节点，取决于解析器自己的位置。云服在境外，
# 用 8.8.8.8 解析百度/淘宝会拿到海外节点，客户端的分流表认不出这些 IP，
# 内容就绕道隧道 —— 白占云服带宽（那是硬瓶颈，直接挤到 Pi Node）。
#
#   bash deploy/update-cn-domains.sh
#
# 表来自 felixonmars/dnsmasq-china-list，内容本身就是 dnsmasq 配置格式。
# 装完或更新完 dnsmasq 已自动重启；这一步失败不影响隧道，只是分流不生效。

set -euo pipefail

TARGET=/etc/dnsmasq.d/antapp-cn.conf
PRIMARY="https://cdn.jsdelivr.net/gh/felixonmars/dnsmasq-china-list@master/accelerated-domains.china.conf"
SECONDARY="https://raw.githubusercontent.com/felixonmars/dnsmasq-china-list/master/accelerated-domains.china.conf"

# 表里默认的上游是 114.114.114.114。从境外访问它不稳，统一换成阿里公共 DNS
# —— 它同样返回国内节点，且境外可达性明显更好。
UPSTREAM_FROM="114.114.114.114"
UPSTREAM_TO="223.5.5.5"

command -v curl >/dev/null || { echo "需要 curl" >&2; exit 1; }

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

ok=0
for url in "$PRIMARY" "$SECONDARY"; do
  echo "下载 $url"
  if curl -fsSL --max-time 60 "$url" -o "$TMP" 2>/dev/null; then
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
  # 留一个空文件：dnsmasq 能正常启动，行为退回「不做分流」
  : > "$TARGET"
  echo "两个源都不可用。已把 $TARGET 置空，DNS 分流未启用（隧道本身不受影响）。" >&2
  exit 1
fi

sed "s|/${UPSTREAM_FROM}/|/${UPSTREAM_TO}/|" "$TMP" > "$TARGET"
echo "已写入 $TARGET（$(wc -l < "$TARGET") 条，上游 $UPSTREAM_TO）"

# 从云服自己验证一下国内 DNS 是否可达 —— 不可达的话这套分流就是白配
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
