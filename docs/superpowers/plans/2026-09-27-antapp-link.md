# AntApp Link 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development 或 superpowers:executing-plans 按任务逐步执行本计划。步骤用 `- [ ]` 勾选跟踪。

**Goal:** 写一套两端自研的虚拟专线，取代云服上的 OpenVPN + rinetd 与客户机上的 OpenVPN，内建 31400-31409 的 TCP/UDP 端口转发。

**Architecture:** 两端都只做「虚拟网卡 ↔ 隧道」的搬运，不碰 TCP/IP 协议栈。外层用 TLS 1.3 over TCP（mTLS），内层是长度前缀帧，只有 `IP` 与 `PING/PONG` 两种数据帧加四个握手帧。端口转发由服务端 iptables DNAT 完成，TCP/UDP 天然都支持。

**Tech Stack:** Go 1.26（标准库为主）、`crypto/tls`、`golang.zx2c4.com/wintun`、Linux netfilter、Wintun。

**Spec:** `docs/superpowers/specs/2026-09-27-antapp-link-design.md`

## Global Constraints

- Go module 名：`github.com/antapp-cc/antapp-link`
- 命名一律 `antapp`：二进制 `antapp-linkd`（服务端）/ `antapp-link.exe`（客户端）；网卡 `AntApp Link`；Linux 设备 `antapp0`；iptables 链 `ANTAPP_LINK`；ALPN `antapp-link/1`；连接码 scheme `antapp://`；配置目录 `/etc/antapp-link/` 与 `%ProgramData%\AntAppLink\`
- 隧道端口 `tcp/62233`，网段 `10.10.0.0/24`，服务端 `.1`、客户端 `.2`，MTU `1400`，内层 MSS `1360`
- 并网验证期转发端口用 `31410-31419`，验收通过后改回 `31400-31409`
- 一条专线只服务一个 Pi 节点；客户端只做 Windows
- 不引入除 Wintun 与 `golang.org/x/sys` 之外的第三方运行时依赖
- 所有非 Linux / 非 Windows 平台要有桩实现，保证 `go test ./...` 在 Windows 开发机上全绿
- 幂等：任何配置/规则操作重复执行结果一致；iptables 用自定义链 `ANTAPP_LINK`，`down` 能整体清理
- 不在日志与界面回显连接码中的私钥

## Review Focus

最容易伤到人的五件事，每条都要有测试钉住：

1. **客户端被强杀后残留的路由与 DNS** —— 用户会直接断网，且重启程序也救不回来。必须有 `state.json` 快照 + 启动时自愈。
2. **隧道自噬** —— 云服公网 IP 被自己的默认路由吞进隧道，表现为连不上却没有任何报错。必须有 `/32` 绕行路由 + 测试断言。
3. **内层 MTU 没压住** —— 小包通、网页打不开（典型 VPN 黑洞）。必须有 MSS clamp + MTU 断言。
4. **iptables 重复执行** —— 跑两次 `up` 产生重复规则，或与仍在跑的 `rinetd` 冲突。必须幂等 + 冲突检测。
5. **同名 Wintun 适配器已存在** —— 重复启动或上次没清干净。必须复用而非报错。

---

## 文件结构

```
go.mod / go.sum                      module github.com/antapp-cc/antapp-link
cmd/antapp-linkd/main.go             Linux 服务端入口，子命令分发
cmd/antapp-link/main.go              Windows 客户端入口
internal/proto/frame.go              帧编解码与常量（两端共用）
internal/proto/frame_test.go
internal/pki/pki.go                  CA / 服务端 / 客户端证书生成（无 easy-rsa）
internal/pki/invite.go               连接码生成、解析、校验
internal/pki/invite_test.go
internal/server/config.go            服务端配置读写与默认值
internal/server/server.go            TLS 监听 + 帧循环 + 网卡搬运 + 心跳
internal/server/tun_linux.go         TUN 设备创建与读写（ioctl）
internal/server/tun_other.go         非 Linux 桩
internal/server/netfilter_linux.go   iptables 规则（NAT/DNAT/MSS/INPUT）
internal/server/netfilter_other.go   非 Linux 桩
internal/server/cli.go               init / invite / up / down / run / install / status
internal/client/config.go            客户端配置读写
internal/client/tunnel.go            TLS 连接、帧循环、重连退避、心跳
internal/client/wintun_windows.go    Wintun 适配器创建与读写
internal/client/wintun_other.go      非 Windows 桩
internal/client/netcfg_windows.go    路由 / DNS 接管与还原、快照自愈
internal/client/netcfg_other.go      非 Windows 桩
internal/client/tray_windows.go      托盘图标与菜单
internal/client/tray_other.go        非 Windows 桩
build.ps1                            交叉编译 + 内嵌 wintun.dll + 签名
docs/                                设计与计划
```

拆分理由：`proto` 与 `pki` 是两端共用的纯逻辑，最容易被单测覆盖；`server` 与 `client` 各自按「平台相关 / 平台无关」切开，平台无关部分在 Windows 开发机上也能测。

---

### Task 1: 帧协议

**Files:**
- Create: `internal/proto/frame.go`, `internal/proto/frame_test.go`

**Interfaces:**
- Produces: `proto.Type` 常量（`TypeHello`/`TypeHelloAck`/`TypeIP`/`TypePing`/`TypePong`/`TypeBye`）；`proto.WriteFrame(w io.Writer, t Type, payload []byte) error`；`proto.ReadFrame(r io.Reader) (Type, []byte, error)`；`proto.MaxPayload = 1500`；`proto.ErrPayloadTooLarge`

- [ ] **Step 1: 写失败测试** —— 表驱动覆盖：各类型往返一致；空载荷；`MaxPayload` 边界；超长载荷返回 `ErrPayloadTooLarge`；截断的流返回 `io.ErrUnexpectedEOF`；未知类型被如实读出（前向兼容，由上层决定忽略）
- [ ] **Step 2: 跑测试确认失败** —— `go test ./internal/proto/ -v`，期望 `undefined: proto.WriteFrame`
- [ ] **Step 3: 实现** —— 4 字节头（1 字节 type + 24 位大端长度），`bufio` 友好的读写，长度上限校验
- [ ] **Step 4: 跑测试确认通过** —— `go test ./internal/proto/ -v`
- [ ] **Step 5: 提交** —— `git add internal/proto && git commit -m "feat(proto): 帧编解码"`

### Task 2: PKI 与连接码

**Files:**
- Create: `internal/pki/pki.go`, `internal/pki/invite.go`, `internal/pki/invite_test.go`

**Interfaces:**
- Produces: `pki.Init(dir string) error`；`pki.Invite(dir, name, serverAddr string, tunnel pki.TunnelParams) (Invite, error)`；`Invite.Encode() (string, error)` / `pki.Decode(s string) (Invite, error)`；`pki.ServerTLSConfig(dir string) (*tls.Config, error)`；`pki.ClientTLSConfig(inv Invite) (*tls.Config, error)`
- Consumes: 无

- [ ] **Step 1: 写失败测试** —— `Init` 生成 CA + 服务端证书且重复执行不覆盖已存在的 CA；`Invite` 签发的客户端证书能被 CA 验证；`Encode`/`Decode` 往返一致；**篡改 base64 中一个字节后 `Decode` 必须失败**；服务端 `tls.Config` 能拒绝用别家 CA 签的客户端证书
- [ ] **Step 2: 跑测试确认失败** —— `go test ./internal/pki/ -v`
- [ ] **Step 3: 实现** —— `crypto/x509` 自签 CA，服务端证书带 `ServerAuth`，客户端证书带 `ClientAuth`；连接码是 JSON 的 base64，`antapp://` 前缀；服务端用 `ClientAuth: RequireAndVerifyClientCert` + CA 池
- [ ] **Step 4: 跑测试确认通过** —— `go test ./internal/pki/ -v`
- [ ] **Step 5: 提交** —— `git add internal/pki && git commit -m "feat(pki): 自签 CA 与连接码"`

