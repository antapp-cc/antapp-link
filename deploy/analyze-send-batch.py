#!/usr/bin/env python3
"""量化客户端发送侧的攒批效果（写合并 M4）。

读 -order-trace 的 tx 记录，统计"连续落在同一个槽"的段长度 —— 那正是
pumpFromDevice 收割循环能攒进一批的包数。收割循环一旦发现包属于另一个槽就
立刻 flush 当前批，所以：

  · 段长度普遍 >1 → 攒批有效，多条帧合成一次 Write；
  · 段长度普遍 =1 → 并发流把包交替分到不同槽，每个包单独一次 Write，
    M4 想省掉的那一半 TLS 记录又回来了。

  python3 analyze-send-batch.py <trace.log>
"""

import sys
from collections import Counter
from statistics import median

path = sys.argv[1]
slots = []
for line in open(path, encoding="utf-8", errors="replace"):
    if line.startswith("#"):
        continue
    f = line.split()
    if len(f) == 10 and f[1] == "tx":
        slots.append(int(f[2]))

if not slots:
    print("没有 tx 记录 —— 客户端是不带 -order-trace 启动的？")
    sys.exit(1)

runs = []
cur = 1
for i in range(1, len(slots)):
    if slots[i] == slots[i - 1]:
        cur += 1
    else:
        runs.append(cur)
        cur = 1
runs.append(cur)

dist = Counter(runs)
print("tx 包数 %d，连续同槽段数 %d" % (len(slots), len(runs)))
print("段长度：平均 %.2f，中位数 %s，最大 %d"
      % (sum(runs) / len(runs), median(runs), max(runs)))
print("长度分布（前 12 档）：")
for k in sorted(dist)[:12]:
    print("   %4d 个包 → %6d 段（%5.1f%%）" % (k, dist[k], 100.0 * dist[k] / len(runs)))

single = dist[1] / len(runs) * 100
print()
if single > 80:
    print("判读：%.0f%% 的段只有 1 个包 —— 写合并基本失效，每包一次 Write。" % single)
elif single > 50:
    print("判读：%.0f%% 的段只有 1 个包 —— 写合并收益被并发流吃掉一半。" % single)
else:
    print("判读：只有 %.0f%% 的段是单包，攒批仍然有效。" % single)
