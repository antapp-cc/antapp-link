# 蚁巢 Pi 节点虚拟专线（antnest-link）设计

日期：2026-09-27
状态：待评审

## 1. 背景

云服 `103.143.11.34` 目前用两套现成软件给 Pi 节点提供「干净出口 + 端口可达」：

| 组件 | 现状 |
|---|---|
| OpenVPN server `antnest-tcp` | tcp/62231，网段 `10.9.0.0/24`，客户端 `10.9.0.2`；`push redirect-gateway def1 bypass-dhcp` 全流量走云服，`push dhcp-option DNS 8.8.8.8 / 149.112.112.112` |
| `rinetd` + `antnest-rinetd-watch` | `0.0.0.0:31400-31409` → `10.9.0.2` 同端口，每 5 秒从 OpenVPN status 日志按证书名反查虚拟 IP |

客户机（Windows Pi 节点机）需要装 `OpenVPN.msi`。

已确认的痛点：

1. `OpenVPN.msi` 从境外下发，几百 MB。项目全貌报告 §3.5 记录：国内到境外下这个包，「慢」比「失败」常见得多。
2. `rinetd` 只转发 TCP。脚本结尾自己打了 `注意: rinetd 不转发 UDP`，所以 31400-31409 的 UDP 不可达。
3. 转发目标靠解析 OpenVPN status 日志反查，查不到就退回硬编码 `10.9.0.2`；为此挂了一个每 5 秒的 systemd 看门狗。
4. OpenVPN 2.7 的 DCO 在 TCP 上不工作，客户端配置必须写 `disable-dco`，否则每 13 秒报一次 `dco connect timeout`（`client-antnest-pinode.ovpn:6`、`_make_client_config.py` 的注释记录了实测过程）。
5. easy-rsa PKI、DH 参数、`crl-verify`、证书续期这一整套都要维护。
6. `rinetd` 是用户态代理，Pi 节点侧看到的源地址全是 `10.9.0.1`，真实对端 IP 丢失。

## 2. 目标

写一套**两端都自研**的虚拟专线，只服务 Pi 节点这一个场景：

- 服务端：Linux 单二进制，取代 `openvpn-server@antnest-tcp` + `rinetd` + 看门狗 + NAT 脚本
- 客户端：Windows 单 exe，取代 `OpenVPN.msi` + OpenVPN GUI，托盘小工具
- 31400-31409 端口转发**内建**，TCP 与 UDP 都支持
- 零外部运行时依赖：不需要装 OpenVPN、不需要 rinetd、不需要 easy-rsa、不需要 .NET/Python

## 3. 非目标

明确不做，避免范围膨胀：

- 不做通用 VPN：不做多用户管理、不做分流规则引擎、不做 Web 控制台
- 不做流量伪装/抗封锁：如果将来 TLS 被针对，再单独评估（预留手段是换 ALPN、换端口、加 TLS 指纹伪装）
- 不做 macOS / Linux 客户端：Pi Node 桌面版的客户机是 Windows
- 不做「一台云服接多个 Pi 节点」：一条专线对一个节点。公网只有一组 31400-31409，多节点需要独立公网 IP 或独立端口段，是另一套设计
- 不做证书吊销列表：靠服务端信任的 CA + `invite` 白名单控制

## 4. 架构

```
Pi 节点机 (Windows)                        云服 (Linux, 公网 103.143.11.34)
┌───────────────────────────┐             ┌────────────────────────────────┐
│ Pi Node (Docker)          │             │ antnest-linkd                  │
│   监听 31400-31409        │             │  ├ antnest0  10.10.0.1/24      │
│        ↕                  │             │  ├ :62233  TLS1.3 mTLS         │
│ Windows 内核 TCP/IP 栈    │             │  └ 隧道 ↔ antnest0 搬运        │
│        ↕                  │             │                                │
│ Wintun "AntNest Link"     │◄──TLS/TCP──►│ netfilter:                     │
│   10.10.0.2/24 gw .1      │             │  MASQUERADE 10.10.0.0/24       │
│        ↕                  │             │  DNAT 31400-31409 → 10.10.0.2  │
│ antnest-link.exe (托盘)   │             │  MSS clamp                     │
└───────────────────────────┘             └────────────────────────────────┘
```

**核心取舍**：两端都只做「虚拟网卡 ↔ 隧道」的搬运，不碰 TCP/IP 协议栈。