### Task 3: 服务端隧道

**Files:**
- Create: `internal/server/config.go`, `internal/server/server.go`, `internal/server/tun_linux.go`, `internal/server/tun_other.go`
- Test: `internal/server/config_test.go`

**Interfaces:**
- Consumes: `proto.ReadFrame` / `proto.WriteFrame`、`pki.ServerTLSConfig`
- Produces: `server.Config` 结构体（与 spec §6 的 JSON 对应）；`server.LoadConfig(path) (Config, error)` / `Config.Validate() error`；`server.Run(ctx, cfg) error`

- [ ] **Step 1: 写失败测试** —— 配置校验：网段非法、`client_ip` 不在网段内、`forward_ports` 起止颠倒、`mtu` 越界都必须报错；默认值填充正确
- [ ] **Step 2: 跑测试确认失败** —— `go test ./internal/server/ -v`
- [ ] **Step 3: 实现 TUN** —— `/dev/net/tun` + `TUNSETIFF`（`IFF_TUN|IFF_NO_PI`），`ip link set antapp0 up` + `ip addr add 10.10.0.1/24`；非 Linux 返回明确错误
- [ ] **Step 4: 实现帧循环** —— TLS 握手后校验证书 CN；收 `HELLO` → 回 `HELLO_ACK`；`IP` 帧写 tun，tun 读出的包封 `IP` 帧写 TLS；`PING`/`PONG`；60 秒无帧判离线；新连接踢掉旧连接并发 `BYE`
- [ ] **Step 5: 在 WSL 里跑通** —— `wsl -d Debian -- ./antapp-linkd run -c /tmp/t.json`，宿主机 `ping 10.10.0.1` 之外的验证放到 Task 6
- [ ] **Step 6: 提交** —— `git add internal/server && git commit -m "feat(server): TUN 与隧道帧循环"`

