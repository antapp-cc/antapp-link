#!/usr/bin/env python3
"""分析 -order-trace 的流量分布：按槽、按进程、单流大小，找出队头阻塞风险。

  python3 analyze-traffic.py <trace.log> [conn-map.txt]

conn-map 是采样期间导出的 `本地端口 进程名 远端:端口 状态`（Get-NetTCPConnection），
用来把内层流对应回进程 —— 它是末段快照，早先关闭的流会显示为 "?"。

判据：
  · 跨槽流应当为 0（同一内层流固定一条外层连接）；
  · 每槽的字节/流数应当接近，否则某些程序会与"大户"挤在同一条连接上排队。
"""

import sys
from collections import defaultdict

trace = sys.argv[1]
cmap_path = sys.argv[2] if len(sys.argv) > 2 else None

port2proc = {}
if cmap_path:
    for line in open(cmap_path, encoding="utf-8", errors="replace"):
        f = line.split()
        if len(f) >= 2:
            port2proc[f[0]] = f[1]

flows = defaultdict(lambda: {"bytes": 0, "pkts": 0, "slots": set(), "dirs": set()})
total_bytes = 0
for line in open(trace, encoding="utf-8", errors="replace"):
    if line.startswith("#"):
        continue
    f = line.split()
    if len(f) != 10:
        continue
    _ts, d, slot, src, sport, dst, dport, _proto, _seq, ln = f
    if src == "10.10.0.2":
        local, remote = sport, "%s:%s" % (dst, dport)
    else:
        local, remote = dport, "%s:%s" % (src, sport)
    fl = flows[(remote, local)]
    fl["bytes"] += int(ln)
    fl["pkts"] += 1
    fl["slots"].add(int(slot))
    fl["dirs"].add(d)
    total_bytes += int(ln)

if not flows:
    print("trace 里没有记录")
    sys.exit(1)

slot_bytes = defaultdict(int)
slot_flows = defaultdict(int)
for (remote, local), fl in flows.items():
    for s in fl["slots"]:
        slot_bytes[s] += fl["bytes"]
        slot_flows[s] += 1

proc_flows = defaultdict(int)
proc_bytes = defaultdict(int)
for (remote, local), fl in flows.items():
    p = port2proc.get(local, "?")
    proc_flows[p] += 1
    proc_bytes[p] += fl["bytes"]

remote_bytes = defaultdict(int)
for (remote, local), fl in flows.items():
    remote_bytes[remote.rsplit(":", 1)[0]] += fl["bytes"]

print("总记录 %.2f MB，%d 条内层流" % (total_bytes / 1e6, len(flows)))
print()
print("=== 按槽（字节按流占用的槽平摊计数）===")
for s in sorted(slot_bytes):
    print("  槽 %d: %4d 条流, %8.2f MB" % (s, slot_flows[s], slot_bytes[s] / 1e6))
print()
print("=== 按进程 ===")
for p, b in sorted(proc_bytes.items(), key=lambda kv: -kv[1])[:10]:
    print("  %-22s %4d 条流, %8.2f MB" % (p, proc_flows[p], b / 1e6))
print()
print("=== 单流最大的 12 条（谁在占带宽）===")
top = sorted(flows.items(), key=lambda kv: -kv[1]["bytes"])[:12]
for (remote, local), fl in top:
    p = port2proc.get(local, "?")
    print("  %-20s -> %-22s %7.2f MB / %6d 包 / 槽 %s"
          % (p, remote, fl["bytes"] / 1e6, fl["pkts"], sorted(fl["slots"])))
print()
print("=== 队头阻塞风险：每槽里最大流占该槽的比例 ===")
per_slot_top = defaultdict(list)
for (remote, local), fl in flows.items():
    for s in fl["slots"]:
        per_slot_top[s].append(fl["bytes"])
for s in sorted(slot_bytes):
    vals = sorted(per_slot_top[s], reverse=True)
    if not vals:
        continue
    share = 100.0 * vals[0] / slot_bytes[s] if slot_bytes[s] else 0
    print("  槽 %d: 最大流 %.2f MB（占该槽 %.0f%%），共 %d 条流"
          % (s, vals[0] / 1e6, share, len(vals)))
print()
bad = [(k, v) for k, v in flows.items() if len(v["slots"]) > 1]
print("=== 跨槽流（同一内层流走了多个槽，应当为 0）===")
print("  %d 条" % len(bad))
for (remote, local), fl in bad[:5]:
    print("  %-20s -> %-22s 槽 %s" % (port2proc.get(local, "?"), remote, sorted(fl["slots"])))