- 客户端把隧道收到的 IP 包**写进 Wintun**，由 Windows 内核处理 —— 不需要用户态 TCP 栈
- 服务端把隧道收到的 IP 包**写进 tun**，由 Linux 内核路由和 NAT —— 不需要用户态端口转发器
- 31400-31409 由 iptables DNAT 完成，TCP/UDP 天然都支持，源 IP 保留真实

结果是协议只剩「IP 包 + 心跳」两种数据帧，`rinetd` 和它的 5 秒看门狗整个消失。

## 5. 协议

外层：**TLS 1.3 over TCP**，双向证书（mTLS），ALPN 固定 `antnest-link/1`。
选 TLS 的理由：握手、证书校验、重连退避全用 Go 标准库 `crypto/tls`，不手写任何密码学；且 TCP 这条路已在现网验证可用（`client-antnest-pinode.ovpn:131` 走的 `tcp-client`，当初放弃 UDP 62230 不是偶然）。

内层：长度前缀帧。

```
+--------+--------+--------+--------+------------------+
| type   |      length (24-bit BE)  | payload          |
| 1 byte | 3 bytes                  | length bytes     |
+--------+--------------------------+------------------+
```

`length` 上限 16777215，实现里按 MTU 限制实际值。

| type | 名称 | 方向 | 载荷 |
|---|---|---|---|
| `0x01` | `HELLO` | C→S | JSON `{version, client}` |
| `0x02` | `HELLO_ACK` | S→C | JSON `{tunnel_ip, prefix, gateway, mtu, dns[], mss}` |
| `0x03` | `IP` | 双向 | 一个完整 IPv4 包 |
| `0x04` | `PING` | 双向 | 8 字节纳秒时间戳 |
| `0x05` | `PONG` | 双向 | 原样回显对端时间戳 |
| `0x06` | `BYE` | 双向 | 可选原因字符串 |

保活：客户端每 10 秒 `PING`，30 秒收不到 `PONG` 判定链路死，主动重连；服务端 60 秒收不到任何帧判定客户端离线（记录日志，保留虚拟 IP 分配不变）。

握手：客户端 TLS 建连成功后立刻发 `HELLO`，服务端校验证书 CN 在允许集合内 → 回 `HELLO_ACK`，把客户端虚拟 IP 固定为配置里的 `client_ip`。**不做 IP 池/动态分配** —— 一条专线对一个节点，固定更简单也更好排查。

同一时刻只服务一个客户端。已有连接在跑时收到新的合法握手，行为是**踢掉旧连接、接受新的**（客户机重装或换机时不会卡住），旧连接收到 `BYE`。

## 6. 服务端设计

单个 Linux 二进制 `antnest-linkd`：

```
antnest-linkd init              生成 CA + 服务端证书（无 easy-rsa）
antnest-linkd invite <name>     签发客户端证书，输出连接码
antnest-linkd install           写 systemd unit + iptables oneshot，开机自启
antnest-linkd up                配置 iptables（幂等）
antnest-linkd down              清理 iptables
antnest-linkd run               前台跑隧道（systemd 调用）
antnest-linkd status            显示隧道与转发状态
```

配置 `/etc/antnest-link/server.json`：

```json
{
  "listen": "0.0.0.0:62233",
  "tunnel": {
    "device": "antnest0",
    "network": "10.10.0.0/24",
    "server_ip": "10.10.0.1",
    "client_ip": "10.10.0.2",
    "mtu": 1400
  },
  "dns": ["8.8.8.8", "149.112.112.112"],
  "forward_ports": { "start": 31400, "end": 31409 },
  "pki_dir": "/etc/antnest-link/pki"
}
```

网卡：`/dev/net/tun`，`IFF_TUN | IFF_NO_PI`，配 `10.10.0.1/24`。

iptables 用自定义链 `ANTNEST_LINK`，先建后插，保证幂等、可干净清理：

```
sysctl net.ipv4.ip_forward=1
iptables -t nat -N ANTNEST_LINK
iptables -t nat -A POSTROUTING -s 10.10.0.0/24 -o <wan> -j MASQUERADE
iptables -A FORWARD -s 10.10.0.0/24 -j ACCEPT
iptables -A FORWARD -d 10.10.0.0/24 -j ACCEPT
iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
iptables -A INPUT -p tcp --dport 62233 -j ACCEPT
# 端口转发：TCP 与 UDP 都做
iptables -t nat -A ANTNEST_LINK -p tcp --dport 31400:31409 -j DNAT --to-destination 10.10.0.2
iptables -t nat -A ANTNEST_LINK -p udp --dport 31400:31409 -j DNAT --to-destination 10.10.0.2
iptables -t nat -A PREROUTING -p tcp --dport 31400:31409 -j ANTNEST_LINK
iptables -t nat -A PREROUTING -p udp --dport 31400:31409 -j ANTNEST_LINK
```