### Task 4: 服务端 netfilter

**Files:**
- Create: `internal/server/netfilter_linux.go`, `internal/server/netfilter_other.go`
- Test: `internal/server/netfilter_test.go`（纯字符串生成部分）

**Interfaces:**
- Produces: `server.Up(cfg) error`、`server.Down() error`；规则生成函数 `server.natRules(cfg) []string` 等（纯函数，便于单测）

- [ ] **Step 1: 写失败测试** —— 生成的规则串必须包含：`MASQUERADE` 对 `10.10.0.0/24`、`DNAT --to-destination 10.10.0.2` 的 **tcp 与 udp 各一条**、`TCPMSS --clamp-mss-to-pmtu`、自定义链 `ANTAPP_LINK`；端口段取自配置而非硬编码
- [ ] **Step 2: 跑测试确认失败** —— `go test ./internal/server/ -run Netfilter -v`
- [ ] **Step 3: 实现** —— 全部走 `ANTAPP_LINK` 链，插入前先 `-C` 检查；`Up` 前检测 `rinetd` 是否在占用同一端口段，占用则明确报错而不是静默冲突
- [ ] **Step 4: 幂等验证** —— WSL 里连跑两次 `up`，`iptables -t nat -S` 输出必须完全一致
- [ ] **Step 5: 提交** —— `git add internal/server && git commit -m "feat(server): netfilter 规则与幂等"`

### Task 5: 服务端 CLI

**Files:**
- Create: `internal/server/cli.go`, `cmd/antapp-linkd/main.go`

**Interfaces:**
- Produces: 子命令 `init` / `invite <name>` / `up` / `down` / `run` / `install` / `status`

- [ ] **Step 1: 实现 `init` + `invite`** —— `invite` 同时输出 `antapp-node-<name>.json` 与单行 `antapp://...`，并把私钥写进文件时 `chmod 600`
- [ ] **Step 2: 实现 `install`** —— 写 `/etc/systemd/system/antapp-linkd.service`（`Restart=always`）+ `antapp-link-up.service`（oneshot，`RemainAfterExit`），`daemon-reload` + `enable`
- [ ] **Step 3: 实现 `status`** —— 显示监听状态、隧道网卡、DNAT 规则、当前客户端连接与在线时长
- [ ] **Step 4: 在 WSL 上端到端验证 CLI** —— `init` → `invite` → `up` → `status` 全链路无报错
- [ ] **Step 5: 提交** —— `git add cmd internal/server && git commit -m "feat(server): CLI 子命令与 systemd 安装"`

### Task 6: 客户端隧道

**Files:**
- Create: `internal/client/config.go`, `internal/client/tunnel.go`, `internal/client/wintun_windows.go`, `internal/client/wintun_other.go`
- Test: `internal/client/tunnel_test.go`

**Interfaces:**
- Consumes: `proto.*`、`pki.ClientTLSConfig`、`pki.Decode`
- Produces: `client.LoadInvite(pathOrCode) (pki.Invite, error)`；`client.Tunnel` 类型带 `Run(ctx) error`、`Stats() Stats`；退避序列函数 `client.backoff(attempt int) time.Duration`

- [ ] **Step 1: 写失败测试** —— 退避序列 1s→2s→4s→…→30s 封顶且不溢出；连接码既能从文件读也能从字符串读
- [ ] **Step 2: 跑测试确认失败** —— `go test ./internal/client/ -v`
- [ ] **Step 3: 实现 Wintun** —— `wintun.CreateAdapter("AntApp Link", "AntApp", guid)`；**已存在同名适配器时先 `OpenAdapter` 复用**，不要报错；ring 容量 4 MiB
- [ ] **Step 4: 实现隧道循环** —— TLS 连接 → `HELLO` → `HELLO_ACK` → 双向搬运；10 秒 `PING`，30 秒无 `PONG` 判定断开重连；断线期间保持网卡与路由不动
- [ ] **Step 5: 用 Task 3/5 的服务端联调** —— 客户端 `ping 10.10.0.1` 通；`curl -4 api.ipify.org` 返回**服务端出口 IP**（WSL 场景下即宿主机出口 IP）
- [ ] **Step 6: 提交** —— `git add internal/client && git commit -m "feat(client): Wintun 与隧道循环"`

### Task 7: 客户端路由与 DNS 接管

**Files:**
- Create: `internal/client/netcfg_windows.go`, `internal/client/netcfg_other.go`
- Test: `internal/client/netcfg_test.go`

