#!/usr/bin/env python3
"""读客户端 -order-trace 的输出，断言"同一内层流始终走同一个槽"。

格式（表头之后每行一个包）：
  # ts_us dir slot src sport dst dport proto seq len
  <ts_us> <tx|rx> <slot> <src> <sport> <dst> <dport> <proto> <seq> <len>

为什么这个判据比抓包硬：slot 是这个包**实际走的连接**（tx = 按流哈希选中的槽，
rx = 实际从哪条连接收到），不受任何抓包工具的时间戳影响。

两条断言：
  1. 同方向、同一条流（五元组方向无关规范化）只能落一个槽；
  2. 同一条流的上下行必须落**同一个**槽号 —— 两端用的是同一个 Slot 函数、
     同一份五元组，这点一旦破就说明分槽实现有问题。

  python3 check-slot-affinity.py <trace.log>
"""

import sys
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "order-trace.log"

dir_slots = defaultdict(set)   # (dir, flow) -> {slot}
both_slots = defaultdict(lambda: {"tx": set(), "rx": set()})  # flow -> {tx/rx slots}
counts = defaultdict(int)
total = 0
rows = []

for line in open(path, encoding="utf-8", errors="replace"):
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    f = line.split()
    if len(f) != 10:
        continue
    rows.append(f)

if not rows:
    print("trace 里没有记录 —— 客户端是不是没带 -order-trace 启动？")
    sys.exit(1)

# 成员连接就位的近似时刻：在那之前槽 1..N 一律是空的，包只能落回控制连接（槽 0）。
# 这是文档规定的「死槽落控制连接」兜底，不是分槽错误，断言时要从这里开始算。
rx_multi = [int(r[0]) for r in rows if r[1] == "rx" and r[2] != "0"]
cutoff = min(rx_multi) if rx_multi else 0
skipped = [r for r in rows if int(r[0]) < cutoff]

for f in rows:
    if int(f[0]) < cutoff:
        continue
    _ts, d, slot, src, sport, dst, dport, proto, _seq, _ln = f
    slot = int(slot)
    total += 1
    counts[d] += 1
    ep1, ep2 = (src, sport), (dst, dport)
    flow = (ep1, ep2) if ep1 <= ep2 else (ep2, ep1)
    dir_slots[(d, flow)].add(slot)
    both_slots[flow][d].add(slot)

if total == 0:
    print("成员连接就位之后没有任何记录")
    sys.exit(1)

print("记录 %d 条（上行 %d，下行 %d），涉及 %d 条流"
      % (total, counts["tx"], counts["rx"], len(both_slots)))
print("已排除成员连接就位前的 %d 条（会话建立窗口，包只能落回控制连接）" % len(skipped))
if rx_multi:
    print("（该窗口约 %.1f 秒，截止于 ts=%.1fs）" % (cutoff / 1e6, cutoff / 1e6))

bad_same_dir = {k: v for k, v in dir_slots.items() if len(v) > 1}
bad_cross = {}
for flow, s in both_slots.items():
    if s["tx"] and s["rx"] and s["tx"] != s["rx"]:
        bad_cross[flow] = s

print("记录 %d 条（上行 %d，下行 %d），涉及 %d 条流"
      % (total, counts["tx"], counts["rx"], len(both_slots)))
print("-" * 92)
print("① 同方向单槽：")
if bad_same_dir:
    print("   *** %d 条流跨了多个槽 ***" % len(bad_same_dir))
    for (d, flow), slots in list(bad_same_dir.items())[:5]:
        print("     %s %s:%s -> %s:%s 槽=%s" % (d, flow[0][0], flow[0][1], flow[1][0], flow[1][1], sorted(slots)))
else:
    print("   OK —— %d 条（方向,流）组合全部只落一个槽" % len(dir_slots))

print("② 上下行同槽：")
paired = [f for f, s in both_slots.items() if s["tx"] and s["rx"]]
if bad_cross:
    print("   *** %d 条流的上下行落在不同槽 ***" % len(bad_cross))
    for flow, s in list(bad_cross.items())[:5]:
        print("     %s:%s <-> %s:%s  tx=%s rx=%s"
              % (flow[0][0], flow[0][1], flow[1][0], flow[1][1], sorted(s["tx"]), sorted(s["rx"])))
else:
    print("   OK —— %d 条同时有上下行的流，tx/rx 槽号一致" % len(paired))
print("-" * 92)

# 顺带给出槽位分布，确认流量确实摊开了
dist = defaultdict(int)
for (d, flow), slots in dir_slots.items():
    for s in slots:
        dist[(d, s)] += 1
if dist:
    print("槽位分布（方向:槽 -> 流数）：")
    for k in sorted(dist):
        print("   %s:%d -> %d 条流" % (k[0], k[1], dist[k]))

print()
if not bad_same_dir and not bad_cross:
    print("结论：分槽正确 —— 每条内层流固定一条外层连接，且上下行一致。")
else:
    print("结论：存在跨槽流，需要排查分槽实现。")
