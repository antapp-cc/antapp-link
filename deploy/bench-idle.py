#!/usr/bin/env python3
"""测量「连接空闲一段时间后，再来流量」时的首 2 秒吞吐。

关键在于**复用同一条 TCP 连接**：先用 HTTP keep-alive 跑 1MB 让连接脱离
慢启动，然后空闲 IDLE 秒，再发起一次传输、只统计前 2 秒 —— 这正是
net.ipv4.tcp_slow_start_after_idle 起作用（或不起作用）的那个窗口。
每次新建连接的测法测不到它，因为新连接本来就要慢启动。

  python bench-idle.py            # 空闲 300 秒
  python bench-idle.py 60 2.0     # 空闲 60 秒，统计前 2 秒
"""
import http.client
import sys
import time

HOST, PORT = "10.10.0.1", 18080
PATH = "/big.bin"

idle = int(sys.argv[1]) if len(sys.argv) > 1 else 300
window = float(sys.argv[2]) if len(sys.argv) > 2 else 2.0


def fetch(conn, nbytes):
    conn.request("GET", PATH, headers={"Range": "bytes=0-%d" % (nbytes - 1)})
    return conn.getresponse()


def main():
    conn = http.client.HTTPConnection(HOST, PORT, timeout=180)

    resp = fetch(conn, 1 << 20)
    resp.read()
    print("预热完成（1MB），同一条连接保持；开始空闲 %d 秒 ……" % idle, flush=True)
    time.sleep(idle)

    resp = fetch(conn, 20 << 20)
    start = time.time()
    total = 0
    while True:
        elapsed = time.time() - start
        if elapsed >= window:
            break
        chunk = resp.read(65536)
        if not chunk:
            break
        total += len(chunk)
    elapsed = time.time() - start

    rate = int(total / elapsed)
    print("空闲 {0}s 后前 {1}s：{2} 字节 / {3:.3f}s = {4:,} B/s".format(
        idle, window, total, elapsed, rate))
    conn.close()


if __name__ == "__main__":
    main()