**Interfaces:**
- Produces: `netcfg.Snapshot` / `netcfg.Capture() (Snapshot, error)` / `Snapshot.Restore() error` / `Snapshot.Apply(tunnel TunnelParams) error`；快照持久化到 `%ProgramData%\AntAppLink\state.json`

- [ ] **Step 1: 写失败测试** —— 快照 JSON 往返一致；`Apply` 的**命令序列**（在测试里注入一个记录用的 executor）必须包含：云服 IP 的 `/32` 绕行路由**先于**默认路由改写；`Restore` 顺序与 `Apply` 严格相反
- [ ] **Step 2: 跑测试确认失败** —— `go test ./internal/client/ -run Netcfg -v`
- [ ] **Step 3: 实现抓取与还原** —— 用 `netsh` / `route` 命令；枚举所有活动网卡的 IPv4 DNS 并记录原值
- [ ] **Step 4: 实现自愈** —— 启动时若 `state.json` 存在，先无条件 `Restore` 再继续；`Restore` 完成才删文件
- [ ] **Step 5: 实机验证** —— 连接后 `curl -4 api.ipify.org` 显示云服 IP；**强制杀进程**后重启程序能修好网络；正常「断开」后能立即正常上网
- [ ] **Step 6: 提交** —— `git add internal/client && git commit -m "feat(client): 路由与 DNS 接管、崩溃自愈"`

### Task 8: 客户端托盘

**Files:**
- Create: `internal/client/tray_windows.go`, `internal/client/tray_other.go`, `internal/client/autostart_windows.go`, `cmd/antapp-link/main.go`

**Interfaces:**
- Produces: 托盘菜单项：状态行、连接/断开、查看日志、导入连接码、开机自启开关、退出；`autostart.Enable()` / `Disable()` 走计划任务

- [ ] **Step 1: 实现托盘** —— 纯 Go 托盘库；状态行显示隧道 IP 与延迟；图标区分已连接/已断开
- [ ] **Step 2: 实现导入** —— 支持粘贴单行 `antapp://` 与选择 `.json` 文件两种
- [ ] **Step 3: 实现自启** —— `schtasks /create /rl highest /sc onlogon`，禁用时删除任务
- [ ] **Step 4: 实机验证** —— 导入连接码 → 连接成功 → 勾选自启 → 注销重登自动连上
- [ ] **Step 5: 提交** —— `git add internal/client cmd && git commit -m "feat(client): 托盘、导入与开机自启"`

### Task 9: 打包与端到端联调

**Files:**
- Create: `build.ps1`, `THIRD-PARTY-NOTICES.md`

- [ ] **Step 1: 写构建脚本** —— `GOOS=linux GOARCH=amd64` 出 `antapp-linkd`；`GOOS=windows` 出 `antapp-link.exe` 并把 `wintun.dll` 内嵌
- [ ] **Step 2: 附许可原文** —— Wintun Prebuilt Binaries License 全文进 `THIRD-PARTY-NOTICES.md`（spec §7 的合规要求）
- [ ] **Step 3: 打包体积检查** —— 客户端 exe 单文件，除它之外不需要任何安装步骤
- [ ] **Step 4: 干净环境验证** —— 在没装过 OpenVPN 的 Windows 上只拷 exe 跑通（spec 验收 10）
- [ ] **Step 5: 提交** —— `git add build.ps1 THIRD-PARTY-NOTICES.md && git commit -m "build: 交叉编译与第三方许可"`

### Task 10: 云服部署与现网切换

**Files:**
- Create: `deploy/install.sh`, `deploy/README.md`

- [ ] **Step 1: 写安装脚本** —— 上传二进制 → `init` → `invite` → `install` → `up`，每步校验退出码
- [ ] **Step 2: 在用户提供的云服上跑验收** —— spec §12 的 1-11 条逐条过，重点是第 4 条（UDP 转发，现状做不到）与第 11 条（openvpn/rinetd 全停仍正常）
- [ ] **Step 3: 切回正式端口** —— 停 `openvpn-server@antnest-tcp` 与 `rinetd`，`forward_ports` 改回 `31400-31409`，重跑 `down` + `up`
- [ ] **Step 4: 提交** —— `git add deploy && git commit -m "deploy: 云服安装脚本与切换说明"`

---

## 自审记录

- **Spec 覆盖**：§5 协议 → T1；§6 服务端 → T3/T4/T5；§7 客户端 → T6/T8；§8 路由 DNS → T7；§9 MTU → T4/T7；§10 连接码 → T2；§11 迁移 → T10；§12 验收 → T9/T10；§13 风险（许可）→ T9。
- **类型一致性**：`proto.Type`、`pki.Invite`、`server.Config`、`netcfg.Snapshot` 在跨任务的 Interfaces 块里名字与签名统一。
- **Review Focus 落点**：1→T7 Step 4；2→T7 Step 1；3→T4 Step 1；4→T4 Step 4；5→T6 Step 3。
