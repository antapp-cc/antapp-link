#!/usr/bin/env python3
"""检查抓到的内层 TCP 流是否保持有序。

判定依据：同一内层流（源/目的五元组）的所有包，如果被分到了不同外层连接，
写入网卡/写入 TUN 就是多个 goroutine 并发做的，抓包会看到 seq 倒退；只看方向
一致的单向流，重传（seq 已见过）不算乱序。

  python3 check-inner-order.py <pcap> [隧道IP]

隧道 IP 用来标注方向：目的=tunnel_ip 是下行（外网→节点机），源=tunnel_ip 是上行。
"""

import re
import subprocess
import sys
from collections import defaultdict

PCAP = sys.argv[1] if len(sys.argv) > 1 else "/root/order.pcap"
TUNNEL_IP = sys.argv[2] if len(sys.argv) > 2 else "10.10.0.2"

raw = subprocess.run(
    ["tcpdump", "-r", PCAP, "-nn", "-S", "tcp"],
    capture_output=True, text=True,
).stdout

pat = re.compile(r"IP (\S+) > (\S+):.*?seq (\d+)(?::(\d+))?.*?length (\d+)")


def direction(src, dst):
    if dst.startswith(TUNNEL_IP + "."):
        return "下行"
    if src.startswith(TUNNEL_IP + "."):
        return "上行"
    return "其它"


flows = defaultdict(list)
for line in raw.splitlines():
    m = pat.search(line)
    if not m:
        continue
    src, dst = m.group(1), m.group(2)
    seq, length = int(m.group(3)), int(m.group(5))
    if length == 0:  # 纯 ACK，不参与有序性判断
        continue
    flows[(src, dst)].append((seq, length))

if not flows:
    print("没有抓到数据包——抓包窗口里隧道没跑流量？")
    sys.exit(1)

stats = defaultdict(lambda: [0, 0, 0])  # 方向 -> [包数, 重传, 乱序]
rows = []
for key, pkts in flows.items():
    max_end, seen, ooo, rt, ex = 0, set(), 0, 0, []
    for seq, length in pkts:
        end = seq + length
        if seq >= max_end:
            max_end = end
        elif seq in seen:
            rt += 1
        else:
            ooo += 1
            if len(ex) < 3:
                ex.append((seq, length, max_end))
        seen.add(seq)
    d = direction(*key)
    rows.append((key, d, len(pkts), ooo, rt, ex))
    s = stats[d]
    s[0] += len(pkts)
    s[1] += rt
    s[2] += ooo

rows.sort(key=lambda r: -r[2])
print("按方向汇总：")
for d in ("上行", "下行", "其它"):
    if d in stats:
        n, rt, ooo = stats[d]
        verdict = "OK" if ooo == 0 else "*** 有乱序 ***"
        print("  %s：流内数据包 %d，重传 %d，乱序 %d  %s" % (d, n, rt, ooo, verdict))
print("-" * 96)
for key, d, n, ooo, rt, ex in rows[:14]:
    verdict = "OK" if ooo == 0 else "*** 乱序 ***"
    print("%-2s %-24s -> %-24s 包=%5d 重传=%4d 乱序=%3d  %s" % (d, key[0], key[1], n, rt, ooo, verdict))
    if ex:
        print("      乱序样本 (seq, len, 已见最大结束位置):", ex)
print("-" * 96)
total_ooo = sum(s[2] for s in stats.values())
if total_ooo == 0:
    print("结论：每个方向的内层流 seq 都单调，同一条流没有跨外层连接乱序。")
else:
    print("结论：存在乱序 —— 同一内层流可能被分到了不同外层连接。")
