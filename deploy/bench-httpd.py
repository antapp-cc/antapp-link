#!/usr/bin/env python3
"""隧道吞吐对照测试用的多线程 HTTP 服务。

单线程的 `python3 -m http.server` 会把并发下载串行化，测不出多流聚合，
所以这里用 ThreadingHTTPServer。文件从 /root 提供（big.bin）。
"""
import os
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

os.chdir("/root")


class Handler(SimpleHTTPRequestHandler):
    # 必须 HTTP/1.1：空闲突发的测试要在同一条 TCP 连接上复用（keep-alive），
    # HTTP/1.0 每个请求都会关连接，就测不到 slow_start_after_idle 了
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 18080), Handler).serve_forever()