DNAT 的回包由 conntrack 自动反向转换，不需要额外 SNAT 规则。客户端未连上时 DNAT 目标不可达，外部连接超时 —— 与现状行为一致。

上面写的是切换后的最终形态。`forward_ports` 是配置项，并网验证期改成 `31410-31419`；`up` 按配置值生成规则、`down` 按自定义链整体清理，切换只需要改配置再跑一次 `down` + `up`。

**不做 DNS 中继**。客户端 DNS 直接用 `HELLO_ACK` 下发的公网 DNS（8.8.8.8/149.112.112.112），靠「默认路由走隧道」保证查询本身也走云服出口。`HELLO_ACK` 里的 `dns` 字段保留扩展位：如果实测出现 DNS 泄漏，服务端加一个极简中继（`10.10.0.1:53` → 上游 UDP/TCP 转发），客户端把 DNS 指向 `10.10.0.1` 即可，协议不用改。

## 7. 客户端设计

单个 Windows exe `antnest-link.exe`，GUI 子系统 + 托盘图标，manifest 声明 `requireAdministrator`（创建虚拟网卡和改路由必须提权）。

内部模块：

| 模块 | 职责 |
|---|---|
| `wintun` | 创建/销毁 Wintun 适配器，读写 ring buffer |
| `tunnel` | TLS 连接、帧编解码、心跳、重连（指数退避 1s→2s→4s…上限 30s） |
| `netcfg` | IP / 路由 / DNS 配置与还原，快照记录 |
| `tray` | 托盘图标、状态、菜单 |
| `store` | 配置读写 `%ProgramData%\AntNestLink\node.conf` |
| `import` | 导入连接码（粘贴单行或选文件） |
| `autostart` | 计划任务注册（要提权，注册表 Run 项不够） |

日志：客户端写 `%ProgramData%\AntNestLink\logs\client.log`（按大小轮转，保留最近 2 份）；服务端走 journald（`journalctl -u antnest-linkd`），另有 `antnest-linkd status` 看隧道与转发状态。

托盘菜单：

```
● 已连接  10.10.0.2   延迟 38 ms
────────────────────────
连接 / 断开
查看日志
导入连接码…
开机自启   [✓]
────────────────────────
退出
```

**Wintun 说明**：虚拟网卡用 WireGuard 官方的 Wintun。`wintun.dll` 用 `go:embed` 内嵌进 exe，运行时释放到 `%ProgramData%\AntNestLink\` 再动态加载，**分发物只有一个 exe**；内嵌的 dll 必须与 exe 架构一致（amd64 / arm64 分开构建）。它是操作系统的网络适配器驱动，不是「要用户另外安装的工具/服务」——用户双击就能用，不需要装 OpenVPN、不需要装 TAP 驱动、不需要单独跑安装程序。实现前需确认 Wintun 的分发许可对本项目（商业分发）适用。

## 8. 路由与 DNS 接管

连接时按顺序执行，断开时逆序还原：

1. 快照现场：默认路由（网关 + 接口）、所有活动网卡的 IPv4 DNS 列表 → 写 `%ProgramData%\AntNestLink\state.json`
2. 创建 Wintun 适配器 `AntNest Link`
3. 配 IP：`10.10.0.2/24`，网关 `10.10.0.1`
4. 加防自噬路由：`<云服 IP>/32` 走**原网关**（否则承载隧道的 TLS/TCP 自己会被送进隧道，死循环）
5. 加默认路由走 `10.10.0.1`，metric 设最低
6. 改写所有活动网卡的 DNS 为下发的 DNS
7. 设 Wintun 接口 MTU 1400

**故意保留原默认路由**（metric 更高）：隧道断了会自动回落到原线路，不会整机断网。

**崩溃自愈**：`state.json` 存在即代表上次没干净退出。程序启动时先做一次还原再连接，避免程序被杀掉后留下坏掉的 DNS 和路由。

## 9. MTU 与 MSS

- Wintun MTU 1400，服务端 tun MTU 1400
- 服务端对转发的 TCP 做 `--clamp-mss-to-pmtu`，内层 MSS 落在 1360
- 这个值与现状一致（现网 `mssfix 1360` + `tun-mtu 1500`），不是新拍的数字

## 10. 配置与连接码

`antnest-linkd invite <name>` 输出两样东西：

1. `antnest-node-<name>.json` 文件，内容：

```json
{
  "server": "103.143.11.34:62233",
  "tunnel_ip": "10.10.0.2",
  "gateway": "10.10.0.1",
  "prefix": 24,
  "mtu": 1400,
  "dns": ["8.8.8.8", "149.112.112.112"],
  "ca_pem": "...",
  "cert_pem": "...",
  "key_pem": "...",
  "created": "2026-09-27T20:38:00+08:00"
}
```

2. 单行 `antnest://<base64(json)>`，方便微信/邮件直接发一段粘贴

