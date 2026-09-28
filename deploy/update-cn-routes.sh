#!/usr/bin/env bash
#
# 更新客户端内置的国内网段列表（internal/client/cn_routes.txt）。
#
# 列表来自 APNIC 的分配记录。APNIC 每次分配都改，但聚合后的整体变化很慢，
# 一两个月跑一次足够 —— 落下的新网段顶多是走了隧道（慢一点），不会不通。
#
#   bash deploy/update-cn-routes.sh
#
# 跑完记得 review 一下 diff 再提交。

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$REPO/internal/client/cn_routes.txt"
SRC_URL="https://ftp.apnic.net/apnic/stats/apnic/delegated-apnic-latest"

command -v python3 >/dev/null || { echo "需要 python3" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "[1/2] 下载 APNIC 分配记录"
curl -fsSL --max-time 120 "$SRC_URL" -o "$TMP/apnic.txt"
echo "      $(wc -l < "$TMP/apnic.txt") 行"

echo "[2/2] 抽取 CN ipv4，聚合，并只保留 /16 及更短"
python3 - "$TMP/apnic.txt" "$OUT" <<'PY'
import ipaddress
import sys

src, out = sys.argv[1], sys.argv[2]
MAX_PREFIX = 16  # 只保留 /16 及更短，理由见下

nets = []
for line in open(src, encoding='utf-8', errors='replace'):
    parts = line.split('|')
    if len(parts) < 7 or parts[1] != 'CN' or parts[2] != 'ipv4':
        continue
    ip, count = parts[3], int(parts[4])
    # APNIC 第 5 列是地址数量，换算成前缀长度
    prefix = 32 - (count.bit_length() - 1)
    nets.append(ipaddress.ip_network(f"{ip}/{prefix}", strict=False))

collapsed = sorted(ipaddress.collapse_addresses(nets), key=lambda n: (int(n.network_address), n.prefixlen))
kept = [n for n in collapsed if n.prefixlen <= MAX_PREFIX]

header = [
    f"# 中国大陆 IPv4 网段，只保留 /{MAX_PREFIX} 及更短的（{len(kept)} 条）。",
    "#",
    "# 为什么不收全：全量要走 route.exe 逐条写，实测 5494 条 34 秒，用户点一次「连接」",
    f"# 要干等半分钟；而 /{MAX_PREFIX} 及更短只要 5 秒就覆盖国内约 96% 的地址。",
    "# 落下的那部分会走隧道 —— 慢一点，但不会不通。",
    "#",
    "# 来源：APNIC delegated-apnic-latest 的 CN ipv4 记录，ipaddress.collapse_addresses 聚合后按前缀长度筛。",
    "# 更新方法：bash deploy/update-cn-routes.sh",
]

with open(out, 'w', encoding='utf-8', newline='\n') as f:
    f.write("\n".join(header) + "\n")
    for n in kept:
        f.write(f"{n}\n")

print(f"      原始 {len(nets)} 条 -> 聚合 {len(collapsed)} 条 -> 保留 /{MAX_PREFIX} 及更短 {len(kept)} 条")
PY

echo "完成: $OUT"