客户端两种都能导入。私钥随配置走，所以连接码等同密码，服务端日志和界面上都要避免回显完整内容。

## 11. 与现状并行的迁移路径

不能直接抢占现网资源，并行期用独立端口和独立网段：

| | 现网（OpenVPN + rinetd） | 新方案（antnest-link） |
|---|---|---|
| 隧道端口 | tcp/62231（另有 udp/62230） | **tcp/62233** |
| 隧道网段 | `10.9.0.0/24` | **`10.10.0.0/24`** |
| 转发端口 | 31400-31409 | **先用 31410-31419 验证** |

验证期新方案用 31410-31419，跟 rinetd 占着的 31400-31409 不冲突；验收全过之后再停掉 `openvpn-server@antnest-tcp` 和 `rinetd`，把新方案的 `forward_ports` 改回 31400-31409。

## 12. 验收标准

每条都要有可复现的观测手段，不接受「看起来通了」：

1. 客户机连上后 `curl -4 https://api.ipify.org` 返回**云服公网 IP**（不是本地宽带出口 IP）
2. 客户机上解析 `api.minepi.com` 得到的 IP 与云服上解析一致 —— 这是整套方案存在的理由，必须单独验
3. 第三方机器 `nc -vz 103.143.11.34 <转发端口段>` 逐端口全部连通（并网验证期 `31410-31419`，切换后 `31400-31409`），且 Pi Node 侧能看到对应连接
4. **UDP 同端口段也能通**（现状做不到，是本次的改进项，单独验：客户端侧起 UDP 回显，外部发 UDP 包能收到回包）
5. Pi Node 侧看到的对端源 IP 是**真实外部 IP**，不是 `10.10.0.1`
6. 客户端点「断开」后，默认路由和所有网卡 DNS 还原，能正常上网（`curl` 国内站点正常）
7. 强杀服务端进程（模拟云服重启），客户端 30 秒内自动重连成功
8. 强杀客户端进程，重新启动后能自动把上次残留的路由/DNS 修好再连上
9. 客户机重启后自动连上（开启自启的前提下）
10. 一台**从未装过 OpenVPN** 的干净 Windows 上，只拷 exe 过去就能跑通全流程
11. 云服上 `openvpn` 和 `rinetd` 进程全部停掉时，本方案功能完全正常

## 13. 风险与已知限制

| 风险 | 说明与应对 |
|---|---|
| TCP-over-TCP 重传叠加 | 外层 TCP 丢包时内层也会重传。Pi 节点是低带宽长连接场景，影响可忽略。若实测上传统慢，再评估 UDP 通道（方案 C），协议帧格式不用改，只换外层传输 |
| Wintun 许可 | 商业分发前必须确认 Wintun 的许可条款适用 |
| 杀软误报 | 未签名的 Go exe + 虚拟网卡驱动容易被拦。交付前用现有的 `签名exe.py` 做代码签名 |
| Windows 11 自动 DoH | 8.8.8.8 是已知 DoH 服务器，Windows 可能自动升级为加密 DNS。但 DoH 走 443、443 也在隧道里，解析结果依然干净，不构成泄漏 |
| 客户端需要管理员权限 | 创建虚拟网卡和改路由的硬性要求。开机自启用计划任务（最高权限）而不是注册表 Run 项 |
| 单点：云服挂了就全断 | 与现状一致，不引入新问题 |

## 14. 里程碑

| | 内容 | 完成判据 |
|---|---|---|
| M1 | 隧道打通 | 客户端 ping 通 `10.10.0.1`，`curl api.ipify.org` 显示云服 IP |
| M2 | 全流量 + DNS 接管 + 断线恢复 | 验收 1/2/6/7/8 全过 |
| M3 | 端口转发 31400-31409（TCP+UDP） | 验收 3/4/5 全过 |
| M4 | 托盘 GUI + 连接码导入 + 开机自启 | 验收 9/10 全过 |
| M5 | 云服一键安装 + 现网切换 | 验收 11 全过；停掉 openvpn/rinetd 后改回 31400-31409 |
